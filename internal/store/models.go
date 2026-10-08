package store

import (
	"encoding/json"
	"time"
)

// User is an account. v1 has exactly one admin; the model allows more later.
type User struct {
	ID           string    `json:"id"`
	Email        string    `json:"email"`
	PasswordHash string    `json:"-"`
	CreatedAt    time.Time `json:"created_at"`
}

// Session is a logged-in browser session.
type Session struct {
	ID        string
	UserID    string
	CSRFToken string
	ExpiresAt time.Time
	CreatedAt time.Time
}

// Connection is a saved source or destination.
type Connection struct {
	ID               string          `json:"id"`
	OwnerID          string          `json:"owner_id"`
	Kind             string          `json:"kind"` // source | destination
	Type             string          `json:"type"` // mongodb | iceberg_rest
	Name             string          `json:"name"`
	Config           map[string]any  `json:"config"` // non-secret fields
	SecretCiphertext []byte          `json:"-"`
	LastTestStatus   *string         `json:"last_test_status"`
	LastTestReport   json.RawMessage `json:"last_test_report,omitempty"`
	LastTestedAt     *time.Time      `json:"last_tested_at"`
	CreatedAt        time.Time       `json:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
}

// Pipeline states (section 7.7).
const (
	StateCreated       = "created"
	StateCopying       = "copying"
	StateStreaming     = "streaming"
	StatePaused        = "paused"
	StateResyncing     = "resyncing"
	StateFailed        = "failed"
	StateSourceRemoved = "source_removed"
	StateStopped       = "stopped"
)

// Desired states set by the user.
const (
	DesiredStopped = "stopped"
	DesiredRunning = "running"
	DesiredPaused  = "paused"
)

// Pipeline links one source to one destination for a set of collections.
type Pipeline struct {
	ID            string         `json:"id"`
	OwnerID       string         `json:"owner_id"`
	Name          string         `json:"name"`
	SourceID      string         `json:"source_id"`
	DestinationID string         `json:"destination_id"`
	Namespace     string         `json:"namespace"`
	SyncIntervalS int            `json:"sync_interval_s"`
	State         string         `json:"state"`
	DesiredState  string         `json:"desired_state"`
	Settings      map[string]any `json:"settings"`
	CreatedAt     time.Time      `json:"created_at"`
	UpdatedAt     time.Time      `json:"updated_at"`
}

// PipelineCollection is the per-collection status row (display copy; the
// Iceberg table is the source of truth for the checkpoint).
type PipelineCollection struct {
	PipelineID       string          `json:"pipeline_id"`
	Collection       string          `json:"collection"`
	Enabled          bool            `json:"enabled"`
	State            string          `json:"state"`
	RawTable         string          `json:"raw_table"`
	CleanTable       string          `json:"clean_table"`
	Generation       int64           `json:"generation"`
	LastSeq          int64           `json:"last_seq"`
	MergedSeq        int64           `json:"merged_seq"`
	SnapshotTotalEst int64           `json:"snapshot_total_est"`
	SnapshotDone     int64           `json:"snapshot_done"`
	LastEventAt      *time.Time      `json:"last_event_at"`
	LastSourceTS     *time.Time      `json:"last_source_ts"`
	LastCommitAt     *time.Time      `json:"last_commit_at"`
	LastMergeAt      *time.Time      `json:"last_merge_at"`
	LastError        *string         `json:"last_error"`
	StateMessage     *string         `json:"state_message"`
	ResyncReason     *string         `json:"resync_reason"`
	RequestedAction  *string         `json:"requested_action"`
	Failures         int             `json:"failures"`
	Schema           json.RawMessage `json:"schema,omitempty"`
	UpdatedAt        time.Time       `json:"updated_at"`
}

// CollectionUpdate carries the fields a worker changes on a collection row.
// Nil pointers leave the column untouched.
type CollectionUpdate struct {
	State            *string
	Generation       *int64
	LastSeq          *int64
	MergedSeq        *int64
	SnapshotTotalEst *int64
	SnapshotDone     *int64
	LastEventAt      *time.Time
	LastSourceTS     *time.Time
	LastCommitAt     *time.Time
	LastMergeAt      *time.Time
	LastError        *string // pointer to "" clears
	StateMessage     *string
	ResyncReason     *string
	RequestedAction  *string // pointer to "" clears
	Failures         *int
	Schema           json.RawMessage
}

// FieldMapping records a source field's column and its conflict count.
type FieldMapping struct {
	PipelineID    string    `json:"pipeline_id"`
	Collection    string    `json:"collection"`
	SourceField   string    `json:"source_field"`
	ColumnName    string    `json:"column_name"`
	Type          string    `json:"type"`
	ConflictCount int64     `json:"conflict_count"`
	Reason        *string   `json:"reason"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Run is one unit of work (copy, stream segment, merge, maintenance, resync).
type Run struct {
	ID         int64           `json:"id"`
	PipelineID string          `json:"pipeline_id"`
	Collection string          `json:"collection"`
	Kind       string          `json:"kind"`
	StartedAt  time.Time       `json:"started_at"`
	EndedAt    *time.Time      `json:"ended_at"`
	Status     string          `json:"status"`
	Rows       int64           `json:"rows"`
	Bytes      int64           `json:"bytes"`
	Error      *string         `json:"error"`
	Details    json.RawMessage `json:"details,omitempty"`
}

// LogEntry is one pipeline log line.
type LogEntry struct {
	ID         int64           `json:"id"`
	PipelineID string          `json:"pipeline_id"`
	Collection string          `json:"collection"`
	Level      string          `json:"level"`
	Message    string          `json:"message"`
	Fields     json.RawMessage `json:"fields,omitempty"`
	At         time.Time       `json:"at"`
}

// QueryHistoryEntry is one SQL console run.
type QueryHistoryEntry struct {
	ID          int64     `json:"id"`
	Destination *string   `json:"destination"`
	SQL         string    `json:"sql"`
	DurationMs  int       `json:"duration_ms"`
	Rows        int       `json:"rows"`
	Error       *string   `json:"error"`
	At          time.Time `json:"at"`
}

// MinuteCount is one point of an events-per-minute series.
type MinuteCount struct {
	Minute time.Time `json:"minute"`
	Events int64     `json:"events"`
}
