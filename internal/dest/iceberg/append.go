package iceberg

import (
	"context"
	"errors"
	"fmt"
	"github.com/apache/iceberg-go/catalog"
	"strconv"
	"strings"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/iceberg-go"

	"lakesync/internal/core"
	"lakesync/internal/dest"
)

// AppendRaw writes the batch and the checkpoint in one commit. Table metadata
// is reloaded on every call so a retry after *core.CommitConflictError sees the
// latest snapshot. A nil or empty batch only stamps the checkpoint.
func (d *Destination) AppendRaw(ctx context.Context, t core.TableSpec, batch *core.Batch, cp core.Checkpoint) (dest.AppendResult, error) {
	if batch == nil || len(batch.Rows) == 0 {
		return dest.AppendResult{}, d.StampCheckpoint(ctx, t, cp)
	}
	tbl, err := d.loadTable(ctx, t.Namespace, t.Raw)
	if err != nil {
		return dest.AppendResult{}, err
	}
	rec, err := rowsToRecord(tbl.Schema(), batch)
	if err != nil {
		return dest.AppendResult{}, fmt.Errorf("build arrow batch for %s.%s: %w", t.Namespace, t.Raw, err)
	}
	defer rec.Release()
	rdr, err := array.NewRecordReader(rec.Schema(), []arrow.RecordBatch{rec})
	if err != nil {
		return dest.AppendResult{}, err
	}
	defer rdr.Release()

	props := iceberg.Properties(cp.Properties())
	tx := tbl.NewTransaction()
	if err := tx.SetProperties(props); err != nil {
		return dest.AppendResult{}, fmt.Errorf("stage checkpoint properties: %w", err)
	}
	if err := tx.Append(ctx, rdr, props); err != nil {
		return dest.AppendResult{}, fmt.Errorf("stage append to %s.%s: %w", t.Namespace, t.Raw, mapCommitErr(err, d.cfg))
	}
	committed, err := tx.Commit(ctx)
	if err != nil {
		return dest.AppendResult{}, fmt.Errorf("commit append to %s.%s: %w", t.Namespace, t.Raw, mapCommitErr(err, d.cfg))
	}
	res := dest.AppendResult{Rows: int64(len(batch.Rows))}
	if snap := committed.CurrentSnapshot(); snap != nil {
		res.SnapshotID = snap.SnapshotID
		if snap.Summary != nil {
			res.Bytes = atoi(snap.Summary.Properties["added-files-size"])
			res.DataFiles = int(atoi(snap.Summary.Properties["added-data-files"]))
		}
	}
	return res, nil
}

// StampCheckpoint commits the checkpoint as table properties without data.
func (d *Destination) StampCheckpoint(ctx context.Context, t core.TableSpec, cp core.Checkpoint) error {
	tbl, err := d.loadTable(ctx, t.Namespace, t.Raw)
	if err != nil {
		return err
	}
	tx := tbl.NewTransaction()
	if err := tx.SetProperties(iceberg.Properties(cp.Properties())); err != nil {
		return fmt.Errorf("stage checkpoint properties: %w", err)
	}
	if _, err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit checkpoint on %s.%s: %w", t.Namespace, t.Raw, mapCommitErr(err, d.cfg))
	}
	return nil
}

// LastCheckpoint reads the checkpoint from the raw table: table properties
// first (they survive maintenance commits and catalogs that trim snapshot
// history), then the current snapshot's summary.
func (d *Destination) LastCheckpoint(ctx context.Context, t core.TableSpec) (*core.Checkpoint, error) {
	tbl, err := d.loadTable(ctx, t.Namespace, t.Raw)
	if err != nil {
		if isNoSuchTable(err) {
			return nil, nil // no table yet: nothing to resume from
		}
		return nil, err
	}
	cp, ok, err := core.CheckpointFromProperties(tbl.Properties())
	if err != nil {
		return nil, fmt.Errorf("table properties of %s.%s: %w", t.Namespace, t.Raw, err)
	}
	if ok {
		return &cp, nil
	}
	if snap := tbl.CurrentSnapshot(); snap != nil && snap.Summary != nil {
		cp, ok, err = core.CheckpointFromProperties(snap.Summary.Properties)
		if err != nil {
			return nil, fmt.Errorf("snapshot summary of %s.%s: %w", t.Namespace, t.Raw, err)
		}
		if ok {
			return &cp, nil
		}
	}
	return nil, nil
}

func atoi(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

// isNoSuchTable reports whether err means the table or namespace is missing.
func isNoSuchTable(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, catalog.ErrNoSuchTable) || errors.Is(err, catalog.ErrNoSuchNamespace) ||
		strings.Contains(err.Error(), "NoSuchTableException") || strings.Contains(err.Error(), "NoSuchNamespaceException")
}
