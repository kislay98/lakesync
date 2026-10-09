// Command lakesync is the single binary: API + embedded UI + worker, plus the
// migrate, seed, verify and helper subcommands.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"lakesync/internal/api"
	"lakesync/internal/config"
	"lakesync/internal/core"
	"lakesync/internal/core/engine"
	"lakesync/internal/query"
	"lakesync/internal/secrets"
	"lakesync/internal/seed"
	"lakesync/internal/store"
	"lakesync/internal/verify"

	_ "lakesync/internal/dest/iceberg"
	_ "lakesync/internal/source/faulty"
	_ "lakesync/internal/source/mongodb"
)

func usage() {
	fmt.Fprintf(os.Stderr, `%s %s (%s)

Usage: %s <command> [flags]

Commands:
  serve            Run the API, the embedded UI and the worker (default for a single VM)
  serve --no-worker  Run only the API and UI (pair with "worker")
  worker           Run only the sync worker
  migrate          Apply database migrations (serve does this automatically)
  seed             Generate realistic test data in MongoDB (see seed -h)
  verify           Compare MongoDB collections with their clean Iceberg tables (see verify -h)
  gen-key          Print a fresh APP_SECRET_KEY
  duckdb-install   Download the DuckDB extensions into a directory (used by the Dockerfile)
  version          Print the version
`, core.AppName, config.Version, config.Commit, core.AppName)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "serve":
		err = runServe(args, true)
	case "worker":
		err = runServe(args, false)
	case "migrate":
		err = runMigrate(args)
	case "seed":
		err = seed.Main(args)
	case "verify":
		err = runVerify(args)
	case "gen-key":
		fmt.Println(secrets.GenerateKey())
	case "duckdb-install":
		err = runDuckDBInstall(args)
	case "version":
		fmt.Printf("%s %s (%s)\n", core.AppName, config.Version, config.Commit)
	case "-h", "--help", "help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", core.RedactURI(err.Error()))
		os.Exit(1)
	}
}

func newLogger(cfg *config.Config) *slog.Logger {
	var level slog.Level
	switch strings.ToLower(cfg.LogLevel) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	var h slog.Handler
	if cfg.LogFormat == "text" {
		h = slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	} else {
		h = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	}
	l := slog.New(h)
	slog.SetDefault(l)
	return l
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
}

func runServe(args []string, withAPI bool) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	noWorker := fs.Bool("no-worker", false, "run only the API and UI")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := newLogger(cfg)
	ctx, cancel := signalContext()
	defer cancel()

	key, err := secrets.ParseKey(cfg.SecretKeyRaw)
	if err != nil {
		return err
	}
	sec, err := secrets.NewAESGCM(key)
	if err != nil {
		return err
	}
	if err := store.Migrate(ctx, cfg.DatabaseURL); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()
	if withAPI {
		if err := api.EnsureAdmin(ctx, st, cfg.AdminEmail, cfg.AdminPassword); err != nil {
			return err
		}
	}
	q, err := query.Open(ctx, query.Options{ExtensionDir: cfg.DuckDBExtensionDir, MemoryLimit: cfg.DuckDBMemoryLimit, Threads: cfg.DuckDBThreads}, log)
	if err != nil {
		return err
	}
	defer q.Close()
	log.Info("duckdb ready", "version", q.Version(ctx))

	g, gctx := errgroup.WithContext(ctx)
	var worker *engine.Worker
	runWorker := !withAPI || !*noWorker
	if runWorker {
		worker = engine.New(engine.Deps{Store: st, Secrets: sec, Query: q, Config: cfg, Log: log})
		g.Go(func() error { return worker.Run(gctx) })
	}
	if withAPI {
		srv := api.New(api.Deps{Config: cfg, Store: st, Secrets: sec, Query: q, Log: log, Worker: worker})
		g.Go(func() error { return srv.Run(gctx) })
	}
	log.Info("started", "version", cfg.Version, "commit", cfg.Commit, "api", withAPI, "worker", runWorker)
	err = g.Wait()
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

func runMigrate(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		// migrate only needs the database URL
		if !strings.Contains(err.Error(), "APP_SECRET_KEY") || strings.Count(err.Error(), "\n") > 0 {
			return err
		}
		cfg = &config.Config{DatabaseURL: os.Getenv("DATABASE_URL")}
		if cfg.DatabaseURL == "" {
			cfg.DatabaseURL = "postgres://lakesync:lakesync@localhost:5432/lakesync?sslmode=disable"
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := store.Migrate(ctx, cfg.DatabaseURL); err != nil {
		return err
	}
	v, _ := store.MigrationStatus(ctx, cfg.DatabaseURL)
	fmt.Printf("database schema at version %d\n", v)
	return nil
}

func runVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	pipeline := fs.String("pipeline", "", "pipeline name or id (default: every pipeline)")
	collection := fs.String("collection", "", "only this collection (namespace.name)")
	maxShow := fs.Int("show", 20, "number of mismatches to print")
	jsonOut := fs.Bool("json", false, "print the report as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	cfg.LogLevel = "warn"
	log := newLogger(cfg)
	ctx, cancel := signalContext()
	defer cancel()
	key, err := secrets.ParseKey(cfg.SecretKeyRaw)
	if err != nil {
		return err
	}
	sec, err := secrets.NewAESGCM(key)
	if err != nil {
		return err
	}
	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()
	q, err := query.Open(ctx, query.Options{ExtensionDir: cfg.DuckDBExtensionDir}, log)
	if err != nil {
		return err
	}
	defer q.Close()
	ok, err := verify.Run(ctx, verify.Options{Store: st, Secrets: sec, Query: q, Pipeline: *pipeline, Collection: *collection, Show: *maxShow, JSON: *jsonOut, Out: os.Stdout})
	if err != nil {
		return err
	}
	if !ok {
		os.Exit(3)
	}
	return nil
}

func runDuckDBInstall(args []string) error {
	fs := flag.NewFlagSet("duckdb-install", flag.ContinueOnError)
	dir := fs.String("dir", "/opt/duckdb/extensions", "extension directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := query.InstallExtensions(ctx, *dir); err != nil {
		return err
	}
	fmt.Println("extensions installed into", *dir)
	return nil
}
