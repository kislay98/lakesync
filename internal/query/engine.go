// Package query embeds DuckDB and attaches Iceberg REST catalogs to it. It serves
// the data viewer, document history, the read-only SQL console, the raw-to-clean
// merge and the verify tool. It talks only Iceberg REST and the S3 API.
package query

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	_ "github.com/duckdb/duckdb-go/v2"
)

// Options configure the embedded DuckDB.
type Options struct {
	ExtensionDir string // bundled extensions; empty = DuckDB default (may download)
	MemoryLimit  string // e.g. "4GB"; empty = DuckDB default
	Threads      int    // 0 = DuckDB default
	// LockdownFilesystems disables DuckDB's local filesystem after setup so SQL
	// console users cannot read files from the server. It is on by default; the
	// `lakesync duckdb-install` command turns it off to install extensions.
	NoLockdown bool
}

// CatalogConfig is what the engine needs to attach one Iceberg REST catalog and
// its storage. It mirrors the destination config without importing it.
type CatalogConfig struct {
	URI             string
	Warehouse       string
	Prefix          string
	AuthType        string // none | bearer | oauth2
	Token           string
	ClientID        string
	ClientSecret    string
	OAuth2ServerURI string
	Scope           string

	S3Endpoint        string // "" = use catalog-vended storage settings
	S3Region          string
	S3AccessKey       string
	S3SecretKey       string
	S3PathStyle       bool
	S3Scope           string // optional secret scope, e.g. s3://warehouse
	VendedCredentials bool

	// Staleness lets DuckDB cache table metadata for this long (viewer only).
	Staleness time.Duration
}

func (c CatalogConfig) signature() string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%s|%s|%s|%s|%s|%s|%s|%s|%s|%s|%s|%v|%s|%v|%s", c.URI, c.Warehouse, c.Prefix, c.AuthType, c.Token, c.ClientID, c.ClientSecret, c.OAuth2ServerURI, c.Scope, c.S3Endpoint, c.S3Region, c.S3AccessKey, c.S3SecretKey, c.S3PathStyle, c.S3Scope, c.VendedCredentials, c.Staleness)
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// Engine is one embedded DuckDB instance shared by the process.
type Engine struct {
	db       *sql.DB
	mu       sync.Mutex
	attached map[string]string // alias -> config signature
	opts     Options
	log      *slog.Logger
}

// Open starts DuckDB, loads the httpfs and iceberg extensions and applies the
// security settings.
func Open(ctx context.Context, opts Options, log *slog.Logger) (*Engine, error) {
	if log == nil {
		log = slog.Default()
	}
	db, err := sql.Open("duckdb", "")
	if err != nil {
		return nil, fmt.Errorf("duckdb: %w", err)
	}
	db.SetMaxOpenConns(8)
	db.SetConnMaxIdleTime(10 * time.Minute)
	e := &Engine{db: db, attached: map[string]string{}, opts: opts, log: log}
	if err := e.setup(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return e, nil
}

func (e *Engine) setup(ctx context.Context) error {
	stmts := []string{}
	if e.opts.ExtensionDir != "" {
		stmts = append(stmts, fmt.Sprintf("SET extension_directory = %s", quoteString(e.opts.ExtensionDir)))
	}
	if e.opts.MemoryLimit != "" {
		stmts = append(stmts, fmt.Sprintf("SET memory_limit = %s", quoteString(e.opts.MemoryLimit)))
	} else {
		// DuckDB's default is 80% of physical RAM, too much for a process that shares
		// the machine with the catalog and storage; cap it unless configured.
		stmts = append(stmts, "SET memory_limit = '4GB'")
	}
	if e.opts.Threads > 0 {
		stmts = append(stmts, fmt.Sprintf("SET threads = %d", e.opts.Threads))
	} else if runtime.NumCPU() > 8 {
		stmts = append(stmts, "SET threads = 8")
	}
	for _, s := range stmts {
		if _, err := e.db.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("duckdb setup %q: %w", s, err)
		}
	}
	for _, ext := range []string{"httpfs", "iceberg"} {
		if _, err := e.db.ExecContext(ctx, "LOAD "+ext); err != nil {
			// Not bundled: install from the official repository (dev machines).
			if _, err2 := e.db.ExecContext(ctx, "INSTALL "+ext+"; LOAD "+ext); err2 != nil {
				return fmt.Errorf("duckdb: cannot load extension %s: %v (install failed: %v)", ext, err, err2)
			}
		}
	}
	// Secrets stay in memory only; initialise the secret manager now, because it
	// cannot initialise once the configuration is locked.
	if _, err := e.db.ExecContext(ctx, "SET allow_persistent_secrets = false"); err != nil {
		return fmt.Errorf("duckdb setup: %w", err)
	}
	if _, err := e.db.ExecContext(ctx, "CREATE OR REPLACE SECRET __lakesync_init (TYPE s3, KEY_ID 'init', SECRET 'init'); DROP SECRET __lakesync_init"); err != nil {
		return fmt.Errorf("duckdb secret manager: %w", err)
	}
	if !e.opts.NoLockdown {
		// Nothing should read files from the server's disk through SQL. S3 keeps working.
		if _, err := e.db.ExecContext(ctx, "SET disabled_filesystems = 'LocalFileSystem'"); err != nil {
			return fmt.Errorf("duckdb lockdown: %w", err)
		}
		if _, err := e.db.ExecContext(ctx, "SET lock_configuration = true"); err != nil {
			return fmt.Errorf("duckdb lockdown: %w", err)
		}
	}
	return nil
}

// Close shuts DuckDB down.
func (e *Engine) Close() error { return e.db.Close() }

// DB exposes the underlying handle for tests and tools.
func (e *Engine) DB() *sql.DB { return e.db }

// Version returns the DuckDB version.
func (e *Engine) Version(ctx context.Context) string {
	var v string
	_ = e.db.QueryRowContext(ctx, "SELECT version()").Scan(&v)
	return v
}

// Extensions reports loaded extension versions.
func (e *Engine) Extensions(ctx context.Context) (map[string]string, error) {
	rows, err := e.db.QueryContext(ctx, "SELECT extension_name, extension_version FROM duckdb_extensions() WHERE loaded")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var n, v string
		if err := rows.Scan(&n, &v); err != nil {
			return nil, err
		}
		out[n] = v
	}
	return out, rows.Err()
}

// Alias returns the DuckDB catalog alias used for a destination id.
func Alias(destinationID string) string {
	h := sha256.Sum256([]byte(destinationID))
	return "d_" + hex.EncodeToString(h[:])[:10]
}

// Attach makes a catalog available under alias, re-attaching when its config
// changed. It is safe to call often.
func (e *Engine) Attach(ctx context.Context, alias string, cfg CatalogConfig) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	sig := cfg.signature()
	if e.attached[alias] == sig {
		return nil
	}
	if _, ok := e.attached[alias]; ok {
		e.detachLocked(ctx, alias)
	}
	conn, err := e.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	var stmts []string
	catSecret := alias + "_catalog"
	s3Secret := alias + "_storage"
	stmts = append(stmts, "DROP SECRET IF EXISTS "+catSecret, "DROP SECRET IF EXISTS "+s3Secret)
	attachOpts := []string{"TYPE iceberg", "ENDPOINT " + quoteString(cfg.URI)}
	switch strings.ToLower(cfg.AuthType) {
	case "", "none":
		attachOpts = append(attachOpts, "AUTHORIZATION_TYPE 'none'")
	case "bearer":
		stmts = append(stmts, fmt.Sprintf("CREATE SECRET %s (TYPE iceberg, TOKEN %s)", catSecret, quoteString(cfg.Token)))
		attachOpts = append(attachOpts, "SECRET "+catSecret)
	case "oauth2":
		parts := []string{"TYPE iceberg", "CLIENT_ID " + quoteString(cfg.ClientID), "CLIENT_SECRET " + quoteString(cfg.ClientSecret)}
		if cfg.OAuth2ServerURI != "" {
			parts = append(parts, "OAUTH2_SERVER_URI "+quoteString(cfg.OAuth2ServerURI))
		}
		if cfg.Scope != "" {
			parts = append(parts, "OAUTH2_SCOPE "+quoteString(cfg.Scope))
		}
		stmts = append(stmts, fmt.Sprintf("CREATE SECRET %s (%s)", catSecret, strings.Join(parts, ", ")))
		attachOpts = append(attachOpts, "SECRET "+catSecret)
	default:
		return fmt.Errorf("unknown auth type %q", cfg.AuthType)
	}
	if cfg.S3Endpoint != "" && !cfg.VendedCredentials {
		attachOpts = append(attachOpts, "ACCESS_DELEGATION_MODE 'none'")
	}
	if cfg.Staleness > 0 {
		attachOpts = append(attachOpts, fmt.Sprintf("MAX_TABLE_STALENESS '%d seconds'", int(cfg.Staleness.Seconds())))
	}
	if cfg.S3Endpoint != "" {
		host, ssl := splitEndpoint(cfg.S3Endpoint)
		parts := []string{"TYPE s3", "KEY_ID " + quoteString(cfg.S3AccessKey), "SECRET " + quoteString(cfg.S3SecretKey),
			"ENDPOINT " + quoteString(host), fmt.Sprintf("USE_SSL %v", ssl), "REGION " + quoteString(orDefault(cfg.S3Region, "us-east-1"))}
		if cfg.S3PathStyle {
			parts = append(parts, "URL_STYLE 'path'")
		} else {
			parts = append(parts, "URL_STYLE 'vhost'")
		}
		if cfg.S3Scope != "" {
			parts = append(parts, "SCOPE "+quoteString(cfg.S3Scope))
		}
		stmts = append(stmts, fmt.Sprintf("CREATE SECRET %s (%s)", s3Secret, strings.Join(parts, ", ")))
	} else if cfg.S3AccessKey != "" {
		// Credentials without an endpoint: real AWS S3 (or vended endpoint).
		parts := []string{"TYPE s3", "KEY_ID " + quoteString(cfg.S3AccessKey), "SECRET " + quoteString(cfg.S3SecretKey), "REGION " + quoteString(orDefault(cfg.S3Region, "us-east-1"))}
		if cfg.S3Scope != "" {
			parts = append(parts, "SCOPE "+quoteString(cfg.S3Scope))
		}
		stmts = append(stmts, fmt.Sprintf("CREATE SECRET %s (%s)", s3Secret, strings.Join(parts, ", ")))
	}
	stmts = append(stmts, fmt.Sprintf("ATTACH IF NOT EXISTS %s AS %s (%s)", quoteString(cfg.Warehouse), quoteIdent(alias), strings.Join(attachOpts, ", ")))
	for _, s := range stmts {
		if _, err := conn.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("attach %s: %w", alias, redactErr(err, cfg))
		}
	}
	e.attached[alias] = sig
	e.log.Info("attached catalog", "alias", alias, "uri", cfg.URI)
	return nil
}

// Detach removes a catalog.
func (e *Engine) Detach(ctx context.Context, alias string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.detachLocked(ctx, alias)
}

func (e *Engine) detachLocked(ctx context.Context, alias string) {
	if _, err := e.db.ExecContext(ctx, "DETACH DATABASE IF EXISTS "+quoteIdent(alias)); err != nil {
		e.log.Warn("detach catalog", "alias", alias, "error", err)
	}
	_, _ = e.db.ExecContext(ctx, "DROP SECRET IF EXISTS "+alias+"_catalog")
	_, _ = e.db.ExecContext(ctx, "DROP SECRET IF EXISTS "+alias+"_storage")
	delete(e.attached, alias)
}

// Attached reports whether alias is attached.
func (e *Engine) Attached(alias string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, ok := e.attached[alias]
	return ok
}

// InstallExtensions downloads the extensions into dir (used at image build time).
func InstallExtensions(ctx context.Context, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	db, err := sql.Open("duckdb", "")
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, "SET extension_directory = "+quoteString(dir)); err != nil {
		return err
	}
	for _, ext := range []string{"httpfs", "iceberg", "avro"} {
		if _, err := db.ExecContext(ctx, "INSTALL "+ext+"; LOAD "+ext); err != nil {
			return fmt.Errorf("install %s: %w", ext, err)
		}
	}
	return nil
}

func splitEndpoint(endpoint string) (host string, ssl bool) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return strings.TrimSuffix(endpoint, "/"), strings.HasPrefix(endpoint, "https://")
	}
	return u.Host, u.Scheme == "https"
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func redactErr(err error, cfg CatalogConfig) error {
	msg := err.Error()
	for _, s := range []string{cfg.Token, cfg.ClientSecret, cfg.S3SecretKey, cfg.S3AccessKey} {
		if len(s) >= 4 {
			msg = strings.ReplaceAll(msg, s, "***")
		}
	}
	return fmt.Errorf("%s", msg)
}

// quoteIdent double-quotes an identifier.
func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// quoteString single-quotes a string literal.
func quoteString(s string) string { return `'` + strings.ReplaceAll(s, `'`, `''`) + `'` }

// qualified returns "alias"."ns"."table".
func qualified(alias, ns, table string) string {
	return quoteIdent(alias) + "." + quoteIdent(ns) + "." + quoteIdent(table)
}
