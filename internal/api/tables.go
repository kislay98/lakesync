package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"lakesync/internal/conn"
	"lakesync/internal/core"
	"lakesync/internal/core/engine"
	"lakesync/internal/dest"
	"lakesync/internal/query"
	"lakesync/internal/store"
)

type tableView struct {
	DestinationID   string    `json:"destination_id"`
	DestinationName string    `json:"destination_name"`
	Namespace       string    `json:"namespace"`
	Name            string    `json:"name"`
	Kind            string    `json:"kind"` // raw | clean | other
	PipelineID      string    `json:"pipeline_id,omitempty"`
	PipelineName    string    `json:"pipeline_name,omitempty"`
	Collection      string    `json:"collection,omitempty"`
	State           string    `json:"state,omitempty"`
	RowCount        int64     `json:"row_count"`
	SizeBytes       int64     `json:"size_bytes"`
	LastUpdated     time.Time `json:"last_updated"`
	Snapshots       int       `json:"snapshots"`
	Health          string    `json:"health"` // healthy | attention | unhealthy | unknown
}

type cachedTables struct {
	at     time.Time
	tables []tableView
	err    error
}

// destinationFor resolves which destination a namespace.table belongs to.
func (s *Server) destinationFor(ctx context.Context, r *http.Request, ns, table string) (*store.Connection, *store.Pipeline, *store.PipelineCollection, error) {
	if id := r.URL.Query().Get("destination"); id != "" {
		c, err := s.st.GetConnection(ctx, id)
		if err != nil {
			return nil, nil, nil, err
		}
		if p, pc := s.pipelineForTable(ctx, c.ID, ns, table); p != nil {
			return c, p, pc, nil
		}
		return c, nil, nil, nil
	}
	ps, _ := s.st.ListPipelines(ctx)
	for _, p := range ps {
		if p.Namespace != ns {
			continue
		}
		if pc, err := s.st.FindCollectionByTable(ctx, p.DestinationID, ns, table); err == nil {
			c, err := s.st.GetConnection(ctx, p.DestinationID)
			return c, p, pc, err
		}
	}
	dests, _ := s.st.ListConnections(ctx, "destination")
	if len(dests) == 1 {
		return dests[0], nil, nil, nil
	}
	return nil, nil, nil, fmt.Errorf("cannot tell which destination holds %s.%s; pass ?destination=<id>", ns, table)
}

func (s *Server) pipelineForTable(ctx context.Context, destID, ns, table string) (*store.Pipeline, *store.PipelineCollection) {
	pc, err := s.st.FindCollectionByTable(ctx, destID, ns, table)
	if err != nil {
		return nil, nil
	}
	p, err := s.st.GetPipeline(ctx, pc.PipelineID)
	if err != nil {
		return nil, nil
	}
	return p, pc
}

func (s *Server) openInspector(ctx context.Context, c *store.Connection) (dest.Destination, dest.Inspector, error) {
	d, err := conn.OpenDestination(ctx, s.sec, c)
	if err != nil {
		return nil, nil, err
	}
	insp, ok := d.(dest.Inspector)
	if !ok {
		d.Close()
		return nil, nil, errors.New("this destination cannot be inspected")
	}
	return d, insp, nil
}

func healthVerdict(h dest.Health, cfg dest.MaintenanceConfig) (string, string) {
	var reasons []string
	status := "healthy"
	if h.SmallFiles > cfg.SmallFilesTrigger {
		reasons = append(reasons, fmt.Sprintf("%d small files (under %d MB)", h.SmallFiles, cfg.SmallFileBytes>>20))
		status = "attention"
	}
	if h.DeleteFiles > cfg.DeleteFilesTrigger {
		reasons = append(reasons, fmt.Sprintf("%d delete files slow down reads", h.DeleteFiles))
		status = "attention"
	}
	if h.SmallFiles > cfg.SmallFilesTrigger*4 || h.DeleteFiles > cfg.DeleteFilesTrigger*4 {
		status = "unhealthy"
	}
	if len(reasons) == 0 {
		return status, "Files and delete files are within the maintenance thresholds."
	}
	return status, strings.Join(reasons, "; ") + ". Run maintenance to compact."
}

func (s *Server) listTables(w http.ResponseWriter, r *http.Request) {
	refresh := r.URL.Query().Get("refresh") == "true"
	s.tableCacheMu.Lock()
	cached, ok := s.tableCache["all"]
	s.tableCacheMu.Unlock()
	if ok && !refresh && time.Since(cached.at) < 15*time.Second {
		s.writeJSON(w, 200, cached.tables)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	dests, err := s.st.ListConnections(ctx, "destination")
	if err != nil {
		s.internal(w, err)
		return
	}
	cols, _ := s.st.ListAllCollections(ctx)
	pipelines, _ := s.st.ListPipelines(ctx)
	byPipeline := map[string]*store.Pipeline{}
	for _, p := range pipelines {
		byPipeline[p.ID] = p
	}
	type key struct{ dest, ns, table string }
	owner := map[key]struct {
		p *store.Pipeline
		c *store.PipelineCollection
	}{}
	for _, c := range cols {
		p := byPipeline[c.PipelineID]
		if p == nil {
			continue
		}
		owner[key{p.DestinationID, p.Namespace, c.RawTable}] = struct {
			p *store.Pipeline
			c *store.PipelineCollection
		}{p, c}
		owner[key{p.DestinationID, p.Namespace, c.CleanTable}] = struct {
			p *store.Pipeline
			c *store.PipelineCollection
		}{p, c}
	}
	var out []tableView
	var errs []string
	for _, dc := range dests {
		d, insp, err := s.openInspector(ctx, dc)
		if err != nil {
			errs = append(errs, dc.Name+": "+core.RedactURI(err.Error()))
			continue
		}
		nss, err := insp.ListNamespaces(ctx)
		if err != nil {
			errs = append(errs, dc.Name+": "+core.RedactURI(err.Error()))
			d.Close()
			continue
		}
		for _, ns := range nss {
			tables, err := insp.ListTables(ctx, ns)
			if err != nil {
				continue
			}
			for _, t := range tables {
				v := tableView{DestinationID: dc.ID, DestinationName: dc.Name, Namespace: ns, Name: t.Name, Kind: "other", RowCount: t.RowCount, SizeBytes: t.SizeBytes, LastUpdated: t.LastUpdated, Snapshots: t.Snapshots, Health: "unknown"}
				if o, ok := owner[key{dc.ID, ns, t.Name}]; ok {
					v.PipelineID, v.PipelineName, v.Collection, v.State = o.p.ID, o.p.Name, o.c.Collection, o.c.State
					if t.Name == o.c.RawTable {
						v.Kind = "raw"
					} else {
						v.Kind = "clean"
					}
				} else if strings.HasSuffix(t.Name, core.RawSuffix) {
					v.Kind = "raw"
				}
				out = append(out, v)
			}
		}
		d.Close()
	}
	if out == nil {
		out = []tableView{}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	s.tableCacheMu.Lock()
	s.tableCache["all"] = cachedTables{at: time.Now(), tables: out}
	s.tableCacheMu.Unlock()
	if len(out) == 0 && len(errs) > 0 {
		s.writeError(w, http.StatusBadGateway, "destination_error", strings.Join(errs, "; "), "Check the destination connection test.")
		return
	}
	s.writeJSON(w, 200, out)
}

func (s *Server) tableSchema(w http.ResponseWriter, r *http.Request) {
	ns, table := r.PathValue("ns"), r.PathValue("table")
	dc, p, pc, err := s.destinationFor(r.Context(), r, ns, table)
	if err != nil {
		s.badRequest(w, err.Error(), "")
		return
	}
	d, insp, err := s.openInspector(r.Context(), dc)
	if err != nil {
		s.internal(w, err)
		return
	}
	defer d.Close()
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	cols, err := insp.TableColumns(ctx, ns, table)
	if err != nil {
		s.writeError(w, http.StatusBadGateway, "destination_error", core.RedactURI(err.Error()), "")
		return
	}
	var mappings []store.FieldMapping
	if p != nil && pc != nil {
		mappings, _ = s.st.ListFieldMappings(ctx, p.ID, pc.Collection)
	}
	if mappings == nil {
		mappings = []store.FieldMapping{}
	}
	conflicts := map[string]int64{}
	sourceField := map[string]string{}
	for _, m := range mappings {
		conflicts[m.ColumnName] = m.ConflictCount
		sourceField[m.ColumnName] = m.SourceField
	}
	type colView struct {
		dest.ColumnInfo
		SourceField string `json:"source_field,omitempty"`
		Conflicts   int64  `json:"conflicts"`
		Meta        bool   `json:"meta"`
	}
	var out []colView
	for _, c := range cols {
		out = append(out, colView{ColumnInfo: c, SourceField: sourceField[c.Name], Conflicts: conflicts[c.Name], Meta: core.IsMetaColumn(c.Name)})
	}
	resp := map[string]any{"columns": out, "mappings": mappings, "namespace": ns, "table": table, "destination_id": dc.ID}
	if p != nil && pc != nil {
		resp["pipeline"] = map[string]any{"id": p.ID, "name": p.Name, "collection": pc.Collection, "raw_table": pc.RawTable, "clean_table": pc.CleanTable, "state": pc.State}
	}
	s.writeJSON(w, 200, resp)
}

func (s *Server) tableRows(w http.ResponseWriter, r *http.Request) {
	ns, table := r.PathValue("ns"), r.PathValue("table")
	dc, _, _, err := s.destinationFor(r.Context(), r, ns, table)
	if err != nil {
		s.badRequest(w, err.Error(), "")
		return
	}
	alias, err := conn.Attach(r.Context(), s.q, s.sec, dc)
	if err != nil {
		s.writeError(w, http.StatusBadGateway, "destination_error", core.RedactURI(err.Error()), "Run the destination's connection test.")
		return
	}
	q := r.URL.Query()
	req := query.RowsRequest{Alias: alias, Namespace: ns, Table: table, Cursor: q.Get("cursor"), Search: q.Get("q"), WithTotal: q.Get("total") != "false", Timeout: s.cfg.QueryTimeout}
	req.Limit, _ = strconv.Atoi(q.Get("limit"))
	if c := q.Get("columns"); c != "" {
		req.Columns = strings.Split(c, ",")
	}
	if sortSpec := q.Get("sort"); sortSpec != "" {
		desc := strings.HasPrefix(sortSpec, "-")
		req.Sort = &query.SortSpec{Column: strings.TrimPrefix(sortSpec, "-"), Desc: desc}
	}
	if f := q.Get("filter"); f != "" {
		raw, err := url.QueryUnescape(f)
		if err != nil {
			raw = f
		}
		if err := json.Unmarshal([]byte(raw), &req.Filters); err != nil {
			s.badRequest(w, "filter must be a JSON array of {column, op, value}", "")
			return
		}
	}
	res, err := s.q.Rows(r.Context(), req)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "query_error", core.RedactURI(err.Error()), "Adjust the filters or sort and try again.")
		return
	}
	if res.Rows == nil {
		res.Rows = [][]any{}
	}
	s.writeJSON(w, 200, res)
}

func (s *Server) tableHealth(w http.ResponseWriter, r *http.Request) {
	ns, table := r.PathValue("ns"), r.PathValue("table")
	dc, p, pc, err := s.destinationFor(r.Context(), r, ns, table)
	if err != nil {
		s.badRequest(w, err.Error(), "")
		return
	}
	d, insp, err := s.openInspector(r.Context(), dc)
	if err != nil {
		s.internal(w, err)
		return
	}
	defer d.Close()
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	cfg := engine.LoadMaintenanceSettings(ctx, s.st).Config()
	h, err := insp.TableHealth(ctx, ns, table, cfg)
	if err != nil {
		s.writeError(w, http.StatusBadGateway, "destination_error", core.RedactURI(err.Error()), "")
		return
	}
	status, reason := healthVerdict(h, cfg)
	resp := map[string]any{"health": h, "status": status, "reason": reason, "thresholds": cfg, "destination_id": dc.ID, "namespace": ns, "table": table}
	if p != nil && pc != nil {
		mappings, _ := s.st.ListFieldMappings(ctx, p.ID, pc.Collection)
		if mappings == nil {
			mappings = []store.FieldMapping{}
		}
		resp["mappings"] = mappings
		resp["pipeline"] = map[string]any{"id": p.ID, "name": p.Name, "collection": pc.Collection, "state": pc.State, "raw_table": pc.RawTable, "clean_table": pc.CleanTable}
		var conflicts int64
		for _, m := range mappings {
			conflicts += m.ConflictCount
		}
		resp["conflicts"] = conflicts
		if pc.State == store.StateFailed || pc.State == store.StateSourceRemoved {
			resp["status"] = "unhealthy"
			resp["reason"] = fmt.Sprintf("The pipeline collection is in state %s. %s", pc.State, reason)
		}
	}
	// maintenance history: last maintenance runs for this collection
	if p != nil && pc != nil {
		runs, _ := s.st.ListRuns(ctx, p.ID, 200)
		var maint []store.Run
		for _, run := range runs {
			if run.Kind == "maintenance" && run.Collection == pc.Collection {
				maint = append(maint, run)
				if len(maint) == 10 {
					break
				}
			}
		}
		if maint == nil {
			maint = []store.Run{}
		}
		resp["maintenance_runs"] = maint
	}
	s.writeJSON(w, 200, resp)
}

func (s *Server) tableSnapshots(w http.ResponseWriter, r *http.Request) {
	ns, table := r.PathValue("ns"), r.PathValue("table")
	dc, _, _, err := s.destinationFor(r.Context(), r, ns, table)
	if err != nil {
		s.badRequest(w, err.Error(), "")
		return
	}
	d, insp, err := s.openInspector(r.Context(), dc)
	if err != nil {
		s.internal(w, err)
		return
	}
	defer d.Close()
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	snaps, err := insp.Snapshots(ctx, ns, table)
	if err != nil {
		s.writeError(w, http.StatusBadGateway, "destination_error", core.RedactURI(err.Error()), "")
		return
	}
	if snaps == nil {
		snaps = []dest.SnapshotInfo{}
	}
	s.writeJSON(w, 200, snaps)
}

func (s *Server) keyHistory(w http.ResponseWriter, r *http.Request) {
	ns, table := r.PathValue("ns"), r.PathValue("table")
	key, err := url.PathUnescape(r.PathValue("key"))
	if err != nil {
		key = r.PathValue("key")
	}
	dc, _, pc, err := s.destinationFor(r.Context(), r, ns, table)
	if err != nil {
		s.badRequest(w, err.Error(), "")
		return
	}
	raw := table
	if pc != nil {
		raw = pc.RawTable
	} else if !strings.HasSuffix(table, core.RawSuffix) {
		raw = table + core.RawSuffix
	}
	alias, err := conn.Attach(r.Context(), s.q, s.sec, dc)
	if err != nil {
		s.writeError(w, http.StatusBadGateway, "destination_error", core.RedactURI(err.Error()), "")
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.QueryTimeout)
	defer cancel()
	events, err := s.q.History(ctx, alias, ns, raw, key, limit)
	if err != nil {
		s.writeError(w, http.StatusBadGateway, "query_error", core.RedactURI(err.Error()), "")
		return
	}
	if events == nil {
		events = []query.HistoryEvent{}
	}
	s.writeJSON(w, 200, map[string]any{"key": key, "raw_table": raw, "events": events})
}

// runMaintenance runs every job now. When a runner owns the collection the
// request is queued for it; otherwise the API takes the lock and runs directly.
func (s *Server) runMaintenance(w http.ResponseWriter, r *http.Request) {
	ns, table := r.PathValue("ns"), r.PathValue("table")
	dc, p, pc, err := s.destinationFor(r.Context(), r, ns, table)
	if err != nil {
		s.badRequest(w, err.Error(), "")
		return
	}
	if p == nil || pc == nil {
		s.badRequest(w, "Maintenance runs on tables managed by a pipeline.", "")
		return
	}
	lock, err := s.st.TryLock(r.Context(), p.ID, pc.Collection)
	if err != nil {
		s.internal(w, err)
		return
	}
	if lock == nil {
		if err := s.st.RequestAction(r.Context(), p.ID, pc.Collection, "maintenance"); err != nil {
			s.internal(w, err)
			return
		}
		s.writeJSON(w, 202, map[string]any{"queued": true, "message": "Maintenance was queued; the worker runs it after the current batch."})
		return
	}
	defer lock.Unlock(context.WithoutCancel(r.Context()))
	d, err := conn.OpenDestination(r.Context(), s.sec, dc)
	if err != nil {
		s.internal(w, err)
		return
	}
	defer d.Close()
	m, ok := d.(dest.Maintainer)
	if !ok {
		s.badRequest(w, "This destination does not support maintenance.", "")
		return
	}
	spec := core.TableSpec{Namespace: p.Namespace, Raw: pc.RawTable, Clean: pc.CleanTable, Entity: pc.Collection}
	cp, _ := d.LastCheckpoint(r.Context(), spec)
	cfg := engine.LoadMaintenanceSettings(r.Context(), s.st).Config()
	deps := engine.Deps{Store: s.st, Log: s.log, Metrics: engine.NewMetrics(nil)}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Minute)
	defer cancel()
	var results []dest.MaintenanceResult
	var errs []string
	for _, j := range []struct {
		table string
		kind  dest.MaintenanceKind
	}{{spec.Raw, dest.MaintCompactRaw}, {spec.Clean, dest.MaintRewriteClean}, {spec.Raw, dest.MaintExpireSnapshots}, {spec.Clean, dest.MaintExpireSnapshots}, {spec.Raw, dest.MaintOrphanCleanup}, {spec.Clean, dest.MaintOrphanCleanup}} {
		res, err := engine.RunMaintenanceJob(ctx, deps, m, p.ID, pc.Collection, spec.Namespace, j.table, j.kind, cfg, cp, true)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s on %s: %s", j.kind, j.table, core.RedactURI(err.Error())))
			continue
		}
		results = append(results, res)
	}
	s.tableCacheMu.Lock()
	delete(s.tableCache, "all")
	s.tableCacheMu.Unlock()
	s.writeJSON(w, 200, map[string]any{"queued": false, "results": results, "errors": errs})
}
