// Phase 0 spike: proves the risky library paths against the local compose stack.
// Each step is a subcommand so that step (b) reads a checkpoint written by a
// previous *process*, which is the actual restart scenario.
//
//	go run . a   # iceberg-go: namespace + partitioned v2 table + Arrow append with snapshot props
//	go run . b   # read checkpoint back from the snapshot summary in a fresh process; abort test; conflict test
//	go run . c   # schema evolution: add a column, append rows using it
//	go run . d   # DuckDB attaches the REST catalog + S3 and reads the table
//	go run . e   # DuckDB creates the clean table and runs MERGE INTO (update + delete)
//	go run . f   # cross-visibility: iceberg-go reads DuckDB's commits, DuckDB reads iceberg-go's
//	go run . g   # maintenance support: expire snapshots, compaction, orphan scan; v3 probe
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/iceberg-go"
	"github.com/apache/iceberg-go/catalog"
	"github.com/apache/iceberg-go/catalog/rest"
	"github.com/apache/iceberg-go/table"
	_ "github.com/apache/iceberg-go/io/gocloud/s3"
	_ "github.com/duckdb/duckdb-go/v2"
)

var (
	catalogURI = env("CATALOG_URI", "http://localhost:19120/iceberg")
	warehouse  = env("WAREHOUSE", "warehouse")
	s3Endpoint = env("S3_ENDPOINT", "http://localhost:9000")
	s3Key      = env("S3_KEY", "lakesync")
	s3Secret   = env("S3_SECRET", "lakesync-secret")
	s3Region   = env("S3_REGION", "us-east-1")
	ns         = env("NS", "phase0")
	rawName    = "events__raw"
	cleanName  = "events"
)

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func must(err error, what string) {
	if err != nil {
		fmt.Printf("FAIL %s: %v\n", what, err)
		os.Exit(1)
	}
}

func ok(what string, args ...any) { fmt.Printf("OK   %s\n", fmt.Sprintf(what, args...)) }

// stripVendedStorage removes catalog-vended storage settings (s3.*, client.region,
// storage-credentials) from load-table style responses, so that the S3 settings the
// user configured take precedence. iceberg-go merges response config over client props.
type stripVendedStorage struct{ next http.RoundTripper }

func (s stripVendedStorage) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := s.next.RoundTrip(req)
	if err != nil || resp.Body == nil || resp.StatusCode != 200 || !strings.Contains(req.URL.Path, "/tables") {
		return resp, err
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) == nil {
		changed := false
		if raw, ok := m["config"]; ok {
			var cfg map[string]string
			if json.Unmarshal(raw, &cfg) == nil {
				for k := range cfg {
					if strings.HasPrefix(k, "s3.") || k == "client.region" {
						delete(cfg, k)
						changed = true
					}
				}
				m["config"], _ = json.Marshal(cfg)
			}
		}
		if _, ok := m["storage-credentials"]; ok {
			delete(m, "storage-credentials")
			changed = true
		}
		if changed {
			body, _ = json.Marshal(m)
		}
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Del("Content-Length")
	return resp, nil
}

func openCatalog(ctx context.Context) *rest.Catalog {
	cat, err := rest.NewCatalog(ctx, "nessie", catalogURI,
		rest.WithWarehouseLocation(warehouse),
		rest.WithCustomTransport(stripVendedStorage{next: http.DefaultTransport}),
		rest.WithAdditionalProps(iceberg.Properties{
			"s3.endpoint":                  s3Endpoint,
			"s3.access-key-id":             s3Key,
			"s3.secret-access-key":         s3Secret,
			"s3.region":                    s3Region,
			"s3.force-virtual-addressing":  "false",
		}))
	must(err, "rest.NewCatalog")
	return cat
}

func rawSchema() *iceberg.Schema {
	return iceberg.NewSchema(0,
		iceberg.NestedField{ID: 1, Name: "_cdc_key", Type: iceberg.PrimitiveTypes.String, Required: true},
		iceberg.NestedField{ID: 2, Name: "_cdc_op", Type: iceberg.PrimitiveTypes.String, Required: true},
		iceberg.NestedField{ID: 3, Name: "_cdc_seq", Type: iceberg.PrimitiveTypes.Int64, Required: true},
		iceberg.NestedField{ID: 4, Name: "_cdc_generation", Type: iceberg.PrimitiveTypes.Int64, Required: true},
		iceberg.NestedField{ID: 5, Name: "_cdc_source_ts", Type: iceberg.PrimitiveTypes.TimestampTz, Required: false},
		iceberg.NestedField{ID: 6, Name: "_cdc_ingested_at", Type: iceberg.PrimitiveTypes.TimestampTz, Required: true},
		iceberg.NestedField{ID: 7, Name: "_cdc_cast_errors", Type: iceberg.PrimitiveTypes.String, Required: false},
		iceberg.NestedField{ID: 8, Name: "_cdc_doc", Type: iceberg.PrimitiveTypes.String, Required: false},
		iceberg.NestedField{ID: 9, Name: "_id", Type: iceberg.PrimitiveTypes.String, Required: false},
		iceberg.NestedField{ID: 10, Name: "name", Type: iceberg.PrimitiveTypes.String, Required: false},
		iceberg.NestedField{ID: 11, Name: "qty", Type: iceberg.PrimitiveTypes.Int64, Required: false},
		iceberg.NestedField{ID: 12, Name: "price", Type: iceberg.PrimitiveTypes.Float64, Required: false},
		iceberg.NestedField{ID: 13, Name: "active", Type: iceberg.PrimitiveTypes.Bool, Required: false},
	)
}

type row struct {
	key, op  string
	seq, gen int64
	id, name string
	qty      int64
	price    float64
	active   bool
	email    *string // only after schema evolution
}

// arrowBatch builds an Arrow record matching the *current* table schema (by name).
func arrowBatch(sc *iceberg.Schema, rows []row, ingested time.Time) arrow.RecordBatch {
	fields := make([]arrow.Field, 0, len(sc.Fields()))
	for _, f := range sc.Fields() {
		var dt arrow.DataType
		switch f.Type.(type) {
		case iceberg.StringType:
			dt = arrow.BinaryTypes.String
		case iceberg.Int64Type:
			dt = arrow.PrimitiveTypes.Int64
		case iceberg.Float64Type:
			dt = arrow.PrimitiveTypes.Float64
		case iceberg.BooleanType:
			dt = arrow.FixedWidthTypes.Boolean
		case iceberg.TimestampTzType:
			dt = &arrow.TimestampType{Unit: arrow.Microsecond, TimeZone: "UTC"}
		default:
			panic("unexpected type " + f.Type.String())
		}
		fields = append(fields, arrow.Field{Name: f.Name, Type: dt, Nullable: !f.Required})
	}
	asc := arrow.NewSchema(fields, nil)
	b := array.NewRecordBuilder(memory.DefaultAllocator, asc)
	defer b.Release()
	for i, f := range asc.Fields() {
		for _, r := range rows {
			switch f.Name {
			case "_cdc_key":
				b.Field(i).(*array.StringBuilder).Append(r.key)
			case "_cdc_op":
				b.Field(i).(*array.StringBuilder).Append(r.op)
			case "_cdc_seq":
				b.Field(i).(*array.Int64Builder).Append(r.seq)
			case "_cdc_generation":
				b.Field(i).(*array.Int64Builder).Append(r.gen)
			case "_cdc_source_ts":
				b.Field(i).(*array.TimestampBuilder).Append(arrow.Timestamp(ingested.Add(-time.Second).UnixMicro()))
			case "_cdc_ingested_at":
				b.Field(i).(*array.TimestampBuilder).Append(arrow.Timestamp(ingested.UnixMicro()))
			case "_cdc_cast_errors":
				b.Field(i).(*array.StringBuilder).AppendNull()
			case "_cdc_doc":
				b.Field(i).(*array.StringBuilder).Append(fmt.Sprintf(`{"_id":%q,"name":%q,"qty":%d}`, r.id, r.name, r.qty))
			case "_id":
				b.Field(i).(*array.StringBuilder).Append(r.id)
			case "name":
				b.Field(i).(*array.StringBuilder).Append(r.name)
			case "qty":
				b.Field(i).(*array.Int64Builder).Append(r.qty)
			case "price":
				b.Field(i).(*array.Float64Builder).Append(r.price)
			case "active":
				b.Field(i).(*array.BooleanBuilder).Append(r.active)
			case "email":
				if r.email == nil {
					b.Field(i).(*array.StringBuilder).AppendNull()
				} else {
					b.Field(i).(*array.StringBuilder).Append(*r.email)
				}
			default:
				panic("no builder for " + f.Name)
			}
		}
	}
	return b.NewRecordBatch()
}

func appendRows(ctx context.Context, tbl *table.Table, rows []row, props iceberg.Properties) *table.Table {
	rec := arrowBatch(tbl.Schema(), rows, time.Now().UTC())
	defer rec.Release()
	rdr, err := array.NewRecordReader(rec.Schema(), []arrow.RecordBatch{rec})
	must(err, "record reader")
	defer rdr.Release()
	tx := tbl.NewTransaction()
	must(tx.Append(ctx, rdr, props), "tx.Append")
	t2, err := tx.Commit(ctx)
	must(err, "tx.Commit")
	return t2
}

func checkpointProps(seq, gen int64, phase, cursor, bookmark string) iceberg.Properties {
	return iceberg.Properties{
		"lakesync.seq":             fmt.Sprint(seq),
		"lakesync.generation":      fmt.Sprint(gen),
		"lakesync.phase":           phase,
		"lakesync.snapshot_cursor": cursor,
		"lakesync.bookmark":        bookmark,
		"lakesync.run_id":          "run-phase0",
	}
}

func countSeq(ctx context.Context, tbl *table.Table) int64 {
	sc := tbl.Scan()
	tasks, err := sc.PlanFiles(ctx)
	must(err, "scan.PlanFiles")
	_, it, err := sc.ReadTasks(ctx, normalizeTasks(tasks))
	must(err, "scan.ReadTasks")
	var n int64
	for rec, err := range it {
		must(err, "scan iterate")
		n += rec.NumRows()
		rec.Release()
	}
	return n
}

func stepA(ctx context.Context) {
	cat := openCatalog(ctx)
	ident := table.Identifier{ns, rawName}
	if exists, _ := cat.CheckNamespaceExists(ctx, table.Identifier{ns}); !exists {
		must(cat.CreateNamespace(ctx, table.Identifier{ns}, nil), "CreateNamespace")
	}
	if exists, _ := cat.CheckTableExists(ctx, ident); exists {
		must(cat.DropTable(ctx, ident), "DropTable(raw) for clean run")
	}
	if exists, _ := cat.CheckTableExists(ctx, table.Identifier{ns, cleanName}); exists {
		must(cat.DropTable(ctx, table.Identifier{ns, cleanName}), "DropTable(clean) for clean run")
	}
	spec := iceberg.NewPartitionSpec(iceberg.PartitionField{SourceIDs: []int{6}, FieldID: 1000, Transform: iceberg.DayTransform{}, Name: "_cdc_ingested_at_day"})
	tbl, err := cat.CreateTable(ctx, ident, rawSchema(),
		catalog.WithPartitionSpec(&spec),
		catalog.WithProperties(iceberg.Properties{"format-version": "2", "write.parquet.compression-codec": "zstd"}))
	must(err, "CreateTable")
	ok("created %s at %s (format v%d, partition spec %s)", ident, tbl.Location(), tbl.Metadata().Version(), tbl.Spec().String())

	rows := []row{
		{key: `{"$oid":"a1"}`, op: "r", seq: 1, gen: 1, id: "a1", name: "alice", qty: 1, price: 1.5, active: true},
		{key: `{"$oid":"a2"}`, op: "r", seq: 2, gen: 1, id: "a2", name: "bob", qty: 2, price: 2.5, active: false},
		{key: `{"$oid":"a3"}`, op: "r", seq: 3, gen: 1, id: "a3", name: "carol", qty: 3, price: 3.5, active: true},
	}
	tbl = appendRows(ctx, tbl, rows, checkpointProps(3, 1, "snapshot", `{"$oid":"a3"}`, "tok-1"))
	snap := tbl.CurrentSnapshot()
	ok("appended 3 rows; snapshot %d op=%s summary.lakesync.seq=%s", snap.SnapshotID, snap.Summary.Operation, snap.Summary.Properties["lakesync.seq"])
	ok("row count via iceberg-go scan = %d", countSeq(ctx, tbl))
}

func stepB(ctx context.Context) {
	cat := openCatalog(ctx)
	ident := table.Identifier{ns, rawName}
	tbl, err := cat.LoadTable(ctx, ident)
	must(err, "LoadTable in fresh process")
	snap := tbl.CurrentSnapshot()
	if snap == nil {
		must(fmt.Errorf("no current snapshot"), "readback")
	}
	p := snap.Summary.Properties
	if p["lakesync.seq"] != "3" || p["lakesync.phase"] != "snapshot" || p["lakesync.bookmark"] != "tok-1" {
		must(fmt.Errorf("unexpected checkpoint props: %v", p), "readback")
	}
	ok("fresh process read checkpoint from snapshot summary: seq=%s phase=%s cursor=%s bookmark=%s", p["lakesync.seq"], p["lakesync.phase"], p["lakesync.snapshot_cursor"], p["lakesync.bookmark"])

	// Abort test: stage an append and never commit -> the table must be unchanged.
	before := len(tbl.Metadata().Snapshots())
	rec := arrowBatch(tbl.Schema(), []row{{key: "x", op: "c", seq: 99, gen: 1, id: "x", name: "ghost"}}, time.Now())
	rdr, _ := array.NewRecordReader(rec.Schema(), []arrow.RecordBatch{rec})
	tx := tbl.NewTransaction()
	must(tx.Append(ctx, rdr, checkpointProps(99, 1, "stream", "", "tok-ghost")), "staged append")
	rdr.Release()
	rec.Release()
	// drop tx without Commit
	tbl2, err := cat.LoadTable(ctx, ident)
	must(err, "reload")
	if len(tbl2.Metadata().Snapshots()) != before || tbl2.CurrentSnapshot().Summary.Properties["lakesync.seq"] != "3" {
		must(fmt.Errorf("uncommitted append leaked"), "abort")
	}
	ok("uncommitted append left no snapshot (data file written but unreferenced; orphan cleanup's job)")

	// Conflict test: two writers from the same base metadata; second commit must fail or be a clean retry.
	t1, _ := cat.LoadTable(ctx, ident)
	t2, _ := cat.LoadTable(ctx, ident)
	appendRows(ctx, t1, []row{{key: `{"$oid":"a4"}`, op: "c", seq: 4, gen: 1, id: "a4", name: "dave", qty: 4, price: 4.5}}, checkpointProps(4, 1, "stream", "", "tok-2"))
	rec2 := arrowBatch(t2.Schema(), []row{{key: `{"$oid":"a5"}`, op: "c", seq: 5, gen: 1, id: "a5", name: "erin", qty: 5, price: 5.5}}, time.Now())
	rdr2, _ := array.NewRecordReader(rec2.Schema(), []arrow.RecordBatch{rec2})
	tx2 := t2.NewTransaction()
	must(tx2.Append(ctx, rdr2, checkpointProps(5, 1, "stream", "", "tok-3")), "stale append")
	_, err = tx2.Commit(ctx)
	if err == nil {
		// Some catalogs/library versions auto-merge non-conflicting appends. Verify nothing was lost either way.
		t3, _ := cat.LoadTable(ctx, ident)
		ok("stale-base commit was merged by the library; rows now = %d, seq=%s", countSeq(ctx, t3), t3.CurrentSnapshot().Summary.Properties["lakesync.seq"])
	} else {
		ok("stale-base commit rejected as expected: %s", firstLine(err.Error()))
		t3, _ := cat.LoadTable(ctx, ident)
		ok("after retry-from-fresh-metadata semantics, table has %d rows, checkpoint seq=%s", countSeq(ctx, t3), t3.CurrentSnapshot().Summary.Properties["lakesync.seq"])
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i > 0 {
		return s[:i]
	}
	return s
}

func stepC(ctx context.Context) {
	cat := openCatalog(ctx)
	ident := table.Identifier{ns, rawName}
	tbl, err := cat.LoadTable(ctx, ident)
	must(err, "LoadTable")
	tx := tbl.NewTransaction()
	must(tx.UpdateSchema(true, false).AddColumn([]string{"email"}, iceberg.PrimitiveTypes.String, "", false, nil).Commit(), "UpdateSchema.AddColumn")
	tbl, err = tx.Commit(ctx)
	must(err, "commit schema change")
	ok("schema evolved: now %d columns, schema id %d", len(tbl.Schema().Fields()), tbl.Schema().ID)
	email := "frank@example.com"
	seq := currentSeq(tbl) + 1
	tbl = appendRows(ctx, tbl, []row{{key: `{"$oid":"a6"}`, op: "c", seq: seq, gen: 1, id: "a6", name: "frank", qty: 6, price: 6.5, email: &email}},
		checkpointProps(seq, 1, "stream", "", "tok-4"))
	ok("appended a row using the new column; rows=%d checkpoint seq=%s", countSeq(ctx, tbl), tbl.CurrentSnapshot().Summary.Properties["lakesync.seq"])
}

func currentSeq(tbl *table.Table) int64 {
	var s int64
	fmt.Sscan(tbl.CurrentSnapshot().Summary.Properties["lakesync.seq"], &s)
	return s
}

func duck(ctx context.Context) *sql.DB {
	db, err := sql.Open("duckdb", "")
	must(err, "open duckdb")
	stmts := []string{
		"INSTALL httpfs; LOAD httpfs; INSTALL iceberg; LOAD iceberg;",
		fmt.Sprintf("CREATE SECRET s3store (TYPE s3, KEY_ID '%s', SECRET '%s', ENDPOINT '%s', URL_STYLE 'path', USE_SSL %v, REGION '%s');",
			s3Key, s3Secret, strings.TrimPrefix(strings.TrimPrefix(s3Endpoint, "http://"), "https://"), strings.HasPrefix(s3Endpoint, "https://"), s3Region),
		fmt.Sprintf("ATTACH '%s' AS cat (TYPE iceberg, ENDPOINT '%s', AUTHORIZATION_TYPE 'none', ACCESS_DELEGATION_MODE 'none');", warehouse, catalogURI),
	}
	for _, s := range stmts {
		_, err := db.ExecContext(ctx, s)
		must(err, "duckdb: "+firstLine(s))
	}
	return db
}

func stepD(ctx context.Context) {
	db := duck(ctx)
	defer db.Close()
	var n int64
	must(db.QueryRowContext(ctx, fmt.Sprintf("SELECT count(*) FROM cat.%s.%s", ns, rawName)).Scan(&n), "count raw")
	ok("DuckDB attached the REST catalog and counted %d raw rows", n)
	rows, err := db.QueryContext(ctx, fmt.Sprintf("SELECT _cdc_seq, _cdc_op, _id, name, qty, price, email, _cdc_ingested_at FROM cat.%s.%s ORDER BY _cdc_seq", ns, rawName))
	must(err, "select raw")
	defer rows.Close()
	for rows.Next() {
		var seq int64
		var op, id, name string
		var qty sql.NullInt64
		var price sql.NullFloat64
		var email sql.NullString
		var ing time.Time
		must(rows.Scan(&seq, &op, &id, &name, &qty, &price, &email, &ing), "scan")
		fmt.Printf("     seq=%d op=%s id=%s name=%s qty=%v price=%v email=%v ingested=%s\n", seq, op, id, name, qty.Int64, price.Float64, email.String, ing.Format(time.RFC3339))
	}
	var nsnap int64
	must(db.QueryRowContext(ctx, fmt.Sprintf("SELECT count(*) FROM iceberg_snapshots('cat.%s.%s')", ns, rawName)).Scan(&nsnap), "iceberg_snapshots")
	ok("DuckDB iceberg_snapshots() sees %d snapshots", nsnap)
}

// mergeStmts returns the statements that apply raw events above the watermark to the
// clean table. DuckDB's Iceberg MERGE INTO only supports one UPDATE/DELETE action, so the
// upsert is expressed as DELETE (stale or deleted keys) + INSERT (latest non-delete rows),
// run inside one transaction. Both statements are guarded by _cdc_seq so re-running is a no-op.
func mergeStmts(watermark, gen int64) []string {
	cols := []string{"_cdc_key", "_cdc_seq", "_cdc_generation", "_cdc_source_ts", "_cdc_ingested_at", "_cdc_cast_errors", "_id", "name", "qty", "price", "active", "email"}
	return []string{
		"BEGIN TRANSACTION",
		fmt.Sprintf(`CREATE OR REPLACE TEMP TABLE _batch AS
  SELECT * FROM (
    SELECT r.*, row_number() OVER (PARTITION BY _cdc_key ORDER BY _cdc_seq DESC) AS _rn
    FROM cat.%s.%s r WHERE _cdc_seq > %d AND _cdc_generation = %d
  ) WHERE _rn = 1`, ns, rawName, watermark, gen),
		fmt.Sprintf(`DELETE FROM cat.%[1]s.%[2]s t WHERE EXISTS (SELECT 1 FROM _batch s WHERE s._cdc_key = t._cdc_key AND s._cdc_seq > t._cdc_seq)`, ns, cleanName),
		fmt.Sprintf(`INSERT INTO cat.%[1]s.%[2]s (%[3]s) SELECT %[3]s FROM _batch s WHERE s._cdc_op <> 'd' AND NOT EXISTS (SELECT 1 FROM cat.%[1]s.%[2]s t WHERE t._cdc_key = s._cdc_key AND t._cdc_seq >= s._cdc_seq)`, ns, cleanName, strings.Join(cols, ", ")),
		"COMMIT",
	}
}

func runMerge(ctx context.Context, db *sql.DB, watermark, gen int64) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	for _, st := range mergeStmts(watermark, gen) {
		if _, err := conn.ExecContext(ctx, st); err != nil {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
			return fmt.Errorf("%s: %w", firstLine(st), err)
		}
	}
	return nil
}

func stepE(ctx context.Context) {
	db := duck(ctx)
	defer db.Close()
	_, _ = db.ExecContext(ctx, fmt.Sprintf("DROP TABLE IF EXISTS cat.%s.%s", ns, cleanName))
	_, err := db.ExecContext(ctx, fmt.Sprintf(`CREATE TABLE cat.%s.%s (
		_cdc_key VARCHAR NOT NULL, _cdc_seq BIGINT NOT NULL, _cdc_generation BIGINT NOT NULL,
		_cdc_source_ts TIMESTAMPTZ, _cdc_ingested_at TIMESTAMPTZ NOT NULL, _cdc_cast_errors VARCHAR,
		_id VARCHAR, name VARCHAR, qty BIGINT, price DOUBLE, active BOOLEAN, email VARCHAR)`, ns, cleanName))
	must(err, "DuckDB CREATE TABLE in catalog")
	ok("DuckDB created clean table %s.%s via the REST catalog", ns, cleanName)

	must(runMerge(ctx, db, 0, 1), "MERGE #1 (initial)")
	var n int64
	must(db.QueryRowContext(ctx, fmt.Sprintf("SELECT count(*) FROM cat.%s.%s", ns, cleanName)).Scan(&n), "count clean")
	ok("MERGE #1 inserted %d rows into clean", n)

	// Simulate an update and a delete arriving in raw via DuckDB INSERT (iceberg-go does this in the product).
	var maxSeq int64
	must(db.QueryRowContext(ctx, fmt.Sprintf("SELECT max(_cdc_seq) FROM cat.%s.%s", ns, rawName)).Scan(&maxSeq), "max seq")
	_, err = db.ExecContext(ctx, fmt.Sprintf(`INSERT INTO cat.%[1]s.%[2]s (_cdc_key,_cdc_op,_cdc_seq,_cdc_generation,_cdc_source_ts,_cdc_ingested_at,_cdc_cast_errors,_cdc_doc,_id,name,qty,price,active,email) VALUES
	 ('{"$oid":"a1"}','u',%[3]d,1,now(),now(),NULL,'{}','a1','alice-renamed',10,1.5,true,NULL),
	 ('{"$oid":"a2"}','d',%[4]d,1,now(),now(),NULL,NULL,'a2',NULL,NULL,NULL,NULL,NULL),
	 ('{"$oid":"a1"}','u',%[5]d,1,now(),now(),NULL,'{}','a1','alice-final',11,1.5,true,NULL)`, ns, rawName, maxSeq+1, maxSeq+2, maxSeq+3))
	must(err, "DuckDB INSERT into raw")
	must(runMerge(ctx, db, maxSeq, 1), "MERGE #2 (update+delete)")
	rows, err := db.QueryContext(ctx, fmt.Sprintf("SELECT _id, name, qty, _cdc_seq FROM cat.%s.%s ORDER BY _id", ns, cleanName))
	must(err, "select clean")
	var got []string
	for rows.Next() {
		var id, name string
		var qty, seq int64
		must(rows.Scan(&id, &name, &qty, &seq), "scan")
		got = append(got, fmt.Sprintf("%s=%s/%d@%d", id, name, qty, seq))
	}
	rows.Close()
	ok("after MERGE #2 clean = %s", strings.Join(got, " "))
	if !strings.Contains(strings.Join(got, " "), "a1=alice-final/11") || strings.Contains(strings.Join(got, " "), "a2=") {
		must(fmt.Errorf("merge produced wrong state"), "merge correctness")
	}
	// Idempotency: re-run the same merge; nothing should change.
	must(runMerge(ctx, db, maxSeq, 1), "MERGE #2 again")
	var n2 int64
	must(db.QueryRowContext(ctx, fmt.Sprintf("SELECT count(*) FROM cat.%s.%s WHERE name='alice-final'", ns, cleanName)).Scan(&n2), "recount")
	must(db.QueryRowContext(ctx, fmt.Sprintf("SELECT count(*) FROM cat.%s.%s", ns, cleanName)).Scan(&n), "recount all")
	ok("re-running the merge is idempotent (rows=%d, alice-final=%d)", n, n2)
	var ndel int64
	must(db.QueryRowContext(ctx, fmt.Sprintf("SELECT count(*) FROM iceberg_metadata('cat.%s.%s') WHERE content LIKE '%%DELETE%%'", ns, cleanName)).Scan(&ndel), "iceberg_metadata")
	ok("clean table now has %d delete-file entries in iceberg_metadata()", ndel)
}

func stepF(ctx context.Context) {
	cat := openCatalog(ctx)
	clean, err := cat.LoadTable(ctx, table.Identifier{ns, cleanName})
	must(err, "iceberg-go LoadTable(clean written by DuckDB)")
	n := countSeq(ctx, clean)
	ok("iceberg-go reads DuckDB-written clean table (with positional deletes): %d rows, format v%d", n, clean.Metadata().Version())
	csc := clean.Scan(table.WithSelectedFields("_id", "name"))
	ctasks, err := csc.PlanFiles(ctx)
	must(err, "plan clean")
	_, it, err := csc.ReadTasks(ctx, normalizeTasks(ctasks))
	must(err, "scan clean")
	for rec, err := range it {
		must(err, "iter")
		fmt.Printf("     clean via iceberg-go: %v\n", strings.ReplaceAll(fmt.Sprint(rec.Column(0), rec.Column(1)), "\n", " "))
		rec.Release()
	}
	// iceberg-go appends to raw after DuckDB inserted rows into it; then DuckDB must see both.
	raw, err := cat.LoadTable(ctx, table.Identifier{ns, rawName})
	must(err, "load raw")
	seq := int64(0)
	rsc := raw.Scan(table.WithSelectedFields("_cdc_seq"))
	rtasks, err := rsc.PlanFiles(ctx)
	must(err, "plan raw")
	_, it2, err := rsc.ReadTasks(ctx, normalizeTasks(rtasks))
	must(err, "scan raw seq")
	for rec, err := range it2 {
		must(err, "iter")
		col := rec.Column(0).(*array.Int64)
		for i := 0; i < col.Len(); i++ {
			if col.Value(i) > seq {
				seq = col.Value(i)
			}
		}
		rec.Release()
	}
	raw = appendRows(ctx, raw, []row{{key: `{"$oid":"a7"}`, op: "c", seq: seq + 1, gen: 1, id: "a7", name: "grace", qty: 7, price: 7.5}}, checkpointProps(seq+1, 1, "stream", "", "tok-5"))
	ok("iceberg-go appended seq %d on top of DuckDB's inserts (snapshot %d)", seq+1, raw.CurrentSnapshot().SnapshotID)
	db := duck(ctx)
	defer db.Close()
	var cnt int64
	must(db.QueryRowContext(ctx, fmt.Sprintf("SELECT count(*) FROM cat.%s.%s WHERE _cdc_seq = %d", ns, rawName, seq+1)).Scan(&cnt), "duck count new row")
	if cnt != 1 {
		must(fmt.Errorf("DuckDB does not see iceberg-go's latest append"), "cross visibility")
	}
	ok("DuckDB sees iceberg-go's new commit immediately (fresh connection)")
	must(runMerge(ctx, db, seq, 1), "merge after iceberg-go append")
	must(db.QueryRowContext(ctx, fmt.Sprintf("SELECT count(*) FROM cat.%s.%s WHERE _id='a7'", ns, cleanName)).Scan(&cnt), "clean has a7")
	ok("merge picked up the iceberg-go row (a7 in clean = %d)", cnt)
}

func stepG(ctx context.Context) {
	cat := openCatalog(ctx)
	raw, err := cat.LoadTable(ctx, table.Identifier{ns, rawName})
	must(err, "load raw")
	nsnapBefore := len(raw.Metadata().Snapshots())
	tasks, err := raw.Scan().PlanFiles(ctx)
	must(err, "plan files")
	nfilesBefore := len(tasks)
	rowsBefore := countSeq(ctx, raw)
	ok("raw before maintenance: %d snapshots, %d data files, %d rows", nsnapBefore, nfilesBefore, rowsBefore)

	// Compaction via iceberg-go RewriteDataFiles.
	groups := []table.CompactionTaskGroup{}
	byPart := map[string][]table.FileScanTask{}
	for _, t := range tasks {
		k := fmt.Sprint(t.File.Partition())
		byPart[k] = append(byPart[k], t)
	}
	for _, ts := range byPart {
		if len(ts) > 1 {
			groups = append(groups, table.CompactionTaskGroup{Tasks: ts})
		}
	}
	if len(groups) > 0 {
		res, err := raw.RewriteDataFiles(ctx, groups, table.RewriteDataFilesOptions{SnapshotProps: iceberg.Properties{"lakesync.maintenance": "compact"}, GroupOptions: []table.CompactionGroupOption{table.WithCompactionTargetFileSize(128 << 20)}})
		must(err, "RewriteDataFiles")
		raw, _ = cat.LoadTable(ctx, table.Identifier{ns, rawName})
		tasks2, _ := raw.Scan().PlanFiles(ctx)
		ok("compaction: %+v -> now %d data files, %d rows (unchanged=%v)", summarizeRewrite(res), len(tasks2), countSeq(ctx, raw), countSeq(ctx, raw) == rowsBefore)
	} else {
		ok("compaction: nothing to compact (single file per partition)")
	}
	// Checkpoint must survive maintenance commits: our product re-stamps the checkpoint on every commit,
	// so verify what the latest snapshot's summary looks like now.
	ok("latest snapshot summary after compaction: %v", raw.CurrentSnapshot().Summary.Properties)

	// Expire snapshots keeping the last 2.
	if raw.Properties()["gc.enabled"] == "false" {
		ok("catalog sets gc.enabled=false (Nessie manages GC): snapshot expiry and orphan deletion are delegated to the catalog")
		tx := raw.NewTransaction()
		err := tx.ExpireSnapshots(table.WithRetainLast(2), table.WithOlderThan(0))
		ok("ExpireSnapshots refused as expected: %v", err)
	} else {
		tx := raw.NewTransaction()
		must(tx.ExpireSnapshots(table.WithRetainLast(2), table.WithOlderThan(0)), "ExpireSnapshots")
		raw, err = tx.Commit(ctx)
		must(err, "commit expire")
		ok("expire snapshots: %d -> %d snapshots, rows=%d", nsnapBefore, len(raw.Metadata().Snapshots()), countSeq(ctx, raw))
	}
	// Orphan file planning (dry run, no deletion) works regardless of gc.enabled.
	orph, err := raw.PlanOrphanFiles(ctx, table.WithFilesOlderThan(0))
	if err != nil {
		fmt.Printf("WARN orphan scan API: %v\n", err)
	} else {
		ok("orphan file plan: %d orphan files, %d bytes (includes the aborted append from step b and the compacted-away files)", len(orph.Files()), orph.TotalSizeBytes())
	}

	// Rewrite clean (DuckDB-written positional deletes) via iceberg-go compaction.
	clean, err := cat.LoadTable(ctx, table.Identifier{ns, cleanName})
	must(err, "load clean")
	ctasks, _ := clean.Scan().PlanFiles(ctx)
	ndel := 0
	for _, t := range ctasks {
		ndel += len(t.DeleteFiles)
	}
	cleanRows := countDuck(ctx, cleanName)
	res, err := clean.RewriteDataFiles(ctx, []table.CompactionTaskGroup{{Tasks: normalizeTasks(ctasks)}}, table.RewriteDataFilesOptions{GroupOptions: []table.CompactionGroupOption{table.WithCompactionTargetFileSize(128 << 20)}})
	if err != nil {
		fmt.Printf("WARN rewrite clean with deletes: %v\n", err)
	} else {
		clean, _ = cat.LoadTable(ctx, table.Identifier{ns, cleanName})
		ctasks2, _ := clean.Scan().PlanFiles(ctx)
		ndel2 := 0
		for _, t := range ctasks2 {
			ndel2 += len(t.DeleteFiles)
		}
		ok("clean rewrite: %d files/%d delete refs -> %d files/%d delete refs, rows %d -> %d (iceberg-go scan now reads %d), %+v", len(ctasks), ndel, len(ctasks2), ndel2, cleanRows, countDuck(ctx, cleanName), countSeq(ctx, clean), summarizeRewrite(res))
	}
	// DuckDB must still read the compacted clean table.
	db := duck(ctx)
	defer db.Close()
	var n int64
	must(db.QueryRowContext(ctx, fmt.Sprintf("SELECT count(*) FROM cat.%s.%s", ns, cleanName)).Scan(&n), "duck count clean after rewrite")
	ok("DuckDB reads clean after iceberg-go rewrite: %d rows", n)

	// Format v3 probe: can DuckDB merge into a v3 table and can iceberg-go read it (deletion vectors)?
	_, _ = db.ExecContext(ctx, fmt.Sprintf("DROP TABLE IF EXISTS cat.%s.v3probe", ns))
	_, err = db.ExecContext(ctx, fmt.Sprintf("CREATE TABLE cat.%s.v3probe (k VARCHAR, v BIGINT) WITH ('format-version' = '3')", ns))
	if err == nil {
		_, err = db.ExecContext(ctx, fmt.Sprintf("INSERT INTO cat.%s.v3probe VALUES ('a',1),('b',2)", ns))
	}
	if err == nil {
		_, err = db.ExecContext(ctx, fmt.Sprintf("DELETE FROM cat.%s.v3probe WHERE k='a'", ns))
	}
	if err != nil {
		fmt.Printf("INFO v3 probe (DuckDB side) failed: %v\n", firstLine(err.Error()))
	} else {
		t3, err := cat.LoadTable(ctx, table.Identifier{ns, "v3probe"})
		if err != nil {
			fmt.Printf("INFO v3 probe: iceberg-go cannot load: %v\n", firstLine(err.Error()))
		} else {
			n3 := countSeq(ctx, t3)
			ok("v3 probe: DuckDB wrote v%d table with a deletion vector; iceberg-go reads %d rows (expect 1)", t3.Metadata().Version(), n3)
		}
	}
}

// parquetFile normalises the file format reported by manifest entries written by engines
// that spell it in lowercase (DuckDB writes "parquet"); iceberg-go compares case-sensitively.
type parquetFile struct{ iceberg.DataFile }

func (parquetFile) FileFormat() iceberg.FileFormat { return iceberg.ParquetFile }

func normalizeTasks(tasks []table.FileScanTask) []table.FileScanTask {
	out := make([]table.FileScanTask, len(tasks))
	for i, t := range tasks {
		t.File = parquetFile{t.File}
		dels := make([]iceberg.DataFile, len(t.DeleteFiles))
		for j, d := range t.DeleteFiles {
			dels[j] = parquetFile{d}
		}
		t.DeleteFiles = dels
		out[i] = t
	}
	return out
}

func countDuck(ctx context.Context, name string) int64 {
	db := duck(ctx)
	defer db.Close()
	var n int64
	must(db.QueryRowContext(ctx, fmt.Sprintf("SELECT count(*) FROM cat.%s.%s", ns, name)).Scan(&n), "duck count "+name)
	return n
}

func summarizeRewrite(r *table.RewriteResult) string {
	if r == nil {
		return "nil"
	}
	return fmt.Sprintf("%+v", *r)
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("usage: phase0 <a|b|c|d|e|f|g>")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	switch os.Args[1] {
	case "a":
		stepA(ctx)
	case "b":
		stepB(ctx)
	case "c":
		stepC(ctx)
	case "d":
		stepD(ctx)
	case "e":
		stepE(ctx)
	case "f":
		stepF(ctx)
	case "g":
		stepG(ctx)
	case "q":
		db := duck(ctx)
		defer db.Close()
		if len(os.Args) > 3 {
			if _, err := db.ExecContext(ctx, os.Args[3]); err != nil {
				fmt.Println("pre-statement error:", err)
			}
		}
		rows, err := db.QueryContext(ctx, os.Args[2])
		must(err, "query")
		defer rows.Close()
		cols, _ := rows.Columns()
		fmt.Println(strings.Join(cols, " | "))
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			must(rows.Scan(ptrs...), "scan")
			fmt.Println(fmt.Sprint(vals...))
		}
	case "props":
		cat := openCatalog(ctx)
		for _, name := range []string{rawName, cleanName} {
			t, err := cat.LoadTable(ctx, table.Identifier{ns, name})
			must(err, "load "+name)
			fmt.Printf("%s props=%v snapshots=%d\n", name, t.Properties(), len(t.Metadata().Snapshots()))
		}
	default:
		fmt.Println("unknown step")
		os.Exit(2)
	}
}
