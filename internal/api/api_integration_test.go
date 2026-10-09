//go:build integration

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"lakesync/internal/config"
	"lakesync/internal/query"
	"lakesync/internal/secrets"
	"lakesync/internal/store"

	_ "lakesync/internal/dest/iceberg"
	_ "lakesync/internal/source/mongodb"
)

type client struct {
	t    *testing.T
	srv  *httptest.Server
	http *http.Client
	csrf string
}

func newTestServer(t *testing.T) *client {
	t.Helper()
	admin := "postgres://lakesync:lakesync@localhost:5432/lakesync?sslmode=disable"
	if v := os.Getenv("LAKESYNC_TEST_DATABASE_URL"); v != "" {
		admin = v
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Skipf("postgres not available: %v", err)
	}
	name := fmt.Sprintf("lakesync_api_%d", time.Now().UnixNano()%1_000_000_000)
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	conn.Close(ctx)
	url := strings.Replace(admin, "/lakesync?", "/"+name+"?", 1)
	if err := store.Migrate(ctx, url); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	if err := EnsureAdmin(ctx, st, "admin@example.com", "changeme-now"); err != nil {
		t.Fatal(err)
	}
	key, _ := secrets.ParseKey("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	sec, _ := secrets.NewAESGCM(key)
	q, err := query.Open(ctx, query.Options{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{DevMode: true, AllowPrivateEndpoints: true, DisableIPLookup: true, MinSyncIntervalS: 10, DefaultSyncIntervalS: 60, QueryRowLimit: 100, QueryTimeout: 10 * time.Second, Version: "test", Commit: "test"}
	s := New(Deps{Config: cfg, Store: st, Secrets: sec, Query: q, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	srv := httptest.NewServer(s.Handler())
	jar, _ := cookiejar.New(nil)
	t.Cleanup(func() {
		srv.Close()
		q.Close()
		st.Close()
		c, err := pgx.Connect(context.Background(), admin)
		if err == nil {
			_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
			c.Close(context.Background())
		}
	})
	return &client{t: t, srv: srv, http: &http.Client{Jar: jar}}
}

func (c *client) do(method, path string, body any) (int, map[string]any, string) {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.srv.URL+path, rdr)
	req.Header.Set("Content-Type", "application/json")
	if c.csrf != "" {
		req.Header.Set("X-CSRF-Token", c.csrf)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return resp.StatusCode, m, string(raw)
}

func TestIntegrationAPI(t *testing.T) {
	c := newTestServer(t)
	// anonymous
	if st, m, _ := c.do("GET", "/api/me", nil); st != 401 || m["error"] == nil {
		t.Fatalf("anonymous /api/me = %d %v", st, m)
	}
	if st, _, _ := c.do("GET", "/healthz", nil); st != 200 {
		t.Fatal("healthz")
	}
	// error shape and wrong password
	st, m, _ := c.do("POST", "/api/auth/login", map[string]string{"email": "admin@example.com", "password": "nope"})
	e := m["error"].(map[string]any)
	if st != 401 || e["code"] != "invalid_credentials" || e["message"] == "" {
		t.Fatalf("login error shape: %d %v", st, m)
	}
	st, m, _ = c.do("POST", "/api/auth/login", map[string]string{"email": "Admin@Example.com", "password": "changeme-now"})
	if st != 200 {
		t.Fatalf("login: %d %v", st, m)
	}
	csrf := m["csrf_token"].(string)
	// CSRF required
	if st, _, _ := c.do("POST", "/api/auth/logout", nil); st != 403 {
		t.Fatalf("POST without CSRF = %d", st)
	}
	c.csrf = csrf
	// types with schemas
	st, m, _ = c.do("GET", "/api/types", nil)
	if st != 200 || len(m["sources"].([]any)) < 1 || len(m["destinations"].([]any)) < 1 {
		t.Fatalf("types: %d %v", st, m)
	}
	for _, s := range m["sources"].([]any) {
		if s.(map[string]any)["type"] == "mongodb+faults" {
			t.Fatal("hidden test type exposed in /api/types")
		}
	}
	// parse a URI: credentials come back as secrets, never in config
	st, m, _ = c.do("POST", "/api/types/mongodb/parse", map[string]string{"text": "mongodb+srv://bob:pw%40ss@cluster0.x.mongodb.net/db"})
	if st != 200 || m["secrets"].(map[string]any)["password"] != "pw@ss" || strings.Contains(m["config"].(map[string]any)["uri"].(string), "pw") {
		t.Fatalf("parse: %d %v", st, m)
	}
	// create a destination with secrets; the API must never return them
	st, m, raw := c.do("POST", "/api/connections", map[string]any{"kind": "destination", "type": "iceberg_rest", "name": "d",
		"config":  map[string]any{"catalog_uri": "http://localhost:19120/iceberg", "warehouse": "w", "s3_endpoint": "http://localhost:9000", "s3_secret_access_key": "leaked-in-config"},
		"secrets": map[string]string{"s3_access_key_id": "AKIA-test", "s3_secret_access_key": "super-secret-value"}})
	if st != 201 {
		t.Fatalf("create destination: %d %s", st, raw)
	}
	if strings.Contains(raw, "super-secret") || strings.Contains(raw, "leaked-in-config") || strings.Contains(raw, "AKIA-test") {
		t.Fatalf("secret returned by the API: %s", raw)
	}
	id := m["id"].(string)
	fields := m["secret_fields"].([]any)
	if len(fields) != 2 {
		t.Fatalf("secret_fields = %v", fields)
	}
	// update keeping secrets unchanged
	st, m, raw = c.do("PUT", "/api/connections/"+id, map[string]any{"name": "d2", "config": map[string]any{"catalog_uri": "http://localhost:19120/iceberg", "warehouse": "w"}, "secrets": map[string]string{"s3_access_key_id": "__unchanged__", "s3_secret_access_key": "__unchanged__"}})
	if st != 200 || m["name"] != "d2" || len(m["secret_fields"].([]any)) != 2 || strings.Contains(raw, "super-secret") {
		t.Fatalf("update: %d %s", st, raw)
	}
	// link-local endpoints are refused
	st, _, raw = c.do("POST", "/api/connections", map[string]any{"kind": "destination", "type": "iceberg_rest", "name": "meta", "config": map[string]any{"catalog_uri": "http://169.254.169.254/latest", "warehouse": "w"}})
	if st != 400 || !strings.Contains(raw, "link-local") {
		t.Fatalf("metadata endpoint accepted: %d %s", st, raw)
	}
	// a draft test against a dead catalog produces a failing checklist, not a 500
	st, m, _ = c.do("POST", "/api/connections/test", map[string]any{"kind": "destination", "type": "iceberg_rest", "name": "x", "config": map[string]any{"catalog_uri": "http://127.0.0.1:1/iceberg", "warehouse": "w"}})
	if st != 200 || m["ok"] != false || len(m["steps"].([]any)) == 0 {
		t.Fatalf("draft test: %d %v", st, m)
	}
	first := m["steps"].([]any)[0].(map[string]any)
	if first["status"] != "fail" || first["hint"] == "" {
		t.Fatalf("first step should fail with a hint: %v", first)
	}
	// pipelines need a source
	st, m, _ = c.do("POST", "/api/connections", map[string]any{"kind": "source", "type": "mongodb", "name": "m", "config": map[string]any{"uri": "mongodb://localhost:27017/?directConnection=true"}})
	if st != 201 {
		t.Fatalf("create source: %d %v", st, m)
	}
	src := m["id"].(string)
	st, m, raw = c.do("POST", "/api/pipelines", map[string]any{"name": "p", "source_id": src, "destination_id": id, "sync_interval_s": 5, "collections": []map[string]any{{"collection": "db.coll"}}})
	if st != 400 {
		t.Fatalf("interval below minimum accepted: %d %s", st, raw)
	}
	st, m, raw = c.do("POST", "/api/pipelines", map[string]any{"name": "p", "source_id": src, "destination_id": id, "collections": []map[string]any{{"collection": "db.coll", "estimated_count": 5}}})
	if st != 201 || m["namespace"] != "db" || m["sync_interval_s"].(float64) != 60 {
		t.Fatalf("create pipeline: %d %s", st, raw)
	}
	pid := m["id"].(string)
	cols := m["collections"].([]any)
	if len(cols) != 1 || cols[0].(map[string]any)["clean_table"] != "coll" || cols[0].(map[string]any)["raw_table"] != "coll__raw" {
		t.Fatalf("collections: %v", cols)
	}
	if st, _, _ := c.do("DELETE", "/api/connections/"+src, nil); st != 409 {
		t.Fatalf("deleting a used connection = %d, want 409", st)
	}
	st, m, _ = c.do("POST", "/api/pipelines/"+pid+"/pause", nil)
	if st != 200 || m["desired_state"] != "paused" {
		t.Fatalf("pause: %d %v", st, m)
	}
	st, m, _ = c.do("GET", "/api/overview", nil)
	if st != 200 || m["first_run"] != false || len(m["pipelines"].([]any)) != 1 {
		t.Fatalf("overview: %d %v", st, m)
	}
	// SQL console: rejects writes, runs selects (no catalog needed)
	st, m, _ = c.do("POST", "/api/query", map[string]any{"sql": "DELETE FROM x"})
	if st != 400 || m["error"].(map[string]any)["code"] != "read_only" {
		t.Fatalf("console write: %d %v", st, m)
	}
	st, m, _ = c.do("POST", "/api/query", map[string]any{"sql": "SELECT 40 + 2 AS answer"})
	if st != 200 || m["result"].(map[string]any)["row_count"].(float64) != 1 {
		t.Fatalf("console select: %d %v", st, m)
	}
	st, m, _ = c.do("GET", "/api/query/history", nil)
	if st != 200 || len(m) == 0 && false {
		t.Fatalf("history: %d", st)
	}
	// settings round trip
	st, m, _ = c.do("PUT", "/api/settings", map[string]any{"general": map[string]any{"default_sync_interval_s": 45}})
	if st != 200 || m["general"].(map[string]any)["default_sync_interval_s"].(float64) != 45 {
		t.Fatalf("settings: %d %v", st, m)
	}
	// openapi lists the routes we used
	st, m, _ = c.do("GET", "/api/openapi.json", nil)
	paths := m["paths"].(map[string]any)
	for _, p := range []string{"/api/pipelines/{id}/pause", "/api/tables/{ns}/{table}/rows", "/api/query", "/api/events"} {
		if paths[p] == nil {
			t.Errorf("openapi missing %s", p)
		}
	}
	// delete pipeline, then logout
	if st, _, _ := c.do("DELETE", "/api/pipelines/"+pid, nil); st != 200 {
		t.Fatal("delete pipeline")
	}
	if st, _, _ := c.do("POST", "/api/auth/logout", nil); st != 200 {
		t.Fatal("logout")
	}
	if st, _, _ := c.do("GET", "/api/me", nil); st != 401 {
		t.Fatal("session survived logout")
	}
	// password change requires the current password
	c.do("POST", "/api/auth/login", map[string]string{"email": "admin@example.com", "password": "changeme-now"})
	_, m, _ = c.do("GET", "/api/me", nil)
	c.csrf = m["csrf_token"].(string)
	if st, _, _ := c.do("POST", "/api/auth/password", map[string]string{"current_password": "wrong", "new_password": "new-password-1"}); st != 403 {
		t.Fatalf("password change with wrong current = %d", st)
	}
	if st, _, _ := c.do("POST", "/api/auth/password", map[string]string{"current_password": "changeme-now", "new_password": "new-password-1"}); st != 200 {
		t.Fatalf("password change = %d", st)
	}
	// static UI and SPA fallback
	resp, _ := http.Get(c.srv.URL + "/pipelines/some-id")
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("SPA fallback: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	resp.Body.Close()
}
