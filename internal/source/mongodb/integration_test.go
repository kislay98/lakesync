//go:build integration

package mongodb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"lakesync/internal/core"
	"lakesync/internal/source"
)

func testURI() string {
	if u := os.Getenv("LAKESYNC_TEST_MONGODB_URI"); u != "" {
		return u
	}
	return "mongodb://localhost:27017/?directConnection=true"
}

func newTestSource(t *testing.T) (*Source, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dbName := fmt.Sprintf("lakesync_it_%d", time.Now().UnixNano())
	src, err := New(ctx, Config{URI: testURI(), Database: dbName}, Secrets{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cctx, c := context.WithTimeout(context.Background(), 20*time.Second)
		defer c()
		_ = src.client.Database(dbName).Drop(cctx)
		_ = src.Close()
	})
	return src, dbName
}

func collect(t *testing.T, it source.EventIterator, n int, timeout time.Duration) []*core.ChangeEvent {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var out []*core.ChangeEvent
	for len(out) < n {
		ev, err := it.Next(ctx)
		if err != nil {
			t.Fatalf("after %d events: %v", len(out), err)
		}
		out = append(out, ev)
	}
	return out
}

func TestIntegrationChecklist(t *testing.T) {
	src, dbName := newTestSource(t)
	ctx := context.Background()
	if _, err := src.client.Database(dbName).Collection("c1").InsertOne(ctx, bson.D{{Key: "x", Value: 1}}); err != nil {
		t.Fatal(err)
	}
	rep := src.Test(ctx)
	for _, s := range rep.Steps {
		t.Logf("%-24s %-4s %4dms %s %s", s.Name, s.Status, s.Millis, s.Message, s.Hint)
	}
	if !rep.OK {
		t.Fatalf("checklist failed: %+v", rep)
	}
	if len(rep.Steps) != 8 || rep.Info["server_version"] == "" || rep.Info["topology"] == "" {
		t.Errorf("steps=%d info=%v", len(rep.Steps), rep.Info)
	}
	// wrong password against the same server must fail at Authenticate with the credentials hint
	bad, err := New(ctx, Config{URI: testURI(), Username: "nobody"}, Secrets{Password: "wrong"})
	if err != nil {
		t.Fatal(err)
	}
	defer bad.Close()
	brep := bad.Test(ctx)
	if brep.OK || brep.Steps[3].Name != "Authenticate" || brep.Steps[3].Status != core.TestFail || brep.Steps[3].Hint != hintAuth {
		t.Errorf("bad credentials: %+v", brep.Steps)
	}
}

func TestIntegrationEntitiesAndSnapshot(t *testing.T) {
	src, dbName := newTestSource(t)
	ctx := context.Background()
	coll := src.client.Database(dbName).Collection("items")
	const n = 12000
	docs := make([]any, 0, n)
	for i := 0; i < n; i++ {
		var id any
		switch i % 4 { // mixed _id types in one collection
		case 0:
			id = bson.NewObjectID()
		case 1:
			id = fmt.Sprintf("s-%06d", i)
		case 2:
			id = int64(i)
		case 3:
			id = bson.D{{Key: "k", Value: i}}
		}
		docs = append(docs, bson.D{{Key: "_id", Value: id}, {Key: "i", Value: i}, {Key: "tags", Value: bson.A{"a", i}}})
	}
	if _, err := coll.InsertMany(ctx, docs); err != nil {
		t.Fatal(err)
	}
	if err := src.client.Database(dbName).CreateView(ctx, "v1", "items", mongo.Pipeline{}); err != nil {
		t.Fatal(err)
	}
	ents, err := src.ListEntities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || ents[0].Name != "items" || ents[0].EstimatedCount != n || ents[0].EstimatedBytes <= 0 {
		t.Fatalf("entities: %+v", ents)
	}
	e := ents[0]
	sample, err := src.Sample(ctx, e, 10)
	if err != nil || len(sample) != 10 {
		t.Fatalf("sample: %d %v", len(sample), err)
	}

	// full snapshot
	it, err := src.Snapshot(ctx, e, "", source.SnapshotOptions{ChunkSize: 5000})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	var cursorAt6000 core.SnapshotCursor
	var count int
	for {
		ev, err := it.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if ev.Op != core.OpSnapshot || ev.Doc == nil || ev.Bookmark != nil || ev.Key == "" {
			t.Fatalf("bad snapshot event %+v", ev)
		}
		seen[ev.Key]++
		count++
		if count == 6000 {
			cursorAt6000 = core.SnapshotCursor(ev.Key)
		}
	}
	it.Close()
	if count != n || len(seen) != n {
		t.Fatalf("snapshot delivered %d events, %d distinct keys, want %d", count, len(seen), n)
	}

	// resume from the middle: the union of both halves must be exactly n distinct keys with no overlap
	it2, err := src.Snapshot(ctx, e, cursorAt6000, source.SnapshotOptions{ChunkSize: 5000, MaxDocsPerSec: 100000})
	if err != nil {
		t.Fatal(err)
	}
	rest := 0
	for {
		ev, err := it2.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		rest++
		if seen[ev.Key] != 1 {
			t.Fatalf("resumed snapshot produced unknown key %s", ev.Key)
		}
		seen[ev.Key]++
	}
	it2.Close()
	if rest != n-6000 {
		t.Fatalf("resumed snapshot delivered %d events, want %d", rest, n-6000)
	}
	twice := 0
	for _, c := range seen {
		if c == 2 {
			twice++
		}
	}
	if twice != n-6000 {
		t.Fatalf("resume overlap: %d keys seen twice, want %d", twice, n-6000)
	}
}

func TestIntegrationStream(t *testing.T) {
	src, dbName := newTestSource(t)
	ctx := context.Background()
	coll := src.client.Database(dbName).Collection("orders")
	if _, err := coll.InsertOne(ctx, bson.D{{Key: "_id", Value: "warmup"}}); err != nil {
		t.Fatal(err)
	}
	e := core.Entity{Namespace: dbName, Name: "orders"}
	bm, err := src.CurrentBookmark(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	kind, _, _, err := DecodeBookmark(bm)
	if err != nil || kind != "token" {
		t.Fatalf("bookmark kind %q err %v", kind, err)
	}
	it, err := src.Stream(ctx, e, bm)
	if err != nil {
		t.Fatal(err)
	}
	defer it.Close()
	oid := bson.NewObjectID()
	writes := []func() error{
		func() error {
			_, err := coll.InsertOne(ctx, bson.D{{Key: "_id", Value: oid}, {Key: "status", Value: "new"}, {Key: "total", Value: int32(5)}})
			return err
		},
		func() error {
			_, err := coll.UpdateByID(ctx, oid, bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: "paid"}}}})
			return err
		},
		func() error {
			_, err := coll.ReplaceOne(ctx, bson.D{{Key: "_id", Value: oid}}, bson.D{{Key: "status", Value: "shipped"}, {Key: "total", Value: 7.5}})
			return err
		},
		func() error {
			_, err := coll.DeleteOne(ctx, bson.D{{Key: "_id", Value: oid}})
			return err
		},
	}
	var evs []*core.ChangeEvent
	for _, w := range writes {
		if err := w(); err != nil {
			t.Fatal(err)
		}
		evs = append(evs, collect(t, it, 1, 20*time.Second)...)
	}
	wantOps := []core.Op{core.OpInsert, core.OpUpdate, core.OpReplace, core.OpDelete}
	for i, ev := range evs {
		if ev.Op != wantOps[i] || ev.Key != `{"$oid":"`+oid.Hex()+`"}` || ev.DisplayID != oid.Hex() || ev.Entity != e.String() || len(ev.Bookmark) == 0 || ev.SourceTS.IsZero() {
			t.Fatalf("event %d: %+v", i, ev)
		}
		if ev.Op == core.OpDelete {
			if ev.Doc != nil {
				t.Fatalf("delete carries a doc")
			}
			continue
		}
		if ev.Doc == nil {
			t.Fatalf("event %d has no doc", i)
		}
		status, _ := ev.Doc.Get("status")
		want := map[core.Op]string{core.OpInsert: "new", core.OpUpdate: "paid", core.OpReplace: "shipped"}[ev.Op]
		if status.Text() != want {
			t.Fatalf("event %d status %q want %q", i, status.Text(), want)
		}
	}
	// bookmark round trip: continue from the delete's bookmark and see exactly 3 new docs
	last := evs[3].Bookmark
	for i := 0; i < 3; i++ {
		if _, err := coll.InsertOne(ctx, bson.D{{Key: "_id", Value: fmt.Sprintf("late-%d", i)}}); err != nil {
			t.Fatal(err)
		}
	}
	it2, err := src.Stream(ctx, e, last)
	if err != nil {
		t.Fatal(err)
	}
	defer it2.Close()
	late := collect(t, it2, 3, 20*time.Second)
	for i, ev := range late {
		if ev.Op != core.OpInsert || ev.Key != fmt.Sprintf(`"late-%d"`, i) || ev.DisplayID != fmt.Sprintf("late-%d", i) {
			t.Fatalf("late event %d: %+v", i, ev)
		}
	}
	nctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	if ev, err := it2.Next(nctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected no more events, got %+v %v", ev, err)
	}

	// operationTime bookmark form also streams
	ts, err := src.operationTime(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ob, _ := encodeOpTimeBookmark(ts)
	it3, err := src.Stream(ctx, e, ob)
	if err != nil {
		t.Fatal(err)
	}
	defer it3.Close()
	if _, err := coll.InsertOne(ctx, bson.D{{Key: "_id", Value: "optime-1"}}); err != nil {
		t.Fatal(err)
	}
	// startAtOperationTime is inclusive, so writes from the same second may replay first
	deadline := time.Now().Add(20 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("optime stream never delivered optime-1")
		}
		got := collect(t, it3, 1, 20*time.Second)
		if got[0].Key == `"optime-1"` {
			break
		}
	}
}

func TestIntegrationSkippedUpdate(t *testing.T) {
	src, dbName := newTestSource(t)
	ctx := context.Background()
	coll := src.client.Database(dbName).Collection("fast")
	e := core.Entity{Namespace: dbName, Name: "fast"}
	if _, err := coll.InsertOne(ctx, bson.D{{Key: "_id", Value: "w"}}); err != nil {
		t.Fatal(err)
	}
	bm, err := src.CurrentBookmark(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("d-%d", i)
		if _, err := coll.InsertOne(ctx, bson.D{{Key: "_id", Value: id}, {Key: "v", Value: 0}}); err != nil {
			t.Fatal(err)
		}
		if _, err := coll.UpdateByID(ctx, id, bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: 1}}}}); err != nil {
			t.Fatal(err)
		}
		if _, err := coll.DeleteOne(ctx, bson.D{{Key: "_id", Value: id}}); err != nil {
			t.Fatal(err)
		}
	}
	it, err := src.Stream(ctx, e, bm)
	if err != nil {
		t.Fatal(err)
	}
	defer it.Close()
	// inserts and deletes always arrive; updates arrive only when the lookup still found the doc
	var ins, upd, del int
	deadline := time.Now().Add(20 * time.Second)
	for del < 20 && time.Now().Before(deadline) {
		nctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		ev, err := it.Next(nctx)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		switch ev.Op {
		case core.OpInsert:
			ins++
		case core.OpUpdate:
			upd++
			if ev.Doc == nil {
				t.Fatal("update without doc")
			}
		case core.OpDelete:
			del++
		}
	}
	if ins != 20 || del != 20 || upd+int(src.SkippedUpdates()) != 20 {
		t.Fatalf("ins=%d upd=%d del=%d skipped=%d", ins, upd, del, src.SkippedUpdates())
	}
}

func TestIntegrationSourceRemoved(t *testing.T) {
	src, dbName := newTestSource(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		action func(coll *mongo.Collection) error
		want   string
	}{
		{"drop", func(c *mongo.Collection) error { return c.Drop(ctx) }, "was dropped"},
		{"rename", func(c *mongo.Collection) error {
			return src.client.Database("admin").RunCommand(ctx, bson.D{
				{Key: "renameCollection", Value: dbName + "." + c.Name()}, {Key: "to", Value: dbName + "." + c.Name() + "_renamed"}}).Err()
		}, "was renamed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			coll := src.client.Database(dbName).Collection("rm_" + tc.name)
			if _, err := coll.InsertOne(ctx, bson.D{{Key: "_id", Value: 1}}); err != nil {
				t.Fatal(err)
			}
			e := core.Entity{Namespace: dbName, Name: coll.Name()}
			bm, err := src.CurrentBookmark(ctx, e)
			if err != nil {
				t.Fatal(err)
			}
			it, err := src.Stream(ctx, e, bm)
			if err != nil {
				t.Fatal(err)
			}
			defer it.Close()
			if err := tc.action(coll); err != nil {
				t.Fatal(err)
			}
			nctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			_, err = it.Next(nctx)
			var sr *core.SourceRemovedError
			if !errors.As(err, &sr) {
				t.Fatalf("want SourceRemovedError, got %v", err)
			}
			if !contains(sr.Reason, tc.want) {
				t.Fatalf("reason %q", sr.Reason)
			}
		})
	}
}

func TestIntegrationHistoryLost(t *testing.T) {
	// MongoDB only reports ChangeStreamHistoryLost (286) once the oplog has really
	// been truncated past the resume point; a fresh replica set accepts a token with
	// an ancient timestamp and simply replays everything. Forcing truncation needs
	// about 1 GB of churn (replSetResizeOplog's minimum), so this test only proves
	// that such a token does not crash the iterator. The mapping of codes 286/280 is
	// unit-tested in TestClassifyServerErrors and the engine's resync path is
	// exercised by the fault-injection test in test/e2e.
	src, dbName := newTestSource(t)
	ctx := context.Background()
	coll := src.client.Database(dbName).Collection("hl")
	if _, err := coll.InsertOne(ctx, bson.D{{Key: "_id", Value: 1}}); err != nil {
		t.Fatal(err)
	}
	e := core.Entity{Namespace: dbName, Name: "hl"}
	bm, err := src.CurrentBookmark(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	_, tok, _, err := DecodeBookmark(bm)
	if err != nil {
		t.Fatal(err)
	}
	data := tok.Lookup("_data").StringValue()
	if len(data) < 18 || data[:2] != "82" {
		t.Skipf("unexpected resume token layout %q", data)
	}
	patched := data[:2] + "0000000100000001" + data[18:]
	raw, err := bson.Marshal(bson.D{{Key: "_data", Value: patched}})
	if err != nil {
		t.Fatal(err)
	}
	old, err := encodeTokenBookmark(bson.Raw(raw))
	if err != nil {
		t.Fatal(err)
	}
	it, err := src.Stream(ctx, e, old)
	if err != nil {
		if !errors.Is(err, core.ErrHistoryLost) {
			t.Fatalf("unexpected error class: %v", err)
		}
		return
	}
	defer it.Close()
	nctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ev, err := it.Next(nctx)
	switch {
	case errors.Is(err, core.ErrHistoryLost):
		t.Log("server reported history lost")
	case err == nil && ev.Key == `{"$numberLong":"1"}`:
		t.Skip("server replayed from the start of its oplog instead of reporting history lost (oplog never truncated)")
	default:
		t.Fatalf("unexpected result: %+v %v", ev, err)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})())
}
