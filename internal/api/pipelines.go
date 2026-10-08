package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"lakesync/internal/conn"
	"lakesync/internal/core"
	"lakesync/internal/core/engine"
	"lakesync/internal/store"
)

type pipelineView struct {
	*store.Pipeline
	Source      connRef                     `json:"source"`
	Destination connRef                     `json:"destination"`
	Collections []*store.PipelineCollection `json:"collections"`
	Stats       pipelineStats               `json:"stats"`
}

type connRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

type pipelineStats struct {
	RowsSynced     int64    `json:"rows_synced"`
	EventsLastHour int64    `json:"events_last_hour"`
	LagSeconds     *float64 `json:"lag_seconds"`
	Collections    int      `json:"collections"`
	Attention      []string `json:"attention"`
}

func (s *Server) pipelineView(ctx context.Context, p *store.Pipeline) (*pipelineView, error) {
	cols, err := s.st.ListCollections(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	if cols == nil {
		cols = []*store.PipelineCollection{}
	}
	v := &pipelineView{Pipeline: p, Collections: cols}
	if c, err := s.st.GetConnection(ctx, p.SourceID); err == nil {
		v.Source = connRef{c.ID, c.Name, c.Type}
	}
	if c, err := s.st.GetConnection(ctx, p.DestinationID); err == nil {
		v.Destination = connRef{c.ID, c.Name, c.Type}
	}
	v.Stats = s.stats(ctx, p, cols)
	return v, nil
}

func (s *Server) stats(ctx context.Context, p *store.Pipeline, cols []*store.PipelineCollection) pipelineStats {
	st := pipelineStats{Attention: []string{}}
	var maxLag float64
	hasLag := false
	for _, c := range cols {
		if !c.Enabled {
			continue
		}
		st.Collections++
		st.RowsSynced += c.LastSeq
		if c.State == store.StateStreaming && c.LastSourceTS != nil {
			lag := time.Since(*c.LastSourceTS).Seconds()
			if lag > maxLag {
				maxLag = lag
			}
			hasLag = true
		}
		switch c.State {
		case store.StateFailed:
			st.Attention = append(st.Attention, c.Collection+": failed")
		case store.StateSourceRemoved:
			st.Attention = append(st.Attention, c.Collection+": removed at the source")
		}
	}
	if hasLag {
		st.LagSeconds = &maxLag
	}
	st.EventsLastHour, _ = s.st.EventsLastHour(ctx, p.ID)
	return st
}

func (s *Server) listPipelines(w http.ResponseWriter, r *http.Request) {
	ps, err := s.st.ListPipelines(r.Context())
	if err != nil {
		s.internal(w, err)
		return
	}
	out := make([]*pipelineView, 0, len(ps))
	for _, p := range ps {
		v, err := s.pipelineView(r.Context(), p)
		if err != nil {
			s.internal(w, err)
			return
		}
		out = append(out, v)
	}
	s.writeJSON(w, 200, out)
}

func (s *Server) getPipeline(w http.ResponseWriter, r *http.Request) {
	p, err := s.st.GetPipeline(r.Context(), r.PathValue("id"))
	if err != nil {
		s.notFound(w, "pipeline")
		return
	}
	v, err := s.pipelineView(r.Context(), p)
	if err != nil {
		s.internal(w, err)
		return
	}
	s.writeJSON(w, 200, v)
}

type pipelineInput struct {
	Name          string         `json:"name"`
	SourceID      string         `json:"source_id"`
	DestinationID string         `json:"destination_id"`
	Namespace     string         `json:"namespace"`
	SyncIntervalS int            `json:"sync_interval_s"`
	Collections   []collInput    `json:"collections"`
	Start         bool           `json:"start"`
	Settings      map[string]any `json:"settings"`
}

type collInput struct {
	Collection     string `json:"collection"`
	Enabled        *bool  `json:"enabled"`
	EstimatedCount int64  `json:"estimated_count"`
}

func (s *Server) createPipeline(w http.ResponseWriter, r *http.Request) {
	var in pipelineInput
	if err := s.decode(r, &in); err != nil {
		s.badRequest(w, err.Error(), "")
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || in.SourceID == "" || in.DestinationID == "" {
		s.badRequest(w, "Name, source and destination are required.", "")
		return
	}
	src, err := s.st.GetConnection(r.Context(), in.SourceID)
	if err != nil || src.Kind != "source" {
		s.badRequest(w, "Unknown source connection.", "")
		return
	}
	dst, err := s.st.GetConnection(r.Context(), in.DestinationID)
	if err != nil || dst.Kind != "destination" {
		s.badRequest(w, "Unknown destination connection.", "")
		return
	}
	if len(in.Collections) == 0 {
		s.badRequest(w, "Pick at least one collection.", "")
		return
	}
	if in.SyncIntervalS == 0 {
		in.SyncIntervalS = s.defaultInterval(r.Context())
	}
	if in.SyncIntervalS < s.cfg.MinSyncIntervalS {
		s.badRequest(w, fmt.Sprintf("The sync interval must be at least %d seconds.", s.cfg.MinSyncIntervalS), "")
		return
	}
	var cols []store.PipelineCollection
	ns := strings.TrimSpace(in.Namespace)
	for _, c := range in.Collections {
		ent, err := core.ParseEntity(c.Collection)
		if err != nil {
			s.badRequest(w, err.Error(), "")
			return
		}
		if ns == "" {
			ns = ent.Namespace
		}
		spec := core.NewTableSpec(ns, ent)
		enabled := true
		if c.Enabled != nil {
			enabled = *c.Enabled
		}
		cols = append(cols, store.PipelineCollection{Collection: c.Collection, Enabled: enabled, RawTable: spec.Raw, CleanTable: spec.Clean, SnapshotTotalEst: c.EstimatedCount})
	}
	ns = core.SanitizeTableName(ns)
	desired := store.DesiredStopped
	if in.Start {
		desired = store.DesiredRunning
	}
	p, err := s.st.CreatePipeline(r.Context(), &store.Pipeline{OwnerID: userFrom(r.Context()).ID, Name: in.Name, SourceID: in.SourceID, DestinationID: in.DestinationID, Namespace: ns, SyncIntervalS: in.SyncIntervalS, DesiredState: desired, Settings: in.Settings}, cols)
	if err != nil {
		s.internal(w, err)
		return
	}
	_ = s.st.Notify(r.Context(), store.Event{Type: "pipeline", PipelineID: p.ID})
	v, _ := s.pipelineView(r.Context(), p)
	s.writeJSON(w, 201, v)
}

func (s *Server) updatePipeline(w http.ResponseWriter, r *http.Request) {
	p, err := s.st.GetPipeline(r.Context(), r.PathValue("id"))
	if err != nil {
		s.notFound(w, "pipeline")
		return
	}
	var in pipelineInput
	if err := s.decode(r, &in); err != nil {
		s.badRequest(w, err.Error(), "")
		return
	}
	if in.Name = strings.TrimSpace(in.Name); in.Name == "" {
		in.Name = p.Name
	}
	if in.SyncIntervalS == 0 {
		in.SyncIntervalS = p.SyncIntervalS
	}
	if in.SyncIntervalS < s.cfg.MinSyncIntervalS {
		s.badRequest(w, fmt.Sprintf("The sync interval must be at least %d seconds.", s.cfg.MinSyncIntervalS), "")
		return
	}
	if in.Settings == nil {
		in.Settings = p.Settings
	}
	updated, err := s.st.UpdatePipeline(r.Context(), p.ID, in.Name, in.SyncIntervalS, in.Settings)
	if err != nil {
		s.internal(w, err)
		return
	}
	existing, _ := s.st.ListCollections(r.Context(), p.ID)
	known := map[string]bool{}
	for _, c := range existing {
		known[c.Collection] = true
	}
	for _, c := range in.Collections {
		if !known[c.Collection] {
			ent, err := core.ParseEntity(c.Collection)
			if err != nil {
				continue
			}
			spec := core.NewTableSpec(p.Namespace, ent)
			_ = s.st.AddCollection(r.Context(), store.PipelineCollection{PipelineID: p.ID, Collection: c.Collection, Enabled: true, RawTable: spec.Raw, CleanTable: spec.Clean, SnapshotTotalEst: c.EstimatedCount})
		} else if c.Enabled != nil {
			_ = s.st.SetCollectionEnabled(r.Context(), p.ID, c.Collection, *c.Enabled)
		}
	}
	_ = s.st.Notify(r.Context(), store.Event{Type: "pipeline", PipelineID: p.ID})
	v, _ := s.pipelineView(r.Context(), updated)
	s.writeJSON(w, 200, v)
}

func (s *Server) deletePipeline(w http.ResponseWriter, r *http.Request) {
	p, err := s.st.GetPipeline(r.Context(), r.PathValue("id"))
	if err != nil {
		s.notFound(w, "pipeline")
		return
	}
	drop := r.URL.Query().Get("drop_tables") == "true"
	// stop first so runners release the tables
	_ = s.st.SetPipelineDesiredState(r.Context(), p.ID, store.DesiredStopped)
	var dropErrors []string
	if drop {
		dc, err := s.st.GetConnection(r.Context(), p.DestinationID)
		if err == nil {
			d, err := conn.OpenDestination(r.Context(), s.sec, dc)
			if err == nil {
				defer d.Close()
				cols, _ := s.st.ListCollections(r.Context(), p.ID)
				ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
				defer cancel()
				for _, c := range cols {
					if err := d.DropTables(ctx, core.TableSpec{Namespace: p.Namespace, Raw: c.RawTable, Clean: c.CleanTable, Entity: c.Collection}); err != nil {
						dropErrors = append(dropErrors, c.Collection+": "+core.RedactURI(err.Error()))
					}
				}
			} else {
				dropErrors = append(dropErrors, core.RedactURI(err.Error()))
			}
		}
	}
	if err := s.st.DeletePipeline(r.Context(), p.ID); err != nil {
		s.internal(w, err)
		return
	}
	_ = s.st.Notify(r.Context(), store.Event{Type: "pipeline", PipelineID: p.ID, Data: []byte(`{"deleted":true}`)})
	s.writeJSON(w, 200, map[string]any{"ok": true, "tables_dropped": drop && len(dropErrors) == 0, "drop_errors": dropErrors})
}

func (s *Server) pipelineAction(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, err := s.st.GetPipeline(r.Context(), r.PathValue("id"))
		if err != nil {
			s.notFound(w, "pipeline")
			return
		}
		switch action {
		case "start":
			err = s.st.SetPipelineDesiredState(r.Context(), p.ID, store.DesiredRunning)
			if err == nil {
				err = s.st.RequestActionAll(r.Context(), p.ID, "retry")
			}
		case "pause":
			err = s.st.SetPipelineDesiredState(r.Context(), p.ID, store.DesiredPaused)
		case "stop":
			err = s.st.SetPipelineDesiredState(r.Context(), p.ID, store.DesiredStopped)
		case "resync":
			err = s.st.RequestActionAll(r.Context(), p.ID, "resync")
			if err == nil && p.DesiredState != store.DesiredRunning {
				err = s.st.SetPipelineDesiredState(r.Context(), p.ID, store.DesiredRunning)
			}
		case "retry":
			err = s.st.RequestActionAll(r.Context(), p.ID, "retry")
			if err == nil {
				err = s.st.SetPipelineDesiredState(r.Context(), p.ID, store.DesiredRunning)
			}
		}
		if err != nil {
			s.internal(w, err)
			return
		}
		// collections that have no running runner get their display state refreshed by the worker
		_ = s.st.Notify(r.Context(), store.Event{Type: "pipeline", PipelineID: p.ID})
		v, _ := s.pipelineView(r.Context(), p)
		s.writeJSON(w, 200, v)
	}
}

func (s *Server) collectionAction(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, err := s.st.GetPipeline(r.Context(), r.PathValue("id"))
		if err != nil {
			s.notFound(w, "pipeline")
			return
		}
		name := r.PathValue("name")
		if _, err := s.st.GetCollection(r.Context(), p.ID, name); err != nil {
			s.notFound(w, "collection")
			return
		}
		switch action {
		case "resync", "retry":
			err = s.st.RequestAction(r.Context(), p.ID, name, action)
			if err == nil && p.DesiredState != store.DesiredRunning {
				err = s.st.SetPipelineDesiredState(r.Context(), p.ID, store.DesiredRunning)
			}
		case "enable":
			err = s.st.SetCollectionEnabled(r.Context(), p.ID, name, true)
		case "disable":
			err = s.st.SetCollectionEnabled(r.Context(), p.ID, name, false)
		}
		if err != nil {
			s.internal(w, err)
			return
		}
		_ = s.st.Notify(r.Context(), store.Event{Type: "pipeline", PipelineID: p.ID})
		v, _ := s.pipelineView(r.Context(), p)
		s.writeJSON(w, 200, v)
	}
}

func (s *Server) runs(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	runs, err := s.st.ListRuns(r.Context(), r.PathValue("id"), limit)
	if err != nil {
		s.internal(w, err)
		return
	}
	if runs == nil {
		runs = []store.Run{}
	}
	s.writeJSON(w, 200, runs)
}

func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	before, _ := strconv.ParseInt(q.Get("before"), 10, 64)
	limit, _ := strconv.Atoi(q.Get("limit"))
	logs, err := s.st.ListLogs(r.Context(), r.PathValue("id"), store.LogFilter{Level: q.Get("level"), Collection: q.Get("collection"), Search: q.Get("q"), BeforeID: before, Limit: limit})
	if err != nil {
		s.internal(w, err)
		return
	}
	if logs == nil {
		logs = []store.LogEntry{}
	}
	s.writeJSON(w, 200, logs)
}

func (s *Server) series(w http.ResponseWriter, r *http.Request) {
	minutes, _ := strconv.Atoi(r.URL.Query().Get("minutes"))
	if minutes <= 0 || minutes > 24*60 {
		minutes = 60
	}
	pts, err := s.st.EventsSeries(r.Context(), r.PathValue("id"), r.URL.Query().Get("collection"), minutes)
	if err != nil {
		s.internal(w, err)
		return
	}
	if pts == nil {
		pts = []store.MinuteCount{}
	}
	s.writeJSON(w, 200, pts)
}

func (s *Server) mappings(w http.ResponseWriter, r *http.Request) {
	collection := r.URL.Query().Get("collection")
	m, err := s.st.ListFieldMappings(r.Context(), r.PathValue("id"), collection)
	if err != nil {
		s.internal(w, err)
		return
	}
	if m == nil {
		m = []store.FieldMapping{}
	}
	s.writeJSON(w, 200, m)
}

func (s *Server) defaultInterval(ctx context.Context) int {
	var v struct {
		DefaultSyncIntervalS int `json:"default_sync_interval_s"`
	}
	if ok, _ := s.st.GetSetting(ctx, "general", &v); ok && v.DefaultSyncIntervalS >= s.cfg.MinSyncIntervalS {
		return v.DefaultSyncIntervalS
	}
	return s.cfg.DefaultSyncIntervalS
}

// overview feeds the home screen.
func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	ps, err := s.st.ListPipelines(r.Context())
	if err != nil {
		s.internal(w, err)
		return
	}
	var views []*pipelineView
	var rows, events int64
	var maxLag float64
	hasLag := false
	attention := []map[string]string{}
	for _, p := range ps {
		v, err := s.pipelineView(r.Context(), p)
		if err != nil {
			continue
		}
		views = append(views, v)
		rows += v.Stats.RowsSynced
		events += v.Stats.EventsLastHour
		if v.Stats.LagSeconds != nil {
			hasLag = true
			if *v.Stats.LagSeconds > maxLag {
				maxLag = *v.Stats.LagSeconds
			}
		}
		for _, c := range v.Collections {
			if c.State == store.StateFailed || c.State == store.StateSourceRemoved {
				msg := c.State
				if c.StateMessage != nil {
					msg = *c.StateMessage
				}
				attention = append(attention, map[string]string{"pipeline_id": p.ID, "pipeline": p.Name, "collection": c.Collection, "table": p.Namespace + "." + c.CleanTable, "reason": msg, "state": c.State})
			}
			if m, _ := s.st.ListFieldMappings(r.Context(), p.ID, c.Collection); len(m) > 0 {
				var conflicts int64
				for _, f := range m {
					conflicts += f.ConflictCount
				}
				if conflicts > 0 {
					attention = append(attention, map[string]string{"pipeline_id": p.ID, "pipeline": p.Name, "collection": c.Collection, "table": p.Namespace + "." + c.CleanTable, "reason": fmt.Sprintf("%d type conflicts stored as null", conflicts), "state": "conflicts"})
				}
			}
		}
	}
	if views == nil {
		views = []*pipelineView{}
	}
	var lag *float64
	if hasLag {
		lag = &maxLag
	}
	ncon, _ := s.st.ListConnections(r.Context(), "")
	s.writeJSON(w, 200, map[string]any{
		"pipelines":   views,
		"connections": len(ncon),
		"totals":      map[string]any{"rows_synced": rows, "events_last_hour": events, "lag_seconds": lag, "attention": len(attention)},
		"attention":   attention,
		"first_run":   len(ncon) == 0 && len(ps) == 0,
	})
}

var _ = engine.AggregateState
var _ = errors.Is
