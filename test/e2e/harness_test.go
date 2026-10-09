//go:build e2e

// Package e2e runs the correctness, resync, source-removed, split-process and
// security suites of brief section 15 against a real lakesync binary and the
// local compose stack. Run with: go test -tags e2e ./test/e2e/... -v
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"lakesync/internal/query"
	"lakesync/internal/secrets"
	"lakesync/internal/store"
	"lakesync/internal/verify"

	_ "lakesync/internal/dest/iceberg"
	_ "lakesync/internal/source/faulty"
	_ "lakesync/internal/source/mongodb"
)

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

var (
	binPath    = env("LAKESYNC_E2E_BIN", "../../bin/lakesync")
	pgAdminURL = env("LAKESYNC_E2E_PG_ADMIN", "postgres://lakesync:lakesync@localhost:5432/lakesync?sslmode=disable")
	mongoURI   = env("LAKESYNC_E2E_MONGODB_URI", "mongodb://localhost:27017/?directConnection=true")
	catalogURI = env("LAKESYNC_E2E_CATALOG_URI", "http://localhost:19120/iceberg")
	s3Endpoint = env("LAKESYNC_E2E_S3_ENDPOINT", "http://localhost:9000")
	s3Key      = env("LAKESYNC_E2E_S3_KEY", "lakesync")
	s3Secret   = env("LAKESYNC_E2E_S3_SECRET", "lakesync-secret")
	secretKey  = "e2e0123456789abcdef0123456789abcdef0123456789abcdef0123456789abc"
	adminEmail = "e2e@example.com"
	adminPass  = "e2e-password-123"
)

// app is one running lakesync process (serve or worker) with its own database.
type app struct {
	t        *testing.T
	dbURL    string
	port     int
	faultDir string
	logFile  string
	cmd      *exec.Cmd
	mu       sync.Mutex
	client   *http.Client
	csrf     string
	base     string
	extraEnv []string
}

// newDatabase creates an isolated application database.
func newDatabase(t *testing.T) (url, name string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, pgAdminURL)
	if err != nil {
		t.Fatalf("connect postgres admin: %v", err)
	}
	defer conn.Close(ctx)
	name = fmt.Sprintf("lakesync_e2e_%d", time.Now().UnixNano()%1_000_000_000)
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), pgAdminURL)
		if err != nil {
			return
		}
		defer c.Close(context.Background())
		_, _ = c.Exec(context.Background(), "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1", name)
		_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name)
	})
	url = strings.Replace(pgAdminURL, "/lakesync?", "/"+name+"?", 1)
	return url, name
}

func startApp(t *testing.T, dbURL string, port int, mode string, extraEnv ...string) *app {
	t.Helper()
	a := &app{t: t, dbURL: dbURL, port: port, base: fmt.Sprintf("http://localhost:%d", port), extraEnv: extraEnv}
	a.faultDir = t.TempDir()
	a.logFile = filepath.Join(t.TempDir(), "lakesync.log")
	jar, _ := cookiejar.New(nil)
	a.client = &http.Client{Jar: jar, Timeout: 120 * time.Second}
	a.start(mode)
	t.Cleanup(func() { a.stop() })
	return a
}

func (a *app) environ() []string {
	return append(os.Environ(),
		"DATABASE_URL="+a.dbURL,
		"APP_SECRET_KEY="+secretKey,
		"ADMIN_EMAIL="+adminEmail,
		"ADMIN_PASSWORD="+adminPass,
		fmt.Sprintf("LISTEN_ADDR=:%d", a.port),
		"DEV_MODE=true",
		"ALLOW_PRIVATE_ENDPOINTS=true",
		"DISABLE_IP_LOOKUP=true",
		"DEFAULT_SYNC_INTERVAL_S=10",
		"MIN_SYNC_INTERVAL_S=5",
		"LOG_FORMAT=json",
		"LOG_LEVEL=debug",
		"LAKESYNC_ENABLE_FAULT_INJECTION=true",
		"LAKESYNC_FAULT_DIR="+a.faultDir,
	)
}

// start launches the process (mode: "serve", "serve-noworker", "worker").
func (a *app) start(mode string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	args := []string{"serve"}
	switch mode {
	case "serve-noworker":
		args = []string{"serve", "--no-worker"}
	case "worker":
		args = []string{"worker"}
	}
	cmd := exec.Command(binPath, args...)
	cmd.Env = append(a.environ(), a.extraEnv...)
	f, err := os.OpenFile(a.logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		a.t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = f, f
	if err := cmd.Start(); err != nil {
		a.t.Fatalf("start %s: %v", mode, err)
	}
	a.cmd = cmd
	go func() { _ = cmd.Wait(); f.Close() }()
	if mode != "worker" {
		a.waitReady(60 * time.Second)
	} else {
		time.Sleep(2 * time.Second)
	}
}

func (a *app) waitReady(d time.Duration) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		resp, err := http.Get(a.base + "/readyz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	a.t.Fatalf("app on %s not ready; log tail:\n%s", a.base, a.logTail(40))
}

// kill9 sends SIGKILL (the crash scenario) and waits for the process to die.
func (a *app) kill9() {
	a.mu.Lock()
	cmd := a.cmd
	a.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGKILL)
	for i := 0; i < 100; i++ {
		if cmd.ProcessState != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	// wait until the port is free
	for i := 0; i < 100; i++ {
		resp, err := http.Get(a.base + "/healthz")
		if err != nil {
			break
		}
		resp.Body.Close()
		time.Sleep(100 * time.Millisecond)
	}
}

func (a *app) stop() {
	a.mu.Lock()
	cmd := a.cmd
	a.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		for cmd.ProcessState == nil {
			time.Sleep(100 * time.Millisecond)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
	}
}

func (a *app) logTail(n int) string {
	b, err := os.ReadFile(a.logFile)
	if err != nil {
		return err.Error()
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func (a *app) logContents() string {
	b, _ := os.ReadFile(a.logFile)
	return string(b)
}

// --- API client ---

func (a *app) call(method, path string, body any, out any) (int, []byte) {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, a.base+path, rdr)
	if err != nil {
		a.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if a.csrf != "" {
		req.Header.Set("X-CSRF-Token", a.csrf)
	}
	var resp *http.Response
	for attempt := 0; attempt < 30; attempt++ {
		resp, err = a.client.Do(req)
		if err == nil {
			break
		}
		time.Sleep(time.Second) // the process may be restarting
		req.Body = nil
		if body != nil {
			b, _ := json.Marshal(body)
			req.Body = io.NopCloser(bytes.NewReader(b))
		}
	}
	if err != nil {
		a.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if out != nil && resp.StatusCode < 300 {
		if err := json.Unmarshal(data, out); err != nil {
			a.t.Fatalf("%s %s: decode: %v: %s", method, path, err, data)
		}
	}
	return resp.StatusCode, data
}

func (a *app) must(method, path string, body any, out any) []byte {
	st, data := a.call(method, path, body, out)
	if st >= 300 {
		a.t.Fatalf("%s %s -> %d: %s", method, path, st, data)
	}
	return data
}

func (a *app) login() {
	var me struct {
		CSRF string `json:"csrf_token"`
	}
	a.must("POST", "/api/auth/login", map[string]string{"email": adminEmail, "password": adminPass}, &me)
	a.csrf = me.CSRF
}

type connection struct {
	ID string `json:"id"`
}

func (a *app) createSource(name, typ string) string {
	var c connection
	a.must("POST", "/api/connections", map[string]any{"kind": "source", "type": typ, "name": name, "config": map[string]any{"uri": mongoURI}, "secrets": map[string]string{}}, &c)
	return c.ID
}

func (a *app) createDestination(name string) string {
	var c connection
	a.must("POST", "/api/connections", map[string]any{"kind": "destination", "type": "iceberg_rest", "name": name,
		"config":  map[string]any{"catalog_uri": catalogURI, "warehouse": "warehouse", "auth_type": "none", "s3_endpoint": s3Endpoint, "s3_region": "us-east-1", "s3_path_style": true},
		"secrets": map[string]string{"s3_access_key_id": s3Key, "s3_secret_access_key": s3Secret}}, &c)
	return c.ID
}

type pipeline struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	State        string `json:"state"`
	DesiredState string `json:"desired_state"`
	Namespace    string `json:"namespace"`
	Collections  []struct {
		Collection   string  `json:"collection"`
		State        string  `json:"state"`
		Generation   int64   `json:"generation"`
		LastSeq      int64   `json:"last_seq"`
		MergedSeq    int64   `json:"merged_seq"`
		SnapshotDone int64   `json:"snapshot_done"`
		LastError    *string `json:"last_error"`
		StateMessage *string `json:"state_message"`
		ResyncReason *string `json:"resync_reason"`
		CleanTable   string  `json:"clean_table"`
		RawTable     string  `json:"raw_table"`
	} `json:"collections"`
}

func (a *app) createPipeline(name, srcID, dstID string, collections []string, intervalS int) *pipeline {
	cols := make([]map[string]any, 0, len(collections))
	for _, c := range collections {
		cols = append(cols, map[string]any{"collection": c, "enabled": true})
	}
	var p pipeline
	a.must("POST", "/api/pipelines", map[string]any{"name": name, "source_id": srcID, "destination_id": dstID, "sync_interval_s": intervalS, "collections": cols, "start": true}, &p)
	return &p
}

func (a *app) getPipeline(id string) *pipeline {
	var p pipeline
	a.must("GET", "/api/pipelines/"+id, nil, &p)
	return &p
}

// waitFor polls the pipeline until cond is true or the timeout passes.
func (a *app) waitFor(id string, timeout time.Duration, what string, cond func(p *pipeline) bool) *pipeline {
	deadline := time.Now().Add(timeout)
	var last *pipeline
	for time.Now().Before(deadline) {
		st, data := a.call("GET", "/api/pipelines/"+id, nil, nil)
		if st == 200 {
			var p pipeline
			if json.Unmarshal(data, &p) == nil {
				last = &p
				if cond(&p) {
					return &p
				}
			}
		}
		time.Sleep(2 * time.Second)
	}
	b, _ := json.MarshalIndent(last, "", " ")
	a.t.Fatalf("timed out waiting for %s; last state:\n%s\nlog tail:\n%s", what, b, a.logTail(30))
	return nil
}

func allStreaming(p *pipeline) bool {
	if len(p.Collections) == 0 {
		return false
	}
	for _, c := range p.Collections {
		if c.State != "streaming" {
			return false
		}
	}
	return true
}

// waitCaughtUp waits until every collection is streaming and its sequence
// numbers stop moving for `quiet`, with merged == last.
func (a *app) waitCaughtUp(id string, timeout, quiet time.Duration) *pipeline {
	deadline := time.Now().Add(timeout)
	var stableSince time.Time
	var lastSig string
	for time.Now().Before(deadline) {
		p := a.getPipeline(id)
		sig := ""
		ok := allStreaming(p)
		for _, c := range p.Collections {
			sig += fmt.Sprintf("%s:%d/%d;", c.Collection, c.LastSeq, c.MergedSeq)
			if c.MergedSeq != c.LastSeq {
				ok = false
			}
		}
		if sig != lastSig {
			lastSig = sig
			stableSince = time.Now()
		}
		if ok && time.Since(stableSince) >= quiet {
			return p
		}
		time.Sleep(2 * time.Second)
	}
	a.t.Fatalf("pipeline %s did not catch up; log tail:\n%s", id, a.logTail(30))
	return nil
}

// runVerify runs the verify tool in-process against the app's database.
func (a *app) runVerify(pipelineName string) (ok bool, output string) {
	ctx := context.Background()
	st, err := store.Open(ctx, a.dbURL)
	if err != nil {
		a.t.Fatal(err)
	}
	defer st.Close()
	key, _ := secrets.ParseKey(secretKey)
	sec, _ := secrets.NewAESGCM(key)
	q, err := query.Open(ctx, query.Options{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		a.t.Fatal(err)
	}
	defer q.Close()
	var buf bytes.Buffer
	ok, err = verify.Run(ctx, verify.Options{Store: st, Secrets: sec, Query: q, Pipeline: pipelineName, Show: 10, Out: &buf})
	if err != nil {
		a.t.Fatalf("verify: %v", err)
	}
	return ok, buf.String()
}

// restartMongo restarts the MongoDB server: it asks mongod to shut down and the
// supervisor loop in the compose service brings it back. This is deliberately
// not a container restart: republishing a container's ports can break Colima's
// port forwarding on macOS (see DECISIONS.md). From the application's point of
// view the event is the same: every connection drops and the server returns
// with its oplog intact.
func restartMongo(t *testing.T) {
	t.Helper()
	// MongoDB only accepts `shutdown` from localhost when auth is off, so the command
	// runs inside the container (docker exec does not touch port forwarding).
	container := env("LAKESYNC_E2E_MONGO_CONTAINER", "lakesync-mongodb-1")
	cmd := exec.Command("docker", "exec", container, "mongosh", "--quiet", "--eval", "try { db.adminCommand({shutdown: 1, force: true}) } catch (e) { print('shutdown sent') }")
	out, err := cmd.CombinedOutput()
	if err != nil {
		// no docker on this host: ask through the driver (works when MongoDB runs with auth)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		client, cerr := mongo.Connect(options.Client().ApplyURI(mongoURI))
		if cerr != nil {
			t.Fatalf("docker exec failed (%v: %s) and driver connect failed: %v", err, out, cerr)
		}
		derr := client.Database("admin").RunCommand(ctx, bson.D{{Key: "shutdown", Value: 1}, {Key: "force", Value: true}}).Err()
		_ = client.Disconnect(context.Background())
		if derr != nil && !strings.Contains(derr.Error(), "connection") && !strings.Contains(derr.Error(), "EOF") && !strings.Contains(derr.Error(), "closed") {
			t.Fatalf("mongod shutdown failed: docker exec: %v (%s); driver: %v", err, out, derr)
		}
	}
	if !waitMongo(120 * time.Second) {
		t.Fatal("mongod did not come back after the shutdown; is the compose service running under the supervisor loop?")
	}
}

// waitMongo waits until a primary answers pings again.
func waitMongo(d time.Duration) bool {
	deadline := time.Now().Add(d)
	time.Sleep(2 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		client, err := mongo.Connect(options.Client().ApplyURI(mongoURI).SetServerSelectionTimeout(2 * time.Second))
		if err == nil {
			err = client.Database("admin").RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Err()
			_ = client.Disconnect(context.Background())
		}
		cancel()
		if err == nil {
			return true
		}
		time.Sleep(time.Second)
	}
	return false
}

func randomSleep(min, max time.Duration) {
	time.Sleep(min + time.Duration(rand.Int63n(int64(max-min))))
}

func freePort(base int) int { return base + rand.Intn(500) }
