// Package engine is the sync engine: it reconciles the desired state of every
// pipeline with running per-collection runners, each of which performs the full
// copy, streams changes, flushes batches to the destination with an atomic
// checkpoint, merges raw into clean and runs maintenance. It knows nothing about
// MongoDB or Iceberg beyond the source and dest interfaces.
package engine

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"lakesync/internal/config"
	"lakesync/internal/query"
	"lakesync/internal/secrets"
	"lakesync/internal/store"
)

// Deps are the engine's collaborators.
type Deps struct {
	Store   *store.Store
	Secrets secrets.SecretStore
	Query   *query.Engine
	Config  *config.Config
	Log     *slog.Logger
	Metrics *Metrics
}

// Worker runs runners for every collection whose pipeline should be running.
type Worker struct {
	deps    Deps
	pool    *pool
	mu      sync.Mutex
	runners map[string]*runner // key: pipelineID + "|" + collection
	wg      sync.WaitGroup
	// ReconcileInterval is how often desired vs actual state is compared.
	ReconcileInterval time.Duration
	started           time.Time
}

// New creates a worker.
func New(d Deps) *Worker {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.Metrics == nil {
		d.Metrics = NewMetrics(nil)
	}
	return &Worker{deps: d, pool: newPool(d), runners: map[string]*runner{}, ReconcileInterval: 5 * time.Second}
}

// Run reconciles until ctx ends, then waits for runners to stop gracefully.
func (w *Worker) Run(ctx context.Context) error {
	w.started = time.Now()
	w.deps.Log.Info("worker started")
	_ = w.deps.Store.AbandonRunningRuns(ctx)
	w.reconcile(ctx)
	ticker := time.NewTicker(w.ReconcileInterval)
	defer ticker.Stop()
	housekeeping := time.NewTicker(10 * time.Minute)
	defer housekeeping.Stop()
	for {
		select {
		case <-ctx.Done():
			w.deps.Log.Info("worker stopping; waiting for runners to finish in-flight commits")
			w.mu.Lock()
			for _, r := range w.runners {
				r.cancel()
			}
			w.mu.Unlock()
			w.wg.Wait()
			w.pool.closeAll()
			w.deps.Log.Info("worker stopped")
			return nil
		case <-ticker.C:
			w.reconcile(ctx)
		case <-housekeeping.C:
			hctx, cancel := context.WithTimeout(ctx, time.Minute)
			_ = w.deps.Store.TrimLogs(hctx)
			_ = w.deps.Store.TrimEventCounters(hctx)
			_ = w.deps.Store.DeleteExpiredSessions(hctx)
			cancel()
		}
	}
}

// Healthy reports whether the worker loop is alive (for /readyz).
func (w *Worker) Healthy() bool { return !w.started.IsZero() }

// RunnerCount returns the number of active runners (for /readyz and metrics).
func (w *Worker) RunnerCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.runners)
}

func key(pipelineID, collection string) string { return pipelineID + "|" + collection }

// reconcile starts and stops runners to match desired states.
func (w *Worker) reconcile(ctx context.Context) {
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	pipelines, err := w.deps.Store.ListPipelines(rctx)
	if err != nil {
		w.deps.Log.Warn("reconcile: list pipelines", "error", err)
		return
	}
	cols, err := w.deps.Store.ListAllCollections(rctx)
	if err != nil {
		w.deps.Log.Warn("reconcile: list collections", "error", err)
		return
	}
	byPipeline := map[string]*store.Pipeline{}
	for _, p := range pipelines {
		byPipeline[p.ID] = p
	}
	wanted := map[string]bool{}
	for _, c := range cols {
		p := byPipeline[c.PipelineID]
		if p == nil {
			continue
		}
		k := key(c.PipelineID, c.Collection)
		requested := ""
		if c.RequestedAction != nil {
			requested = *c.RequestedAction
		}
		shouldRun := p.DesiredState == store.DesiredRunning && c.Enabled
		// terminal states stay put until the user asks for a retry or a resync
		if c.State == store.StateSourceRemoved && requested != actionResync {
			shouldRun = false
		}
		if c.State == store.StateFailed && requested != actionRetry && requested != actionResync {
			shouldRun = false
		}
		if shouldRun {
			wanted[k] = true
		}
		w.mu.Lock()
		r, running := w.runners[k]
		w.mu.Unlock()
		switch {
		case shouldRun && !running:
			w.start(ctx, p, c)
		case shouldRun && running:
			if c.RequestedAction != nil && *c.RequestedAction != "" {
				r.request(*c.RequestedAction)
			}
			if p.SyncIntervalS != r.intervalS() {
				r.setInterval(p.SyncIntervalS)
			}
		case !shouldRun && running:
			reason := "stopped"
			if p.DesiredState == store.DesiredPaused {
				reason = "paused"
			}
			r.stop(reason)
		case !shouldRun && !running:
			// A collection that is not running but has an action pending that we can
			// serve without a runner: maintenance.
			if c.RequestedAction != nil && *c.RequestedAction == actionMaintenance && p.DesiredState != store.DesiredRunning {
				w.runDetachedMaintenance(ctx, p, c)
			}
			// Reflect pause/stop on rows left in a running-looking state by a crash.
			if p.DesiredState == store.DesiredPaused && (c.State == store.StateCopying || c.State == store.StateStreaming || c.State == store.StateResyncing) {
				st := store.StatePaused
				_ = w.deps.Store.UpdateCollection(rctx, c.PipelineID, c.Collection, store.CollectionUpdate{State: &st})
			}
			if p.DesiredState == store.DesiredStopped && (c.State == store.StateCopying || c.State == store.StateStreaming || c.State == store.StateResyncing) {
				st := store.StateStopped
				_ = w.deps.Store.UpdateCollection(rctx, c.PipelineID, c.Collection, store.CollectionUpdate{State: &st})
			}
		}
	}
	// runners whose rows vanished (pipeline deleted)
	w.mu.Lock()
	for k, r := range w.runners {
		if !wanted[k] {
			r.stop("removed")
		}
	}
	w.mu.Unlock()
	for _, p := range pipelines {
		w.updatePipelineState(rctx, p.ID)
	}
	w.deps.Metrics.runners.Set(float64(w.RunnerCount()))
}

// start launches a runner if the advisory lock is free.
func (w *Worker) start(ctx context.Context, p *store.Pipeline, c *store.PipelineCollection) {
	lock, err := w.deps.Store.TryLock(ctx, p.ID, c.Collection)
	if err != nil {
		w.deps.Log.Warn("advisory lock", "pipeline", p.ID, "collection", c.Collection, "error", err)
		return
	}
	if lock == nil {
		return // another worker owns this collection
	}
	r := newRunner(w, p, c, lock)
	k := key(p.ID, c.Collection)
	w.mu.Lock()
	w.runners[k] = r
	w.mu.Unlock()
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		r.run(ctx)
		w.mu.Lock()
		delete(w.runners, k)
		w.mu.Unlock()
		lock.Unlock(context.WithoutCancel(ctx))
	}()
}

// updatePipelineState aggregates collection states (section 7.7).
func (w *Worker) updatePipelineState(ctx context.Context, pipelineID string) {
	cols, err := w.deps.Store.ListCollections(ctx, pipelineID)
	if err != nil {
		return
	}
	p, err := w.deps.Store.GetPipeline(ctx, pipelineID)
	if err != nil {
		return
	}
	state := AggregateState(p.DesiredState, cols)
	if state != p.State {
		if err := w.deps.Store.SetPipelineState(ctx, pipelineID, state); err == nil {
			_ = w.deps.Store.Notify(ctx, store.Event{Type: "pipeline", PipelineID: pipelineID, Data: jsonRaw(map[string]any{"state": state})})
		}
	}
}

// AggregateState derives a pipeline's state from its collections.
func AggregateState(desired string, cols []*store.PipelineCollection) string {
	if len(cols) == 0 {
		return store.StateCreated
	}
	counts := map[string]int{}
	enabled := 0
	for _, c := range cols {
		if !c.Enabled {
			continue
		}
		enabled++
		counts[c.State]++
	}
	if enabled == 0 {
		return store.StateCreated
	}
	switch {
	case counts[store.StateFailed] > 0:
		return store.StateFailed
	case desired == store.DesiredPaused:
		return store.StatePaused
	case counts[store.StateResyncing] > 0:
		return store.StateResyncing
	case counts[store.StateCopying] > 0:
		return store.StateCopying
	case counts[store.StateSourceRemoved] == enabled:
		return store.StateSourceRemoved
	case counts[store.StateStreaming] > 0:
		return store.StateStreaming
	case desired == store.DesiredStopped && counts[store.StateCreated] == enabled:
		return store.StateCreated
	case desired == store.DesiredStopped:
		return store.StateStopped
	case counts[store.StateSourceRemoved] > 0:
		return store.StateSourceRemoved
	}
	return store.StateCreated
}
