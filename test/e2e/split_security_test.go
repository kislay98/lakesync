//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// TestSplitProcesses runs the API without a worker and a separate worker
// process on the same database, and checks a pipeline still streams.
func TestSplitProcesses(t *testing.T) {
	dbURL, _ := newDatabase(t)
	api := startApp(t, dbURL, freePort(18000), "serve-noworker")
	worker := startApp(t, dbURL, 0, "worker")
	_ = worker
	api.login()
	var ready struct {
		Checks map[string]string `json:"checks"`
	}
	api.must("GET", "/readyz", nil, &ready)
	if !strings.Contains(ready.Checks["worker"], "external") {
		t.Errorf("readyz worker check = %q", ready.Checks["worker"])
	}
	db := fmt.Sprintf("e2e_split_%d", time.Now().Unix()%100000)
	defer dropMongoDB(t, db)
	ctx := context.Background()
	client, err := mongo.Connect(options.Client().ApplyURI(mongoURI).SetTimeout(30 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Disconnect(ctx)
	for i := 0; i < 30; i++ {
		_, _ = client.Database(db).Collection("items").InsertOne(ctx, bson.D{{Key: "n", Value: i}})
	}
	src := api.createSource("mongodb", "mongodb")
	dst := api.createDestination("nessie")
	p := api.createPipeline("e2e-split", src, dst, []string{db + ".items"}, 5)
	api.waitFor(p.ID, 5*time.Minute, "worker process to copy and stream", allStreaming)
	for i := 100; i < 110; i++ {
		_, _ = client.Database(db).Collection("items").InsertOne(ctx, bson.D{{Key: "n", Value: i}})
	}
	api.waitFor(p.ID, 2*time.Minute, "streamed inserts", func(p *pipeline) bool { return p.Collections[0].LastSeq >= 40 })
	ok, out := api.runVerify("e2e-split")
	if !ok {
		t.Fatalf("verify: %s", out)
	}
}

// TestSecurity checks that the API never returns secrets, that logs contain no
// secrets, and that the SQL console rejects writes and file access.
func TestSecurity(t *testing.T) {
	dbURL, _ := newDatabase(t)
	a := startApp(t, dbURL, freePort(18000), "serve")
	// unauthenticated requests are rejected
	if st, _ := a.call("GET", "/api/pipelines", nil, nil); st != 401 {
		t.Errorf("unauthenticated GET /api/pipelines = %d", st)
	}
	a.login()
	src := a.createSource("mongodb", "mongodb")
	dst := a.createDestination("nessie")
	// CSRF: a state-changing request without the token is refused
	saved := a.csrf
	a.csrf = ""
	if st, _ := a.call("POST", "/api/pipelines/nope/start", nil, nil); st != 403 {
		t.Errorf("POST without CSRF token = %d, want 403", st)
	}
	a.csrf = saved
	for _, path := range []string{"/api/connections", "/api/connections/" + dst, "/api/connections/" + src, "/api/pipelines", "/api/overview", "/api/settings"} {
		_, body := a.call("GET", path, nil, nil)
		if strings.Contains(string(body), s3Secret) || strings.Contains(string(body), "lakesync-secret") {
			t.Errorf("%s leaks the S3 secret", path)
		}
	}
	// the stored secret is reported by name only
	var c struct {
		SecretFields []string               `json:"secret_fields"`
		Config       map[string]interface{} `json:"config"`
	}
	a.must("GET", "/api/connections/"+dst, nil, &c)
	if len(c.SecretFields) != 2 {
		t.Errorf("secret_fields = %v", c.SecretFields)
	}
	if _, leaked := c.Config["s3_secret_access_key"]; leaked {
		t.Error("config contains the secret key")
	}
	// SQL console rejects everything but SELECT/EXPLAIN
	for _, q := range []string{"INSERT INTO x VALUES (1)", "COPY (SELECT 1) TO '/tmp/x.csv'", "ATTACH ':memory:' AS y", "INSTALL spatial", "LOAD spatial", "SELECT * FROM read_csv('/etc/passwd')", "SELECT * FROM read_text('/etc/hosts')", "CREATE TABLE t AS SELECT 1", "SELECT * FROM duckdb_secrets()"} {
		st, body := a.call("POST", "/api/query", map[string]any{"destination_id": dst, "sql": q}, nil)
		if st != 400 {
			t.Errorf("query %q accepted with %d: %s", q, st, body)
		}
	}
	var qr struct {
		Result struct {
			RowCount int `json:"row_count"`
		} `json:"result"`
	}
	a.must("POST", "/api/query", map[string]any{"destination_id": dst, "sql": "SELECT 1 AS one"}, &qr)
	if qr.Result.RowCount != 1 {
		t.Errorf("SELECT 1 returned %d rows", qr.Result.RowCount)
	}
	// rate limiting on login
	for i := 0; i < 6; i++ {
		a.call("POST", "/api/auth/login", map[string]string{"email": "nobody@example.com", "password": "wrong"}, nil)
	}
	st, _ := a.call("POST", "/api/auth/login", map[string]string{"email": "nobody@example.com", "password": "wrong"}, nil)
	if st != 429 {
		t.Errorf("login after 6 failures = %d, want 429", st)
	}
	time.Sleep(time.Second)
	if log := a.logContents(); strings.Contains(log, s3Secret) || strings.Contains(log, adminPass) {
		t.Error("log contains a secret")
	}
	// the openapi document lists every route
	_, doc := a.call("GET", "/api/openapi.json", nil, nil)
	var spec struct {
		Paths map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(doc, &spec); err != nil || len(spec.Paths) < 30 {
		t.Errorf("openapi document has %d paths (err %v)", len(spec.Paths), err)
	}
}
