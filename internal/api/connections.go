package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"lakesync/internal/conn"
	"lakesync/internal/core"
	"lakesync/internal/dest"
	"lakesync/internal/query"
	"lakesync/internal/source"
	"lakesync/internal/store"
)

// unchangedSecret is what the UI sends to keep a stored secret.
const unchangedSecret = "__unchanged__"

type typeInfo struct {
	Type        string          `json:"type"`
	Kind        string          `json:"kind"`
	Title       string          `json:"title"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
	CanParse    bool            `json:"can_parse"`
}

func (s *Server) types(w http.ResponseWriter, r *http.Request) {
	var sources, dests []typeInfo
	for _, d := range source.Types() {
		sources = append(sources, typeInfo{Type: d.Type, Kind: "source", Title: d.Title, Description: d.Description, Schema: d.Schema, CanParse: d.Parse != nil})
	}
	for _, d := range dest.Types() {
		dests = append(dests, typeInfo{Type: d.Type, Kind: "destination", Title: d.Title, Description: d.Description, Schema: d.Schema, CanParse: d.Parse != nil})
	}
	s.writeJSON(w, 200, map[string]any{"sources": sources, "destinations": dests})
}

func (s *Server) parseType(w http.ResponseWriter, r *http.Request) {
	typ := r.PathValue("type")
	var body struct {
		Text string `json:"text"`
	}
	if err := s.decode(r, &body); err != nil {
		s.badRequest(w, err.Error(), "")
		return
	}
	var parse func(string) (map[string]any, map[string]string, error)
	if d, err := source.Lookup(typ); err == nil {
		parse = d.Parse
	} else if d, err := dest.Lookup(typ); err == nil {
		parse = d.Parse
	}
	if parse == nil {
		s.badRequest(w, "This type cannot parse pasted text.", "")
		return
	}
	cfg, sec, err := parse(body.Text)
	if err != nil {
		s.badRequest(w, err.Error(), "Paste a full connection string, for example mongodb+srv://user:password@cluster0.example.mongodb.net/")
		return
	}
	s.writeJSON(w, 200, map[string]any{"config": cfg, "secrets": sec})
}

// secretFields returns the names of x-secret properties in a type's schema.
func secretFields(schema json.RawMessage) map[string]bool {
	var sc struct {
		Properties map[string]map[string]any `json:"properties"`
	}
	_ = json.Unmarshal(schema, &sc)
	out := map[string]bool{}
	for name, p := range sc.Properties {
		if v, ok := p["x-secret"].(bool); ok && v {
			out[name] = true
		}
	}
	return out
}

func lookupType(kind, typ string) (json.RawMessage, func(map[string]any) map[string]any, error) {
	switch kind {
	case "source":
		d, err := source.Lookup(typ)
		if err != nil {
			return nil, nil, err
		}
		return d.Schema, d.Redact, nil
	case "destination":
		d, err := dest.Lookup(typ)
		if err != nil {
			return nil, nil, err
		}
		return d.Schema, d.Redact, nil
	}
	return nil, nil, fmt.Errorf("kind must be source or destination")
}

type connectionView struct {
	*store.Connection
	SecretFields []string `json:"secret_fields"`
	InUse        int      `json:"in_use"`
}

func (s *Server) view(ctx context.Context, c *store.Connection) connectionView {
	v := connectionView{Connection: c, SecretFields: []string{}}
	if sec, err := conn.Secrets(s.sec, c); err == nil {
		for k, val := range sec {
			if val != "" {
				v.SecretFields = append(v.SecretFields, k)
			}
		}
		sort.Strings(v.SecretFields)
	}
	schema, redact, err := lookupType(c.Kind, c.Type)
	if err == nil {
		// never return anything secret-like even if it ended up in config
		for f := range secretFields(schema) {
			delete(c.Config, f)
		}
		if redact != nil {
			c.Config = redact(c.Config)
		}
	}
	if n, err := s.st.ConnectionInUse(ctx, c.ID); err == nil {
		v.InUse = n
	}
	return v
}

func (s *Server) listConnections(w http.ResponseWriter, r *http.Request) {
	cs, err := s.st.ListConnections(r.Context(), r.URL.Query().Get("kind"))
	if err != nil {
		s.internal(w, err)
		return
	}
	out := make([]connectionView, 0, len(cs))
	for _, c := range cs {
		out = append(out, s.view(r.Context(), c))
	}
	s.writeJSON(w, 200, out)
}

func (s *Server) getConnection(w http.ResponseWriter, r *http.Request) {
	c, err := s.st.GetConnection(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.notFound(w, "connection")
			return
		}
		s.internal(w, err)
		return
	}
	s.writeJSON(w, 200, s.view(r.Context(), c))
}

type connectionInput struct {
	Kind    string            `json:"kind"`
	Type    string            `json:"type"`
	Name    string            `json:"name"`
	Config  map[string]any    `json:"config"`
	Secrets map[string]string `json:"secrets"`
}

// prepare validates input, splits secrets from config and applies the endpoint policy.
func (s *Server) prepare(ctx context.Context, in *connectionInput, existing map[string]string) (map[string]any, map[string]string, error) {
	in.Name = strings.TrimSpace(in.Name)
	if in.Config == nil {
		in.Config = map[string]any{}
	}
	schema, _, err := lookupType(in.Kind, in.Type)
	if err != nil {
		return nil, nil, err
	}
	secretNames := secretFields(schema)
	secrets := map[string]string{}
	for k, v := range existing {
		secrets[k] = v
	}
	for k, v := range in.Secrets {
		if !secretNames[k] {
			continue
		}
		if v == unchangedSecret {
			continue
		}
		if v == "" {
			delete(secrets, k)
			continue
		}
		secrets[k] = v
	}
	// secrets accidentally sent inside config are moved out
	for k := range secretNames {
		if v, ok := in.Config[k]; ok {
			if str, ok := v.(string); ok && str != "" && str != unchangedSecret {
				secrets[k] = str
			}
			delete(in.Config, k)
		}
	}
	// outbound policy on user-supplied endpoints
	for _, key := range []string{"catalog_uri", "s3_endpoint", "oauth2_server_uri", "uri"} {
		if v, ok := in.Config[key].(string); ok && strings.TrimSpace(v) != "" {
			for _, host := range endpointHosts(v) {
				if err := s.checkEndpoint(ctx, host); err != nil {
					return nil, nil, err
				}
			}
		}
	}
	return in.Config, secrets, nil
}

// endpointHosts extracts host candidates from a URL or MongoDB URI (which may
// list several hosts).
func endpointHosts(v string) []string {
	if strings.HasPrefix(v, "mongodb://") || strings.HasPrefix(v, "mongodb+srv://") {
		rest := strings.TrimPrefix(strings.TrimPrefix(v, "mongodb+srv://"), "mongodb://")
		if i := strings.LastIndex(rest, "@"); i >= 0 {
			rest = rest[i+1:]
		}
		if i := strings.IndexAny(rest, "/?"); i >= 0 {
			rest = rest[:i]
		}
		var out []string
		for _, h := range strings.Split(rest, ",") {
			if h = strings.TrimSpace(h); h != "" {
				out = append(out, h)
			}
		}
		return out
	}
	return []string{v}
}

func (s *Server) createConnection(w http.ResponseWriter, r *http.Request) {
	var in connectionInput
	if err := s.decode(r, &in); err != nil {
		s.badRequest(w, err.Error(), "")
		return
	}
	cfg, sec, err := s.prepare(r.Context(), &in, nil)
	if err != nil {
		s.badRequest(w, err.Error(), "")
		return
	}
	if in.Name == "" {
		s.badRequest(w, "Name is required.", "")
		return
	}
	sealed, err := conn.SealSecrets(s.sec, sec)
	if err != nil {
		s.internal(w, err)
		return
	}
	c, err := s.st.CreateConnection(r.Context(), &store.Connection{OwnerID: userFrom(r.Context()).ID, Kind: in.Kind, Type: in.Type, Name: in.Name, Config: cfg, SecretCiphertext: sealed})
	if err != nil {
		s.internal(w, err)
		return
	}
	s.writeJSON(w, 201, s.view(r.Context(), c))
}

func (s *Server) updateConnection(w http.ResponseWriter, r *http.Request) {
	c, err := s.st.GetConnection(r.Context(), r.PathValue("id"))
	if err != nil {
		s.notFound(w, "connection")
		return
	}
	var in connectionInput
	if err := s.decode(r, &in); err != nil {
		s.badRequest(w, err.Error(), "")
		return
	}
	in.Kind, in.Type = c.Kind, c.Type
	existing, err := conn.Secrets(s.sec, c)
	if err != nil {
		s.internal(w, err)
		return
	}
	cfg, sec, err := s.prepare(r.Context(), &in, existing)
	if err != nil {
		s.badRequest(w, err.Error(), "")
		return
	}
	if in.Name == "" {
		in.Name = c.Name
	}
	sealed, err := conn.SealSecrets(s.sec, sec)
	if err != nil {
		s.internal(w, err)
		return
	}
	if sealed == nil {
		sealed = []byte{}
	}
	updated, err := s.st.UpdateConnection(r.Context(), c.ID, in.Name, cfg, sealed)
	if err != nil {
		s.internal(w, err)
		return
	}
	if c.Kind == "destination" {
		s.q.Detach(r.Context(), query.Alias(c.ID))
	}
	s.writeJSON(w, 200, s.view(r.Context(), updated))
}

func (s *Server) deleteConnection(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if n, err := s.st.ConnectionInUse(r.Context(), id); err == nil && n > 0 {
		s.writeError(w, http.StatusConflict, "in_use", fmt.Sprintf("This connection is used by %d pipeline(s).", n), "Delete those pipelines first.")
		return
	}
	if err := s.st.DeleteConnection(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.notFound(w, "connection")
			return
		}
		s.internal(w, err)
		return
	}
	s.q.Detach(r.Context(), query.Alias(id))
	s.writeJSON(w, 200, map[string]bool{"ok": true})
}

// runTest executes a connection test with the configured timeout and, for
// destinations, appends the DuckDB attach-and-read step.
func (s *Server) runTest(ctx context.Context, kind, typ string, cfg map[string]any, secrets map[string]string, connID string) core.TestReport {
	tctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	var report core.TestReport
	switch kind {
	case "source":
		src, err := source.Open(tctx, typ, cfg, secrets)
		if err != nil {
			report.Add(core.TestStep{Name: "Parse configuration", Status: core.TestFail, Message: err.Error(), Hint: "Check the form values."})
			return report
		}
		defer src.Close()
		report = src.Test(tctx)
	case "destination":
		d, err := dest.Open(tctx, typ, cfg, secrets)
		if err != nil {
			report.Add(core.TestStep{Name: "Parse configuration", Status: core.TestFail, Message: err.Error(), Hint: "Check the form values."})
			return report
		}
		defer d.Close()
		report = d.Test(tctx)
		if !report.Failed() {
			report.Add(s.duckdbStep(tctx, typ, cfg, secrets, connID))
		}
	}
	// substitute the egress IP placeholder in hints
	ip := s.egress.get()
	for i := range report.Steps {
		if ip == "" {
			report.Steps[i].Hint = strings.ReplaceAll(report.Steps[i].Hint, "{egress_ip}", "this server's public IP")
		} else {
			report.Steps[i].Hint = strings.ReplaceAll(report.Steps[i].Hint, "{egress_ip}", ip)
		}
	}
	return report
}

// duckdbStep attaches the catalog to DuckDB and lists its namespaces.
func (s *Server) duckdbStep(ctx context.Context, typ string, cfg map[string]any, secrets map[string]string, connID string) core.TestStep {
	start := time.Now()
	step := core.TestStep{Name: "DuckDB attaches the catalog and reads it"}
	c := &store.Connection{ID: connID, Kind: "destination", Type: typ, Config: cfg, Name: "test"}
	if connID == "" {
		c.ID = "draft-" + store.NewToken()[:8]
	}
	ccfg, err := conn.CatalogConfig(c, secrets)
	if err != nil {
		step.Status, step.Message, step.Hint = core.TestFail, err.Error(), "Only Iceberg REST destinations can be attached."
		step.Duration = time.Since(start)
		return step
	}
	alias := query.Alias(c.ID)
	if connID == "" {
		defer s.q.Detach(context.WithoutCancel(ctx), alias)
	}
	if err := s.q.Attach(ctx, alias, ccfg); err != nil {
		step.Status, step.Message, step.Hint = core.TestFail, core.RedactURI(err.Error()), "DuckDB could not attach the REST catalog. Check the catalog URI, authentication and that DuckDB can reach the storage endpoint."
		step.Duration = time.Since(start)
		return step
	}
	entries, err := s.q.Autocomplete(ctx, alias)
	if err != nil {
		step.Status, step.Message, step.Hint = core.TestFail, core.RedactURI(err.Error()), "DuckDB attached the catalog but could not list its tables."
	} else {
		step.Status = core.TestOK
		step.Message = fmt.Sprintf("DuckDB sees %d table(s) through the catalog", len(entries))
	}
	step.Duration = time.Since(start)
	return step
}

func (s *Server) testConnection(w http.ResponseWriter, r *http.Request) {
	c, err := s.st.GetConnection(r.Context(), r.PathValue("id"))
	if err != nil {
		s.notFound(w, "connection")
		return
	}
	sec, err := conn.Secrets(s.sec, c)
	if err != nil {
		s.internal(w, err)
		return
	}
	report := s.runTest(r.Context(), c.Kind, c.Type, c.Config, sec, c.ID)
	status := "ok"
	if report.Failed() {
		status = "fail"
	} else {
		for _, st := range report.Steps {
			if st.Status == core.TestWarn {
				status = "warn"
			}
		}
	}
	_ = s.st.SetConnectionTest(r.Context(), c.ID, status, report)
	s.writeJSON(w, 200, report)
}

func (s *Server) testDraftConnection(w http.ResponseWriter, r *http.Request) {
	var in connectionInput
	if err := s.decode(r, &in); err != nil {
		s.badRequest(w, err.Error(), "")
		return
	}
	var existing map[string]string
	if id := r.URL.Query().Get("id"); id != "" {
		if c, err := s.st.GetConnection(r.Context(), id); err == nil {
			existing, _ = conn.Secrets(s.sec, c)
			in.Kind, in.Type = c.Kind, c.Type
		}
	}
	cfg, sec, err := s.prepare(r.Context(), &in, existing)
	if err != nil {
		s.badRequest(w, err.Error(), "")
		return
	}
	s.writeJSON(w, 200, s.runTest(r.Context(), in.Kind, in.Type, cfg, sec, ""))
}

func (s *Server) entities(w http.ResponseWriter, r *http.Request) {
	c, err := s.st.GetConnection(r.Context(), r.PathValue("id"))
	if err != nil {
		s.notFound(w, "connection")
		return
	}
	src, err := conn.OpenSource(r.Context(), s.sec, c)
	if err != nil {
		s.badRequest(w, core.RedactURI(err.Error()), "")
		return
	}
	defer src.Close()
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	ents, err := src.ListEntities(ctx)
	if err != nil {
		s.writeError(w, http.StatusBadGateway, "source_error", core.RedactURI(err.Error()), "Run the connection test for a step-by-step diagnosis.")
		return
	}
	if ents == nil {
		ents = []core.Entity{}
	}
	s.writeJSON(w, 200, ents)
}
