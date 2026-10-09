//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"lakesync/internal/seed"
)

// TestCorrectness is brief section 15's correctness test: 50,000 seeded
// documents, the generator at its default rate with every op type, a new field
// and type conflicts, three kill -9 restarts, one MongoDB restart, one pause and
// resume, then verify: zero differences, zero duplicate keys.
func TestCorrectness(t *testing.T) {
	minutes := 10.0
	if v := env("LAKESYNC_E2E_MINUTES", ""); v != "" {
		fmt.Sscan(v, &minutes)
	}
	docs := 50000
	if v := env("LAKESYNC_E2E_DOCS", ""); v != "" {
		fmt.Sscan(v, &docs)
	}
	dbURL, _ := newDatabase(t)
	a := startApp(t, dbURL, freePort(18000), "serve")
	a.login()

	db := fmt.Sprintf("e2e_shop_%d", time.Now().Unix()%100000)
	defer dropMongoDB(t, db)
	// 50,000 documents across the four collections
	initial := map[string]int{"customers": docs / 10, "products": docs / 50, "orders": docs * 28 / 100, "events": docs - docs/10 - docs/50 - docs*28/100}
	ctx := context.Background()
	if err := seed.Run(ctx, seed.Options{URI: mongoURI, Database: db, Initial: initial, Quiet: true, Seed: 1}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	src := a.createSource("e2e mongodb", "mongodb")
	dst := a.createDestination("e2e nessie")
	p := a.createPipeline("e2e-correctness", src, dst, []string{db + ".customers", db + ".products", db + ".orders", db + ".events"}, 10)
	t.Logf("pipeline %s created; waiting for the full copy", p.ID)
	a.waitFor(p.ID, 10*time.Minute, "full copy to finish", allStreaming)
	t.Logf("full copy done; starting the generator for %.1f minutes", minutes)

	// live changes with a new field and type conflicts
	genCtx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = seed.Run(genCtx, seed.Options{URI: mongoURI, Database: db, Rate: 50, Duration: time.Duration(minutes * float64(time.Minute)), NewFieldAfter: time.Duration(minutes*float64(time.Minute)) / 5, TypeConflictAfter: time.Duration(minutes*float64(time.Minute)) * 2 / 5, Quiet: true, Seed: 2})
	}()

	total := time.Duration(minutes * float64(time.Minute))
	start := time.Now()
	// chaos schedule: three kill -9 at random moments, one MongoDB restart, one pause/resume
	type chaos struct {
		at   time.Duration
		name string
		fn   func()
	}
	events := []chaos{
		{at: total * 15 / 100, name: "kill -9 #1", fn: func() { a.kill9(); randomSleep(2*time.Second, 6*time.Second); a.start("serve") }},
		{at: total * 35 / 100, name: "pause", fn: func() { a.must("POST", "/api/pipelines/"+p.ID+"/pause", nil, nil) }},
		{at: total * 42 / 100, name: "resume", fn: func() { a.must("POST", "/api/pipelines/"+p.ID+"/start", nil, nil) }},
		{at: total * 55 / 100, name: "kill -9 #2", fn: func() { a.kill9(); randomSleep(1*time.Second, 3*time.Second); a.start("serve") }},
		{at: total * 68 / 100, name: "mongodb restart", fn: func() { restartMongo(t) }},
		{at: total * 85 / 100, name: "kill -9 #3", fn: func() { a.kill9(); randomSleep(500*time.Millisecond, 2*time.Second); a.start("serve") }},
	}
	for _, e := range events {
		// jitter each moment by up to 5% of the run so the kills land at random points
		at := e.at + time.Duration(float64(total)*0.05*float64(time.Now().UnixNano()%100)/100)
		if wait := time.Until(start.Add(at)); wait > 0 {
			time.Sleep(wait)
		}
		t.Logf("[%s] chaos: %s", time.Since(start).Round(time.Second), e.name)
		e.fn()
	}
	wg.Wait()
	cancel()
	t.Log("generator stopped; waiting for catch-up")
	a.waitCaughtUp(p.ID, 10*time.Minute, 35*time.Second)

	ok, out := a.runVerify("e2e-correctness")
	t.Log(out)
	if !ok {
		t.Fatal("verify reported differences")
	}
	// the new field and the conflicts must be visible
	var mappings []struct {
		SourceField   string `json:"source_field"`
		ConflictCount int64  `json:"conflict_count"`
	}
	a.must("GET", "/api/pipelines/"+p.ID+"/mappings?collection="+db+".customers", nil, &mappings)
	var sawNew bool
	var conflicts int64
	for _, m := range mappings {
		if m.SourceField == "loyaltyTier" {
			sawNew = true
		}
		conflicts += m.ConflictCount
	}
	if !sawNew {
		t.Error("the new field loyaltyTier did not become a column")
	}
	if conflicts == 0 {
		t.Error("expected type conflicts to be counted for customers")
	}
	// no secrets in the log
	if strings.Contains(a.logContents(), s3Secret) || strings.Contains(a.logContents(), adminPass) {
		t.Error("server log contains a secret")
	}
	// maintenance preserves data
	st, body := a.call("POST", "/api/tables/"+p.Namespace+"/orders/maintenance?destination="+dst, nil, nil)
	if st != 200 && st != 202 {
		t.Fatalf("maintenance: %d %s", st, body)
	}
	ok, out = a.runVerify("e2e-correctness")
	if !ok {
		t.Fatalf("verify after maintenance failed:\n%s", out)
	}
}

func dropMongoDB(t *testing.T, db string) {
	client, err := mongo.Connect(options.Client().ApplyURI(mongoURI))
	if err != nil {
		return
	}
	defer client.Disconnect(context.Background())
	_ = client.Database(db).Drop(context.Background())
}
