//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// TestSourceRemoved drops one collection and renames another while streaming,
// and checks the states, the messages, and that the third collection keeps flowing.
func TestSourceRemoved(t *testing.T) {
	dbURL, _ := newDatabase(t)
	a := startApp(t, dbURL, freePort(18000), "serve")
	a.login()
	db := fmt.Sprintf("e2e_removed_%d", time.Now().Unix()%100000)
	defer dropMongoDB(t, db)
	ctx := context.Background()
	client, err := mongo.Connect(options.Client().ApplyURI(mongoURI))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Disconnect(ctx)
	for _, c := range []string{"a", "b", "c"} {
		for i := 0; i < 50; i++ {
			if _, err := client.Database(db).Collection(c).InsertOne(ctx, bson.D{{Key: "n", Value: i}}); err != nil {
				t.Fatal(err)
			}
		}
	}
	src := a.createSource("mongodb", "mongodb")
	dst := a.createDestination("nessie")
	p := a.createPipeline("e2e-removed", src, dst, []string{db + ".a", db + ".b", db + ".c"}, 5)
	a.waitFor(p.ID, 5*time.Minute, "initial copy", allStreaming)

	if err := client.Database(db).Collection("a").Drop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := client.Database(db).RunCommand(ctx, bson.D{{Key: "renameCollection", Value: db + ".b"}, {Key: "to", Value: db + ".b_renamed"}}).Err(); err != nil {
		// renameCollection must run against admin
		if err := client.Database("admin").RunCommand(ctx, bson.D{{Key: "renameCollection", Value: db + ".b"}, {Key: "to", Value: db + ".b_renamed"}}).Err(); err != nil {
			t.Fatal(err)
		}
	}
	// keep c flowing
	stop := make(chan struct{})
	go func() {
		i := 100
		for {
			select {
			case <-stop:
				return
			case <-time.After(200 * time.Millisecond):
				_, _ = client.Database(db).Collection("c").InsertOne(ctx, bson.D{{Key: "n", Value: i}})
				i++
			}
		}
	}()
	defer close(stop)
	final := a.waitFor(p.ID, 3*time.Minute, "a and b to be marked source removed", func(p *pipeline) bool {
		removed := 0
		for _, c := range p.Collections {
			if (c.Collection == db+".a" || c.Collection == db+".b") && c.State == "source_removed" {
				removed++
			}
		}
		return removed == 2
	})
	for _, c := range final.Collections {
		switch c.Collection {
		case db + ".a", db + ".b":
			if c.StateMessage == nil || !strings.Contains(strings.ToLower(*c.StateMessage), "source") {
				t.Errorf("%s: expected a clear message, got %v", c.Collection, c.StateMessage)
			} else {
				t.Logf("%s: %s", c.Collection, *c.StateMessage)
			}
		}
	}
	// c keeps flowing: its sequence grows
	before := seqOf(final, db+".c")
	a.waitFor(p.ID, 2*time.Minute, "c to keep flowing", func(p *pipeline) bool { return seqOf(p, db+".c") > before+5 && stateOf(p, db+".c") == "streaming" })
	if p2 := a.getPipeline(p.ID); p2.State != "source_removed" && p2.State != "streaming" {
		t.Errorf("pipeline state %s", p2.State)
	}
	// a and b no longer exist at the source, so only c can be verified
	ok, out := a.runVerify("e2e-removed")
	if !ok {
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, "FAIL") && strings.Contains(line, db+".c ") {
				t.Fatalf("verify of c failed: %s", out)
			}
		}
	}
}

func seqOf(p *pipeline, coll string) int64 {
	for _, c := range p.Collections {
		if c.Collection == coll {
			return c.LastSeq
		}
	}
	return -1
}

func stateOf(p *pipeline, coll string) string {
	for _, c := range p.Collections {
		if c.Collection == coll {
			return c.State
		}
	}
	return ""
}
