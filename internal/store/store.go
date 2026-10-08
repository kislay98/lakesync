// Package store is the Postgres persistence layer: migrations, repositories,
// advisory locks and the LISTEN/NOTIFY event bus. Synced data never lives here.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"hash/fnv"
	"io/fs"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// Store wraps the connection pool.
type Store struct {
	Pool *pgxpool.Pool
}

// Open connects to Postgres and verifies the connection.
func Open(ctx context.Context, url string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("DATABASE_URL: %w", err)
	}
	cfg.MaxConns = 16
	cfg.MaxConnLifetime = time.Hour
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: %w", err)
	}
	return &Store{Pool: pool}, nil
}

// Close releases the pool.
func (s *Store) Close() { s.Pool.Close() }

// Ping checks the database is reachable.
func (s *Store) Ping(ctx context.Context) error { return s.Pool.Ping(ctx) }

// Migrate applies pending migrations.
func Migrate(ctx context.Context, url string) error {
	sub, err := fs.Sub(migrationFS, "migrations")
	if err != nil {
		return err
	}
	goose.SetBaseFS(sub)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	db, err := sql.Open("pgx", url)
	if err != nil {
		return err
	}
	defer db.Close()
	return goose.UpContext(ctx, db, ".")
}

// MigrationStatus returns the current schema version.
func MigrationStatus(ctx context.Context, url string) (int64, error) {
	db, err := sql.Open("pgx", url)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	return goose.GetDBVersionContext(ctx, db)
}

var _ = stdlib.OpenDB // keep the pgx stdlib driver registered

// isNoRows reports whether err means "no rows".
func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// IsUniqueViolation reports whether err is a unique constraint violation.
func IsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// LockKey derives the advisory lock key for a pipeline collection.
func LockKey(pipelineID, collection string) int64 {
	h := fnv.New64a()
	h.Write([]byte(pipelineID))
	h.Write([]byte{0})
	h.Write([]byte(collection))
	return int64(h.Sum64())
}

// Lock is a held session-level advisory lock on a dedicated connection.
type Lock struct {
	conn *pgxpool.Conn
	key  int64
}

// TryLock attempts to take the advisory lock for a pipeline collection. It
// returns nil, nil when another worker holds it.
func (s *Store) TryLock(ctx context.Context, pipelineID, collection string) (*Lock, error) {
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	key := LockKey(pipelineID, collection)
	var got bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", key).Scan(&got); err != nil {
		conn.Release()
		return nil, err
	}
	if !got {
		conn.Release()
		return nil, nil
	}
	return &Lock{conn: conn, key: key}, nil
}

// Unlock releases the lock and its connection.
func (l *Lock) Unlock(ctx context.Context) {
	if l == nil || l.conn == nil {
		return
	}
	_, _ = l.conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", l.key)
	l.conn.Release()
	l.conn = nil
}

// Alive reports whether the lock's connection still works (a dropped connection
// loses the lock).
func (l *Lock) Alive(ctx context.Context) bool {
	if l == nil || l.conn == nil {
		return false
	}
	return l.conn.Ping(ctx) == nil
}
