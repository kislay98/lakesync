//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"lakesync/internal/seed"
)

// TestBenchmark measures the numbers reported in docs/BENCHMARKS.md. It only
// runs when LAKESYNC_BENCH=1 (it takes a while and loads the machine).
//
//	LAKESYNC_BENCH=1 LAKESYNC_BENCH_DOCS=1000000 go test -tags e2e ./test/e2e/ -run TestBenchmark -v -timeout 3h
func TestBenchmark(t *testing.T) {
	if os.Getenv("LAKESYNC_BENCH") != "1" {
		t.Skip("set LAKESYNC_BENCH=1 to run the benchmark")
	}
	docs := 1_000_000
	if v := env("LAKESYNC_BENCH_DOCS", ""); v != "" {
		fmt.Sscan(v, &docs)
	}
	rate := 500.0
	if v := env("LAKESYNC_BENCH_RATE", ""); v != "" {
		fmt.Sscan(v, &rate)
	}
	streamFor := 120 * time.Second
	if v := env("LAKESYNC_BENCH_STREAM", ""); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			streamFor = d
		}
	}
	commits := 60
	if v := env("LAKESYNC_BENCH_COMMITS", ""); v != "" {
		fmt.Sscan(v, &commits)
	}
	dbURL, _ := newDatabase(t)
	a := startApp(t, dbURL, freePort(18500), "serve", "DUCKDB_MEMORY_LIMIT=6GB")
	a.login()
	db := fmt.Sprintf("bench_%d", time.Now().Unix()%100000)
	defer dropMongoDB(t, db)
	ctx := context.Background()
	report := []string{}
	logf := func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		t.Log(line)
		report = append(report, line)
	}

	// 1. full copy
	t0 := time.Now()
	if err := seed.Run(ctx, seed.Options{URI: mongoURI, Database: db, Initial: map[string]int{"events": docs, "customers": 2000}, Quiet: true, Seed: 7}); err != nil {
		t.Fatal(err)
	}
	logf("seed: %d events + 2,000 customers inserted into MongoDB in %s", docs, time.Since(t0).Round(time.Second))
	src := a.createSource("bench mongodb", "mongodb")
	dst := a.createDestination("bench nessie")
	t0 = time.Now()
	p := a.createPipeline("bench", src, dst, []string{db + ".events", db + ".customers"}, 10)
	a.waitFor(p.ID, 2*time.Hour, "full copy", allStreaming)
	copyDur := time.Since(t0)
	logf("1. full copy: %d documents in %s = %.0f docs/s (table creation, Parquet writes, commits and the first merge included)", docs+2000, copyDur.Round(time.Second), float64(docs+2000)/copyDur.Seconds())

	// 2. streaming throughput + 3. end-to-end latency
	genCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		_ = seed.Run(genCtx, seed.Options{URI: mongoURI, Database: db, Rate: rate, Duration: streamFor, Quiet: true, Seed: 8, Mix: seed.Mix{Insert: 70, Update: 20, Replace: 5, Delete: 5}})
	}()
	before := a.getPipeline(p.ID)
	startSeq := int64(0)
	for _, c := range before.Collections {
		startSeq += c.LastSeq
	}
	client, err := mongo.Connect(options.Client().ApplyURI(mongoURI))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Disconnect(ctx)
	var latencies []time.Duration
	deadline := time.Now().Add(streamFor)
	for i := 0; time.Now().Before(deadline); i++ {
		marker := fmt.Sprintf("marker-%d-%d", time.Now().UnixNano(), i)
		w := time.Now()
		if _, err := client.Database(db).Collection("customers").InsertOne(ctx, bson.D{{Key: "name", Value: marker}, {Key: "active", Value: true}}); err != nil {
			t.Fatal(err)
		}
		filter := url.QueryEscape(fmt.Sprintf(`[{"column":"name","op":"eq","value":%q}]`, marker))
		for {
			st, body := a.call("GET", fmt.Sprintf("/api/tables/%s/customers/rows?limit=1&total=false&destination=%s&filter=%s", p.Namespace, dst, filter), nil, nil)
			if st == 200 {
				var r struct {
					Rows [][]any `json:"rows"`
				}
				if json.Unmarshal(body, &r) == nil && len(r.Rows) > 0 {
					break
				}
			}
			if time.Since(w) > 3*time.Minute {
				t.Fatalf("marker %s never became visible", marker)
			}
			time.Sleep(500 * time.Millisecond)
		}
		latencies = append(latencies, time.Since(w))
		time.Sleep(15 * time.Second)
	}
	cancel()
	// let the last batches commit
	final := a.waitCaughtUp(p.ID, 10*time.Minute, 25*time.Second)
	endSeq := int64(0)
	for _, c := range final.Collections {
		endSeq += c.LastSeq
	}
	logf("2. streaming: generator at %.0f ops/s for %s; %d events written to raw tables = %.0f events/s sustained (bounded by the generator rate, not the sink)", rate, streamFor, endSeq-startSeq, float64(endSeq-startSeq)/streamFor.Seconds())
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	if n := len(latencies); n > 0 {
		logf("3. end-to-end latency (MongoDB insert -> visible in the data viewer, 10 s sync interval, %d samples): median %s, p95 %s, max %s", n, latencies[n/2].Round(100*time.Millisecond), latencies[n*95/100].Round(100*time.Millisecond), latencies[n-1].Round(100*time.Millisecond))
	}

	// 4. viewer page time on the big table
	rowsURL := fmt.Sprintf("/api/tables/%s/events/rows?destination=%s&limit=100", p.Namespace, dst)
	timeIt := func(label, u string) {
		var best time.Duration
		for i := 0; i < 3; i++ {
			t1 := time.Now()
			st, body := a.call("GET", u, nil, nil)
			d := time.Since(t1)
			if st != 200 {
				t.Fatalf("%s: %d %s", label, st, body)
			}
			if i == 0 || d < best {
				best = d
			}
		}
		logf("4. viewer %s on %d rows: %s (best of 3)", label, docs, best.Round(time.Millisecond))
	}
	timeIt("first page (with total count)", rowsURL)
	timeIt("first page (no count)", rowsURL+"&total=false")
	timeIt("sorted page (value desc)", rowsURL+"&sort=-value&total=false")
	timeIt("filtered page (value > 900)", rowsURL+"&total=false&filter="+url.QueryEscape(`[{"column":"value","op":"gt","value":900}]`))
	timeIt("search page (q=checkout)", rowsURL+"&total=false&q=checkout")
	timeIt("second page (keyset cursor)", rowsURL+"&total=false&cursor="+url.QueryEscape(secondPageCursor(a, rowsURL)))

	// 5. files and snapshots after many commits, before and after maintenance
	a.must("PUT", "/api/pipelines/"+p.ID, map[string]any{"name": "bench", "sync_interval_s": 5}, nil)
	gen2, cancel2 := context.WithCancel(ctx)
	go func() {
		_ = seed.Run(gen2, seed.Options{URI: mongoURI, Database: db, Rate: 20, Duration: time.Duration(commits) * 5 * time.Second, Quiet: true, Seed: 9})
	}()
	time.Sleep(time.Duration(commits)*5*time.Second + 10*time.Second)
	cancel2()
	a.waitCaughtUp(p.ID, 5*time.Minute, 15*time.Second)
	health := func(table string) (files, small, deletes, snaps int, bytes int64) {
		var h struct {
			Health struct {
				DataFiles   int   `json:"data_files"`
				SmallFiles  int   `json:"small_files"`
				DeleteFiles int   `json:"delete_files"`
				Snapshots   int   `json:"snapshots"`
				TotalBytes  int64 `json:"total_bytes"`
			} `json:"health"`
		}
		a.must("GET", fmt.Sprintf("/api/tables/%s/%s/health?destination=%s", p.Namespace, table, dst), nil, &h)
		return h.Health.DataFiles, h.Health.SmallFiles, h.Health.DeleteFiles, h.Health.Snapshots, h.Health.TotalBytes
	}
	for _, tbl := range []string{"events__raw", "events"} {
		f, s, d, n, b := health(tbl)
		logf("5. %s after ~%d commits at a 5 s interval: %d data files (%d small), %d delete files, %d snapshots visible through the catalog, %d bytes", tbl, commits, f, s, d, n, b)
	}
	logf("   extrapolation: 24 h at a 60 s interval is 1,440 commits; without maintenance each commit adds one data file per partition day (raw) and one delete file plus one data file per merge that changed rows (clean)")
	st, body := a.call("POST", fmt.Sprintf("/api/tables/%s/events/maintenance?destination=%s", p.Namespace, dst), nil, nil)
	if st != 200 && st != 202 {
		t.Fatalf("maintenance: %d %s", st, body)
	}
	for _, tbl := range []string{"events__raw", "events"} {
		f, s, d, n, b := health(tbl)
		logf("5. %s after maintenance: %d data files (%d small), %d delete files, %d snapshots, %d bytes", tbl, f, s, d, n, b)
	}
	ok, out := a.runVerify("bench")
	if !ok {
		t.Fatalf("verify after benchmark: %s", out)
	}
	logf("   verify after maintenance: OK (zero differences, zero duplicates)")
	_ = os.WriteFile(env("LAKESYNC_BENCH_OUT", "/tmp/lakesync-bench.txt"), []byte(strings.Join(report, "\n")+"\n"), 0o644)
}

func secondPageCursor(a *app, rowsURL string) string {
	_, body := a.call("GET", rowsURL+"&total=false", nil, nil)
	var r struct {
		Next string `json:"next_cursor"`
	}
	_ = json.Unmarshal(body, &r)
	return r.Next
}
