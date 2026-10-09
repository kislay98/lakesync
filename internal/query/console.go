package query

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// ErrNotReadOnly is returned for anything but SELECT/EXPLAIN statements.
var ErrNotReadOnly = errors.New("only SELECT and EXPLAIN statements are allowed in the SQL console")

// QueryResult is the SQL console result.
type QueryResult struct {
	Columns    []ColumnMeta `json:"columns"`
	Rows       [][]any      `json:"rows"`
	RowCount   int          `json:"row_count"`
	Truncated  bool         `json:"truncated"`
	DurationMs int64        `json:"duration_ms"`
}

// deniedFunctions must never run from the console (they leak server state).
var deniedFunctions = map[string]bool{
	"duckdb_secrets": true, "read_text": true, "read_blob": true, "glob": true, "getenv": true,
	"duckdb_settings": true, "which_secret": true,
}

var trailingSemicolons = regexp.MustCompile(`;+\s*$`)

// ValidateReadOnly checks that sqlText is exactly one SELECT (or EXPLAIN) by
// asking DuckDB to parse it. Everything else is rejected before execution.
func (e *Engine) ValidateReadOnly(ctx context.Context, sqlText string) error {
	s := strings.TrimSpace(trailingSemicolons.ReplaceAllString(strings.TrimSpace(sqlText), ""))
	if s == "" {
		return errors.New("empty query")
	}
	if strings.Contains(s, ";") {
		return errors.New("one statement at a time")
	}
	explain := false
	if strings.HasPrefix(strings.ToUpper(s), "EXPLAIN ") {
		explain = true
		s = strings.TrimSpace(s[len("EXPLAIN "):])
		if strings.HasPrefix(strings.ToUpper(s), "ANALYZE ") {
			s = strings.TrimSpace(s[len("ANALYZE "):])
		}
	}
	_ = explain
	var out string
	if err := e.db.QueryRowContext(ctx, "SELECT CAST(json_serialize_sql("+quoteString(s)+") AS VARCHAR)").Scan(&out); err != nil {
		return fmt.Errorf("cannot parse query: %s", friendlyErr(err))
	}
	var parsed struct {
		Error        bool   `json:"error"`
		ErrorType    string `json:"error_type"`
		ErrorMessage string `json:"error_message"`
		Statements   []struct {
			Node json.RawMessage `json:"node"`
		} `json:"statements"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		return fmt.Errorf("cannot parse query")
	}
	if parsed.Error {
		if strings.Contains(strings.ToLower(parsed.ErrorType), "not implemented") || strings.Contains(strings.ToLower(parsed.ErrorMessage), "not implemented") {
			return ErrNotReadOnly
		}
		return fmt.Errorf("%s", parsed.ErrorMessage)
	}
	if len(parsed.Statements) != 1 {
		return errors.New("one statement at a time")
	}
	// walk the AST for function names
	var walk func(v any)
	var denied string
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if fn, ok := x["function_name"].(string); ok && deniedFunctions[strings.ToLower(fn)] {
				denied = fn
			}
			for _, c := range x {
				walk(c)
			}
		case []any:
			for _, c := range x {
				walk(c)
			}
		}
	}
	var tree any
	_ = json.Unmarshal(parsed.Statements[0].Node, &tree)
	walk(tree)
	if denied != "" {
		return fmt.Errorf("the function %s is not available in the SQL console", denied)
	}
	return nil
}

// Query runs a read-only statement with a row limit and timeout. When alias is
// set it becomes the default catalog, so `shop.customers` resolves.
func (e *Engine) Query(ctx context.Context, alias, sqlText string, rowLimit int, timeout time.Duration) (*QueryResult, error) {
	if err := e.ValidateReadOnly(ctx, sqlText); err != nil {
		return nil, err
	}
	if rowLimit <= 0 {
		rowLimit = 10000
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	s := strings.TrimSpace(trailingSemicolons.ReplaceAllString(strings.TrimSpace(sqlText), ""))
	conn, err := e.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if alias != "" {
		// Put every namespace of the catalog on the search path so that both
		// `shop.customers` and `customers` resolve without naming the catalog.
		nss, err := e.namespaces(ctx, alias)
		if err != nil {
			return nil, fmt.Errorf("destination is not attached: %s", friendlyErr(err))
		}
		parts := make([]string, 0, len(nss))
		for _, ns := range nss {
			parts = append(parts, alias+"."+ns)
		}
		if len(parts) > 0 {
			if _, err := conn.ExecContext(ctx, "SET search_path = "+quoteString(strings.Join(parts, ","))); err != nil {
				return nil, fmt.Errorf("cannot set the search path: %s", friendlyErr(err))
			}
			// the connection goes back to the pool: leave no catalog references behind
			defer func() { _, _ = conn.ExecContext(context.WithoutCancel(ctx), "RESET search_path") }()
		}
	}
	wrapped := s
	if !strings.HasPrefix(strings.ToUpper(s), "EXPLAIN") {
		wrapped = fmt.Sprintf("SELECT * FROM (%s) AS _console LIMIT %d", s, rowLimit+1)
	}
	start := time.Now()
	rows, err := conn.QueryContext(ctx, wrapped)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("query cancelled after %s", timeout)
		}
		return nil, friendlyErr(err)
	}
	defer rows.Close()
	cts, err := rows.ColumnTypes()
	if err != nil {
		return nil, err
	}
	res := &QueryResult{}
	for _, ct := range cts {
		res.Columns = append(res.Columns, ColumnMeta{Name: ct.Name(), Type: ct.DatabaseTypeName()})
	}
	for rows.Next() {
		vals := make([]any, len(cts))
		ptrs := make([]any, len(cts))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		if len(res.Rows) == rowLimit {
			res.Truncated = true
			break
		}
		for i := range vals {
			vals[i] = normalizeValue(vals[i])
		}
		res.Rows = append(res.Rows, vals)
	}
	if err := rows.Err(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("query cancelled after %s", timeout)
		}
		return nil, friendlyErr(err)
	}
	res.RowCount = len(res.Rows)
	res.DurationMs = time.Since(start).Milliseconds()
	return res, nil
}

// namespaces lists the schemas of an attached catalog.
func (e *Engine) namespaces(ctx context.Context, alias string) ([]string, error) {
	rows, err := e.db.QueryContext(ctx, "SELECT schema_name FROM information_schema.schemata WHERE catalog_name = ? ORDER BY 1", alias)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var ns string
		if err := rows.Scan(&ns); err != nil {
			return nil, err
		}
		out = append(out, ns)
	}
	return out, rows.Err()
}

// SchemaEntry is one autocomplete entry for the console: a table and its columns.
type SchemaEntry struct {
	Namespace string   `json:"namespace"`
	Table     string   `json:"table"`
	Columns   []string `json:"columns"`
}

// Autocomplete lists namespaces, tables and columns of an attached catalog.
func (e *Engine) Autocomplete(ctx context.Context, alias string) ([]SchemaEntry, error) {
	rows, err := e.db.QueryContext(ctx,
		"SELECT table_schema, table_name, column_name FROM information_schema.columns WHERE table_catalog = ? ORDER BY table_schema, table_name, ordinal_position", alias)
	if err != nil {
		return nil, friendlyErr(err)
	}
	defer rows.Close()
	var out []SchemaEntry
	for rows.Next() {
		var ns, tbl, col string
		if err := rows.Scan(&ns, &tbl, &col); err != nil {
			return nil, err
		}
		if n := len(out); n > 0 && out[n-1].Namespace == ns && out[n-1].Table == tbl {
			out[n-1].Columns = append(out[n-1].Columns, col)
			continue
		}
		out = append(out, SchemaEntry{Namespace: ns, Table: tbl, Columns: []string{col}})
	}
	return out, rows.Err()
}
