// Package api serves the JSON API, the Server-Sent Events stream and the
// embedded web UI.
package api

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"path"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"lakesync/internal/config"
	"lakesync/internal/core"
	"lakesync/internal/core/engine"
	"lakesync/internal/query"
	"lakesync/internal/secrets"
	"lakesync/internal/store"
)

//go:embed static
var staticFS embed.FS

// Server is the HTTP API.
type Server struct {
	cfg     *config.Config
	st      *store.Store
	sec     secrets.SecretStore
	q       *query.Engine
	log     *slog.Logger
	worker  *engine.Worker // nil when running with --no-worker
	hub     *hub
	routes  []route
	mux     *http.ServeMux
	egress  egressIP
	started time.Time

	tableCache   map[string]cachedTables
	tableCacheMu sync.Mutex
}

// Deps are the server's collaborators.
type Deps struct {
	Config  *config.Config
	Store   *store.Store
	Secrets secrets.SecretStore
	Query   *query.Engine
	Log     *slog.Logger
	Worker  *engine.Worker
}

// New builds the server and its routes.
func New(d Deps) *Server {
	s := &Server{cfg: d.Config, st: d.Store, sec: d.Secrets, q: d.Query, log: d.Log, worker: d.Worker, started: time.Now(), tableCache: map[string]cachedTables{}}
	if s.log == nil {
		s.log = slog.Default()
	}
	s.hub = newHub(s.log)
	s.mux = http.NewServeMux()
	s.registerRoutes()
	return s
}

// Run starts the event listener and serves until ctx ends.
func (s *Server) Run(ctx context.Context) error {
	go s.st.Listen(ctx, s.hub.publish)
	go s.egress.lookup(ctx, s.cfg)
	srv := &http.Server{
		Addr:              s.cfg.ListenAddr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	errc := make(chan error, 1)
	go func() {
		s.log.Info("http server listening", "addr", s.cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		s.hub.closeAll()
		return srv.Shutdown(sctx)
	}
}

// Handler returns the root handler (for tests).
func (s *Server) Handler() http.Handler {
	return s.recover(s.logRequests(s.securityHeaders(s.mux)))
}

// route is one API endpoint; it also feeds the OpenAPI document.
type route struct {
	Method  string
	Path    string
	Summary string
	Tag     string
	Auth    bool
	Handler http.HandlerFunc
}

func (s *Server) handle(method, p, tag, summary string, h http.HandlerFunc) {
	s.routes = append(s.routes, route{Method: method, Path: p, Summary: summary, Tag: tag, Auth: true, Handler: h})
	s.mux.HandleFunc(method+" "+p, s.auth(s.csrf(h)))
}

func (s *Server) handlePublic(method, p, tag, summary string, h http.HandlerFunc) {
	s.routes = append(s.routes, route{Method: method, Path: p, Summary: summary, Tag: tag, Auth: false, Handler: h})
	s.mux.HandleFunc(method+" "+p, s.csrf(h))
}

func (s *Server) registerRoutes() {
	// auth
	s.handlePublic("POST", "/api/auth/login", "auth", "Log in with email and password", s.login)
	s.handle("POST", "/api/auth/logout", "auth", "Log out", s.logout)
	s.handle("POST", "/api/auth/password", "auth", "Change the password", s.changePassword)
	s.handle("GET", "/api/me", "auth", "Current user, app info and CSRF token", s.me)
	// types
	s.handle("GET", "/api/types", "types", "Source and destination types with their form schemas", s.types)
	s.handle("POST", "/api/types/{type}/parse", "types", "Parse pasted text (e.g. a connection string) into form values", s.parseType)
	// connections
	s.handle("GET", "/api/connections", "connections", "List connections", s.listConnections)
	s.handle("POST", "/api/connections", "connections", "Create a connection", s.createConnection)
	s.handle("POST", "/api/connections/test", "connections", "Test an unsaved connection", s.testDraftConnection)
	s.handle("GET", "/api/connections/{id}", "connections", "Get a connection", s.getConnection)
	s.handle("PUT", "/api/connections/{id}", "connections", "Update a connection", s.updateConnection)
	s.handle("DELETE", "/api/connections/{id}", "connections", "Delete a connection", s.deleteConnection)
	s.handle("POST", "/api/connections/{id}/test", "connections", "Test a saved connection", s.testConnection)
	s.handle("GET", "/api/connections/{id}/entities", "connections", "Browse databases and collections of a source", s.entities)
	// pipelines
	s.handle("GET", "/api/pipelines", "pipelines", "List pipelines", s.listPipelines)
	s.handle("POST", "/api/pipelines", "pipelines", "Create a pipeline", s.createPipeline)
	s.handle("GET", "/api/pipelines/{id}", "pipelines", "Get a pipeline with its collections", s.getPipeline)
	s.handle("PUT", "/api/pipelines/{id}", "pipelines", "Update a pipeline", s.updatePipeline)
	s.handle("DELETE", "/api/pipelines/{id}", "pipelines", "Delete a pipeline (optionally dropping its tables)", s.deletePipeline)
	s.handle("POST", "/api/pipelines/{id}/start", "pipelines", "Start or resume", s.pipelineAction("start"))
	s.handle("POST", "/api/pipelines/{id}/pause", "pipelines", "Pause (keeps the bookmark)", s.pipelineAction("pause"))
	s.handle("POST", "/api/pipelines/{id}/stop", "pipelines", "Stop", s.pipelineAction("stop"))
	s.handle("POST", "/api/pipelines/{id}/resync", "pipelines", "Resync every collection from scratch", s.pipelineAction("resync"))
	s.handle("POST", "/api/pipelines/{id}/retry", "pipelines", "Retry failed collections", s.pipelineAction("retry"))
	s.handle("POST", "/api/pipelines/{id}/collections/{name}/resync", "pipelines", "Resync one collection", s.collectionAction("resync"))
	s.handle("POST", "/api/pipelines/{id}/collections/{name}/retry", "pipelines", "Retry one failed collection", s.collectionAction("retry"))
	s.handle("POST", "/api/pipelines/{id}/collections/{name}/enable", "pipelines", "Enable a collection", s.collectionAction("enable"))
	s.handle("POST", "/api/pipelines/{id}/collections/{name}/disable", "pipelines", "Disable a collection", s.collectionAction("disable"))
	s.handle("GET", "/api/pipelines/{id}/runs", "activity", "Run history", s.runs)
	s.handle("GET", "/api/pipelines/{id}/logs", "activity", "Log lines (newest first)", s.logs)
	s.handle("GET", "/api/pipelines/{id}/series", "activity", "Events per minute", s.series)
	s.handle("GET", "/api/pipelines/{id}/mappings", "pipelines", "Field-to-column mappings and conflict counts", s.mappings)
	s.handle("GET", "/api/events", "activity", "Server-Sent Events: state changes, progress, runs, logs", s.events)
	s.handle("GET", "/api/overview", "overview", "Home screen summary", s.overview)
	// tables
	s.handle("GET", "/api/tables", "tables", "Namespaces and tables across destinations", s.listTables)
	s.handle("GET", "/api/tables/{ns}/{table}/schema", "tables", "Columns, type-conflict counts and field mappings", s.tableSchema)
	s.handle("GET", "/api/tables/{ns}/{table}/rows", "tables", "Rows with server-side paging, sorting, filtering and search", s.tableRows)
	s.handle("GET", "/api/tables/{ns}/{table}/health", "tables", "File, snapshot and size metrics with a health verdict", s.tableHealth)
	s.handle("GET", "/api/tables/{ns}/{table}/snapshots", "tables", "Snapshot list", s.tableSnapshots)
	s.handle("GET", "/api/tables/{ns}/{table}/keys/{key}/history", "tables", "Every raw event of one document, newest first, with diffs", s.keyHistory)
	s.handle("POST", "/api/tables/{ns}/{table}/maintenance", "tables", "Run maintenance now", s.runMaintenance)
	// query
	s.handle("POST", "/api/query", "query", "Run a read-only SQL statement", s.runQuery)
	s.handle("GET", "/api/query/schema", "query", "Tables and columns for autocomplete", s.querySchema)
	s.handle("GET", "/api/query/history", "query", "Recent queries", s.queryHistory)
	// settings
	s.handle("GET", "/api/settings", "settings", "Settings and about", s.getSettings)
	s.handle("PUT", "/api/settings", "settings", "Update settings", s.putSettings)
	// ops (public)
	s.handlePublic("GET", "/healthz", "ops", "Process is up", s.healthz)
	s.handlePublic("GET", "/readyz", "ops", "Postgres reachable and worker running", s.readyz)
	s.handlePublic("GET", "/api/openapi.json", "ops", "This API's OpenAPI document", s.openapi)
	s.mux.Handle("GET /metrics", promhttp.Handler())
	// UI
	s.mux.HandleFunc("/", s.static)
}

// apiError is the one error shape of the API.
type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

func (s *Server) writeError(w http.ResponseWriter, status int, code, msg, hint string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]apiError{"error": {Code: code, Message: msg, Hint: hint}})
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) badRequest(w http.ResponseWriter, msg string, hint string) {
	s.writeError(w, http.StatusBadRequest, "bad_request", msg, hint)
}

func (s *Server) notFound(w http.ResponseWriter, what string) {
	s.writeError(w, http.StatusNotFound, "not_found", what+" not found", "It may have been deleted. Refresh the page.")
}

func (s *Server) internal(w http.ResponseWriter, err error) {
	msg := core.RedactURI(err.Error())
	if i := strings.Index(msg, "\n"); i > 0 {
		msg = msg[:i]
	}
	s.log.Error("request failed", "error", msg)
	s.writeError(w, http.StatusInternalServerError, "internal", msg, "Try again; if it keeps failing, check the server logs.")
}

func (s *Server) decode(r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, 4<<20)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}

// --- middleware ---

func (s *Server) recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic", "error", fmt.Sprint(rec), "stack", string(debug.Stack()))
				s.writeError(w, http.StatusInternalServerError, "internal", "The server hit an unexpected error.", "Check the server logs.")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) { w.status = code; w.ResponseWriter.WriteHeader(code) }
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(sw, r)
		if r.URL.Path == "/api/events" {
			return
		}
		s.log.Debug("http", "method", r.Method, "path", r.URL.Path, "status", sw.status, "ms", time.Since(start).Milliseconds())
	})
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self' data:; connect-src 'self'")
		}
		next.ServeHTTP(w, r)
	})
}

// csrf requires the X-CSRF-Token header to match the session's token on
// state-changing requests (the SPA reads it from /api/me).
func (s *Server) csrf(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next(w, r)
			return
		}
		if r.URL.Path == "/api/auth/login" {
			// no session yet; same-site cookies plus the JSON content type requirement cover login
			if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				s.writeError(w, http.StatusUnsupportedMediaType, "bad_request", "Send JSON.", "")
				return
			}
			next(w, r)
			return
		}
		sess := sessionFrom(r.Context())
		if sess == nil {
			next(w, r) // auth middleware will reject
			return
		}
		if tok := r.Header.Get("X-CSRF-Token"); tok == "" || tok != sess.CSRFToken {
			s.writeError(w, http.StatusForbidden, "csrf", "Missing or invalid CSRF token.", "Reload the page and try again.")
			return
		}
		next(w, r)
	}
}

// --- static UI ---

func (s *Server) static(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		s.notFound(w, "endpoint")
		return
	}
	sub, _ := fs.Sub(staticFS, "static")
	p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if p == "" {
		p = "index.html"
	}
	if f, err := sub.Open(p); err == nil {
		st, _ := f.Stat()
		f.Close()
		if st != nil && !st.IsDir() {
			if strings.HasPrefix(p, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				w.Header().Set("Cache-Control", "no-cache")
			}
			http.ServeFileFS(w, r, sub, p)
			return
		}
	}
	// SPA fallback
	w.Header().Set("Cache-Control", "no-cache")
	r.URL.Path = "/"
	http.ServeFileFS(w, r, sub, "index.html")
}

// --- ops ---

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, 200, map[string]any{"status": "ok", "uptime_s": int(time.Since(s.started).Seconds())})
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	checks := map[string]string{}
	ok := true
	if err := s.st.Ping(ctx); err != nil {
		checks["postgres"] = "unreachable: " + err.Error()
		ok = false
	} else {
		checks["postgres"] = "ok"
	}
	if s.worker != nil {
		if s.worker.Healthy() {
			checks["worker"] = fmt.Sprintf("running (%d runners)", s.worker.RunnerCount())
		} else {
			checks["worker"] = "not running"
			ok = false
		}
	} else {
		checks["worker"] = "external (api-only process)"
	}
	status := 200
	if !ok {
		status = 503
	}
	s.writeJSON(w, status, map[string]any{"ready": ok, "checks": checks})
}

// clientIP extracts the caller's IP (first X-Forwarded-For hop when behind Caddy).
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return strings.TrimSpace(strings.Split(xff, ",")[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
