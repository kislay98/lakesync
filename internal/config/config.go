// Package config loads the application configuration from environment variables.
// Every setting is documented in .env.example.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the process configuration.
type Config struct {
	DatabaseURL   string
	ListenAddr    string
	BaseURL       string // public URL, used for cookies' Secure flag hints and docs
	DevMode       bool   // relaxes cookie Secure flag, enables Vite proxy hints
	SecretKey     []byte // parsed APP_SECRET_KEY
	SecretKeyRaw  string
	AdminEmail    string
	AdminPassword string

	PublicIP        string
	DisableIPLookup bool

	AllowPrivateEndpoints bool
	LocalStackHints       bool // offer the local compose stack's values in connection forms

	DefaultSyncIntervalS  int
	MinSyncIntervalS      int
	FlushMaxEvents        int
	FlushMaxBytes         int64
	SnapshotChunkSize     int
	SnapshotSampleSize    int
	SnapshotMaxDocsPerSec float64
	FailureThreshold      int
	FailureWindow         time.Duration

	QueryRowLimit int
	QueryTimeout  time.Duration

	DuckDBExtensionDir string
	DuckDBMemoryLimit  string
	DuckDBThreads      int

	LogLevel  string
	LogFormat string // json | text

	Version string
	Commit  string
}

// Build-time information, set with -ldflags "-X lakesync/internal/config.Version=...".
var (
	Version = "dev"
	Commit  = "unknown"
)

// Load reads the environment. It returns an error listing every missing or
// malformed required setting.
func Load() (*Config, error) {
	c := &Config{
		DatabaseURL:           env("DATABASE_URL", "postgres://lakesync:lakesync@localhost:5432/lakesync?sslmode=disable"),
		ListenAddr:            env("LISTEN_ADDR", ":8080"),
		BaseURL:               env("BASE_URL", ""),
		DevMode:               envBool("DEV_MODE", false),
		SecretKeyRaw:          os.Getenv("APP_SECRET_KEY"),
		AdminEmail:            os.Getenv("ADMIN_EMAIL"),
		AdminPassword:         os.Getenv("ADMIN_PASSWORD"),
		PublicIP:              os.Getenv("PUBLIC_IP"),
		DisableIPLookup:       envBool("DISABLE_IP_LOOKUP", false),
		AllowPrivateEndpoints: envBool("ALLOW_PRIVATE_ENDPOINTS", true),
		LocalStackHints:       envBool("LOCAL_STACK_HINTS", false),
		DefaultSyncIntervalS:  envInt("DEFAULT_SYNC_INTERVAL_S", 60),
		MinSyncIntervalS:      envInt("MIN_SYNC_INTERVAL_S", 10),
		FlushMaxEvents:        envInt("FLUSH_MAX_EVENTS", 50000),
		FlushMaxBytes:         int64(envInt("FLUSH_MAX_MB", 64)) << 20,
		SnapshotChunkSize:     envInt("SNAPSHOT_CHUNK_SIZE", 5000),
		SnapshotSampleSize:    envInt("SCHEMA_SAMPLE_SIZE", 1000),
		SnapshotMaxDocsPerSec: envFloat("SNAPSHOT_MAX_DOCS_PER_SEC", 0),
		FailureThreshold:      envInt("FAILURE_THRESHOLD", 5),
		FailureWindow:         envDuration("FAILURE_WINDOW", 10*time.Minute),
		QueryRowLimit:         envInt("QUERY_ROW_LIMIT", 10000),
		QueryTimeout:          envDuration("QUERY_TIMEOUT", 30*time.Second),
		DuckDBExtensionDir:    os.Getenv("DUCKDB_EXTENSION_DIR"),
		DuckDBMemoryLimit:     env("DUCKDB_MEMORY_LIMIT", ""),
		DuckDBThreads:         envInt("DUCKDB_THREADS", 0),
		LogLevel:              env("LOG_LEVEL", "info"),
		LogFormat:             env("LOG_FORMAT", "json"),
		Version:               Version,
		Commit:                Commit,
	}
	var errs []error
	if c.SecretKeyRaw == "" {
		errs = append(errs, errors.New("APP_SECRET_KEY is required (generate one with `lakesync gen-key`)"))
	}
	if c.MinSyncIntervalS < 1 {
		errs = append(errs, errors.New("MIN_SYNC_INTERVAL_S must be >= 1"))
	}
	if c.DefaultSyncIntervalS < c.MinSyncIntervalS {
		errs = append(errs, fmt.Errorf("DEFAULT_SYNC_INTERVAL_S (%d) is below MIN_SYNC_INTERVAL_S (%d)", c.DefaultSyncIntervalS, c.MinSyncIntervalS))
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return c, nil
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func envBool(key string, def bool) bool {
	v := strings.ToLower(env(key, ""))
	switch v {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}

func envInt(key string, def int) int {
	if v := env(key, ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envFloat(key string, def float64) float64 {
	if v := env(key, ""); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			return n
		}
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := env(key, ""); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		if n, err := strconv.Atoi(v); err == nil {
			return time.Duration(n) * time.Second
		}
	}
	return def
}
