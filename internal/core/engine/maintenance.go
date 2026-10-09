package engine

import (
	"context"
	"fmt"
	"time"

	"lakesync/internal/core"
	"lakesync/internal/dest"
	"lakesync/internal/store"
)

// SettingsKeyMaintenance is the settings row holding maintenance thresholds.
const SettingsKeyMaintenance = "maintenance"

// MaintenanceSettings are the user-editable thresholds (Settings screen).
type MaintenanceSettings struct {
	SmallFileMB        int `json:"small_file_mb"`
	SmallFilesTrigger  int `json:"small_files_trigger"`
	TargetFileMB       int `json:"target_file_mb"`
	DeleteFilesTrigger int `json:"delete_files_trigger"`
	SnapshotKeepDays   int `json:"snapshot_keep_days"`
	SnapshotsKeepLast  int `json:"snapshots_keep_last"`
	OrphanOlderDays    int `json:"orphan_older_days"`
	CheckEveryMinutes  int `json:"check_every_minutes"`
}

// DefaultMaintenanceSettings are the brief's defaults.
func DefaultMaintenanceSettings() MaintenanceSettings {
	return MaintenanceSettings{SmallFileMB: 16, SmallFilesTrigger: 50, TargetFileMB: 128, DeleteFilesTrigger: 20, SnapshotKeepDays: 7, SnapshotsKeepLast: 100, OrphanOlderDays: 3, CheckEveryMinutes: 15}
}

// Config converts the settings to the destination's config.
func (m MaintenanceSettings) Config() dest.MaintenanceConfig {
	d := DefaultMaintenanceSettings()
	pick := func(v, def int) int {
		if v <= 0 {
			return def
		}
		return v
	}
	return dest.MaintenanceConfig{
		SmallFileBytes:     int64(pick(m.SmallFileMB, d.SmallFileMB)) << 20,
		SmallFilesTrigger:  pick(m.SmallFilesTrigger, d.SmallFilesTrigger),
		TargetFileBytes:    int64(pick(m.TargetFileMB, d.TargetFileMB)) << 20,
		DeleteFilesTrigger: pick(m.DeleteFilesTrigger, d.DeleteFilesTrigger),
		SnapshotRetention:  time.Duration(pick(m.SnapshotKeepDays, d.SnapshotKeepDays)) * 24 * time.Hour,
		SnapshotsKeepLast:  pick(m.SnapshotsKeepLast, d.SnapshotsKeepLast),
		OrphanOlderThan:    time.Duration(pick(m.OrphanOlderDays, d.OrphanOlderDays)) * 24 * time.Hour,
	}
}

// LoadMaintenanceSettings reads the settings row (defaults when unset).
func LoadMaintenanceSettings(ctx context.Context, st *store.Store) MaintenanceSettings {
	m := DefaultMaintenanceSettings()
	if ok, err := st.GetSetting(ctx, SettingsKeyMaintenance, &m); err != nil || !ok {
		return DefaultMaintenanceSettings()
	}
	return m
}

func (r *runner) loadMaintenanceConfig(ctx context.Context) {
	r.maintCfg = LoadMaintenanceSettings(ctx, r.w.deps.Store).Config()
}

// maybeMaintain runs the scheduled maintenance checks every N minutes.
func (r *runner) maybeMaintain(ctx context.Context) {
	every := time.Duration(LoadMaintenanceSettings(ctx, r.w.deps.Store).CheckEveryMinutes) * time.Minute
	if every <= 0 {
		every = 15 * time.Minute
	}
	if time.Since(r.lastMaint) < every {
		return
	}
	r.lastMaint = time.Now()
	r.loadMaintenanceConfig(ctx)
	r.runMaintenance(ctx, false)
}

// runMaintenance evaluates thresholds (or forces every job) on both tables. It
// runs inside the runner, so it never races with appends or merges.
func (r *runner) runMaintenance(ctx context.Context, force bool) {
	m, ok := r.dst.(dest.Maintainer)
	if !ok {
		return
	}
	cp := r.cp
	jobs := []struct {
		table string
		kind  dest.MaintenanceKind
		due   bool
	}{
		{r.spec.Raw, dest.MaintCompactRaw, true},
		{r.spec.Clean, dest.MaintRewriteClean, true},
		{r.spec.Raw, dest.MaintExpireSnapshots, force || time.Since(r.lastExpire) > 24*time.Hour},
		{r.spec.Clean, dest.MaintExpireSnapshots, force || time.Since(r.lastExpire) > 24*time.Hour},
		{r.spec.Raw, dest.MaintOrphanCleanup, force || time.Since(r.lastOrphan) > 7*24*time.Hour},
		{r.spec.Clean, dest.MaintOrphanCleanup, force || time.Since(r.lastOrphan) > 7*24*time.Hour},
	}
	for _, j := range jobs {
		if !j.due || ctx.Err() != nil {
			continue
		}
		_, _ = RunMaintenanceJob(ctx, r.w.deps, m, r.p.ID, r.c.Collection, r.spec.Namespace, j.table, j.kind, r.maintCfg, &cp, force)
		switch j.kind {
		case dest.MaintExpireSnapshots:
			r.lastExpire = time.Now()
		case dest.MaintOrphanCleanup:
			r.lastOrphan = time.Now()
		}
	}
}

// RunMaintenanceJob runs one job, records a run row and a log line, and
// publishes an event. The caller must hold the collection's advisory lock.
func RunMaintenanceJob(ctx context.Context, d Deps, m dest.Maintainer, pipelineID, collection, ns, table string, kind dest.MaintenanceKind, cfg dest.MaintenanceConfig, cp *core.Checkpoint, forced bool) (dest.MaintenanceResult, error) {
	l := &plog{st: d.Store, log: d.Log, pipelineID: pipelineID, collection: collection}
	if forced {
		// "Run maintenance now": rewrite anything with more than one file or any delete file.
		cfg.SmallFilesTrigger = 1
		cfg.DeleteFilesTrigger = 0
	}
	runID, _ := d.Store.StartRun(ctx, pipelineID, collection, "maintenance")
	jctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	res, err := m.Maintain(jctx, ns, table, kind, cfg, cp)
	details := map[string]any{"kind": kind, "table": table, "forced": forced, "result": res}
	if err != nil {
		d.Metrics.maintenance.WithLabelValues(string(kind), "error").Inc()
		_ = d.Store.FinishRun(ctx, runID, "failed", 0, 0, err.Error(), details)
		l.Error(ctx, fmt.Sprintf("maintenance %s on %s failed", kind, table), map[string]any{"error": err.Error()})
		return res, err
	}
	status := "ok"
	if res.Skipped {
		status = "skipped"
		d.Metrics.maintenance.WithLabelValues(string(kind), "skipped").Inc()
	} else {
		d.Metrics.maintenance.WithLabelValues(string(kind), "ok").Inc()
		l.Info(ctx, fmt.Sprintf("maintenance %s on %s: %s", kind, table, res.Reason), map[string]any{"files_before": res.FilesBefore, "files_after": res.FilesAfter, "rows": res.RowsAfter})
	}
	_ = d.Store.FinishRun(ctx, runID, status, res.RowsAfter, 0, "", details)
	_ = d.Store.Notify(ctx, store.Event{Type: "maintenance", PipelineID: pipelineID, Collection: collection, Data: jsonRaw(details)})
	return res, nil
}

// runDetachedMaintenance serves a "run maintenance now" request for a
// collection that has no runner (pipeline paused or stopped).
func (w *Worker) runDetachedMaintenance(ctx context.Context, p *store.Pipeline, c *store.PipelineCollection) {
	lock, err := w.deps.Store.TryLock(ctx, p.ID, c.Collection)
	if err != nil || lock == nil {
		return
	}
	defer lock.Unlock(context.WithoutCancel(ctx))
	empty := ""
	_ = w.deps.Store.UpdateCollection(ctx, p.ID, c.Collection, store.CollectionUpdate{RequestedAction: &empty})
	dst, _, err := w.pool.destination(ctx, p.DestinationID)
	if err != nil {
		w.deps.Log.Warn("maintenance: open destination", "error", err)
		return
	}
	defer w.pool.releaseDestination(p.DestinationID)
	m, ok := dst.(dest.Maintainer)
	if !ok {
		return
	}
	spec := core.TableSpec{Namespace: p.Namespace, Raw: c.RawTable, Clean: c.CleanTable, Entity: c.Collection}
	cp, _ := dst.LastCheckpoint(ctx, spec)
	cfg := LoadMaintenanceSettings(ctx, w.deps.Store).Config()
	for _, j := range []struct {
		table string
		kind  dest.MaintenanceKind
	}{{spec.Raw, dest.MaintCompactRaw}, {spec.Clean, dest.MaintRewriteClean}, {spec.Raw, dest.MaintExpireSnapshots}, {spec.Clean, dest.MaintExpireSnapshots}, {spec.Raw, dest.MaintOrphanCleanup}, {spec.Clean, dest.MaintOrphanCleanup}} {
		_, _ = RunMaintenanceJob(ctx, w.deps, m, p.ID, c.Collection, spec.Namespace, j.table, j.kind, cfg, cp, true)
	}
}
