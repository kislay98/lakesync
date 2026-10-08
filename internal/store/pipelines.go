package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const pipelineCols = "id, owner_id, name, source_id, destination_id, namespace, sync_interval_s, state, desired_state, settings, created_at, updated_at"

func scanPipeline(row interface{ Scan(...any) error }) (*Pipeline, error) {
	p := &Pipeline{}
	var settings []byte
	if err := row.Scan(&p.ID, &p.OwnerID, &p.Name, &p.SourceID, &p.DestinationID, &p.Namespace, &p.SyncIntervalS, &p.State, &p.DesiredState, &settings, &p.CreatedAt, &p.UpdatedAt); err != nil {
		if isNoRows(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	p.Settings = map[string]any{}
	if len(settings) > 0 {
		_ = json.Unmarshal(settings, &p.Settings)
	}
	return p, nil
}

// ListPipelines returns every pipeline.
func (s *Store) ListPipelines(ctx context.Context) ([]*Pipeline, error) {
	rows, err := s.Pool.Query(ctx, "SELECT "+pipelineCols+" FROM pipelines ORDER BY created_at")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Pipeline
	for rows.Next() {
		p, err := scanPipeline(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetPipeline loads one pipeline.
func (s *Store) GetPipeline(ctx context.Context, id string) (*Pipeline, error) {
	return scanPipeline(s.Pool.QueryRow(ctx, "SELECT "+pipelineCols+" FROM pipelines WHERE id = $1", id))
}

// CreatePipeline inserts the pipeline and its collection rows.
func (s *Store) CreatePipeline(ctx context.Context, p *Pipeline, cols []PipelineCollection) (*Pipeline, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	settings, _ := json.Marshal(p.Settings)
	if p.Settings == nil {
		settings = []byte("{}")
	}
	created, err := scanPipeline(tx.QueryRow(ctx,
		"INSERT INTO pipelines (owner_id, name, source_id, destination_id, namespace, sync_interval_s, state, desired_state, settings) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING "+pipelineCols,
		p.OwnerID, p.Name, p.SourceID, p.DestinationID, p.Namespace, p.SyncIntervalS, StateCreated, p.DesiredState, settings))
	if err != nil {
		return nil, err
	}
	for _, c := range cols {
		if _, err := tx.Exec(ctx,
			"INSERT INTO pipeline_collections (pipeline_id, collection, enabled, state, raw_table, clean_table, snapshot_total_est) VALUES ($1,$2,$3,$4,$5,$6,$7)",
			created.ID, c.Collection, c.Enabled, StateCreated, c.RawTable, c.CleanTable, c.SnapshotTotalEst); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return created, nil
}

// UpdatePipeline changes the editable fields.
func (s *Store) UpdatePipeline(ctx context.Context, id, name string, intervalS int, settings map[string]any) (*Pipeline, error) {
	st, _ := json.Marshal(settings)
	if settings == nil {
		st = []byte("{}")
	}
	return scanPipeline(s.Pool.QueryRow(ctx,
		"UPDATE pipelines SET name = $2, sync_interval_s = $3, settings = $4, updated_at = now() WHERE id = $1 RETURNING "+pipelineCols,
		id, name, intervalS, st))
}

// SetPipelineDesiredState sets what the user wants (running/paused/stopped).
func (s *Store) SetPipelineDesiredState(ctx context.Context, id, desired string) error {
	tag, err := s.Pool.Exec(ctx, "UPDATE pipelines SET desired_state = $2, updated_at = now() WHERE id = $1", id, desired)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetPipelineState sets the aggregate state computed by the worker.
func (s *Store) SetPipelineState(ctx context.Context, id, state string) error {
	_, err := s.Pool.Exec(ctx, "UPDATE pipelines SET state = $2, updated_at = now() WHERE id = $1 AND state <> $2", id, state)
	return err
}

// DeletePipeline removes the pipeline and everything attached to it.
func (s *Store) DeletePipeline(ctx context.Context, id string) error {
	tag, err := s.Pool.Exec(ctx, "DELETE FROM pipelines WHERE id = $1", id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

const collectionCols = "pipeline_id, collection, enabled, state, raw_table, clean_table, generation, last_seq, merged_seq, snapshot_total_est, snapshot_done, last_event_at, last_source_ts, last_commit_at, last_merge_at, last_error, state_message, resync_reason, requested_action, failures, schema, updated_at"

func scanCollection(row interface{ Scan(...any) error }) (*PipelineCollection, error) {
	c := &PipelineCollection{}
	if err := row.Scan(&c.PipelineID, &c.Collection, &c.Enabled, &c.State, &c.RawTable, &c.CleanTable, &c.Generation, &c.LastSeq, &c.MergedSeq, &c.SnapshotTotalEst, &c.SnapshotDone, &c.LastEventAt, &c.LastSourceTS, &c.LastCommitAt, &c.LastMergeAt, &c.LastError, &c.StateMessage, &c.ResyncReason, &c.RequestedAction, &c.Failures, &c.Schema, &c.UpdatedAt); err != nil {
		if isNoRows(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return c, nil
}

// ListCollections returns the collections of a pipeline.
func (s *Store) ListCollections(ctx context.Context, pipelineID string) ([]*PipelineCollection, error) {
	rows, err := s.Pool.Query(ctx, "SELECT "+collectionCols+" FROM pipeline_collections WHERE pipeline_id = $1 ORDER BY collection", pipelineID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*PipelineCollection
	for rows.Next() {
		c, err := scanCollection(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListAllCollections returns every collection row across pipelines.
func (s *Store) ListAllCollections(ctx context.Context) ([]*PipelineCollection, error) {
	rows, err := s.Pool.Query(ctx, "SELECT "+collectionCols+" FROM pipeline_collections ORDER BY pipeline_id, collection")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*PipelineCollection
	for rows.Next() {
		c, err := scanCollection(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetCollection loads one collection row.
func (s *Store) GetCollection(ctx context.Context, pipelineID, collection string) (*PipelineCollection, error) {
	return scanCollection(s.Pool.QueryRow(ctx, "SELECT "+collectionCols+" FROM pipeline_collections WHERE pipeline_id = $1 AND collection = $2", pipelineID, collection))
}

// UpdateCollection applies the non-nil fields of u.
func (s *Store) UpdateCollection(ctx context.Context, pipelineID, collection string, u CollectionUpdate) error {
	sets := []string{"updated_at = now()"}
	args := []any{pipelineID, collection}
	add := func(col string, v any) {
		args = append(args, v)
		sets = append(sets, fmt.Sprintf("%s = $%d", col, len(args)))
	}
	if u.State != nil {
		add("state", *u.State)
	}
	if u.Generation != nil {
		add("generation", *u.Generation)
	}
	if u.LastSeq != nil {
		add("last_seq", *u.LastSeq)
	}
	if u.MergedSeq != nil {
		add("merged_seq", *u.MergedSeq)
	}
	if u.SnapshotTotalEst != nil {
		add("snapshot_total_est", *u.SnapshotTotalEst)
	}
	if u.SnapshotDone != nil {
		add("snapshot_done", *u.SnapshotDone)
	}
	if u.LastEventAt != nil {
		add("last_event_at", *u.LastEventAt)
	}
	if u.LastSourceTS != nil {
		add("last_source_ts", *u.LastSourceTS)
	}
	if u.LastCommitAt != nil {
		add("last_commit_at", *u.LastCommitAt)
	}
	if u.LastMergeAt != nil {
		add("last_merge_at", *u.LastMergeAt)
	}
	if u.LastError != nil {
		if *u.LastError == "" {
			sets = append(sets, "last_error = NULL")
		} else {
			add("last_error", *u.LastError)
		}
	}
	if u.StateMessage != nil {
		if *u.StateMessage == "" {
			sets = append(sets, "state_message = NULL")
		} else {
			add("state_message", *u.StateMessage)
		}
	}
	if u.ResyncReason != nil {
		if *u.ResyncReason == "" {
			sets = append(sets, "resync_reason = NULL")
		} else {
			add("resync_reason", *u.ResyncReason)
		}
	}
	if u.RequestedAction != nil {
		if *u.RequestedAction == "" {
			sets = append(sets, "requested_action = NULL")
		} else {
			add("requested_action", *u.RequestedAction)
		}
	}
	if u.Failures != nil {
		add("failures", *u.Failures)
	}
	if u.Schema != nil {
		add("schema", u.Schema)
	}
	_, err := s.Pool.Exec(ctx, "UPDATE pipeline_collections SET "+strings.Join(sets, ", ")+" WHERE pipeline_id = $1 AND collection = $2", args...)
	return err
}

// RequestAction asks the worker to do something with a collection (resync,
// retry, maintenance). It returns ErrNotFound for unknown collections.
func (s *Store) RequestAction(ctx context.Context, pipelineID, collection, action string) error {
	tag, err := s.Pool.Exec(ctx, "UPDATE pipeline_collections SET requested_action = $3, updated_at = now() WHERE pipeline_id = $1 AND collection = $2", pipelineID, collection, action)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// RequestActionAll requests an action for every enabled collection of a pipeline.
func (s *Store) RequestActionAll(ctx context.Context, pipelineID, action string) error {
	_, err := s.Pool.Exec(ctx, "UPDATE pipeline_collections SET requested_action = $2, updated_at = now() WHERE pipeline_id = $1 AND enabled", pipelineID, action)
	return err
}

// SetCollectionEnabled toggles a collection.
func (s *Store) SetCollectionEnabled(ctx context.Context, pipelineID, collection string, enabled bool) error {
	_, err := s.Pool.Exec(ctx, "UPDATE pipeline_collections SET enabled = $3, updated_at = now() WHERE pipeline_id = $1 AND collection = $2", pipelineID, collection, enabled)
	return err
}

// AddCollection adds a collection row to an existing pipeline.
func (s *Store) AddCollection(ctx context.Context, c PipelineCollection) error {
	_, err := s.Pool.Exec(ctx,
		"INSERT INTO pipeline_collections (pipeline_id, collection, enabled, state, raw_table, clean_table, snapshot_total_est) VALUES ($1,$2,$3,$4,$5,$6,$7) ON CONFLICT DO NOTHING",
		c.PipelineID, c.Collection, c.Enabled, StateCreated, c.RawTable, c.CleanTable, c.SnapshotTotalEst)
	return err
}

// RemoveCollection deletes a collection row.
func (s *Store) RemoveCollection(ctx context.Context, pipelineID, collection string) error {
	_, err := s.Pool.Exec(ctx, "DELETE FROM pipeline_collections WHERE pipeline_id = $1 AND collection = $2", pipelineID, collection)
	return err
}

// UpsertFieldMappings stores the schema's field-to-column mapping.
func (s *Store) UpsertFieldMappings(ctx context.Context, pipelineID, collection string, m []FieldMapping) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, f := range m {
		if _, err := tx.Exec(ctx, `
INSERT INTO field_mappings (pipeline_id, collection, source_field, column_name, iceberg_type, reason)
VALUES ($1,$2,$3,$4,$5,$6)
ON CONFLICT (pipeline_id, collection, source_field) DO UPDATE SET column_name = EXCLUDED.column_name, iceberg_type = EXCLUDED.iceberg_type, reason = EXCLUDED.reason, updated_at = now()`,
			pipelineID, collection, f.SourceField, f.ColumnName, f.Type, f.Reason); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// AddConflicts increments per-field conflict counters.
func (s *Store) AddConflicts(ctx context.Context, pipelineID, collection string, counts map[string]int) error {
	for field, n := range counts {
		if n == 0 {
			continue
		}
		if _, err := s.Pool.Exec(ctx, `
INSERT INTO field_mappings (pipeline_id, collection, source_field, column_name, iceberg_type, conflict_count)
VALUES ($1,$2,$3,$3,'',$4)
ON CONFLICT (pipeline_id, collection, source_field) DO UPDATE SET conflict_count = field_mappings.conflict_count + EXCLUDED.conflict_count, updated_at = now()`,
			pipelineID, collection, field, n); err != nil {
			return err
		}
	}
	return nil
}

// ListFieldMappings returns the mappings of a collection.
func (s *Store) ListFieldMappings(ctx context.Context, pipelineID, collection string) ([]FieldMapping, error) {
	rows, err := s.Pool.Query(ctx, "SELECT pipeline_id, collection, source_field, column_name, iceberg_type, conflict_count, reason, updated_at FROM field_mappings WHERE pipeline_id = $1 AND collection = $2 ORDER BY source_field", pipelineID, collection)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FieldMapping
	for rows.Next() {
		var f FieldMapping
		if err := rows.Scan(&f.PipelineID, &f.Collection, &f.SourceField, &f.ColumnName, &f.Type, &f.ConflictCount, &f.Reason, &f.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// FindCollectionByTable locates the pipeline collection behind a destination table.
func (s *Store) FindCollectionByTable(ctx context.Context, destinationID, namespace, table string) (*PipelineCollection, error) {
	cols := make([]string, 0, 24)
	for _, c := range strings.Split(collectionCols, ", ") {
		cols = append(cols, "c."+c)
	}
	return scanCollection(s.Pool.QueryRow(ctx, `
SELECT `+strings.Join(cols, ", ")+`
FROM pipeline_collections c JOIN pipelines p ON p.id = c.pipeline_id
WHERE p.destination_id = $1 AND p.namespace = $2 AND (c.clean_table = $3 OR c.raw_table = $3)
LIMIT 1`, destinationID, namespace, table))
}

// StartRun records the start of a run.
func (s *Store) StartRun(ctx context.Context, pipelineID, collection, kind string) (int64, error) {
	var id int64
	err := s.Pool.QueryRow(ctx, "INSERT INTO runs (pipeline_id, collection, kind) VALUES ($1,$2,$3) RETURNING id", pipelineID, collection, kind).Scan(&id)
	return id, err
}

// FinishRun records the end of a run.
func (s *Store) FinishRun(ctx context.Context, id int64, status string, rows, bytes int64, errMsg string, details any) error {
	var det []byte
	if details != nil {
		det, _ = json.Marshal(details)
	}
	var e *string
	if errMsg != "" {
		e = &errMsg
	}
	_, err := s.Pool.Exec(ctx, "UPDATE runs SET ended_at = now(), status = $2, rows = $3, bytes = $4, error = $5, details = $6 WHERE id = $1", id, status, rows, bytes, e, det)
	return err
}

// UpdateRunProgress updates the row counter of a running run.
func (s *Store) UpdateRunProgress(ctx context.Context, id int64, rows, bytes int64) error {
	_, err := s.Pool.Exec(ctx, "UPDATE runs SET rows = $2, bytes = $3 WHERE id = $1", id, rows, bytes)
	return err
}

// ListRuns returns recent runs of a pipeline.
func (s *Store) ListRuns(ctx context.Context, pipelineID string, limit int) ([]Run, error) {
	rows, err := s.Pool.Query(ctx, "SELECT id, pipeline_id, collection, kind, started_at, ended_at, status, rows, bytes, error, details FROM runs WHERE pipeline_id = $1 ORDER BY started_at DESC LIMIT $2", pipelineID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var r Run
		if err := rows.Scan(&r.ID, &r.PipelineID, &r.Collection, &r.Kind, &r.StartedAt, &r.EndedAt, &r.Status, &r.Rows, &r.Bytes, &r.Error, &r.Details); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AbandonRunningRuns marks runs left "running" by a crashed worker.
func (s *Store) AbandonRunningRuns(ctx context.Context) error {
	_, err := s.Pool.Exec(ctx, "UPDATE runs SET status = 'interrupted', ended_at = now() WHERE status = 'running' AND started_at < now() - interval '1 minute'")
	return err
}

// AppendLog stores a log line.
func (s *Store) AppendLog(ctx context.Context, pipelineID, collection, level, message string, fields map[string]any) (int64, error) {
	var f []byte
	if len(fields) > 0 {
		f, _ = json.Marshal(fields)
	}
	var id int64
	err := s.Pool.QueryRow(ctx, "INSERT INTO logs (pipeline_id, collection, level, message, fields) VALUES ($1,$2,$3,$4,$5) RETURNING id", pipelineID, collection, level, message, f).Scan(&id)
	return id, err
}

// LogFilter narrows a log query.
type LogFilter struct {
	Level      string // minimum level: debug, info, warn, error
	Collection string
	Search     string
	BeforeID   int64
	Limit      int
}

// ListLogs returns log lines newest first.
func (s *Store) ListLogs(ctx context.Context, pipelineID string, f LogFilter) ([]LogEntry, error) {
	q := "SELECT id, pipeline_id, collection, level, message, fields, at FROM logs WHERE pipeline_id = $1"
	args := []any{pipelineID}
	if f.Level != "" && f.Level != "debug" {
		levels := map[string][]string{"info": {"info", "warn", "error"}, "warn": {"warn", "error"}, "error": {"error"}}[f.Level]
		args = append(args, levels)
		q += fmt.Sprintf(" AND level = ANY($%d)", len(args))
	}
	if f.Collection != "" {
		args = append(args, f.Collection)
		q += fmt.Sprintf(" AND collection = $%d", len(args))
	}
	if f.Search != "" {
		args = append(args, "%"+f.Search+"%")
		q += fmt.Sprintf(" AND (message ILIKE $%d OR fields::text ILIKE $%d)", len(args), len(args))
	}
	if f.BeforeID > 0 {
		args = append(args, f.BeforeID)
		q += fmt.Sprintf(" AND id < $%d", len(args))
	}
	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	args = append(args, limit)
	q += fmt.Sprintf(" ORDER BY id DESC LIMIT $%d", len(args))
	rows, err := s.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LogEntry
	for rows.Next() {
		var l LogEntry
		if err := rows.Scan(&l.ID, &l.PipelineID, &l.Collection, &l.Level, &l.Message, &l.Fields, &l.At); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// TrimLogs enforces the retention: 7 days, or 10,000 rows per pipeline.
func (s *Store) TrimLogs(ctx context.Context) error {
	if _, err := s.Pool.Exec(ctx, "DELETE FROM logs WHERE at < now() - interval '7 days'"); err != nil {
		return err
	}
	_, err := s.Pool.Exec(ctx, `
DELETE FROM logs WHERE id IN (
  SELECT id FROM (SELECT id, row_number() OVER (PARTITION BY pipeline_id ORDER BY id DESC) AS rn FROM logs) t WHERE rn > 10000)`)
	return err
}

// AddEvents adds to the per-minute counter of a collection.
func (s *Store) AddEvents(ctx context.Context, pipelineID, collection string, at time.Time, n int64) error {
	if n == 0 {
		return nil
	}
	_, err := s.Pool.Exec(ctx, `
INSERT INTO event_counters (pipeline_id, collection, minute, events) VALUES ($1,$2,date_trunc('minute', $3::timestamptz),$4)
ON CONFLICT (pipeline_id, collection, minute) DO UPDATE SET events = event_counters.events + EXCLUDED.events`, pipelineID, collection, at, n)
	return err
}

// EventsSeries returns per-minute counts for the last `minutes` minutes
// (missing minutes are returned as zero).
func (s *Store) EventsSeries(ctx context.Context, pipelineID, collection string, minutes int) ([]MinuteCount, error) {
	q := `
WITH m AS (SELECT generate_series(date_trunc('minute', now()) - ($3::int - 1) * interval '1 minute', date_trunc('minute', now()), interval '1 minute') AS minute)
SELECT m.minute, COALESCE(sum(e.events), 0) FROM m
LEFT JOIN event_counters e ON e.minute = m.minute AND e.pipeline_id = $1 AND ($2 = '' OR e.collection = $2)
GROUP BY m.minute ORDER BY m.minute`
	rows, err := s.Pool.Query(ctx, q, pipelineID, collection, minutes)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MinuteCount
	for rows.Next() {
		var mc MinuteCount
		if err := rows.Scan(&mc.Minute, &mc.Events); err != nil {
			return nil, err
		}
		out = append(out, mc)
	}
	return out, rows.Err()
}

// EventsLastHour sums the last 60 minutes across all pipelines or one.
func (s *Store) EventsLastHour(ctx context.Context, pipelineID string) (int64, error) {
	var n int64
	err := s.Pool.QueryRow(ctx, "SELECT COALESCE(sum(events),0) FROM event_counters WHERE minute > now() - interval '60 minutes' AND ($1 = '' OR pipeline_id::text = $1)", pipelineID).Scan(&n)
	return n, err
}

// TrimEventCounters drops counters older than two days.
func (s *Store) TrimEventCounters(ctx context.Context) error {
	_, err := s.Pool.Exec(ctx, "DELETE FROM event_counters WHERE minute < now() - interval '2 days'")
	return err
}

// GetSetting loads a JSON setting into v; ok=false when unset.
func (s *Store) GetSetting(ctx context.Context, key string, v any) (bool, error) {
	var raw []byte
	err := s.Pool.QueryRow(ctx, "SELECT value FROM settings WHERE key = $1", key).Scan(&raw)
	if isNoRows(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(raw, v)
}

// SetSetting stores a JSON setting.
func (s *Store) SetSetting(ctx context.Context, key string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.Pool.Exec(ctx, "INSERT INTO settings (key, value) VALUES ($1, $2) ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()", key, raw)
	return err
}

// AddQueryHistory records a SQL console run and keeps the last 100 per user.
func (s *Store) AddQueryHistory(ctx context.Context, userID string, destination *string, sqlText string, durationMs, rows int, errMsg string) error {
	var e *string
	if errMsg != "" {
		e = &errMsg
	}
	if _, err := s.Pool.Exec(ctx, "INSERT INTO query_history (user_id, destination, sql, duration_ms, rows, error) VALUES ($1,$2,$3,$4,$5,$6)", userID, destination, sqlText, durationMs, rows, e); err != nil {
		return err
	}
	_, err := s.Pool.Exec(ctx, "DELETE FROM query_history WHERE user_id = $1 AND id NOT IN (SELECT id FROM query_history WHERE user_id = $1 ORDER BY id DESC LIMIT 100)", userID)
	return err
}

// ListQueryHistory returns a user's recent queries.
func (s *Store) ListQueryHistory(ctx context.Context, userID string, limit int) ([]QueryHistoryEntry, error) {
	rows, err := s.Pool.Query(ctx, "SELECT id, destination, sql, duration_ms, rows, error, at FROM query_history WHERE user_id = $1 ORDER BY id DESC LIMIT $2", userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QueryHistoryEntry
	for rows.Next() {
		var q QueryHistoryEntry
		if err := rows.Scan(&q.ID, &q.Destination, &q.SQL, &q.DurationMs, &q.Rows, &q.Error, &q.At); err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}
