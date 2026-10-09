package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"lakesync/internal/core"
	"lakesync/internal/dest"
	"lakesync/internal/source"
	"lakesync/internal/store"
)

// Actions a user can request on a collection.
const (
	actionResync      = "resync"
	actionRetry       = "retry"
	actionMaintenance = "maintenance"
)

// runner owns one collection of one pipeline while it holds the advisory lock.
type runner struct {
	w        *Worker
	p        *store.Pipeline
	c        *store.PipelineCollection
	entity   core.Entity
	spec     core.TableSpec
	lock     *store.Lock
	log      *plog
	ctx      context.Context
	cancel   context.CancelFunc
	stopMsg  atomic.Value // string
	interval atomic.Int64 // seconds
	requests chan string

	src    source.Source
	dst    dest.Destination
	alias  string
	schema core.Schema
	cp     core.Checkpoint
	runID  string

	mergedSeq          int64
	lastMerge          time.Time
	lastMaint          time.Time
	lastExpire         time.Time
	lastOrphan         time.Time
	failures           []time.Time
	lastCommitAt       time.Time
	lastCommitSourceTS time.Time
	resyncOnStart      bool
	maintCfg           dest.MaintenanceConfig
	mu                 sync.Mutex
	lastNotify         time.Time
}

func newRunner(w *Worker, p *store.Pipeline, c *store.PipelineCollection, lock *store.Lock) *runner {
	ent, _ := core.ParseEntity(c.Collection)
	r := &runner{
		w: w, p: p, c: c, lock: lock, entity: ent,
		spec:     core.TableSpec{Namespace: p.Namespace, Raw: c.RawTable, Clean: c.CleanTable, Entity: c.Collection},
		log:      &plog{st: w.deps.Store, log: w.deps.Log, pipelineID: p.ID, collection: c.Collection},
		requests: make(chan string, 4),
		maintCfg: dest.DefaultMaintenanceConfig(),
	}
	r.interval.Store(int64(p.SyncIntervalS))
	r.mergedSeq = c.MergedSeq
	return r
}

func (r *runner) intervalS() int { return int(r.interval.Load()) }
func (r *runner) setInterval(s int) {
	r.interval.Store(int64(s))
}
func (r *runner) syncInterval() time.Duration {
	s := r.intervalS()
	if s < r.w.deps.Config.MinSyncIntervalS {
		s = r.w.deps.Config.MinSyncIntervalS
	}
	return time.Duration(s) * time.Second
}

// request hands a user action to the runner.
func (r *runner) request(action string) {
	select {
	case r.requests <- action:
	default:
	}
}

// stop asks the runner to finish; reason becomes the collection state.
func (r *runner) stop(reason string) {
	r.stopMsg.Store(reason)
	if r.cancel != nil {
		r.cancel()
	}
}

func (r *runner) update(ctx context.Context, u store.CollectionUpdate) {
	uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := r.w.deps.Store.UpdateCollection(uctx, r.p.ID, r.c.Collection, u); err != nil {
		r.w.deps.Log.Warn("update collection row", "error", err)
		return
	}
	r.notify(uctx, false)
}

// notify publishes the collection row for live UI updates, rate limited unless forced.
func (r *runner) notify(ctx context.Context, force bool) {
	r.mu.Lock()
	if !force && time.Since(r.lastNotify) < 1500*time.Millisecond {
		r.mu.Unlock()
		return
	}
	r.lastNotify = time.Now()
	r.mu.Unlock()
	c, err := r.w.deps.Store.GetCollection(ctx, r.p.ID, r.c.Collection)
	if err != nil {
		return
	}
	_ = r.w.deps.Store.Notify(ctx, store.Event{Type: "collection", PipelineID: r.p.ID, Collection: r.c.Collection, Data: jsonRaw(c)})
}

func (r *runner) setState(ctx context.Context, state, message string) {
	r.update(ctx, store.CollectionUpdate{State: &state, StateMessage: &message})
	r.notify(ctx, true)
}

// run is the runner's main loop with retries.
func (r *runner) run(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	r.ctx, r.cancel = ctx, cancel
	defer cancel()
	// a resync requested while the runner was not running (failed, source removed,
	// paused) starts a new generation instead of resuming the old bookmark
	if r.c.RequestedAction != nil && *r.c.RequestedAction == actionResync {
		r.resyncOnStart = true
	}
	empty := ""
	r.update(ctx, store.CollectionUpdate{RequestedAction: &empty})
	if r.c.State == store.StateFailed || r.c.State == store.StatePaused || r.c.State == store.StateStopped {
		r.setState(ctx, store.StateCreated, "Starting")
	}
	backoff := time.Second
	for {
		err := r.session(ctx)
		if ctx.Err() != nil {
			// stopped by the user or shutdown
			reason, _ := r.stopMsg.Load().(string)
			switch reason {
			case "paused":
				r.setState(ctx, store.StatePaused, "Paused by the user; the bookmark is kept.")
			case "stopped":
				r.setState(ctx, store.StateStopped, "Stopped.")
			case "removed":
			default:
				// process shutdown: leave the state as is; the next worker continues
			}
			r.releaseConnections()
			return
		}
		if err == nil {
			r.releaseConnections()
			return // terminal (source removed, failed)
		}
		if r.recordFailure(ctx, err) {
			r.releaseConnections()
			return
		}
		r.releaseConnections()
		// exponential backoff with jitter, capped at 2 minutes
		sleep := backoff + time.Duration(rand.Int63n(int64(backoff/2)+1))
		r.log.Warn(ctx, "retrying after error", map[string]any{"error": err.Error(), "in": sleep.String()})
		select {
		case <-ctx.Done():
			r.releaseConnections()
			return
		case <-time.After(sleep):
		}
		if backoff < 2*time.Minute {
			backoff *= 2
		}
	}
}

func (r *runner) releaseConnections() {
	if r.src != nil {
		r.w.pool.releaseSource(r.p.SourceID)
		r.src = nil
	}
	if r.dst != nil {
		r.w.pool.releaseDestination(r.p.DestinationID)
		r.dst = nil
	}
}

// recordFailure counts consecutive failures within the window and returns true
// when the collection must enter the Failed state.
func (r *runner) recordFailure(ctx context.Context, err error) bool {
	now := time.Now()
	window := r.w.deps.Config.FailureWindow
	kept := r.failures[:0]
	for _, t := range r.failures {
		if now.Sub(t) < window {
			kept = append(kept, t)
		}
	}
	r.failures = append(kept, now)
	msg := err.Error()
	n := len(r.failures)
	r.w.deps.Metrics.errors.WithLabelValues(r.p.ID, r.c.Collection, "session").Inc()
	r.update(ctx, store.CollectionUpdate{LastError: &msg, Failures: &n})
	if n >= r.w.deps.Config.FailureThreshold {
		r.log.Error(ctx, "too many consecutive failures; marking the collection as failed", map[string]any{"error": msg, "failures": n})
		r.setState(ctx, store.StateFailed, fmt.Sprintf("Failed after %d consecutive errors. Fix the cause and press Retry.", n))
		return true
	}
	return false
}

// session connects, decides where to continue from and runs the lifecycle
// until an error, a terminal state or cancellation.
func (r *runner) session(ctx context.Context) error {
	// Earlier sessions of this runner wrote state, generation and seq to the
	// store; a retry must decide from that row, not from the one it started with.
	if c, err := r.w.deps.Store.GetCollection(ctx, r.p.ID, r.c.Collection); err == nil {
		r.c = c
	}
	var err error
	r.src, err = r.w.pool.source(ctx, r.p.SourceID)
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	r.dst, r.alias, err = r.w.pool.destination(ctx, r.p.DestinationID)
	if err != nil {
		return fmt.Errorf("open destination: %w", err)
	}
	r.loadMaintenanceConfig(ctx)

	// Where do we continue from? The raw table is the source of truth.
	cp, err := r.dst.LastCheckpoint(ctx, r.spec)
	if err != nil {
		return fmt.Errorf("read checkpoint: %w", err)
	}
	if cp == nil {
		if err := r.initialize(ctx, max(r.c.Generation, 1), "initial copy"); err != nil {
			return err
		}
	} else if r.resyncOnStart {
		r.resyncOnStart = false
		r.cp = *cp
		if err := r.initialize(ctx, cp.Generation+1, "requested by the user"); err != nil {
			return err
		}
	} else if r.c.LastSeq == 0 && r.c.State == store.StateCreated {
		// A pipeline that has never run found tables with another pipeline's
		// checkpoint (for example a pipeline deleted without dropping its tables).
		// Its bookmark belongs to a different history: keep the data, start a fresh
		// generation and rebuild the clean table from it.
		r.cp = *cp
		r.log.Warn(ctx, "existing tables carry a checkpoint from another pipeline; starting a fresh generation", map[string]any{"generation": cp.Generation + 1})
		if err := r.initialize(ctx, cp.Generation+1, "existing tables were found with an older checkpoint"); err != nil {
			return err
		}
	} else {
		r.cp = *cp
		r.schema, err = r.dst.CurrentSchema(ctx, r.spec)
		if err != nil {
			return fmt.Errorf("read table schema: %w", err)
		}
		r.log.Info(ctx, "resuming from checkpoint", map[string]any{"phase": string(r.cp.Phase), "seq": r.cp.Seq, "generation": r.cp.Generation, "cursor": string(r.cp.SnapshotCursor)})
		gen := r.cp.Generation
		r.update(ctx, store.CollectionUpdate{Generation: &gen, LastSeq: &r.cp.Seq, Schema: jsonRaw(r.schema)})
	}
	r.runID = uuid.NewString()
	r.failuresReset(ctx)

	for {
		switch r.cp.Phase {
		case core.PhaseSnapshot:
			if err := r.fullCopy(ctx); err != nil {
				return err
			}
		case core.PhaseStream:
			err := r.stream(ctx)
			switch {
			case err == nil:
				return nil
			case errors.Is(err, core.ErrHistoryLost):
				r.log.Warn(ctx, "history lost at the source; resyncing from scratch", map[string]any{"reason": err.Error()})
				if err := r.initialize(ctx, r.cp.Generation+1, "MongoDB no longer has the oplog entries for the saved bookmark"); err != nil {
					return err
				}
			case core.IsSourceRemoved(err):
				r.log.Warn(ctx, "collection removed at the source", map[string]any{"reason": err.Error()})
				r.setState(ctx, store.StateSourceRemoved, "The collection was dropped, renamed or its database was dropped at the source. The Iceberg tables are kept. "+err.Error())
				return nil
			case errors.Is(err, errResyncRequested):
				if err := r.initialize(ctx, r.cp.Generation+1, "requested by the user"); err != nil {
					return err
				}
			default:
				return err
			}
		default:
			return fmt.Errorf("unknown checkpoint phase %q", r.cp.Phase)
		}
	}
}

func (r *runner) failuresReset(ctx context.Context) {
	zero := 0
	empty := ""
	r.update(ctx, store.CollectionUpdate{Failures: &zero, LastError: &empty})
}

var errResyncRequested = errors.New("resync requested")

// initialize takes a fresh bookmark, infers the schema, ensures the tables and
// stamps a snapshot-phase checkpoint. gen > 1 means a resync.
func (r *runner) initialize(ctx context.Context, gen int64, reason string) error {
	resync := gen > 1
	if resync {
		r.setState(ctx, store.StateResyncing, "Resyncing from scratch: "+reason)
		r.update(ctx, store.CollectionUpdate{ResyncReason: &reason})
		r.log.Info(ctx, "resync started", map[string]any{"generation": gen, "reason": reason})
	} else {
		r.setState(ctx, store.StateCopying, "Preparing the full copy")
	}
	// 1. bookmark first, so changes made during the copy are replayed afterwards
	bm, err := r.src.CurrentBookmark(ctx, r.entity)
	if err != nil {
		return fmt.Errorf("take bookmark: %w", err)
	}
	// 2. schema: existing table columns (if any) plus what a sample adds
	schema, err := r.dst.CurrentSchema(ctx, r.spec)
	if err != nil {
		return fmt.Errorf("read table schema: %w", err)
	}
	docs, err := r.src.Sample(ctx, r.entity, r.w.deps.Config.SnapshotSampleSize)
	if err != nil {
		return fmt.Errorf("sample documents: %w", err)
	}
	var mappings []core.NameMapping
	if len(schema.Columns) == 0 {
		schema, mappings = core.InferSchema(docs)
		if err := r.dst.EnsureTables(ctx, r.spec, schema); err != nil {
			return fmt.Errorf("create tables: %w", err)
		}
		r.log.Info(ctx, "tables created", map[string]any{"raw": r.spec.Raw, "clean": r.spec.Clean, "columns": len(schema.Columns)})
	} else {
		var events []core.ChangeEvent
		for _, d := range docs {
			events = append(events, core.ChangeEvent{Doc: d})
		}
		if add := core.NewFields(schema, events); len(add) > 0 {
			named, m := core.AddColumnNames(schema, add)
			if err := r.dst.EvolveSchema(ctx, r.spec, named); err != nil {
				return fmt.Errorf("evolve schema: %w", err)
			}
			schema.Columns = append(schema.Columns, named...)
			mappings = m
		}
	}
	r.schema = schema
	r.saveMappings(ctx, schema, mappings)
	// 3. checkpoint: snapshot phase, empty cursor, new generation
	seq := int64(0)
	if prev, _ := r.dst.LastCheckpoint(ctx, r.spec); prev != nil {
		seq = prev.Seq
	}
	r.runID = uuid.NewString()
	r.cp = core.Checkpoint{Bookmark: bm, Seq: seq, Generation: gen, Phase: core.PhaseSnapshot, SnapshotCursor: "", RunID: r.runID}
	if err := r.dst.StampCheckpoint(ctx, r.spec, r.cp); err != nil {
		return fmt.Errorf("write checkpoint: %w", err)
	}
	zero := int64(0)
	r.c.SnapshotDone = 0
	r.update(ctx, store.CollectionUpdate{Generation: &gen, SnapshotDone: &zero, LastSeq: &seq, Schema: jsonRaw(schema)})
	return nil
}

// saveMappings stores the field-to-column mapping for the UI.
func (r *runner) saveMappings(ctx context.Context, schema core.Schema, maps []core.NameMapping) {
	reasons := map[string]string{}
	for _, m := range maps {
		reasons[m.SourceField] = m.Reason
	}
	var fm []store.FieldMapping
	for _, c := range schema.Columns {
		var reason *string
		if rs, ok := reasons[c.SourceField]; ok && rs != "" {
			reason = &rs
		}
		fm = append(fm, store.FieldMapping{PipelineID: r.p.ID, Collection: r.c.Collection, SourceField: c.SourceField, ColumnName: c.Name, Type: string(c.Type), Reason: reason})
	}
	if err := r.w.deps.Store.UpsertFieldMappings(ctx, r.p.ID, r.c.Collection, fm); err != nil {
		r.w.deps.Log.Warn("save field mappings", "error", err)
	}
}

// fullCopy copies every document from the snapshot cursor onwards.
func (r *runner) fullCopy(ctx context.Context) error {
	resync := r.cp.Generation > 1
	if resync {
		r.setState(ctx, store.StateResyncing, "Copying every document again (resync)")
	} else {
		r.setState(ctx, store.StateCopying, "Copying existing documents")
	}
	kind := "copy"
	if resync {
		kind = "resync"
	}
	runID, _ := r.w.deps.Store.StartRun(ctx, r.p.ID, r.c.Collection, kind)
	it, err := r.src.Snapshot(ctx, r.entity, r.cp.SnapshotCursor, source.SnapshotOptions{ChunkSize: r.w.deps.Config.SnapshotChunkSize, MaxDocsPerSec: r.w.deps.Config.SnapshotMaxDocsPerSec})
	if err != nil {
		_ = r.w.deps.Store.FinishRun(ctx, runID, "failed", 0, 0, err.Error(), nil)
		return fmt.Errorf("start full copy: %w", err)
	}
	defer it.Close()
	b := newBatcher(r)
	var copied, bytes int64
	if r.cp.SnapshotCursor != "" {
		copied = r.c.SnapshotDone // resuming a copy after a restart
	}
	flush := func(last bool) error {
		if b.empty() && !last {
			return nil
		}
		cursor := b.lastKey()
		if b.empty() {
			cursor = r.cp.SnapshotCursor
		}
		res, err := r.flush(ctx, b, core.Checkpoint{Bookmark: r.cp.Bookmark, Seq: 0, Generation: r.cp.Generation, Phase: core.PhaseSnapshot, SnapshotCursor: cursor, RunID: r.runID}, !resync)
		if err != nil {
			return err
		}
		bytes += res.Bytes
		r.w.deps.Metrics.snapshotDone.WithLabelValues(r.p.ID, r.c.Collection).Set(float64(copied))
		r.update(ctx, store.CollectionUpdate{SnapshotDone: &copied})
		_ = r.w.deps.Store.UpdateRunProgress(ctx, runID, copied, bytes)
		return nil
	}
	for {
		ev, err := it.Next(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			_ = r.w.deps.Store.FinishRun(ctx, runID, "failed", copied, bytes, err.Error(), nil)
			return fmt.Errorf("full copy: %w", err)
		}
		b.add(*ev)
		copied++
		if b.shouldFlush() {
			if err := flush(false); err != nil {
				_ = r.w.deps.Store.FinishRun(ctx, runID, "failed", copied, bytes, err.Error(), nil)
				return err
			}
		}
		select {
		case action := <-r.requests:
			r.handleRequest(ctx, action)
		default:
		}
	}
	if err := flush(true); err != nil {
		_ = r.w.deps.Store.FinishRun(ctx, runID, "failed", copied, bytes, err.Error(), nil)
		return err
	}
	// switch to streaming in the same checkpoint slot
	r.cp.Phase = core.PhaseStream
	r.cp.SnapshotCursor = ""
	if err := r.dst.StampCheckpoint(ctx, r.spec, r.cp); err != nil {
		_ = r.w.deps.Store.FinishRun(ctx, runID, "failed", copied, bytes, err.Error(), nil)
		return fmt.Errorf("finish full copy: %w", err)
	}
	if resync {
		if err := r.rebuildClean(ctx); err != nil {
			_ = r.w.deps.Store.FinishRun(ctx, runID, "failed", copied, bytes, err.Error(), nil)
			return err
		}
	} else if err := r.merge(ctx); err != nil {
		_ = r.w.deps.Store.FinishRun(ctx, runID, "failed", copied, bytes, err.Error(), nil)
		return err
	}
	_ = r.w.deps.Store.FinishRun(ctx, runID, "ok", copied, bytes, "", map[string]any{"generation": r.cp.Generation})
	r.log.Info(ctx, "full copy finished", map[string]any{"documents": copied, "generation": r.cp.Generation})
	done := copied
	r.update(ctx, store.CollectionUpdate{SnapshotDone: &done, SnapshotTotalEst: &done})
	return nil
}

// stream follows live changes until an error or cancellation.
func (r *runner) stream(ctx context.Context) error {
	r.setState(ctx, store.StateStreaming, "")
	it, err := r.src.Stream(ctx, r.entity, r.cp.Bookmark)
	if err != nil {
		return fmt.Errorf("open change stream: %w", err)
	}
	defer it.Close()
	runID, _ := r.w.deps.Store.StartRun(ctx, r.p.ID, r.c.Collection, "stream")
	var total, bytes int64
	finish := func(status, errMsg string) {
		_ = r.w.deps.Store.FinishRun(context.WithoutCancel(ctx), runID, status, total, bytes, errMsg, nil)
	}
	b := newBatcher(r)
	events := make(chan *core.ChangeEvent, 1024)
	errc := make(chan error, 1)
	sctx, scancel := context.WithCancel(ctx)
	defer scancel()
	go func() {
		for {
			ev, err := it.Next(sctx)
			if err != nil {
				errc <- err
				return
			}
			select {
			case events <- ev:
			case <-sctx.Done():
				return
			}
		}
	}()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	lastFlush := time.Now()
	flush := func() error {
		if b.empty() {
			lastFlush = time.Now()
			r.maybeMaintain(ctx)
			return nil
		}
		res, err := r.flush(ctx, b, core.Checkpoint{Bookmark: b.lastBookmark(), Generation: r.cp.Generation, Phase: core.PhaseStream, RunID: r.runID}, true)
		if err != nil {
			return err
		}
		total += res.Rows
		bytes += res.Bytes
		lastFlush = time.Now()
		_ = r.w.deps.Store.UpdateRunProgress(ctx, runID, total, bytes)
		r.maybeMaintain(ctx)
		return nil
	}
	for {
		select {
		case <-ctx.Done():
			// graceful shutdown: commit what we have (bounded), never half-commit
			if !b.empty() {
				fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
				_, err := r.flush(fctx, b, core.Checkpoint{Bookmark: b.lastBookmark(), Generation: r.cp.Generation, Phase: core.PhaseStream, RunID: r.runID}, false)
				cancel()
				if err != nil {
					r.log.Warn(ctx, "shutdown: the in-flight batch was not committed; it will be re-read from the bookmark", map[string]any{"error": err.Error()})
				}
			}
			finish("stopped", "")
			return ctx.Err()
		case err := <-errc:
			if ferr := flush(); ferr != nil {
				finish("failed", ferr.Error())
				return ferr
			}
			if errors.Is(err, core.ErrHistoryLost) || core.IsSourceRemoved(err) {
				finish("stopped", err.Error())
				return err
			}
			finish("failed", err.Error())
			if core.IsTransient(err) {
				return err
			}
			return core.Transient(err)
		case ev := <-events:
			b.add(*ev)
			if b.shouldFlush() {
				if err := flush(); err != nil {
					finish("failed", err.Error())
					return err
				}
			}
		case <-ticker.C:
			if time.Since(lastFlush) >= r.syncInterval() {
				if err := flush(); err != nil {
					finish("failed", err.Error())
					return err
				}
			}
			r.updateLag(ctx, b)
		case action := <-r.requests:
			if action == actionResync {
				if err := flush(); err != nil {
					finish("failed", err.Error())
					return err
				}
				finish("stopped", "resync requested")
				return errResyncRequested
			}
			r.handleRequest(ctx, action)
		}
	}
}

func (r *runner) handleRequest(ctx context.Context, action string) {
	empty := ""
	r.update(ctx, store.CollectionUpdate{RequestedAction: &empty})
	switch action {
	case actionMaintenance:
		r.runMaintenance(ctx, true)
	case actionResync:
		// handled by the caller in streaming; during a copy it is a no-op (already copying)
		r.log.Info(ctx, "resync requested during a copy; the running copy already produces a fresh generation", nil)
	}
}

// updateLag publishes the replication delay: while events are buffered it is
// now minus the oldest buffered source time; otherwise the delay of the last
// commit (commit time minus the newest source time it contained).
func (r *runner) updateLag(ctx context.Context, b *batcher) {
	_ = ctx
	switch {
	case !b.firstSourceTS.IsZero():
		r.w.deps.Metrics.lag.WithLabelValues(r.p.ID, r.c.Collection).Set(time.Since(b.firstSourceTS).Seconds())
	case !r.lastCommitAt.IsZero() && !r.lastCommitSourceTS.IsZero():
		r.w.deps.Metrics.lag.WithLabelValues(r.p.ID, r.c.Collection).Set(math.Max(0, r.lastCommitAt.Sub(r.lastCommitSourceTS).Seconds()))
	}
}
