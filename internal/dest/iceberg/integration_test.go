//go:build integration

package iceberg

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"testing"
	"time"

	"lakesync/internal/core"
	"lakesync/internal/dest"
)

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func testDest(t *testing.T) (*Destination, core.TableSpec, func()) {
	t.Helper()
	cfg, err := ParseConfig(map[string]any{
		"catalog_uri": envOr("LAKESYNC_TEST_CATALOG_URI", "http://localhost:19120/iceberg"),
		"warehouse":   envOr("LAKESYNC_TEST_WAREHOUSE", "warehouse"),
		"s3_endpoint": envOr("LAKESYNC_TEST_S3_ENDPOINT", "http://localhost:9000"),
		"s3_region":   "us-east-1",
	}, map[string]string{
		"s3_access_key_id":     envOr("LAKESYNC_TEST_S3_KEY", "lakesync"),
		"s3_secret_access_key": envOr("LAKESYNC_TEST_S3_SECRET", "lakesync-secret"),
	})
	if err != nil {
		t.Fatal(err)
	}
	d, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	ns := fmt.Sprintf("lakesync_it_%06x", rand.Intn(1<<24))
	spec := core.NewTableSpec(ns, core.Entity{Namespace: "shop", Name: "orders"})
	cleanup := func() {
		ctx := context.Background()
		_ = d.DropTables(ctx, spec)
		if cat, err := d.catalog(ctx); err == nil {
			_ = cat.DropNamespace(ctx, namespaceIdent(ns))
		}
		_ = d.Close()
	}
	return d, spec, cleanup
}

func userSchema() core.Schema {
	return core.Schema{Columns: []core.Column{
		{Name: "_id", SourceField: "_id", Type: core.TypeString},
		{Name: "total", SourceField: "total", Type: core.TypeDouble},
		{Name: "status", SourceField: "status", Type: core.TypeString},
	}}
}

func batch(s core.Schema, firstSeq int64, n int) *core.Batch {
	now := time.Now().UTC()
	rows := make([]core.Row, n)
	for i := range rows {
		id := fmt.Sprintf("id-%d", firstSeq+int64(i))
		vals := make([]core.Value, len(s.Columns))
		for j, c := range s.Columns {
			switch c.Name {
			case "_id":
				vals[j] = core.StringValue(id)
			case "total":
				vals[j] = core.FloatValue(float64(i) + 0.5)
			case "status":
				vals[j] = core.StringValue("new")
			case "note":
				vals[j] = core.StringValue("evolved")
			default:
				vals[j] = core.NullValue()
			}
		}
		rows[i] = core.Row{Key: `{"$oid":"` + id + `"}`, Op: core.OpInsert, Seq: firstSeq + int64(i), Generation: 1, SourceTS: now, IngestedAt: now, Doc: `{"_id":"` + id + `"}`, Values: vals}
	}
	return &core.Batch{Schema: s, Rows: rows}
}

func rowCount(t *testing.T, d *Destination, ns, name string) int64 {
	t.Helper()
	tbl, err := d.loadTable(context.Background(), ns, name)
	if err != nil {
		t.Fatal(err)
	}
	n, _, err := contentChecksum(context.Background(), tbl)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestIntegrationChecklist(t *testing.T) {
	d, _, cleanup := testDest(t)
	defer cleanup()
	rep := d.Test(context.Background())
	for _, s := range rep.Steps {
		t.Logf("%-5s %-40s %4dms %s %s", s.Status, s.Name, s.Millis, s.Message, s.Hint)
	}
	if !rep.OK {
		t.Fatal("checklist failed")
	}
	if len(rep.Steps) < 7 {
		t.Errorf("expected at least 7 steps, got %d", len(rep.Steps))
	}
	bad := Config{CatalogURI: "http://localhost:1", Warehouse: "w", AuthType: AuthNone}
	bd, _ := New(context.Background(), bad)
	brep := bd.Test(context.Background())
	if brep.OK || brep.Steps[0].Status != core.TestFail || !strings.Contains(brep.Steps[0].Hint, "reachable") {
		t.Errorf("unreachable catalog should fail step 1 with a hint: %+v", brep.Steps)
	}
}

func TestIntegrationLifecycle(t *testing.T) {
	ctx := context.Background()
	d, spec, cleanup := testDest(t)
	defer cleanup()
	s := userSchema()
	if err := d.EnsureTables(ctx, spec, s); err != nil {
		t.Fatal(err)
	}
	if err := d.EnsureTables(ctx, spec, s); err != nil {
		t.Fatalf("EnsureTables must be idempotent: %v", err)
	}
	if cp, err := d.LastCheckpoint(ctx, spec); err != nil || cp != nil {
		t.Fatalf("fresh table should have no checkpoint: %+v %v", cp, err)
	}
	got, err := d.CurrentSchema(ctx, spec)
	if err != nil || len(got.Columns) != 3 || got.Columns[1].Type != core.TypeDouble {
		t.Fatalf("CurrentSchema: %+v %v", got, err)
	}

	// three batches with checkpoints
	var seq int64
	for i := 0; i < 3; i++ {
		b := batch(s, seq+1, 4)
		cp := core.Checkpoint{Bookmark: []byte(fmt.Sprintf("tok-%d-\x00\xff", i)), Seq: b.LastSeq(), Generation: 1, Phase: core.PhaseSnapshot, SnapshotCursor: core.SnapshotCursor(b.Rows[3].Key), RunID: "run-1"}
		res, err := d.AppendRaw(ctx, spec, b, cp)
		if err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		if res.Rows != 4 || res.DataFiles < 1 || res.Bytes <= 0 || res.SnapshotID == 0 {
			t.Errorf("append result: %+v", res)
		}
		seq = b.LastSeq()
	}
	cp, err := d.LastCheckpoint(ctx, spec)
	if err != nil || cp == nil {
		t.Fatalf("LastCheckpoint: %+v %v", cp, err)
	}
	if cp.Seq != 12 || string(cp.Bookmark) != "tok-2-\x00\xff" || cp.Phase != core.PhaseSnapshot || cp.SnapshotCursor == "" || cp.RunID != "run-1" {
		t.Errorf("checkpoint round trip: %+v", cp)
	}
	if n := rowCount(t, d, spec.Namespace, spec.Raw); n != 12 {
		t.Errorf("raw rows = %d want 12", n)
	}

	// stamp only
	st := core.Checkpoint{Bookmark: []byte("tok-stream"), Seq: 12, Generation: 1, Phase: core.PhaseStream, RunID: "run-1"}
	if err := d.StampCheckpoint(ctx, spec, st); err != nil {
		t.Fatal(err)
	}
	if cp, _ = d.LastCheckpoint(ctx, spec); cp.Phase != core.PhaseStream || string(cp.Bookmark) != "tok-stream" || cp.Seq != 12 {
		t.Errorf("stamped checkpoint: %+v", cp)
	}
	if _, err := d.AppendRaw(ctx, spec, &core.Batch{Schema: s}, core.Checkpoint{Seq: 12, Generation: 1, Phase: core.PhaseStream, Bookmark: []byte("tok-empty")}); err != nil {
		t.Fatal(err)
	}
	if cp, _ = d.LastCheckpoint(ctx, spec); string(cp.Bookmark) != "tok-empty" {
		t.Errorf("empty batch should stamp: %+v", cp)
	}
	if n := rowCount(t, d, spec.Namespace, spec.Raw); n != 12 {
		t.Errorf("empty batch changed rows: %d", n)
	}

	// evolve, then append using the new column
	add := []core.Column{{Name: "note", SourceField: "note", Type: core.TypeString}}
	if err := d.EvolveSchema(ctx, spec, add); err != nil {
		t.Fatal(err)
	}
	if err := d.EvolveSchema(ctx, spec, add); err != nil {
		t.Fatalf("EvolveSchema must be idempotent: %v", err)
	}
	s2 := core.Schema{Columns: append(s.Clone().Columns, add...)}
	b := batch(s2, 13, 2)
	if _, err := d.AppendRaw(ctx, spec, b, core.Checkpoint{Seq: 14, Generation: 1, Phase: core.PhaseStream, Bookmark: []byte("tok-3")}); err != nil {
		t.Fatal(err)
	}
	got, err = d.CurrentSchema(ctx, spec)
	if err != nil || len(got.Columns) != 4 || got.Columns[3].Name != "note" || got.Columns[3].SourceField != "note" || got.Columns[3].Type != core.TypeString {
		t.Fatalf("evolved schema: %+v %v", got, err)
	}
	cols, err := d.TableColumns(ctx, spec.Namespace, spec.Clean)
	if err != nil {
		t.Fatal(err)
	}
	var cleanHasNote, cleanHasDoc bool
	for _, c := range cols {
		cleanHasNote = cleanHasNote || c.Name == "note"
		cleanHasDoc = cleanHasDoc || c.Name == core.ColDoc
	}
	if !cleanHasNote || cleanHasDoc {
		t.Errorf("clean columns: %+v", cols)
	}

	// inspection
	infos, err := d.ListTables(ctx, spec.Namespace)
	if err != nil || len(infos) != 2 {
		t.Fatalf("ListTables: %+v %v", infos, err)
	}
	for _, info := range infos {
		if info.Name == spec.Raw && (info.RowCount != 14 || info.SizeBytes <= 0 || info.LastUpdated.IsZero()) {
			t.Errorf("raw table info: %+v", info)
		}
	}
	nss, err := d.ListNamespaces(ctx)
	if err != nil || !contains(nss, spec.Namespace) {
		t.Errorf("ListNamespaces: %v %v", nss, err)
	}
	h, err := d.TableHealth(ctx, spec.Namespace, spec.Raw, dest.DefaultMaintenanceConfig())
	if err != nil {
		t.Fatal(err)
	}
	if h.DataFiles != 4 || h.SmallFiles != 4 || h.RowCount != 14 || h.Partitions < 1 || h.FormatVersion != 2 || h.GCEnabled || h.LastCommit.IsZero() {
		t.Errorf("health: %+v", h)
	}
	snaps, err := d.Snapshots(ctx, spec.Namespace, spec.Raw)
	if err != nil || len(snaps) < 1 || !snaps[0].IsCurrent || snaps[0].Summary[core.PropSeq] != "14" {
		t.Errorf("snapshots: %+v %v", snaps, err)
	}

	// maintenance: compaction preserves rows and must not regress the checkpoint,
	// even when the caller passes a stale one (cp is still the seq-12 checkpoint).
	before := rowCount(t, d, spec.Namespace, spec.Raw)
	res, err := d.Maintain(ctx, spec.Namespace, spec.Raw, dest.MaintCompactRaw, dest.MaintenanceConfig{SmallFilesTrigger: 1}, cp)
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped || res.FilesBefore != 4 || res.FilesAfter != 1 || res.RowsBefore != before || res.RowsAfter != before {
		t.Errorf("compaction: %+v", res)
	}
	h2, _ := d.TableHealth(ctx, spec.Namespace, spec.Raw, dest.DefaultMaintenanceConfig())
	if h2.DataFiles != 1 || h2.LastCompaction.IsZero() {
		t.Errorf("health after compaction: %+v", h2)
	}
	if cp2, _ := d.LastCheckpoint(ctx, spec); cp2 == nil || cp2.Seq != 14 || string(cp2.Bookmark) != "tok-3" {
		t.Errorf("checkpoint regressed by maintenance: %+v", cp2)
	}
	if snaps, _ := d.Snapshots(ctx, spec.Namespace, spec.Raw); len(snaps) == 0 || snaps[0].Summary[core.PropSeq] != "14" || snaps[0].Summary[PropMaintenance] != string(dest.MaintCompactRaw) {
		t.Errorf("rewrite snapshot summary should carry the newest checkpoint: %+v", snaps)
	}
	res, err = d.Maintain(ctx, spec.Namespace, spec.Raw, dest.MaintCompactRaw, dest.DefaultMaintenanceConfig(), nil)
	if err != nil || !res.Skipped {
		t.Errorf("second compaction should skip: %+v %v", res, err)
	}
	res, err = d.Maintain(ctx, spec.Namespace, spec.Raw, dest.MaintExpireSnapshots, dest.DefaultMaintenanceConfig(), nil)
	if err != nil || !res.Skipped || !strings.Contains(res.Reason, "gc.enabled") {
		t.Errorf("expire on Nessie: %+v %v", res, err)
	}
	res, err = d.Maintain(ctx, spec.Namespace, spec.Raw, dest.MaintOrphanCleanup, dest.MaintenanceConfig{OrphanOlderThan: 0}, nil)
	if err != nil || !res.Skipped || res.FilesBefore < 1 {
		t.Errorf("orphan cleanup on Nessie should plan but skip: %+v %v", res, err)
	}
	res, err = d.Maintain(ctx, spec.Namespace, spec.Clean, dest.MaintRewriteClean, dest.DefaultMaintenanceConfig(), nil)
	if err != nil || !res.Skipped {
		t.Errorf("rewrite of an empty clean table should skip: %+v %v", res, err)
	}

	if err := d.DropTables(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if err := d.DropTables(ctx, spec); err != nil {
		t.Fatalf("DropTables must tolerate missing tables: %v", err)
	}
}

func TestIntegrationConcurrentAppends(t *testing.T) {
	ctx := context.Background()
	d1, spec, cleanup := testDest(t)
	defer cleanup()
	d2, err := New(ctx, d1.Config())
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	s := userSchema()
	if err := d1.EnsureTables(ctx, spec, s); err != nil {
		t.Fatal(err)
	}
	type result struct {
		conflicts int
		err       error
	}
	run := func(d *Destination, firstSeq int64, n int) result {
		var r result
		for i := 0; i < n; i++ {
			b := batch(s, firstSeq+int64(i)*3, 3)
			cp := core.Checkpoint{Seq: b.LastSeq(), Generation: 1, Phase: core.PhaseStream, Bookmark: []byte("x")}
			for attempt := 0; attempt < 10; attempt++ {
				_, err := d.AppendRaw(ctx, spec, b, cp)
				if err == nil {
					break
				}
				if core.IsCommitConflict(err) {
					r.conflicts++
					continue
				}
				r.err = err
				return r
			}
		}
		return r
	}
	done := make(chan result, 2)
	go func() { done <- run(d1, 1, 5) }()
	go func() { done <- run(d2, 1000, 5) }()
	r1, r2 := <-done, <-done
	if r1.err != nil || r2.err != nil {
		t.Fatalf("errors: %v %v", r1.err, r2.err)
	}
	t.Logf("conflicts seen: %d (iceberg-go may also replay appends internally)", r1.conflicts+r2.conflicts)
	if n := rowCount(t, d1, spec.Namespace, spec.Raw); n != 30 {
		t.Fatalf("rows after concurrent appends = %d want 30 (no lost or duplicated batches)", n)
	}
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
