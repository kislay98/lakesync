//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"lakesync/internal/seed"
)

// TestResync injects a history-lost error through the fault-injection source,
// after deleting documents while the stream was paused, and checks that the
// generation increments, the clean table is rebuilt without the deleted
// documents, the UI state carries the reason, and verify passes.
func TestResync(t *testing.T) {
	dbURL, _ := newDatabase(t)
	a := startApp(t, dbURL, freePort(18000), "serve")
	a.login()
	db := fmt.Sprintf("e2e_resync_%d", time.Now().Unix()%100000)
	defer dropMongoDB(t, db)
	ctx := context.Background()
	if err := seed.Run(ctx, seed.Options{URI: mongoURI, Database: db, Initial: map[string]int{"customers": 500, "products": 50}, Quiet: true, Seed: 3}); err != nil {
		t.Fatal(err)
	}
	src := a.createSource("faulty mongodb", "mongodb+faults")
	dst := a.createDestination("nessie")
	p := a.createPipeline("e2e-resync", src, dst, []string{db + ".customers", db + ".products"}, 5)
	a.waitFor(p.ID, 5*time.Minute, "initial copy", allStreaming)

	client, err := mongo.Connect(options.Client().ApplyURI(mongoURI).SetTimeout(30 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Disconnect(ctx)
	coll := client.Database(db).Collection("customers")
	// some live changes first, so the stream has a real bookmark
	for i := 0; i < 20; i++ {
		if _, err := coll.InsertOne(ctx, bson.D{{Key: "name", Value: fmt.Sprintf("live-%d", i)}, {Key: "active", Value: true}}); err != nil {
			t.Fatal(err)
		}
	}
	a.waitCaughtUp(p.ID, 3*time.Minute, 12*time.Second)

	// pause: the stream stops, then documents are deleted "during the gap"
	a.must("POST", "/api/pipelines/"+p.ID+"/pause", nil, nil)
	a.waitFor(p.ID, 2*time.Minute, "pause", func(p *pipeline) bool { return p.Collections[0].State == "paused" })
	res, err := coll.DeleteMany(ctx, bson.D{{Key: "active", Value: false}})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("deleted %d customers during the gap", res.DeletedCount)
	if _, err := coll.InsertOne(ctx, bson.D{{Key: "name", Value: "after-gap"}, {Key: "active", Value: true}}); err != nil {
		t.Fatal(err)
	}
	// make the resume fail with history lost
	if err := os.WriteFile(filepath.Join(a.faultDir, "history_lost."+db+".customers"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	a.must("POST", "/api/pipelines/"+p.ID+"/start", nil, nil)
	sawResyncing := false
	a.waitFor(p.ID, 5*time.Minute, "resync to complete", func(p *pipeline) bool {
		for _, c := range p.Collections {
			if c.Collection == db+".customers" {
				if c.State == "resyncing" {
					sawResyncing = true
				}
				return c.State == "streaming" && c.Generation == 2
			}
		}
		return false
	})
	final := a.waitCaughtUp(p.ID, 3*time.Minute, 12*time.Second)
	for _, c := range final.Collections {
		if c.Collection == db+".customers" {
			if c.Generation != 2 {
				t.Errorf("generation = %d, want 2", c.Generation)
			}
			if c.ResyncReason == nil || *c.ResyncReason == "" {
				t.Error("resync reason not recorded for the UI")
			} else {
				t.Logf("resync reason shown in UI: %s", *c.ResyncReason)
			}
		}
	}
	if !sawResyncing {
		t.Log("note: the resyncing state was not observed by polling (it may have been brief)")
	}
	ok, out := a.runVerify("e2e-resync")
	t.Log(out)
	if !ok {
		t.Fatal("verify after resync reported differences (deleted documents must be gone)")
	}
	// the other collection kept its generation
	for _, c := range final.Collections {
		if c.Collection == db+".products" && c.Generation != 1 {
			t.Errorf("products generation changed to %d", c.Generation)
		}
	}
}
