// Package source defines what a data source must provide and keeps the registry
// of source types. MongoDB is the first implementation (package mongodb).
package source

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"lakesync/internal/core"
)

// EventIterator yields standard change events. Next blocks until an event is
// available, the context ends, or an error occurs. A full-copy iterator returns
// io.EOF when the copy is complete; a stream iterator never returns io.EOF.
type EventIterator interface {
	Next(ctx context.Context) (*core.ChangeEvent, error)
	Close() error
}

// Source produces standard change events for one source connection.
type Source interface {
	// Test runs the step-by-step connection checklist shown in the UI.
	Test(ctx context.Context) core.TestReport
	// ListEntities returns the syncable entities with estimated counts and sizes.
	ListEntities(ctx context.Context) ([]core.Entity, error)
	// CurrentBookmark returns the position streaming should start from. It is
	// taken BEFORE a full copy starts so that changes made during the copy are
	// replayed afterwards.
	CurrentBookmark(ctx context.Context, e core.Entity) (core.Bookmark, error)
	// Sample returns up to limit documents for schema inference.
	Sample(ctx context.Context, e core.Entity, limit int) ([]*core.Document, error)
	// Snapshot copies every document, ordered by key, starting after the cursor.
	// Events carry Op = r and a Bookmark of nil.
	Snapshot(ctx context.Context, e core.Entity, from core.SnapshotCursor, opts SnapshotOptions) (EventIterator, error)
	// Stream follows live changes from the bookmark. It returns core.ErrHistoryLost
	// when the bookmark cannot be resumed and a *core.SourceRemovedError when the
	// entity disappears. Other errors are transient when wrapped with core.Transient.
	Stream(ctx context.Context, e core.Entity, from core.Bookmark) (EventIterator, error)
	// Close releases connections. A Source is shared by every entity of one
	// source connection.
	Close() error
}

// SnapshotOptions tune the full copy.
type SnapshotOptions struct {
	ChunkSize     int     // documents per query (default 5000)
	MaxDocsPerSec float64 // 0 = unlimited; keeps Atlas Free under its limits
}

// Descriptor describes a registered source type for the API and UI.
type Descriptor struct {
	Type        string `json:"type"`
	Title       string `json:"title"`
	Description string `json:"description"`
	// Schema is the JSON schema of the connection form. Properties marked with
	// "x-secret": true are sealed at rest and never returned by the API.
	Schema json.RawMessage `json:"schema"`
	// New builds a source from the non-secret config and the opened secrets.
	New func(ctx context.Context, config map[string]any, secrets map[string]string) (Source, error) `json:"-"`
	// Redact returns a copy of config safe to log/display (secrets are never in
	// config, but connection strings may embed credentials).
	Redact func(config map[string]any) map[string]any `json:"-"`
	// Parse (optional) turns pasted text such as a connection string into form
	// values: non-secret config and secret fields, so the UI can fill its form.
	Parse func(text string) (config map[string]any, secrets map[string]string, err error) `json:"-"`
	// Hidden types are usable but not offered in the UI (test helpers).
	Hidden bool `json:"-"`
}

var (
	mu       sync.RWMutex
	registry = map[string]Descriptor{}
)

// Register adds a source type. It panics on duplicate registration.
func Register(d Descriptor) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := registry[d.Type]; dup {
		panic("source: duplicate type " + d.Type)
	}
	registry[d.Type] = d
}

// Lookup returns the descriptor of a type.
func Lookup(typ string) (Descriptor, error) {
	mu.RLock()
	defer mu.RUnlock()
	d, ok := registry[typ]
	if !ok {
		return Descriptor{}, fmt.Errorf("unknown source type %q", typ)
	}
	return d, nil
}

// Types lists registered types sorted by name.
func Types() []Descriptor {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]Descriptor, 0, len(registry))
	for _, d := range registry {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Type < out[j].Type })
	return out
}

// Open instantiates a source of the given type.
func Open(ctx context.Context, typ string, config map[string]any, secrets map[string]string) (Source, error) {
	d, err := Lookup(typ)
	if err != nil {
		return nil, err
	}
	return d.New(ctx, config, secrets)
}
