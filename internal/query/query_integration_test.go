//go:build integration

package query

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

// TestIntegrationCatalog attaches the local Nessie catalog, creates raw and
// clean tables through DuckDB, and exercises merge, rebuild, viewer paging,
// filters, search, history and the console end to end.
func TestIntegrationCatalog(t *testing.T) {
	ctx := context.Background()
	e, err := Open(ctx, Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	cfg := CatalogConfig{
		URI: envOr("LAKESYNC_TEST_CATALOG_URI", "http://localhost:19120/iceberg"), Warehouse: "warehouse", AuthType: "none",
		S3Endpoint: envOr("LAKESYNC_TEST_S3_ENDPOINT", "http://localhost:9000"), S3Region: "us-east-1", S3AccessKey: "lakesync", S3SecretKey: "lakesync-secret", S3PathStyle: true,
	}
	alias := Alias("it-" + fmt.Sprint(time.Now().UnixNano()))
	if err := e.Attach(ctx, alias, cfg); err != nil {
		t.Skipf("catalog not available: %v", err)
	}
	ns := fmt.Sprintf("qit_%d", time.Now().UnixNano()%1_000_000)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := e.DB().ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(fmt.Sprintf("CREATE SCHEMA %s.%s", quoteIdent(alias), quoteIdent(ns)))
	defer func() {
		_, _ = e.DB().ExecContext(ctx, fmt.Sprintf("DROP TABLE IF EXISTS %s", qualified(alias, ns, "t__raw")))
		_, _ = e.DB().ExecContext(ctx, fmt.Sprintf("DROP TABLE IF EXISTS %s", qualified(alias, ns, "t")))
		_, _ = e.DB().ExecContext(ctx, fmt.Sprintf("DROP SCHEMA IF EXISTS %s.%s", quoteIdent(alias), quoteIdent(ns)))
	}()
	cols := "_cdc_key VARCHAR, _cdc_seq BIGINT, _cdc_generation BIGINT, _cdc_source_ts TIMESTAMPTZ, _cdc_ingested_at TIMESTAMPTZ, _cdc_cast_errors VARCHAR, _id VARCHAR, name VARCHAR, qty BIGINT, meta VARCHAR"
	exec(fmt.Sprintf("CREATE TABLE %s (%s, _cdc_op VARCHAR, _cdc_doc VARCHAR)", qualified(alias, ns, "t__raw"), cols))
	exec(fmt.Sprintf("CREATE TABLE %s (%s)", qualified(alias, ns, "t"), cols))
	// generation 1: 5 inserts, then an update and a delete
	rows := []string{}
	for i := 1; i <= 5; i++ {
		rows = append(rows, fmt.Sprintf(`('k%d', %d, 1, now(), now(), NULL, 'k%d', 'name %d', %d, '{"a":%d}', 'r', '{"_id":"k%d","name":"name %d","qty":%d}')`, i, i, i, i, i*10, i, i, i, i*10))
	}
	rows = append(rows, `('k2', 6, 1, now(), now(), NULL, 'k2', 'name 2 updated', 99, '{"a":2,"b":true}', 'u', '{"_id":"k2","name":"name 2 updated","qty":99}')`)
	rows = append(rows, `('k3', 7, 1, now(), now(), NULL, NULL, NULL, NULL, NULL, 'd', NULL)`)
	exec(fmt.Sprintf("INSERT INTO %s VALUES %s", qualified(alias, ns, "t__raw"), joinRows(rows)))

	res, err := e.Merge(ctx, alias, ns, "t__raw", "t", 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if res.Candidates != 5 || res.Inserted != 4 || res.Watermark != 7 {
		t.Fatalf("merge #1: %+v", res)
	}
	if n, _ := e.Count(ctx, alias, ns, "t"); n != 4 {
		t.Fatalf("clean rows = %d, want 4 (k3 deleted)", n)
	}
	// idempotent
	res, err = e.Merge(ctx, alias, ns, "t__raw", "t", 0, 1)
	if err != nil || res.Deleted != 0 || res.Inserted != 0 {
		t.Fatalf("re-merge not a no-op: %+v %v", res, err)
	}
	if n, _ := e.DuplicateKeys(ctx, alias, ns, "t"); n != 0 {
		t.Fatalf("duplicates = %d", n)
	}
	// viewer: paging, sort, filter, search
	page1, err := e.Rows(ctx, RowsRequest{Alias: alias, Namespace: ns, Table: "t", Limit: 2, Sort: &SortSpec{Column: "qty", Desc: true}, WithTotal: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(page1.Rows) != 2 || !page1.HasMore || page1.Total == nil || *page1.Total != 4 {
		t.Fatalf("page1 %+v", page1)
	}
	page2, err := e.Rows(ctx, RowsRequest{Alias: alias, Namespace: ns, Table: "t", Limit: 2, Sort: &SortSpec{Column: "qty", Desc: true}, Cursor: page1.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[any]bool{}
	for _, r := range append(page1.Rows, page2.Rows...) {
		seen[r[0]] = true
	}
	if len(seen) != 4 || page2.HasMore {
		t.Fatalf("keyset paging lost or duplicated rows: %v more=%v", seen, page2.HasMore)
	}
	qtyIdx := indexOf(page1.Columns, "qty")
	if page1.Rows[0][qtyIdx].(int64) != 99 {
		t.Fatalf("sort desc: first qty = %v", page1.Rows[0][qtyIdx])
	}
	f, err := e.Rows(ctx, RowsRequest{Alias: alias, Namespace: ns, Table: "t", Filters: []Filter{{Column: "qty", Op: "gte", Value: "40"}}, WithTotal: true})
	if err != nil || *f.Total != 3 {
		t.Fatalf("filter: %+v %v", f, err)
	}
	s, err := e.Rows(ctx, RowsRequest{Alias: alias, Namespace: ns, Table: "t", Search: "updated", WithTotal: true})
	if err != nil || *s.Total != 1 {
		t.Fatalf("search: %+v %v", s, err)
	}
	// history with diffs
	h, err := e.History(ctx, alias, ns, "t__raw", "k2", 10)
	if err != nil || len(h) != 2 || h[0].Op != "u" || h[1].Op != "r" {
		t.Fatalf("history: %+v %v", h, err)
	}
	if len(h[0].Changed) != 2 || h[0].Diff["qty"].Before == nil {
		t.Fatalf("diff: %+v", h[0])
	}
	// console with search_path
	qr, err := e.Query(ctx, alias, fmt.Sprintf("SELECT count(*) AS n FROM %s.t", ns), 100, 10*time.Second)
	if err != nil || qr.Rows[0][0].(int64) != 4 {
		t.Fatalf("console: %+v %v", qr, err)
	}
	if ac, err := e.Autocomplete(ctx, alias); err != nil || len(ac) < 2 {
		t.Fatalf("autocomplete: %v %v", ac, err)
	}
	// schema drift over the latest document versions
	drift, err := e.SchemaDrift(ctx, alias, ns, "t__raw", 50)
	if err != nil {
		t.Fatalf("drift: %v", err)
	}
	found := map[string]FieldDrift{}
	for _, d := range drift {
		found[d.Field] = d
	}
	if q := found["qty"]; q.Documents != 4 || len(q.Types) != 1 || q.Types[0].Type != "int" || q.Types[0].Percent != 100 {
		t.Fatalf("drift qty: %+v", q)
	}
	if n := found["name"]; n.FirstSeen == nil || n.LastSeen == nil || n.Types[0].Type != "string" {
		t.Fatalf("drift name: %+v", n)
	}
	// generation 2 rebuild drops gen-1 rows that were not copied again
	exec(fmt.Sprintf("INSERT INTO %s VALUES ('k1', 8, 2, now(), now(), NULL, 'k1', 'name 1', 10, NULL, 'r', '{}'), ('k9', 9, 2, now(), now(), NULL, 'k9', 'name 9', 90, NULL, 'r', '{}')", qualified(alias, ns, "t__raw")))
	rb, err := e.RebuildClean(ctx, alias, ns, "t__raw", "t", 2)
	if err != nil || rb.Inserted != 2 {
		t.Fatalf("rebuild: %+v %v", rb, err)
	}
	if n, _ := e.Count(ctx, alias, ns, "t"); n != 2 {
		t.Fatalf("after rebuild rows = %d", n)
	}
	// re-attach with the same config is a no-op; a changed config re-attaches
	if err := e.Attach(ctx, alias, cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Staleness = 5 * time.Second
	if err := e.Attach(ctx, alias, cfg); err != nil {
		t.Fatal(err)
	}
	if n, _ := e.Count(ctx, alias, ns, "t"); n != 2 {
		t.Fatalf("after re-attach rows = %d", n)
	}
}

func joinRows(rows []string) string {
	out := ""
	for i, r := range rows {
		if i > 0 {
			out += ", "
		}
		out += r
	}
	return out
}

func indexOf(cols []ColumnMeta, name string) int {
	for i, c := range cols {
		if c.Name == name {
			return i
		}
	}
	return -1
}
