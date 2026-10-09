package iceberg

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/apache/iceberg-go"
	"github.com/apache/iceberg-go/table"

	"lakesync/internal/core"
	"lakesync/internal/dest"
)

// ErrDataMismatch is returned when a rewrite changed the row count or content
// checksum. The job tries to roll the table back to the previous snapshot; the
// error says whether that worked.
var ErrDataMismatch = errors.New("maintenance verification failed: data differs after rewrite")

// Maintain runs one maintenance job on one table (dest.Maintainer).
//
// Thresholds come from cfg. A caller that wants the job to run regardless of
// thresholds (the "Run maintenance now" button) passes cfg.SmallFilesTrigger <= 1
// and cfg.DeleteFilesTrigger <= 0. Jobs that rewrite data verify the row count
// and a content checksum of (_cdc_key, _cdc_seq) before and after; on a
// mismatch the previous snapshot is restored when the catalog allows it.
func (d *Destination) Maintain(ctx context.Context, namespace, name string, kind dest.MaintenanceKind, cfg dest.MaintenanceConfig, cp *core.Checkpoint) (dest.MaintenanceResult, error) {
	def := dest.DefaultMaintenanceConfig()
	if cfg.SmallFileBytes <= 0 {
		cfg.SmallFileBytes = def.SmallFileBytes
	}
	if cfg.TargetFileBytes <= 0 {
		cfg.TargetFileBytes = def.TargetFileBytes
	}
	if cfg.SnapshotsKeepLast <= 0 {
		cfg.SnapshotsKeepLast = def.SnapshotsKeepLast
	}
	if cfg.SnapshotRetention <= 0 {
		cfg.SnapshotRetention = def.SnapshotRetention
	}
	if cfg.OrphanOlderThan < 0 {
		cfg.OrphanOlderThan = def.OrphanOlderThan
	}
	res := dest.MaintenanceResult{Kind: kind}
	switch kind {
	case dest.MaintCompactRaw, dest.MaintRewriteClean:
		return d.rewrite(ctx, namespace, name, kind, cfg, cp, res)
	case dest.MaintExpireSnapshots:
		return d.expireSnapshots(ctx, namespace, name, cfg, res)
	case dest.MaintOrphanCleanup:
		return d.orphanCleanup(ctx, namespace, name, cfg, res)
	}
	return res, fmt.Errorf("unknown maintenance job %q", kind)
}

// rewrite implements compact_raw and rewrite_clean. compact_raw targets
// partitions with too many small files; rewrite_clean targets a table with too
// many delete files (or small files) and rewrites it whole so deletes are applied.
func (d *Destination) rewrite(ctx context.Context, namespace, name string, kind dest.MaintenanceKind, cfg dest.MaintenanceConfig, cp *core.Checkpoint, res dest.MaintenanceResult) (dest.MaintenanceResult, error) {
	tbl, err := d.loadTable(ctx, namespace, name)
	if err != nil {
		return res, err
	}
	tasks, err := tbl.Scan().PlanFiles(ctx)
	if err != nil {
		return res, fmt.Errorf("plan files of %s.%s: %w", namespace, name, sanitizeErr(err, d.cfg))
	}
	st := planStats(tasks, cfg.SmallFileBytes)
	res.FilesBefore, res.DeletesBefore, res.FilesAfter, res.DeletesAfter = st.dataFiles, st.deleteFiles, st.dataFiles, st.deleteFiles
	forced := cfg.SmallFilesTrigger <= 1

	var groups []table.CompactionTaskGroup
	switch kind {
	case dest.MaintCompactRaw:
		for key, ts := range st.byPartition {
			small := 0
			for _, t := range ts {
				if t.File.FileSizeBytes() < cfg.SmallFileBytes {
					small++
				}
			}
			if small > cfg.SmallFilesTrigger || (forced && len(ts) > 1) {
				groups = append(groups, table.CompactionTaskGroup{PartitionKey: key, Tasks: normalizeTasks(ts)})
			}
		}
		if len(groups) == 0 {
			res.Skipped = true
			res.Reason = fmt.Sprintf("no partition has more than %d small files (%d data files in %d partitions)", cfg.SmallFilesTrigger, st.dataFiles, st.partitions)
			return res, nil
		}
	case dest.MaintRewriteClean:
		needs := st.deleteFiles > cfg.DeleteFilesTrigger || st.smallFiles > cfg.SmallFilesTrigger
		if !needs && (!forced || (st.deleteFiles == 0 && st.dataFiles <= 1)) {
			res.Skipped = true
			res.Reason = fmt.Sprintf("%d delete files and %d small files are within thresholds (%d / %d)", st.deleteFiles, st.smallFiles, cfg.DeleteFilesTrigger, cfg.SmallFilesTrigger)
			return res, nil
		}
		if st.dataFiles == 0 {
			res.Skipped = true
			res.Reason = "table has no data files"
			return res, nil
		}
		for key, ts := range st.byPartition {
			groups = append(groups, table.CompactionTaskGroup{PartitionKey: key, Tasks: normalizeTasks(ts)})
		}
	}

	rowsBefore, sumBefore, err := contentChecksum(ctx, tbl)
	if err != nil {
		return res, fmt.Errorf("checksum %s.%s before rewrite: %w", namespace, name, sanitizeErr(err, d.cfg))
	}
	res.RowsBefore = rowsBefore
	var prevSnapshot int64
	if snap := tbl.CurrentSnapshot(); snap != nil {
		prevSnapshot = snap.SnapshotID
	}

	// The rewrite snapshot's summary carries the latest known checkpoint so
	// summary readers stay correct. The checkpoint in the table properties is
	// never touched here: it persists across commits, and a caller-supplied
	// checkpoint may be older than the one already stored.
	snapProps := iceberg.Properties{PropMaintenance: string(kind)}
	if eff := newestCheckpoint(tbl.Properties(), cp); eff != nil {
		for k, v := range eff.Properties() {
			snapProps[k] = v
		}
	}
	tableProps := iceberg.Properties{PropLastCompaction: time.Now().UTC().Format(time.RFC3339)}
	tx := tbl.NewTransaction()
	if err := tx.SetProperties(tableProps); err != nil {
		return res, fmt.Errorf("stage properties: %w", err)
	}
	rw, err := tx.RewriteDataFiles(ctx, groups, table.RewriteDataFilesOptions{
		SnapshotProps: snapProps,
		GroupOptions:  []table.CompactionGroupOption{table.WithCompactionTargetFileSize(cfg.TargetFileBytes)},
	})
	if err != nil {
		return res, fmt.Errorf("rewrite %s.%s: %w", namespace, name, mapCommitErr(err, d.cfg))
	}
	if _, err := tx.Commit(ctx); err != nil {
		return res, fmt.Errorf("commit rewrite of %s.%s: %w", namespace, name, mapCommitErr(err, d.cfg))
	}

	after, err := d.loadTable(ctx, namespace, name)
	if err != nil {
		return res, err
	}
	afterTasks, err := after.Scan().PlanFiles(ctx)
	if err != nil {
		return res, fmt.Errorf("plan files after rewrite: %w", sanitizeErr(err, d.cfg))
	}
	ast := planStats(afterTasks, cfg.SmallFileBytes)
	rowsAfter, sumAfter, err := contentChecksum(ctx, after)
	if err != nil {
		return res, fmt.Errorf("checksum %s.%s after rewrite: %w", namespace, name, sanitizeErr(err, d.cfg))
	}
	res.FilesAfter, res.DeletesAfter, res.RowsAfter = ast.dataFiles, ast.deleteFiles, rowsAfter
	if rw != nil {
		res.Removed = rw.RemovedDataFiles + rw.RemovedPositionDeleteFiles + rw.RemovedEqualityDeleteFiles + rw.RemovedDeletionVectorFiles
	}
	if rowsAfter != rowsBefore || sumAfter != sumBefore {
		rollbackErr := d.rollback(ctx, namespace, name, prevSnapshot)
		msg := fmt.Sprintf("%v: rows %d -> %d, checksum %x -> %x", ErrDataMismatch, rowsBefore, rowsAfter, sumBefore, sumAfter)
		if rollbackErr != nil {
			return res, fmt.Errorf("%s; rollback to snapshot %d failed (%v): the table must be inspected", msg, prevSnapshot, rollbackErr)
		}
		return res, fmt.Errorf("%s; rolled back to snapshot %d", msg, prevSnapshot)
	}
	res.Reason = fmt.Sprintf("rewrote files; now %d data files, %d delete files (%d rows verified)", ast.dataFiles, ast.deleteFiles, rowsAfter)
	if rw != nil {
		res.Reason = fmt.Sprintf("rewrote %d data files and %d delete files into %d new files; now %d data files, %d delete files (%d rows verified)",
			rw.RemovedDataFiles, rw.RemovedPositionDeleteFiles+rw.RemovedEqualityDeleteFiles+rw.RemovedDeletionVectorFiles, rw.AddedDataFiles, ast.dataFiles, ast.deleteFiles, rowsAfter)
	}
	d.log.Info("maintenance rewrite", "namespace", namespace, "table", name, "kind", kind, "result", res.Reason)
	return res, nil
}

// newestCheckpoint picks the newer of the checkpoint stored in the table
// properties and the one supplied by the caller.
func newestCheckpoint(props iceberg.Properties, supplied *core.Checkpoint) *core.Checkpoint {
	stored, ok, err := core.CheckpointFromProperties(props)
	if err != nil || !ok {
		return supplied
	}
	if supplied != nil && supplied.Seq > stored.Seq {
		return supplied
	}
	return &stored
}

// rollback restores the previous snapshot after a failed verification.
func (d *Destination) rollback(ctx context.Context, namespace, name string, snapshotID int64) error {
	if snapshotID == 0 {
		return errors.New("no previous snapshot")
	}
	tbl, err := d.loadTable(ctx, namespace, name)
	if err != nil {
		return err
	}
	tx := tbl.NewTransaction()
	if err := tx.RollbackToSnapshot(snapshotID); err != nil {
		return err
	}
	_, err = tx.Commit(ctx)
	return mapCommitErr(err, d.cfg)
}

func (d *Destination) expireSnapshots(ctx context.Context, namespace, name string, cfg dest.MaintenanceConfig, res dest.MaintenanceResult) (dest.MaintenanceResult, error) {
	tbl, err := d.loadTable(ctx, namespace, name)
	if err != nil {
		return res, err
	}
	before := len(tbl.Metadata().Snapshots())
	res.FilesBefore, res.FilesAfter = before, before
	if tbl.Properties()["gc.enabled"] == "false" {
		res.Skipped = true
		res.Reason = "the catalog manages garbage collection (gc.enabled=false); snapshots are expired by the catalog's own tooling"
		return res, nil
	}
	if before <= 1 {
		res.Skipped = true
		res.Reason = "nothing to expire: the table has a single snapshot"
		return res, nil
	}
	err = withRetry(ctx, 3, func() error {
		t, err := d.loadTable(ctx, namespace, name)
		if err != nil {
			return err
		}
		tx := t.NewTransaction()
		if err := tx.ExpireSnapshots(table.WithRetainLast(cfg.SnapshotsKeepLast), table.WithOlderThan(cfg.SnapshotRetention)); err != nil {
			return fmt.Errorf("expire snapshots: %w", sanitizeErr(err, d.cfg))
		}
		_, err = tx.Commit(ctx)
		return mapCommitErr(err, d.cfg)
	})
	if err != nil {
		return res, err
	}
	after, err := d.loadTable(ctx, namespace, name)
	if err != nil {
		return res, err
	}
	res.FilesAfter = len(after.Metadata().Snapshots())
	res.Removed = before - res.FilesAfter
	res.Reason = fmt.Sprintf("expired %d snapshots older than %s (kept at least %d); %d remain", res.Removed, cfg.SnapshotRetention, cfg.SnapshotsKeepLast, res.FilesAfter)
	return res, nil
}

func (d *Destination) orphanCleanup(ctx context.Context, namespace, name string, cfg dest.MaintenanceConfig, res dest.MaintenanceResult) (dest.MaintenanceResult, error) {
	tbl, err := d.loadTable(ctx, namespace, name)
	if err != nil {
		return res, err
	}
	plan, err := tbl.PlanOrphanFiles(ctx, table.WithFilesOlderThan(cfg.OrphanOlderThan))
	if err != nil {
		return res, fmt.Errorf("plan orphan files of %s.%s: %w", namespace, name, sanitizeErr(err, d.cfg))
	}
	res.FilesBefore = len(plan.Files())
	res.FilesAfter = res.FilesBefore
	if tbl.Properties()["gc.enabled"] == "false" {
		res.Skipped = true
		res.Reason = fmt.Sprintf("found %d unreferenced files (%d bytes) older than %s, but the catalog manages garbage collection (gc.enabled=false), so nothing was deleted", len(plan.Files()), plan.TotalSizeBytes(), cfg.OrphanOlderThan)
		return res, nil
	}
	if len(plan.Files()) == 0 {
		res.Skipped = true
		res.Reason = fmt.Sprintf("no unreferenced files older than %s", cfg.OrphanOlderThan)
		return res, nil
	}
	result, err := tbl.ExecuteOrphanCleanup(ctx, plan, table.WithFilesOlderThan(cfg.OrphanOlderThan))
	if err != nil {
		return res, fmt.Errorf("delete orphan files of %s.%s: %w", namespace, name, sanitizeErr(err, d.cfg))
	}
	res.Removed = len(result.DeletedFiles)
	res.FilesAfter = res.FilesBefore - res.Removed
	res.Reason = fmt.Sprintf("deleted %d unreferenced files (%d bytes) older than %s", res.Removed, result.TotalSizeBytes, cfg.OrphanOlderThan)
	return res, nil
}
