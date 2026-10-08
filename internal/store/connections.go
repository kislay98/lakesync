package store

import (
	"context"
	"encoding/json"
	"time"
)

const connectionCols = "id, owner_id, kind, type, name, config, secret_ciphertext, last_test_status, last_test_report, last_tested_at, created_at, updated_at"

func scanConnection(row interface{ Scan(...any) error }) (*Connection, error) {
	c := &Connection{}
	var cfg []byte
	if err := row.Scan(&c.ID, &c.OwnerID, &c.Kind, &c.Type, &c.Name, &cfg, &c.SecretCiphertext, &c.LastTestStatus, &c.LastTestReport, &c.LastTestedAt, &c.CreatedAt, &c.UpdatedAt); err != nil {
		if isNoRows(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	c.Config = map[string]any{}
	if len(cfg) > 0 {
		_ = json.Unmarshal(cfg, &c.Config)
	}
	return c, nil
}

// ListConnections returns every connection, optionally filtered by kind.
func (s *Store) ListConnections(ctx context.Context, kind string) ([]*Connection, error) {
	q := "SELECT " + connectionCols + " FROM connections"
	args := []any{}
	if kind != "" {
		q += " WHERE kind = $1"
		args = append(args, kind)
	}
	q += " ORDER BY created_at"
	rows, err := s.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Connection
	for rows.Next() {
		c, err := scanConnection(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetConnection loads one connection.
func (s *Store) GetConnection(ctx context.Context, id string) (*Connection, error) {
	return scanConnection(s.Pool.QueryRow(ctx, "SELECT "+connectionCols+" FROM connections WHERE id = $1", id))
}

// CreateConnection inserts a connection.
func (s *Store) CreateConnection(ctx context.Context, c *Connection) (*Connection, error) {
	cfg, _ := json.Marshal(c.Config)
	return scanConnection(s.Pool.QueryRow(ctx,
		"INSERT INTO connections (owner_id, kind, type, name, config, secret_ciphertext) VALUES ($1, $2, $3, $4, $5, $6) RETURNING "+connectionCols,
		c.OwnerID, c.Kind, c.Type, c.Name, cfg, c.SecretCiphertext))
}

// UpdateConnection replaces name, config and (when non-nil) the sealed secrets.
func (s *Store) UpdateConnection(ctx context.Context, id, name string, config map[string]any, secret []byte) (*Connection, error) {
	cfg, _ := json.Marshal(config)
	return scanConnection(s.Pool.QueryRow(ctx,
		"UPDATE connections SET name = $2, config = $3, secret_ciphertext = COALESCE($4, secret_ciphertext), updated_at = now() WHERE id = $1 RETURNING "+connectionCols,
		id, name, cfg, secret))
}

// SetConnectionTest stores the latest test result.
func (s *Store) SetConnectionTest(ctx context.Context, id, status string, report any) error {
	rep, _ := json.Marshal(report)
	_, err := s.Pool.Exec(ctx, "UPDATE connections SET last_test_status = $2, last_test_report = $3, last_tested_at = $4 WHERE id = $1", id, status, rep, time.Now())
	return err
}

// DeleteConnection removes a connection. It fails when a pipeline uses it.
func (s *Store) DeleteConnection(ctx context.Context, id string) error {
	tag, err := s.Pool.Exec(ctx, "DELETE FROM connections WHERE id = $1", id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ConnectionInUse reports the number of pipelines referencing a connection.
func (s *Store) ConnectionInUse(ctx context.Context, id string) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx, "SELECT count(*) FROM pipelines WHERE source_id = $1 OR destination_id = $1", id).Scan(&n)
	return n, err
}
