package api

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"lakesync/internal/conn"
	"lakesync/internal/core"
	"lakesync/internal/query"
)

func (s *Server) runQuery(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DestinationID string `json:"destination_id"`
		SQL           string `json:"sql"`
	}
	if err := s.decode(r, &body); err != nil {
		s.badRequest(w, err.Error(), "")
		return
	}
	body.SQL = strings.TrimSpace(body.SQL)
	if body.SQL == "" {
		s.badRequest(w, "Write a SELECT statement.", "")
		return
	}
	alias := ""
	var destID *string
	if body.DestinationID != "" {
		dc, err := s.st.GetConnection(r.Context(), body.DestinationID)
		if err != nil {
			s.notFound(w, "destination")
			return
		}
		alias, err = conn.Attach(r.Context(), s.q, s.sec, dc)
		if err != nil {
			s.writeError(w, http.StatusBadGateway, "destination_error", core.RedactURI(err.Error()), "Run the destination's connection test.")
			return
		}
		destID = &dc.ID
	}
	start := time.Now()
	res, err := s.q.Query(r.Context(), alias, body.SQL, s.cfg.QueryRowLimit, s.cfg.QueryTimeout)
	user := userFrom(r.Context())
	if err != nil {
		_ = s.st.AddQueryHistory(r.Context(), user.ID, destID, body.SQL, int(time.Since(start).Milliseconds()), 0, err.Error())
		code, hint := "query_error", "Check the statement. Only SELECT and EXPLAIN are allowed."
		if err == query.ErrNotReadOnly {
			code = "read_only"
			hint = "The console is read-only; writes happen through pipelines."
		}
		s.writeError(w, http.StatusBadRequest, code, core.RedactURI(err.Error()), hint)
		return
	}
	_ = s.st.AddQueryHistory(r.Context(), user.ID, destID, body.SQL, int(res.DurationMs), res.RowCount, "")
	if res.Rows == nil {
		res.Rows = [][]any{}
	}
	s.writeJSON(w, 200, map[string]any{"result": res, "row_limit": s.cfg.QueryRowLimit, "read_only": true})
}

func (s *Server) querySchema(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("destination_id")
	if id == "" {
		s.badRequest(w, "destination_id is required", "")
		return
	}
	dc, err := s.st.GetConnection(r.Context(), id)
	if err != nil {
		s.notFound(w, "destination")
		return
	}
	alias, err := conn.Attach(r.Context(), s.q, s.sec, dc)
	if err != nil {
		s.writeError(w, http.StatusBadGateway, "destination_error", core.RedactURI(err.Error()), "")
		return
	}
	entries, err := s.q.Autocomplete(r.Context(), alias)
	if err != nil {
		s.writeError(w, http.StatusBadGateway, "destination_error", core.RedactURI(err.Error()), "")
		return
	}
	if entries == nil {
		entries = []query.SchemaEntry{}
	}
	s.writeJSON(w, 200, map[string]any{"catalog": alias, "tables": entries})
}

func (s *Server) queryHistory(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	h, err := s.st.ListQueryHistory(r.Context(), userFrom(r.Context()).ID, limit)
	if err != nil {
		s.internal(w, err)
		return
	}
	if h == nil {
		h = nil
	}
	s.writeJSON(w, 200, h)
}
