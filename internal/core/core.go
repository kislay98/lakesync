// Package core holds the source- and destination-neutral data model of lakesync:
// entities, change events, documents, checkpoints and table schemas.
//
// Nothing in this package may import a cloud SDK or a concrete source/destination.
package core

import (
	"fmt"
	"time"
)

// AppName is the single place the product name lives for the Go side.
// The UI reads it from GET /api/me, the binary from cmd/lakesync.
const AppName = "lakesync"

// Op is the kind of change an event carries.
type Op string

const (
	OpSnapshot Op = "r" // full-copy read
	OpInsert   Op = "c"
	OpUpdate   Op = "u"
	OpReplace  Op = "x"
	OpDelete   Op = "d"
)

// Valid reports whether op is one of the known operations.
func (o Op) Valid() bool {
	switch o {
	case OpSnapshot, OpInsert, OpUpdate, OpReplace, OpDelete:
		return true
	}
	return false
}

// Entity is something a source can sync: for MongoDB a collection in a database.
type Entity struct {
	Namespace      string `json:"namespace"` // e.g. MongoDB database
	Name           string `json:"name"`      // e.g. collection
	EstimatedCount int64  `json:"estimated_count"`
	EstimatedBytes int64  `json:"estimated_bytes"`
}

// String returns the canonical "namespace.name" form.
func (e Entity) String() string { return e.Namespace + "." + e.Name }

// ParseEntity splits "namespace.name" at the first dot.
func ParseEntity(s string) (Entity, error) {
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			if i == 0 || i == len(s)-1 {
				break
			}
			return Entity{Namespace: s[:i], Name: s[i+1:]}, nil
		}
	}
	return Entity{}, fmt.Errorf("invalid entity %q: want namespace.name", s)
}

// Bookmark is an opaque position in a source's change feed. Only the source that
// produced it can interpret it. It is stored verbatim in the checkpoint.
type Bookmark []byte

// SnapshotCursor is the canonical key of the last document copied during a full copy.
// Empty means "start from the beginning".
type SnapshotCursor string

// ChangeEvent is the standard event every source produces. Nothing after the
// source sees source-specific structures.
type ChangeEvent struct {
	Entity    string    // "namespace.name"
	Key       string    // canonical, unique key of the document (MongoDB: canonical EJSON of _id)
	DisplayID string    // human form of the key
	Op        Op        //
	SourceTS  time.Time // when the change happened in the source
	Bookmark  Bookmark  // position after this event (may be nil during a full copy)
	Doc       *Document // full document after the change; nil on delete
}

// Phase of a collection's lifecycle as stored in the checkpoint.
type Phase string

const (
	PhaseSnapshot Phase = "snapshot"
	PhaseStream   Phase = "stream"
)

// Checkpoint is committed atomically with the data it describes.
type Checkpoint struct {
	Bookmark       Bookmark       `json:"bookmark"`
	Seq            int64          `json:"seq"`        // last _cdc_seq written
	Generation     int64          `json:"generation"` // increments on every resync
	Phase          Phase          `json:"phase"`
	SnapshotCursor SnapshotCursor `json:"snapshot_cursor"`
	RunID          string         `json:"run_id"`
}

// TestStatus of one connection-test step.
type TestStatus string

const (
	TestOK   TestStatus = "ok"
	TestWarn TestStatus = "warn"
	TestFail TestStatus = "fail"
)

// TestStep is one line of a connection test checklist.
type TestStep struct {
	Name     string        `json:"name"`
	Status   TestStatus    `json:"status"`
	Duration time.Duration `json:"-"`
	Millis   int64         `json:"duration_ms"`
	Message  string        `json:"message,omitempty"`
	Hint     string        `json:"hint,omitempty"`
}

// TestReport is the ordered result of a connection test.
type TestReport struct {
	OK    bool              `json:"ok"`
	Steps []TestStep        `json:"steps"`
	Info  map[string]string `json:"info,omitempty"` // free-form facts (server version, topology, ...)
}

// Add appends a step, filling the millisecond duration, and returns the report.
func (r *TestReport) Add(step TestStep) {
	step.Millis = step.Duration.Milliseconds()
	r.Steps = append(r.Steps, step)
	r.OK = true
	for _, s := range r.Steps {
		if s.Status == TestFail {
			r.OK = false
		}
	}
}

// Failed reports whether any step failed.
func (r *TestReport) Failed() bool { return !r.OK && len(r.Steps) > 0 }

// TableSpec names the destination tables of one synced entity.
type TableSpec struct {
	Namespace string // destination namespace (default: source namespace)
	Raw       string // append-only event table
	Clean     string // latest-version table
	Entity    string // "namespace.name" of the source entity
}

// RawSuffix is appended to the clean table name to form the raw table name.
const RawSuffix = "__raw"

// NewTableSpec derives table names from an entity and a destination namespace.
func NewTableSpec(namespace string, e Entity) TableSpec {
	clean := SanitizeTableName(e.Name)
	return TableSpec{Namespace: namespace, Raw: clean + RawSuffix, Clean: clean, Entity: e.String()}
}
