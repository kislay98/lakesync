package store

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
)

// Event is a live-update message fanned out to the UI over SSE. It travels
// through Postgres LISTEN/NOTIFY so that API and worker processes can be split.
type Event struct {
	Type       string          `json:"type"` // pipeline, collection, run, log, maintenance
	PipelineID string          `json:"pipeline_id,omitempty"`
	Collection string          `json:"collection,omitempty"`
	At         time.Time       `json:"at"`
	Data       json.RawMessage `json:"data,omitempty"`
}

const eventChannel = "lakesync_events"

// Notify publishes an event. Payloads over Postgres' 8000-byte limit are trimmed
// to the envelope only; subscribers then refetch.
func (s *Store) Notify(ctx context.Context, e Event) error {
	if e.At.IsZero() {
		e.At = time.Now()
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if len(b) > 7000 {
		e.Data = nil
		b, _ = json.Marshal(e)
	}
	_, err = s.Pool.Exec(ctx, "SELECT pg_notify($1, $2)", eventChannel, string(b))
	return err
}

// Listen delivers events to fn until ctx ends. It reconnects on failure.
func (s *Store) Listen(ctx context.Context, fn func(Event)) {
	for ctx.Err() == nil {
		if err := s.listenOnce(ctx, fn); err != nil && ctx.Err() == nil {
			slog.Warn("event listener reconnecting", "error", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
	}
}

func (s *Store) listenOnce(ctx context.Context, fn func(Event)) error {
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN "+eventChannel); err != nil {
		return err
	}
	for {
		n, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}
		var e Event
		if err := json.Unmarshal([]byte(n.Payload), &e); err == nil {
			fn(e)
		}
	}
}

var _ = pgx.ErrNoRows
