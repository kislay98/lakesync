//go:build integration

package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	admin := "postgres://lakesync:lakesync@localhost:5432/lakesync?sslmode=disable"
	if v := envOr("LAKESYNC_TEST_DATABASE_URL", ""); v != "" {
		admin = v
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Skipf("postgres not available: %v", err)
	}
	name := fmt.Sprintf("lakesync_it_%d", time.Now().UnixNano()%1_000_000_000)
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	conn.Close(ctx)
	url := strings.Replace(admin, "/lakesync?", "/"+name+"?", 1)
	if err := Migrate(ctx, url); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	st, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		st.Close()
		c, err := pgx.Connect(context.Background(), admin)
		if err == nil {
			_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
			c.Close(context.Background())
		}
	})
	return st
}

func envOr(k, d string) string {
	if v := strings.TrimSpace(getenv(k)); v != "" {
		return v
	}
	return d
}

func TestIntegrationStoreRoundTrips(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	// migrations are idempotent
	if v, err := MigrationStatus(ctx, st.Pool.Config().ConnString()); err != nil || v < 1 {
		t.Fatalf("migration status %d %v", v, err)
	}
	u, err := st.CreateUser(ctx, "a@example.com", "hash")
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := st.CountUsers(ctx); n != 1 {
		t.Fatalf("users = %d", n)
	}
	if _, err := st.GetUserByEmail(ctx, "A@EXAMPLE.COM"); err != nil {
		t.Fatalf("case-insensitive email lookup: %v", err)
	}
	// sessions
	tok := NewToken()
	sess, err := st.CreateSession(ctx, HashToken(tok), u.ID, NewToken(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := st.GetSession(ctx, HashToken(tok)); err != nil || got.UserID != u.ID {
		t.Fatalf("get session: %v", err)
	}
	if err := st.DeleteSession(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetSession(ctx, HashToken(tok)); err != ErrNotFound {
		t.Fatalf("deleted session still readable: %v", err)
	}
	// login rate limiting
	for i := 0; i < 3; i++ {
		_ = st.LoginFailed(ctx, "email:x", 3, time.Minute, time.Minute)
	}
	if ok, wait, _ := st.LoginAllowed(ctx, "email:x"); ok || wait <= 0 {
		t.Fatalf("expected a block after 3 failures (ok=%v wait=%s)", ok, wait)
	}
	// connections
	c, err := st.CreateConnection(ctx, &Connection{OwnerID: u.ID, Kind: "source", Type: "mongodb", Name: "m", Config: map[string]any{"uri": "mongodb://h"}, SecretCiphertext: []byte{1, 2, 3}})
	if err != nil {
		t.Fatal(err)
	}
	d, err := st.CreateConnection(ctx, &Connection{OwnerID: u.ID, Kind: "destination", Type: "iceberg_rest", Name: "d", Config: map[string]any{"catalog_uri": "http://c"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetConnection(ctx, c.ID); got.Config["uri"] != "mongodb://h" || string(got.SecretCiphertext) != "\x01\x02\x03" {
		t.Fatalf("connection round trip: %+v", got)
	}
	if _, err := st.UpdateConnection(ctx, c.ID, "m2", map[string]any{"uri": "mongodb://h2"}, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetConnection(ctx, c.ID); got.Name != "m2" || len(got.SecretCiphertext) != 3 {
		t.Fatalf("update kept secrets? %+v", got)
	}
	// pipelines and collections
	p, err := st.CreatePipeline(ctx, &Pipeline{OwnerID: u.ID, Name: "p", SourceID: c.ID, DestinationID: d.ID, Namespace: "ns", SyncIntervalS: 30, DesiredState: DesiredRunning}, []PipelineCollection{{Collection: "db.a", Enabled: true, RawTable: "a__raw", CleanTable: "a", SnapshotTotalEst: 10}})
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := st.ConnectionInUse(ctx, c.ID); n != 1 {
		t.Fatalf("in use = %d", n)
	}
	if err := st.DeleteConnection(ctx, c.ID); err == nil {
		t.Fatal("deleting a used connection must fail (foreign key)")
	}
	seq := int64(42)
	state := StateStreaming
	errMsg := ""
	if err := st.UpdateCollection(ctx, p.ID, "db.a", CollectionUpdate{State: &state, LastSeq: &seq, LastError: &errMsg}); err != nil {
		t.Fatal(err)
	}
	col, err := st.GetCollection(ctx, p.ID, "db.a")
	if err != nil || col.State != StateStreaming || col.LastSeq != 42 || col.LastError != nil {
		t.Fatalf("collection update: %+v %v", col, err)
	}
	found, err := st.FindCollectionByTable(ctx, d.ID, "ns", "a")
	if err != nil || found.Collection != "db.a" {
		t.Fatalf("find by table: %+v %v", found, err)
	}
	if err := st.RequestAction(ctx, p.ID, "db.a", "resync"); err != nil {
		t.Fatal(err)
	}
	if col, _ = st.GetCollection(ctx, p.ID, "db.a"); col.RequestedAction == nil || *col.RequestedAction != "resync" {
		t.Fatalf("requested action not stored: %+v", col)
	}
	// mappings and conflicts
	if err := st.UpsertFieldMappings(ctx, p.ID, "db.a", []FieldMapping{{SourceField: "x", ColumnName: "x", Type: "long"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddConflicts(ctx, p.ID, "db.a", map[string]int{"x": 3, "y": 1}); err != nil {
		t.Fatal(err)
	}
	m, _ := st.ListFieldMappings(ctx, p.ID, "db.a")
	if len(m) != 2 || m[0].ConflictCount+m[1].ConflictCount != 4 {
		t.Fatalf("mappings %+v", m)
	}
	// runs, logs, counters
	runID, _ := st.StartRun(ctx, p.ID, "db.a", "copy")
	_ = st.FinishRun(ctx, runID, "ok", 10, 100, "", map[string]any{"k": 1})
	runs, _ := st.ListRuns(ctx, p.ID, 10)
	if len(runs) != 1 || runs[0].Status != "ok" || runs[0].Rows != 10 {
		t.Fatalf("runs %+v", runs)
	}
	for i := 0; i < 5; i++ {
		_, _ = st.AppendLog(ctx, p.ID, "db.a", "info", fmt.Sprintf("line %d", i), map[string]any{"i": i})
	}
	_, _ = st.AppendLog(ctx, p.ID, "db.a", "error", "boom", nil)
	logs, _ := st.ListLogs(ctx, p.ID, LogFilter{Level: "error"})
	if len(logs) != 1 || logs[0].Message != "boom" {
		t.Fatalf("logs by level %+v", logs)
	}
	logs, _ = st.ListLogs(ctx, p.ID, LogFilter{Search: "line 3"})
	if len(logs) != 1 {
		t.Fatalf("logs by search %+v", logs)
	}
	_ = st.AddEvents(ctx, p.ID, "db.a", time.Now(), 7)
	if n, _ := st.EventsLastHour(ctx, p.ID); n != 7 {
		t.Fatalf("events last hour = %d", n)
	}
	series, _ := st.EventsSeries(ctx, p.ID, "", 5)
	if len(series) != 5 || series[4].Events != 7 {
		t.Fatalf("series %+v", series)
	}
	// settings
	if err := st.SetSetting(ctx, "k", map[string]int{"a": 1}); err != nil {
		t.Fatal(err)
	}
	var got map[string]int
	if ok, _ := st.GetSetting(ctx, "k", &got); !ok || got["a"] != 1 {
		t.Fatalf("setting %v", got)
	}
	// deleting the pipeline cascades
	if err := st.DeletePipeline(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetCollection(ctx, p.ID, "db.a"); err != ErrNotFound {
		t.Fatal("collections not cascaded")
	}
}

func TestIntegrationAdvisoryLockAndNotify(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	l1, err := st.TryLock(ctx, "p1", "c1")
	if err != nil || l1 == nil {
		t.Fatalf("first lock: %v %v", l1, err)
	}
	l2, err := st.TryLock(ctx, "p1", "c1")
	if err != nil || l2 != nil {
		t.Fatalf("second lock must fail while held: %v %v", l2, err)
	}
	if l3, _ := st.TryLock(ctx, "p1", "c2"); l3 == nil {
		t.Fatal("different collection must lock")
	} else {
		l3.Unlock(ctx)
	}
	l1.Unlock(ctx)
	if l4, _ := st.TryLock(ctx, "p1", "c1"); l4 == nil {
		t.Fatal("lock not released")
	} else {
		l4.Unlock(ctx)
	}
	// notify/listen round trip
	got := make(chan Event, 1)
	lctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go st.Listen(lctx, func(e Event) {
		select {
		case got <- e:
		default:
		}
	})
	time.Sleep(500 * time.Millisecond)
	if err := st.Notify(ctx, Event{Type: "pipeline", PipelineID: "p1"}); err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-got:
		if e.Type != "pipeline" || e.PipelineID != "p1" {
			t.Fatalf("event %+v", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("notification not received")
	}
}
