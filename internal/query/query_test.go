package query

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestMergeStatements(t *testing.T) {
	st := MergeStatements("d_1", "shop", "orders__raw", "orders", []string{"_cdc_key", "_cdc_seq", "_cdc_generation", "total"}, 41, 2, "_batch_orders")
	if len(st) != 3 {
		t.Fatalf("want 3 statements got %d", len(st))
	}
	if !strings.Contains(st[0], `"_cdc_seq" > 41`) || !strings.Contains(st[0], `"_cdc_generation" = 2`) || !strings.Contains(st[0], `"d_1"."shop"."orders__raw"`) {
		t.Errorf("stage: %s", st[0])
	}
	if !strings.HasPrefix(st[1], `DELETE FROM "d_1"."shop"."orders" t WHERE EXISTS`) || !strings.Contains(st[1], `s."_cdc_seq" > t."_cdc_seq"`) {
		t.Errorf("delete: %s", st[1])
	}
	if !strings.Contains(st[2], `INSERT INTO "d_1"."shop"."orders" ("_cdc_key", "_cdc_seq", "_cdc_generation", "total")`) || !strings.Contains(st[2], `s."_cdc_op" <> 'd'`) || !strings.Contains(st[2], `t."_cdc_seq" >= s."_cdc_seq"`) {
		t.Errorf("insert: %s", st[2])
	}
}

func TestCursorRoundTrip(t *testing.T) {
	c := cursor{SortCol: "total", SortDesc: true, SortVal: 12.5, Key: `{"$oid":"x"}`}
	got, err := decodeCursor(encodeCursor(c))
	if err != nil || got.SortCol != "total" || !got.SortDesc || got.Key != c.Key || got.SortVal != 12.5 {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	if _, err := decodeCursor("!!!"); err == nil {
		t.Error("bad cursor accepted")
	}
}

func TestBuildWhere(t *testing.T) {
	cols := []ColumnMeta{{"_cdc_key", "VARCHAR"}, {"name", "VARCHAR"}, {"qty", "BIGINT"}, {"price", "DOUBLE"}, {"when", "TIMESTAMP WITH TIME ZONE"}, {"blob", "BLOB"}, {"_cdc_seq", "BIGINT"}}
	types := map[string]string{}
	for _, c := range cols {
		types[c.Name] = c.Type
	}
	where, args, err := buildWhere([]Filter{
		{Column: "name", Op: "contains", Value: "a%b"},
		{Column: "qty", Op: "gte", Value: "5"},
		{Column: "price", Op: "between", Value: 1.5, Value2: "2.5"},
		{Column: "when", Op: "lt", Value: "2026-01-02"},
		{Column: "qty", Op: "isnull"},
		{Column: "name", Op: "in", Value: []any{"a", "b"}},
	}, "needle", cols, types)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(where, " AND ")
	for _, want := range []string{`CAST("name" AS VARCHAR) ILIKE ?`, `"qty" >= ?`, `"price" BETWEEN ? AND ?`, `"when" < ?`, `"qty" IS NULL`, `"name" IN (?,?)`, `CAST("_cdc_key" AS VARCHAR) ILIKE ?`} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %s in %s", want, joined)
		}
	}
	if strings.Contains(joined, `"blob"`) || strings.Contains(joined, `"_cdc_seq"`) {
		t.Errorf("search must skip blobs and metadata: %s", joined)
	}
	if args[0] != `%a\%b%` || args[1] != int64(5) || args[3] != 2.5 {
		t.Errorf("args: %#v", args)
	}
	if _, ok := args[4].(time.Time); !ok {
		t.Errorf("timestamp arg not parsed: %#v", args[4])
	}
	if _, _, err := buildWhere([]Filter{{Column: "nope", Op: "eq"}}, "", cols, types); err == nil {
		t.Error("unknown column accepted")
	}
	if _, _, err := buildWhere([]Filter{{Column: "qty", Op: "weird"}}, "", cols, types); err == nil {
		t.Error("unknown op accepted")
	}
}

func TestDiffDocs(t *testing.T) {
	changed, diff := diffDocs([]byte(`{"a":1,"b":"x","c":{"n":1}}`), []byte(`{"a":1,"b":"y","d":true}`))
	if strings.Join(changed, ",") != "b,c,d" {
		t.Errorf("changed %v", changed)
	}
	if string(diff["c"].After) != "null" || string(diff["d"].Before) != "null" || string(diff["b"].Before) != `"x"` {
		t.Errorf("diff %+v", diff)
	}
	changed, _ = diffDocs(nil, []byte(`{"a":1}`))
	if len(changed) != 1 {
		t.Errorf("insert diff %v", changed)
	}
}

func TestReadOnlyValidation(t *testing.T) {
	ctx := context.Background()
	e, err := Open(ctx, Options{}, nil)
	if err != nil {
		t.Skipf("duckdb unavailable: %v", err)
	}
	defer e.Close()
	ok := []string{"SELECT 1", "select 1;", "EXPLAIN SELECT 1", "WITH x AS (SELECT 1 AS a) SELECT * FROM x", "SELECT * FROM range(3)"}
	for _, q := range ok {
		if err := e.ValidateReadOnly(ctx, q); err != nil {
			t.Errorf("%q rejected: %v", q, err)
		}
	}
	bad := []string{"INSERT INTO t VALUES (1)", "COPY (SELECT 1) TO '/tmp/x.csv'", "ATTACH 'x' AS y", "INSTALL httpfs", "LOAD httpfs", "CREATE TABLE t (a INT)", "DELETE FROM t", "UPDATE t SET a=1", "SET threads=1", "SELECT 1; SELECT 2", "SELECT * FROM duckdb_secrets()", "SELECT * FROM read_text('/etc/passwd')", "PRAGMA version", "CALL dbgen(sf=1)", "DROP TABLE t", "MERGE INTO t USING s ON 1=1 WHEN MATCHED THEN DELETE", "", "SELECT * FROM glob('/etc/*')"}
	for _, q := range bad {
		if err := e.ValidateReadOnly(ctx, q); err == nil {
			t.Errorf("%q accepted", q)
		}
	}
	// the local filesystem is disabled even for SELECTs that slip through parsing
	if _, err := e.Query(ctx, "", "SELECT * FROM read_csv('/etc/passwd')", 10, 5*time.Second); err == nil {
		t.Error("local file read succeeded")
	}
	// a configuration change is locked
	if _, err := e.db.ExecContext(ctx, "SET disabled_filesystems = ''"); err == nil {
		t.Error("configuration not locked")
	}
	// secrets must still be creatable after the lockdown (catalog attach needs them)
	if _, err := e.db.ExecContext(ctx, "CREATE SECRET t1 (TYPE s3, KEY_ID 'a', SECRET 'b'); DROP SECRET t1"); err != nil {
		t.Errorf("secret creation after lockdown: %v", err)
	}
	res, err := e.Query(ctx, "", "SELECT i, i*2 AS d FROM range(5) t(i)", 3, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if res.RowCount != 3 || !res.Truncated || len(res.Columns) != 2 {
		t.Errorf("result %+v", res)
	}
}
