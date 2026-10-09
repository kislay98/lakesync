package query

import (
	"context"
	"fmt"
	"strings"
	"time"

	"lakesync/internal/core"
)

// MergeResult reports one raw-to-clean merge.
type MergeResult struct {
	Watermark  int64 // highest _cdc_seq applied (unchanged when nothing new)
	Candidates int64 // raw rows above the previous watermark
	Deleted    int64
	Inserted   int64
	DurationMs int64
}

// MergeStatements returns the SQL that applies raw events with _cdc_seq above
// the watermark and the given generation to the clean table. DuckDB's Iceberg
// MERGE INTO only supports one UPDATE/DELETE action, so the upsert is a DELETE of
// keys that have a newer event followed by an INSERT of the latest non-delete
// version. Both are guarded by _cdc_seq, so re-running them is a no-op. They
// must run inside one transaction (see Merge).
func MergeStatements(alias, ns, rawTable, cleanTable string, cleanColumns []string, watermark, generation int64, batchTable string) []string {
	cols := make([]string, len(cleanColumns))
	for i, c := range cleanColumns {
		cols[i] = quoteIdent(c)
	}
	colList := strings.Join(cols, ", ")
	raw := qualified(alias, ns, rawTable)
	clean := qualified(alias, ns, cleanTable)
	key := quoteIdent(core.ColKey)
	seq := quoteIdent(core.ColSeq)
	return []string{
		fmt.Sprintf(`CREATE OR REPLACE TEMP TABLE %s AS
SELECT * EXCLUDE (_rn) FROM (
  SELECT r.*, row_number() OVER (PARTITION BY %s ORDER BY %s DESC) AS _rn
  FROM %s r
  WHERE %s > %d AND %s = %d
) WHERE _rn = 1`, batchTable, key, seq, raw, seq, watermark, quoteIdent(core.ColGeneration), generation),
		fmt.Sprintf(`DELETE FROM %s t WHERE EXISTS (SELECT 1 FROM %s s WHERE s.%s = t.%s AND s.%s > t.%s)`, clean, batchTable, key, key, seq, seq),
		fmt.Sprintf(`INSERT INTO %s (%s) SELECT %s FROM %s s WHERE s.%s <> 'd' AND NOT EXISTS (SELECT 1 FROM %s t WHERE t.%s = s.%s AND t.%s >= s.%s)`,
			clean, colList, colList, batchTable, quoteIdent(core.ColOp), clean, key, key, seq, seq),
	}
}

// CleanColumns returns the clean table's column names in order.
func (e *Engine) CleanColumns(ctx context.Context, alias, ns, cleanTable string) ([]string, error) {
	cols, err := e.Columns(ctx, alias, ns, cleanTable)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = c.Name
	}
	return out, nil
}

// Merge applies new raw events to the clean table in one DuckDB transaction and
// returns the new watermark.
func (e *Engine) Merge(ctx context.Context, alias, ns, rawTable, cleanTable string, watermark, generation int64) (*MergeResult, error) {
	start := time.Now()
	cols, err := e.CleanColumns(ctx, alias, ns, cleanTable)
	if err != nil {
		return nil, fmt.Errorf("describe clean table: %w", err)
	}
	conn, err := e.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	batch := "_batch_" + strings.NewReplacer(".", "_", "-", "_").Replace(cleanTable)
	stmts := MergeStatements(alias, ns, rawTable, cleanTable, cols, watermark, generation, batch)
	res := &MergeResult{Watermark: watermark}
	// stage the batch outside the transaction (temp table), then look at it
	if _, err := conn.ExecContext(ctx, stmts[0]); err != nil {
		return nil, fmt.Errorf("stage batch: %w", friendlyErr(err))
	}
	defer func() { _, _ = conn.ExecContext(context.WithoutCancel(ctx), "DROP TABLE IF EXISTS "+batch) }()
	var maxSeq *int64
	if err := conn.QueryRowContext(ctx, fmt.Sprintf("SELECT count(*), max(%s) FROM %s", quoteIdent(core.ColSeq), batch)).Scan(&res.Candidates, &maxSeq); err != nil {
		return nil, err
	}
	if res.Candidates == 0 {
		res.DurationMs = time.Since(start).Milliseconds()
		return res, nil
	}
	if _, err := conn.ExecContext(ctx, "BEGIN TRANSACTION"); err != nil {
		return nil, err
	}
	rollback := func() { _, _ = conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK") }
	r1, err := conn.ExecContext(ctx, stmts[1])
	if err != nil {
		rollback()
		return nil, fmt.Errorf("merge delete: %w", friendlyErr(err))
	}
	r2, err := conn.ExecContext(ctx, stmts[2])
	if err != nil {
		rollback()
		return nil, fmt.Errorf("merge insert: %w", friendlyErr(err))
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		rollback()
		return nil, fmt.Errorf("merge commit: %w", friendlyErr(err))
	}
	if n, err := r1.RowsAffected(); err == nil {
		res.Deleted = n
	}
	if n, err := r2.RowsAffected(); err == nil {
		res.Inserted = n
	}
	// the watermark advances to the highest seq in the batch, not the highest seq
	// in raw, so that rows committed after staging are merged next time
	if maxSeq != nil {
		res.Watermark = *maxSeq
	}
	res.DurationMs = time.Since(start).Milliseconds()
	return res, nil
}

// RebuildClean replaces the clean table's content with the latest version of
// every document of the given generation (used after a resync's full copy).
func (e *Engine) RebuildClean(ctx context.Context, alias, ns, rawTable, cleanTable string, generation int64) (*MergeResult, error) {
	start := time.Now()
	cols, err := e.CleanColumns(ctx, alias, ns, cleanTable)
	if err != nil {
		return nil, err
	}
	qcols := make([]string, len(cols))
	for i, c := range cols {
		qcols[i] = quoteIdent(c)
	}
	colList := strings.Join(qcols, ", ")
	conn, err := e.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	batch := "_rebuild_" + strings.NewReplacer(".", "_", "-", "_").Replace(cleanTable)
	stage := fmt.Sprintf(`CREATE OR REPLACE TEMP TABLE %s AS
SELECT %s FROM (
  SELECT r.*, row_number() OVER (PARTITION BY %s ORDER BY %s DESC) AS _rn
  FROM %s r WHERE %s = %d
) WHERE _rn = 1 AND %s <> 'd'`, batch, colList, quoteIdent(core.ColKey), quoteIdent(core.ColSeq), qualified(alias, ns, rawTable), quoteIdent(core.ColGeneration), generation, quoteIdent(core.ColOp))
	if _, err := conn.ExecContext(ctx, stage); err != nil {
		return nil, fmt.Errorf("stage rebuild: %w", friendlyErr(err))
	}
	defer func() { _, _ = conn.ExecContext(context.WithoutCancel(ctx), "DROP TABLE IF EXISTS "+batch) }()
	res := &MergeResult{}
	var maxSeq *int64
	if err := conn.QueryRowContext(ctx, fmt.Sprintf("SELECT count(*), max(%s) FROM %s", quoteIdent(core.ColSeq), batch)).Scan(&res.Candidates, &maxSeq); err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "BEGIN TRANSACTION"); err != nil {
		return nil, err
	}
	rollback := func() { _, _ = conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK") }
	clean := qualified(alias, ns, cleanTable)
	r1, err := conn.ExecContext(ctx, "DELETE FROM "+clean)
	if err != nil {
		rollback()
		return nil, fmt.Errorf("rebuild delete: %w", friendlyErr(err))
	}
	r2, err := conn.ExecContext(ctx, fmt.Sprintf("INSERT INTO %s (%s) SELECT %s FROM %s", clean, colList, colList, batch))
	if err != nil {
		rollback()
		return nil, fmt.Errorf("rebuild insert: %w", friendlyErr(err))
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		rollback()
		return nil, fmt.Errorf("rebuild commit: %w", friendlyErr(err))
	}
	res.Deleted, _ = r1.RowsAffected()
	res.Inserted, _ = r2.RowsAffected()
	if maxSeq != nil {
		res.Watermark = *maxSeq
	}
	res.DurationMs = time.Since(start).Milliseconds()
	return res, nil
}

// Count returns the number of rows of a table.
func (e *Engine) Count(ctx context.Context, alias, ns, table string) (int64, error) {
	var n int64
	err := e.db.QueryRowContext(ctx, "SELECT count(*) FROM "+qualified(alias, ns, table)).Scan(&n)
	return n, err
}

// MaxSeq returns the highest _cdc_seq of a table (0 when empty).
func (e *Engine) MaxSeq(ctx context.Context, alias, ns, table string) (int64, error) {
	var n *int64
	if err := e.db.QueryRowContext(ctx, fmt.Sprintf("SELECT max(%s) FROM %s", quoteIdent(core.ColSeq), qualified(alias, ns, table))).Scan(&n); err != nil {
		return 0, err
	}
	if n == nil {
		return 0, nil
	}
	return *n, nil
}

// DuplicateKeys returns how many keys appear more than once in a clean table
// (must be zero; used by verify and tests).
func (e *Engine) DuplicateKeys(ctx context.Context, alias, ns, table string) (int64, error) {
	var n int64
	err := e.db.QueryRowContext(ctx, fmt.Sprintf("SELECT count(*) FROM (SELECT %s FROM %s GROUP BY 1 HAVING count(*) > 1)", quoteIdent(core.ColKey), qualified(alias, ns, table))).Scan(&n)
	return n, err
}

// StreamRows calls fn for every row of a query (used by verify). Values are
// normalised like the viewer's.
func (e *Engine) StreamRows(ctx context.Context, sqlText string, fn func(cols []string, vals []any) error) error {
	rows, err := e.db.QueryContext(ctx, sqlText)
	if err != nil {
		return friendlyErr(err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		for i := range vals {
			vals[i] = normalizeValue(vals[i])
		}
		if err := fn(cols, vals); err != nil {
			return err
		}
	}
	return rows.Err()
}

// Qualified exposes the quoting helper for other packages' ad-hoc SQL.
func Qualified(alias, ns, table string) string { return qualified(alias, ns, table) }

// QuoteIdent exposes identifier quoting.
func QuoteIdent(s string) string { return quoteIdent(s) }
