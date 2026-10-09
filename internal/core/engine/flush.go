package engine

import (
	"context"
	"fmt"
	"time"

	"lakesync/internal/core"
	"lakesync/internal/dest"
	"lakesync/internal/store"
)

// batcher buffers events until a flush threshold is reached.
type batcher struct {
	r             *runner
	events        []core.ChangeEvent
	bytes         int64
	lastSourceTS  time.Time
	firstSourceTS time.Time
}

func newBatcher(r *runner) *batcher { return &batcher{r: r} }

func (b *batcher) add(e core.ChangeEvent) {
	b.events = append(b.events, e)
	if e.Doc != nil {
		b.bytes += int64(len(e.Doc.JSON))
	}
	b.bytes += int64(len(e.Key)) + 64
	if e.SourceTS.After(b.lastSourceTS) {
		b.lastSourceTS = e.SourceTS
	}
	if b.firstSourceTS.IsZero() || e.SourceTS.Before(b.firstSourceTS) {
		b.firstSourceTS = e.SourceTS
	}
}

func (b *batcher) empty() bool { return len(b.events) == 0 }

func (b *batcher) shouldFlush() bool {
	cfg := b.r.w.deps.Config
	return len(b.events) >= cfg.FlushMaxEvents || b.bytes >= cfg.FlushMaxBytes
}

func (b *batcher) lastKey() core.SnapshotCursor {
	if len(b.events) == 0 {
		return ""
	}
	return core.SnapshotCursor(b.events[len(b.events)-1].Key)
}

func (b *batcher) lastBookmark() core.Bookmark {
	for i := len(b.events) - 1; i >= 0; i-- {
		if len(b.events[i].Bookmark) > 0 {
			return b.events[i].Bookmark
		}
	}
	return b.r.cp.Bookmark
}

func (b *batcher) reset() {
	b.events = b.events[:0]
	b.bytes = 0
	b.firstSourceTS = time.Time{}
}

// flush evolves the schema if needed, assigns sequence numbers, writes ONE commit
// carrying the checkpoint, then merges into clean (when merge is true). On
// success the batch is cleared; on failure it is kept for the next attempt.
func (r *runner) flush(ctx context.Context, b *batcher, cp core.Checkpoint, merge bool) (dest.AppendResult, error) {
	start := time.Now()
	events := b.events
	// 1. schema evolution (never fails the pipeline over a type conflict)
	if add := core.NewFields(r.schema, events); len(add) > 0 {
		named, maps := core.AddColumnNames(r.schema, add)
		if err := r.dst.EvolveSchema(ctx, r.spec, named); err != nil {
			return dest.AppendResult{}, fmt.Errorf("evolve schema: %w", err)
		}
		r.schema.Columns = append(r.schema.Columns, named...)
		r.saveMappings(ctx, r.schema, maps)
		names := make([]string, len(named))
		for i, c := range named {
			names[i] = c.Name
		}
		r.log.Info(ctx, "new fields added as columns", map[string]any{"columns": names})
		r.update(ctx, store.CollectionUpdate{Schema: jsonRaw(r.schema)})
	}
	// 2. flatten with consecutive sequence numbers
	firstSeq := r.cp.Seq + 1
	batch, conflicts := core.FlattenBatch(r.schema, events, firstSeq, cp.Generation, time.Now().UTC())
	cp.Seq = batch.LastSeq()
	if len(events) == 0 {
		cp.Seq = r.cp.Seq
	}
	// 3. one atomic commit, retried on conflicts with fresh metadata
	var res dest.AppendResult
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		res, err = r.dst.AppendRaw(ctx, r.spec, batch, cp)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			break
		}
		if !core.IsCommitConflict(err) {
			// The catalog may have accepted a commit whose response was lost. The
			// table is the source of truth: if its checkpoint already carries our
			// sequence number, the batch is durable and must not be written twice.
			if latest, lerr := r.dst.LastCheckpoint(ctx, r.spec); lerr == nil && latest != nil && latest.Seq == cp.Seq && latest.RunID == cp.RunID {
				r.log.Warn(ctx, "commit response was lost but the table carries the checkpoint; treating the batch as committed", nil)
				err = nil
				res = dest.AppendResult{Rows: int64(len(events))}
			}
			break
		}
		r.log.Warn(ctx, "commit conflict; refreshing table metadata and retrying", map[string]any{"attempt": attempt + 1})
		time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
	}
	if err != nil {
		r.w.deps.Metrics.commits.WithLabelValues(r.p.ID, r.c.Collection, "error").Inc()
		return res, fmt.Errorf("commit batch: %w", err)
	}
	r.cp = cp
	r.w.deps.Metrics.commits.WithLabelValues(r.p.ID, r.c.Collection, "ok").Inc()
	r.w.deps.Metrics.commitSeconds.WithLabelValues(r.p.ID, r.c.Collection).Observe(time.Since(start).Seconds())
	r.w.deps.Metrics.batchRows.WithLabelValues(r.p.ID, r.c.Collection).Observe(float64(len(events)))
	ops := map[core.Op]int{}
	for _, e := range events {
		ops[e.Op]++
	}
	for op, n := range ops {
		r.w.deps.Metrics.events.WithLabelValues(r.p.ID, r.c.Collection, string(op)).Add(float64(n))
	}
	now := time.Now()
	u := store.CollectionUpdate{LastSeq: &cp.Seq, LastCommitAt: &now}
	r.lastCommitAt = now
	if len(events) > 0 {
		u.LastEventAt = &now
		if !b.lastSourceTS.IsZero() {
			ts := b.lastSourceTS
			u.LastSourceTS = &ts
			r.lastCommitSourceTS = ts
		}
		_ = r.w.deps.Store.AddEvents(ctx, r.p.ID, r.c.Collection, now, int64(len(events)))
	}
	r.update(ctx, u)
	if len(conflicts) > 0 {
		_ = r.w.deps.Store.AddConflicts(ctx, r.p.ID, r.c.Collection, conflicts)
		r.log.Warn(ctx, "type conflicts stored as null (true values kept in _cdc_doc)", map[string]any{"fields": conflicts})
	}
	r.log.Debug(ctx, "batch committed", map[string]any{"rows": len(events), "seq": cp.Seq, "bytes": res.Bytes, "ms": time.Since(start).Milliseconds()})
	b.reset()
	if merge {
		if err := r.merge(ctx); err != nil {
			return res, err
		}
	}
	return res, nil
}

// merge applies raw events above the watermark into the clean table.
func (r *runner) merge(ctx context.Context) error {
	start := time.Now()
	res, err := r.w.deps.Query.Merge(ctx, r.alias, r.spec.Namespace, r.spec.Raw, r.spec.Clean, r.mergedSeq, r.cp.Generation)
	if err != nil {
		r.w.deps.Metrics.errors.WithLabelValues(r.p.ID, r.c.Collection, "merge").Inc()
		return fmt.Errorf("merge raw into clean: %w", err)
	}
	r.w.deps.Metrics.mergeSeconds.WithLabelValues(r.p.ID, r.c.Collection).Observe(time.Since(start).Seconds())
	if res.Candidates > 0 {
		r.mergedSeq = res.Watermark
		now := time.Now()
		r.update(ctx, store.CollectionUpdate{MergedSeq: &res.Watermark, LastMergeAt: &now})
		runID, _ := r.w.deps.Store.StartRun(ctx, r.p.ID, r.c.Collection, "merge")
		_ = r.w.deps.Store.FinishRun(ctx, runID, "ok", res.Inserted+res.Deleted, 0, "", map[string]any{"candidates": res.Candidates, "inserted": res.Inserted, "deleted": res.Deleted, "watermark": res.Watermark, "ms": res.DurationMs})
		r.log.Debug(ctx, "merged into clean", map[string]any{"candidates": res.Candidates, "inserted": res.Inserted, "deleted": res.Deleted, "watermark": res.Watermark})
	}
	r.lastMerge = time.Now()
	return nil
}

// rebuildClean replaces the clean table with the latest generation after a resync.
func (r *runner) rebuildClean(ctx context.Context) error {
	r.log.Info(ctx, "rebuilding the clean table from the new generation", map[string]any{"generation": r.cp.Generation})
	res, err := r.w.deps.Query.RebuildClean(ctx, r.alias, r.spec.Namespace, r.spec.Raw, r.spec.Clean, r.cp.Generation)
	if err != nil {
		return fmt.Errorf("rebuild clean: %w", err)
	}
	r.mergedSeq = res.Watermark
	if res.Watermark == 0 {
		r.mergedSeq = r.cp.Seq
	}
	now := time.Now()
	r.update(ctx, store.CollectionUpdate{MergedSeq: &r.mergedSeq, LastMergeAt: &now})
	runID, _ := r.w.deps.Store.StartRun(ctx, r.p.ID, r.c.Collection, "merge")
	_ = r.w.deps.Store.FinishRun(ctx, runID, "ok", res.Inserted, 0, "", map[string]any{"rebuild": true, "generation": r.cp.Generation, "rows": res.Inserted, "removed": res.Deleted})
	r.log.Info(ctx, "clean table rebuilt", map[string]any{"rows": res.Inserted, "removed": res.Deleted})
	return nil
}
