// Package dest defines what a destination must provide and keeps the registry of
// destination types. Apache Iceberg over a REST catalog is the first
// implementation (package iceberg).
package dest

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"lakesync/internal/core"
)

// Destination stores batches of events durably, with the checkpoint committed
// atomically alongside the data.
type Destination interface {
	// Test runs the step-by-step connection checklist shown in the UI.
	Test(ctx context.Context) core.TestReport
	// EnsureTables creates the raw and clean tables (and namespace) if missing.
	EnsureTables(ctx context.Context, t core.TableSpec, schema core.Schema) error
	// CurrentSchema returns the user columns the destination tables have now
	// (the table is the source of truth after a restart).
	CurrentSchema(ctx context.Context, t core.TableSpec) (core.Schema, error)
	// EvolveSchema adds columns to both tables without rewriting data.
	EvolveSchema(ctx context.Context, t core.TableSpec, add []core.Column) error
	// AppendRaw writes one batch to the raw table in ONE atomic commit that also
	// carries the checkpoint. It returns *core.CommitConflictError when the commit
	// lost a race (safe to retry after refreshing).
	AppendRaw(ctx context.Context, t core.TableSpec, batch *core.Batch, cp core.Checkpoint) (AppendResult, error)
	// StampCheckpoint writes a checkpoint without data (used when a full copy
	// finishes with an empty batch, and after a resync's generation bump).
	StampCheckpoint(ctx context.Context, t core.TableSpec, cp core.Checkpoint) error
	// LastCheckpoint reads the checkpoint back from the raw table itself; nil when
	// the table has none yet.
	LastCheckpoint(ctx context.Context, t core.TableSpec) (*core.Checkpoint, error)
	// DropTables removes both tables (used when deleting a pipeline with purge).
	DropTables(ctx context.Context, t core.TableSpec) error
	// Close releases resources.
	Close() error
}

// AppendResult reports what one append wrote.
type AppendResult struct {
	Rows       int64
	Bytes      int64
	DataFiles  int
	SnapshotID int64
}

// TableInfo is what the catalog browser shows per table.
type TableInfo struct {
	Namespace   string    `json:"namespace"`
	Name        string    `json:"name"`
	Location    string    `json:"location,omitempty"`
	RowCount    int64     `json:"row_count"`
	SizeBytes   int64     `json:"size_bytes"`
	LastUpdated time.Time `json:"last_updated"`
	Snapshots   int       `json:"snapshots"`
}

// Health metrics of one table (section 11.2 screen 9).
type Health struct {
	DataFiles      int               `json:"data_files"`
	SmallFiles     int               `json:"small_files"`
	AvgFileBytes   int64             `json:"avg_file_bytes"`
	DeleteFiles    int               `json:"delete_files"`
	Snapshots      int               `json:"snapshots"`
	TotalBytes     int64             `json:"total_bytes"`
	RowCount       int64             `json:"row_count"`
	LastCommit     time.Time         `json:"last_commit"`
	LastCompaction time.Time         `json:"last_compaction"`
	Partitions     int               `json:"partitions"`
	OrphanFiles    int               `json:"orphan_files"`
	OrphanBytes    int64             `json:"orphan_bytes"`
	GCEnabled      bool              `json:"gc_enabled"`
	FormatVersion  int               `json:"format_version"`
	Properties     map[string]string `json:"properties,omitempty"`
}

// SnapshotInfo is one entry of a table's snapshot list.
type SnapshotInfo struct {
	ID        int64             `json:"id"`
	ParentID  *int64            `json:"parent_id,omitempty"`
	Timestamp time.Time         `json:"timestamp"`
	Operation string            `json:"operation"`
	Summary   map[string]string `json:"summary"`
	SchemaID  *int              `json:"schema_id,omitempty"`
	IsCurrent bool              `json:"is_current"`
}

// ColumnInfo describes a destination column including metadata columns.
type ColumnInfo struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Required bool   `json:"required"`
	ID       int    `json:"id"`
}

// MaintenanceKind names a maintenance job.
type MaintenanceKind string

const (
	MaintCompactRaw      MaintenanceKind = "compact_raw"
	MaintRewriteClean    MaintenanceKind = "rewrite_clean"
	MaintExpireSnapshots MaintenanceKind = "expire_snapshots"
	MaintOrphanCleanup   MaintenanceKind = "orphan_cleanup"
)

// MaintenanceResult reports what a job did.
type MaintenanceResult struct {
	Kind          MaintenanceKind `json:"kind"`
	Skipped       bool            `json:"skipped"`
	Reason        string          `json:"reason,omitempty"` // why skipped or what changed
	FilesBefore   int             `json:"files_before"`
	FilesAfter    int             `json:"files_after"`
	DeletesBefore int             `json:"deletes_before"`
	DeletesAfter  int             `json:"deletes_after"`
	RowsBefore    int64           `json:"rows_before"`
	RowsAfter     int64           `json:"rows_after"`
	Removed       int             `json:"removed"`
}

// MaintenanceConfig are the thresholds from Settings.
type MaintenanceConfig struct {
	SmallFileBytes     int64         // files under this size count as small (default 16 MB)
	SmallFilesTrigger  int           // compact when more than this many small files per partition (default 50)
	TargetFileBytes    int64         // rewrite target (default 128 MB)
	DeleteFilesTrigger int           // rewrite clean when delete files exceed this (default 20)
	SnapshotRetention  time.Duration // keep snapshots newer than this (default 7 days)
	SnapshotsKeepLast  int           // and at least this many (default 100)
	OrphanOlderThan    time.Duration // remove unreferenced files older than this (default 3 days)
}

// DefaultMaintenanceConfig returns the brief's defaults.
func DefaultMaintenanceConfig() MaintenanceConfig {
	return MaintenanceConfig{
		SmallFileBytes: 16 << 20, SmallFilesTrigger: 50, TargetFileBytes: 128 << 20,
		DeleteFilesTrigger: 20, SnapshotRetention: 7 * 24 * time.Hour, SnapshotsKeepLast: 100,
		OrphanOlderThan: 3 * 24 * time.Hour,
	}
}

// Inspector is implemented by destinations that can describe their tables.
type Inspector interface {
	ListNamespaces(ctx context.Context) ([]string, error)
	ListTables(ctx context.Context, namespace string) ([]TableInfo, error)
	TableColumns(ctx context.Context, namespace, table string) ([]ColumnInfo, error)
	TableHealth(ctx context.Context, namespace, table string, cfg MaintenanceConfig) (Health, error)
	Snapshots(ctx context.Context, namespace, table string) ([]SnapshotInfo, error)
}

// Maintainer is implemented by destinations that can run maintenance jobs.
type Maintainer interface {
	// Maintain runs one job on one table and verifies row count (and, for
	// rewrites, a content checksum) before and after. It never changes data.
	Maintain(ctx context.Context, namespace, table string, kind MaintenanceKind, cfg MaintenanceConfig, cp *core.Checkpoint) (MaintenanceResult, error)
}

// Descriptor describes a registered destination type.
type Descriptor struct {
	Type        string                                                                                           `json:"type"`
	Title       string                                                                                           `json:"title"`
	Description string                                                                                           `json:"description"`
	Schema      json.RawMessage                                                                                  `json:"schema"`
	New         func(ctx context.Context, config map[string]any, secrets map[string]string) (Destination, error) `json:"-"`
	Redact      func(config map[string]any) map[string]any                                                       `json:"-"`
	Parse       func(text string) (config map[string]any, secrets map[string]string, err error)                  `json:"-"`
}

var (
	mu       sync.RWMutex
	registry = map[string]Descriptor{}
)

// Register adds a destination type. It panics on duplicate registration.
func Register(d Descriptor) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := registry[d.Type]; dup {
		panic("dest: duplicate type " + d.Type)
	}
	registry[d.Type] = d
}

// Lookup returns the descriptor of a type.
func Lookup(typ string) (Descriptor, error) {
	mu.RLock()
	defer mu.RUnlock()
	d, ok := registry[typ]
	if !ok {
		return Descriptor{}, fmt.Errorf("unknown destination type %q", typ)
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

// Open instantiates a destination of the given type.
func Open(ctx context.Context, typ string, config map[string]any, secrets map[string]string) (Destination, error) {
	d, err := Lookup(typ)
	if err != nil {
		return nil, err
	}
	return d.New(ctx, config, secrets)
}
