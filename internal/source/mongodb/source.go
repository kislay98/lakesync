package mongodb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"
	"golang.org/x/time/rate"

	"lakesync/internal/core"
	"lakesync/internal/source"
)

// Source is one MongoDB connection shared by every collection it syncs.
type Source struct {
	cfg    Config
	sec    Secrets
	client *mongo.Client

	mu             sync.Mutex
	skippedUpdates atomic.Int64
	prePostImages  map[string]bool // "db.coll" -> enabled
}

var _ source.Source = (*Source)(nil)

// New connects to MongoDB. The connection is verified lazily by the driver; use
// Test for an eager, explained check.
func New(_ context.Context, cfg Config, sec Secrets) (*Source, error) {
	if cfg.AuthSource == "" && cfg.Username != "" {
		cfg.AuthSource = "admin"
	}
	cl, err := connect(cfg, sec)
	if err != nil {
		return nil, err
	}
	return &Source{cfg: cfg, sec: sec, client: cl, prePostImages: map[string]bool{}}, nil
}

// Client exposes the shared driver client (used by the seed and verify tools).
func (s *Source) Client() *mongo.Client { return s.client }

// SkippedUpdates returns how many update events were skipped because the
// document had already been deleted when it was looked up.
func (s *Source) SkippedUpdates() int64 { return s.skippedUpdates.Load() }

// Close disconnects the client.
func (s *Source) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.client.Disconnect(ctx)
}

var systemDatabases = map[string]bool{"admin": true, "local": true, "config": true}

// ListEntities lists collections with estimated counts and data sizes.
func (s *Source) ListEntities(ctx context.Context) ([]core.Entity, error) {
	var dbs []string
	if s.cfg.Database != "" {
		dbs = []string{s.cfg.Database}
	} else {
		names, err := s.client.ListDatabaseNames(ctx, bson.D{})
		if err != nil {
			return nil, fmt.Errorf("list databases: %w", s.redact(err))
		}
		for _, n := range names {
			if !systemDatabases[n] {
				dbs = append(dbs, n)
			}
		}
	}
	sort.Strings(dbs)
	var out []core.Entity
	for _, dbName := range dbs {
		db := s.client.Database(dbName)
		specs, err := db.ListCollectionSpecifications(ctx, bson.D{})
		if err != nil {
			return nil, fmt.Errorf("list collections of %s: %w", dbName, s.redact(err))
		}
		sort.Slice(specs, func(i, j int) bool { return specs[i].Name < specs[j].Name })
		for _, spec := range specs {
			if spec.Type != "collection" || strings.HasPrefix(spec.Name, "system.") {
				continue
			}
			e := core.Entity{Namespace: dbName, Name: spec.Name}
			coll := db.Collection(spec.Name)
			if n, err := coll.EstimatedDocumentCount(ctx); err == nil {
				e.EstimatedCount = n
			}
			e.EstimatedBytes = s.dataSize(ctx, coll)
			out = append(out, e)
		}
	}
	return out, nil
}

// dataSize returns the uncompressed data size from $collStats, or 0 when the
// user may not run it.
func (s *Source) dataSize(ctx context.Context, coll *mongo.Collection) int64 {
	cur, err := coll.Aggregate(ctx, bson.A{bson.D{{Key: "$collStats", Value: bson.D{{Key: "storageStats", Value: bson.D{}}}}}})
	if err != nil {
		return 0
	}
	defer cur.Close(ctx)
	var total int64
	for cur.Next(ctx) {
		if v, err := cur.Current.LookupErr("storageStats", "size"); err == nil {
			if n, ok := v.AsInt64OK(); ok {
				total += n
			}
		}
	}
	return total
}

// Sample returns up to limit documents in natural order.
func (s *Source) Sample(ctx context.Context, e core.Entity, limit int) ([]*core.Document, error) {
	if limit <= 0 {
		limit = 1000
	}
	cur, err := s.coll(e).Find(ctx, bson.D{}, options.Find().SetLimit(int64(limit)))
	if err != nil {
		return nil, fmt.Errorf("sample %s: %w", e, s.redact(err))
	}
	defer cur.Close(ctx)
	var docs []*core.Document
	for cur.Next(ctx) {
		d, err := toDocument(cur.Current)
		if err != nil {
			return nil, fmt.Errorf("sample %s: %w", e, err)
		}
		docs = append(docs, d)
	}
	if err := cur.Err(); err != nil {
		return nil, fmt.Errorf("sample %s: %w", e, s.redact(err))
	}
	return docs, nil
}

func (s *Source) coll(e core.Entity) *mongo.Collection {
	return s.client.Database(e.Namespace).Collection(e.Name)
}

func (s *Source) redact(err error) error { return redactErr(err, s.sec) }

// ---- bookmarks ------------------------------------------------------------

// bookmark is the JSON form stored in checkpoints. Exactly one of Token or
// (T, I) is set.
type bookmark struct {
	Kind  string          `json:"kind"`            // "token" | "optime"
	Token json.RawMessage `json:"token,omitempty"` // canonical Extended JSON of the resume token
	T     uint32          `json:"t,omitempty"`
	I     uint32          `json:"i,omitempty"`
}

func encodeTokenBookmark(token bson.Raw) (core.Bookmark, error) {
	js, err := bson.MarshalExtJSON(token, true, false)
	if err != nil {
		return nil, fmt.Errorf("encode resume token: %w", err)
	}
	return json.Marshal(bookmark{Kind: "token", Token: js})
}

func encodeOpTimeBookmark(ts bson.Timestamp) (core.Bookmark, error) {
	return json.Marshal(bookmark{Kind: "optime", T: ts.T, I: ts.I})
}

// DecodeBookmark parses a bookmark produced by this source (exported for tests
// and diagnostics).
func DecodeBookmark(b core.Bookmark) (kind string, token bson.Raw, ts bson.Timestamp, err error) {
	var bm bookmark
	if err := json.Unmarshal(b, &bm); err != nil {
		return "", nil, bson.Timestamp{}, fmt.Errorf("invalid bookmark: %w", err)
	}
	switch bm.Kind {
	case "token":
		var raw bson.Raw
		if err := bson.UnmarshalExtJSON(bm.Token, true, &raw); err != nil {
			return "", nil, bson.Timestamp{}, fmt.Errorf("invalid resume token in bookmark: %w", err)
		}
		return "token", raw, bson.Timestamp{}, nil
	case "optime":
		return "optime", nil, bson.Timestamp{T: bm.T, I: bm.I}, nil
	}
	return "", nil, bson.Timestamp{}, fmt.Errorf("invalid bookmark kind %q", bm.Kind)
}

// CurrentBookmark opens a change stream, reads at most one event and returns the
// post-batch resume token; without one it falls back to the server's operationTime.
func (s *Source) CurrentBookmark(ctx context.Context, e core.Entity) (core.Bookmark, error) {
	cs, err := s.coll(e).Watch(ctx, mongo.Pipeline{}, options.ChangeStream().
		SetFullDocument(options.UpdateLookup).SetMaxAwaitTime(500*time.Millisecond))
	if err != nil {
		return nil, fmt.Errorf("open change stream on %s: %w", e, classify(s.redact(err)))
	}
	defer cs.Close(ctx)
	tctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	cs.TryNext(tctx)
	cancel()
	if err := cs.Err(); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return nil, fmt.Errorf("read change stream on %s: %w", e, classify(s.redact(err)))
	}
	if tok := cs.ResumeToken(); len(tok) > 0 {
		return encodeTokenBookmark(tok)
	}
	ts, err := s.operationTime(ctx)
	if err != nil {
		return nil, err
	}
	return encodeOpTimeBookmark(ts)
}

// operationTime asks the server for its current operationTime.
func (s *Source) operationTime(ctx context.Context) (bson.Timestamp, error) {
	res := s.client.Database("admin").RunCommand(ctx, bson.D{{Key: "hello", Value: 1}})
	raw, err := res.Raw()
	if err != nil {
		return bson.Timestamp{}, fmt.Errorf("hello: %w", s.redact(err))
	}
	for _, path := range [][]string{{"operationTime"}, {"$clusterTime", "clusterTime"}} {
		if v, err := raw.LookupErr(path...); err == nil {
			if t, i, ok := v.TimestampOK(); ok {
				return bson.Timestamp{T: t, I: i}, nil
			}
		}
	}
	return bson.Timestamp{}, errors.New("server returned no operationTime; is it a replica set?")
}

// ---- full copy ------------------------------------------------------------

// Snapshot reads the collection ordered by _id in chunks, resuming after the
// cursor. Chunks use `_id > last` range queries; when a chunk comes back short the
// iterator hops to the next BSON type bracket with an aggregation comparison so
// collections with mixed _id types are copied completely.
func (s *Source) Snapshot(ctx context.Context, e core.Entity, from core.SnapshotCursor, opts source.SnapshotOptions) (source.EventIterator, error) {
	if opts.ChunkSize <= 0 {
		opts.ChunkSize = 5000
	}
	it := &snapshotIter{src: s, entity: e, coll: s.coll(e), chunk: opts.ChunkSize, started: time.Now().UTC()}
	if from != "" {
		rv, err := idFromKey(string(from))
		if err != nil {
			return nil, fmt.Errorf("snapshot cursor: %w", err)
		}
		it.last = &rv
	}
	if opts.MaxDocsPerSec > 0 {
		it.limiter = rate.NewLimiter(rate.Limit(opts.MaxDocsPerSec), opts.ChunkSize)
	}
	return it, nil
}

type snapshotIter struct {
	src     *Source
	entity  core.Entity
	coll    *mongo.Collection
	chunk   int
	started time.Time
	limiter *rate.Limiter

	last   *bson.RawValue
	cur    *mongo.Cursor
	curN   int  // docs delivered from the current cursor
	hopped bool // the current cursor is a type-bracket hop
	done   bool
}

func (it *snapshotIter) Next(ctx context.Context) (*core.ChangeEvent, error) {
	for {
		if it.done {
			return nil, io.EOF
		}
		if it.cur == nil {
			if err := it.openChunk(ctx); err != nil {
				return nil, err
			}
		}
		if it.cur.Next(ctx) {
			it.curN++
			raw := it.cur.Current
			id, err := lookupID(raw)
			if err != nil {
				return nil, fmt.Errorf("snapshot %s: %w", it.entity, err)
			}
			key, disp, err := keyOf(id)
			if err != nil {
				return nil, fmt.Errorf("snapshot %s: %w", it.entity, err)
			}
			doc, err := toDocument(raw)
			if err != nil {
				return nil, fmt.Errorf("snapshot %s: %w", it.entity, err)
			}
			cp := bson.RawValue{Type: id.Type, Value: append([]byte(nil), id.Value...)}
			it.last = &cp
			return &core.ChangeEvent{Entity: it.entity.String(), Key: key, DisplayID: disp, Op: core.OpSnapshot, SourceTS: it.started, Doc: doc}, nil
		}
		err := it.cur.Err()
		it.cur.Close(ctx)
		it.cur = nil
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, core.Transient(fmt.Errorf("snapshot %s: %w", it.entity, it.src.redact(err)))
		}
		full := it.curN >= it.chunk
		short := !full
		if short && (it.hopped || it.last == nil) {
			// a short hop (or an empty collection) means nothing is left
			it.done = true
			continue
		}
		it.hopped = short // short normal chunk -> try the next type bracket once
	}
}

func (it *snapshotIter) openChunk(ctx context.Context) error {
	if it.limiter != nil {
		if err := it.limiter.WaitN(ctx, it.chunk); err != nil {
			return err
		}
	}
	filter := bson.D{}
	if it.last != nil {
		if it.hopped {
			// cross-type ordering: aggregation $gt follows BSON sort order
			filter = bson.D{{Key: "$expr", Value: bson.D{{Key: "$gt", Value: bson.A{"$_id", bson.D{{Key: "$literal", Value: *it.last}}}}}}}
		} else {
			filter = bson.D{{Key: "_id", Value: bson.D{{Key: "$gt", Value: *it.last}}}}
		}
	}
	cur, err := it.coll.Find(ctx, filter, options.Find().
		SetSort(bson.D{{Key: "_id", Value: 1}}).SetLimit(int64(it.chunk)).SetBatchSize(int32(it.chunk)))
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return core.Transient(fmt.Errorf("snapshot %s: %w", it.entity, it.src.redact(err)))
	}
	it.cur = cur
	it.curN = 0
	return nil
}

func (it *snapshotIter) Close() error {
	if it.cur != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := it.cur.Close(ctx)
		it.cur = nil
		return err
	}
	return nil
}

// ---- change stream --------------------------------------------------------

// Stream follows the collection's change stream from the bookmark.
func (s *Source) Stream(ctx context.Context, e core.Entity, from core.Bookmark) (source.EventIterator, error) {
	opts := options.ChangeStream().SetFullDocument(options.UpdateLookup).SetMaxAwaitTime(time.Second)
	if len(from) > 0 {
		kind, tok, ts, err := DecodeBookmark(from)
		if err != nil {
			return nil, err
		}
		switch kind {
		case "token":
			opts.SetStartAfter(tok)
		case "optime":
			opts.SetStartAtOperationTime(&ts)
		}
	}
	s.recordPrePostImages(ctx, e)
	cs, err := s.coll(e).Watch(ctx, mongo.Pipeline{}, opts)
	if err != nil {
		return nil, classify(fmt.Errorf("open change stream on %s: %w", e, s.redact(err)))
	}
	return &streamIter{src: s, entity: e, cs: cs}, nil
}

// recordPrePostImages notes whether the collection has pre/post images enabled
// (MongoDB 6.0+). lakesync does not depend on them.
func (s *Source) recordPrePostImages(ctx context.Context, e core.Entity) {
	s.mu.Lock()
	_, known := s.prePostImages[e.String()]
	s.mu.Unlock()
	if known {
		return
	}
	enabled := false
	specs, err := s.client.Database(e.Namespace).ListCollectionSpecifications(ctx, bson.D{{Key: "name", Value: e.Name}})
	if err == nil {
		for _, sp := range specs {
			if v, err := sp.Options.LookupErr("changeStreamPreAndPostImages", "enabled"); err == nil {
				enabled, _ = v.BooleanOK()
			}
		}
	}
	s.mu.Lock()
	s.prePostImages[e.String()] = enabled
	s.mu.Unlock()
	slog.Debug("change stream pre/post images", "entity", e.String(), "enabled", enabled)
}

// PrePostImagesEnabled reports what recordPrePostImages found for an entity.
func (s *Source) PrePostImagesEnabled(e core.Entity) (enabled, known bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	enabled, known = s.prePostImages[e.String()]
	return
}

type streamIter struct {
	src    *Source
	entity core.Entity
	cs     *mongo.ChangeStream
}

type changeDoc struct {
	OperationType string         `bson:"operationType"`
	ClusterTime   bson.Timestamp `bson:"clusterTime"`
	FullDocument  bson.Raw       `bson:"fullDocument"`
	DocumentKey   bson.Raw       `bson:"documentKey"`
	NS            struct {
		DB   string `bson:"db"`
		Coll string `bson:"coll"`
	} `bson:"ns"`
	To struct {
		DB   string `bson:"db"`
		Coll string `bson:"coll"`
	} `bson:"to"`
}

func (it *streamIter) Next(ctx context.Context) (*core.ChangeEvent, error) {
	for {
		// TryNext issues one getMore (bounded by maxAwaitTime) and returns when it
		// is empty. The blocking Next would wrap the whole wait for an event in the
		// client-level operation timeout, so an idle stream would fail every 2 minutes.
		if !it.cs.TryNext(ctx) {
			if err := it.cs.Err(); err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				return nil, classify(fmt.Errorf("change stream %s: %w", it.entity, it.src.redact(err)))
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue // no event within maxAwaitTime
		}
		var ev changeDoc
		if err := it.cs.Decode(&ev); err != nil {
			return nil, fmt.Errorf("decode change event on %s: %w", it.entity, err)
		}
		switch ev.OperationType {
		case "drop":
			return nil, &core.SourceRemovedError{Reason: fmt.Sprintf("collection %s was dropped", it.entity)}
		case "rename":
			return nil, &core.SourceRemovedError{Reason: fmt.Sprintf("collection %s was renamed to %s.%s", it.entity, ev.To.DB, ev.To.Coll)}
		case "dropDatabase":
			return nil, &core.SourceRemovedError{Reason: fmt.Sprintf("database %s was dropped", it.entity.Namespace)}
		case "invalidate":
			return nil, &core.SourceRemovedError{Reason: fmt.Sprintf("the change stream on %s was invalidated by the server", it.entity)}
		}
		var op core.Op
		switch ev.OperationType {
		case "insert":
			op = core.OpInsert
		case "update":
			op = core.OpUpdate
		case "replace":
			op = core.OpReplace
		case "delete":
			op = core.OpDelete
		default:
			continue // createIndexes, modify, ... (only with showExpandedEvents)
		}
		idv, err := lookupID(ev.DocumentKey)
		if err != nil {
			return nil, fmt.Errorf("change event on %s: documentKey without _id", it.entity)
		}
		key, disp, err := keyOf(idv)
		if err != nil {
			return nil, fmt.Errorf("change event on %s: %w", it.entity, err)
		}
		out := &core.ChangeEvent{
			Entity:    it.entity.String(),
			Key:       key,
			DisplayID: disp,
			Op:        op,
			SourceTS:  time.Unix(int64(ev.ClusterTime.T), 0).UTC(),
		}
		if op != core.OpDelete {
			if len(ev.FullDocument) == 0 {
				// the document was deleted before the lookup; the delete event follows
				it.src.skippedUpdates.Add(1)
				continue
			}
			doc, err := toDocument(ev.FullDocument)
			if err != nil {
				return nil, fmt.Errorf("change event on %s: %w", it.entity, err)
			}
			out.Doc = doc
		}
		if tok := it.cs.ResumeToken(); len(tok) > 0 {
			bm, err := encodeTokenBookmark(tok)
			if err != nil {
				return nil, err
			}
			out.Bookmark = bm
		}
		return out, nil
	}
}

func (it *streamIter) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return it.cs.Close(ctx)
}

// ---- error classification -------------------------------------------------

// Server error codes that mean the resume point is gone for good.
const (
	codeCappedPositionLost      = 136
	codeChangeStreamFatalError  = 280
	codeChangeStreamHistoryLost = 286
)

// classify maps driver errors to the core's categories: history lost (needs a
// resync), source removed, or transient (retry with backoff).
func classify(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, core.ErrHistoryLost) || core.IsSourceRemoved(err) || core.IsTransient(err) {
		return err
	}
	var se mongo.ServerError
	if errors.As(err, &se) {
		if se.HasErrorCode(codeChangeStreamHistoryLost) || se.HasErrorCode(codeChangeStreamFatalError) ||
			se.HasErrorCode(codeCappedPositionLost) || se.HasErrorLabel("NonResumableChangeStreamError") {
			return fmt.Errorf("%w: %v", core.ErrHistoryLost, err)
		}
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "resume token") && (strings.Contains(msg, "not found") || strings.Contains(msg, "no longer")) ||
			strings.Contains(msg, "oplog") && strings.Contains(msg, "truncat") {
			return fmt.Errorf("%w: %v", core.ErrHistoryLost, err)
		}
	}
	return core.Transient(err)
}

// pingPrimary checks the connection against the primary.
func (s *Source) pingPrimary(ctx context.Context) error {
	return s.client.Ping(ctx, readpref.Primary())
}
