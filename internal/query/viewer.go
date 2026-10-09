package query

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"time"

	"lakesync/internal/core"
)

// ColumnMeta describes a result column.
type ColumnMeta struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// SortSpec orders the rows by one column.
type SortSpec struct {
	Column string `json:"column"`
	Desc   bool   `json:"desc"`
}

// Filter narrows the rows on one column.
type Filter struct {
	Column string `json:"column"`
	Op     string `json:"op"` // eq, neq, contains, starts, gt, gte, lt, lte, between, isnull, notnull, in
	Value  any    `json:"value,omitempty"`
	Value2 any    `json:"value2,omitempty"` // between upper bound
}

// RowsRequest is a viewer page request.
type RowsRequest struct {
	Alias, Namespace, Table string
	Columns                 []string // empty = all
	Sort                    *SortSpec
	Filters                 []Filter
	Search                  string
	Limit                   int
	Cursor                  string
	WithTotal               bool
	Timeout                 time.Duration
}

// RowsResult is a page of rows.
type RowsResult struct {
	Columns    []ColumnMeta `json:"columns"`
	Rows       [][]any      `json:"rows"`
	NextCursor string       `json:"next_cursor,omitempty"`
	HasMore    bool         `json:"has_more"`
	Total      *int64       `json:"total,omitempty"` // filtered count when requested
	DurationMs int64        `json:"duration_ms"`
}

type cursor struct {
	SortCol  string `json:"s,omitempty"`
	SortDesc bool   `json:"d,omitempty"`
	SortVal  any    `json:"v,omitempty"`
	SortNull bool   `json:"n,omitempty"`
	Key      string `json:"k"`
}

func encodeCursor(c cursor) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(s string) (cursor, error) {
	var c cursor
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return c, fmt.Errorf("bad cursor")
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("bad cursor")
	}
	return c, nil
}

// Columns returns the columns of a table as DuckDB sees them.
func (e *Engine) Columns(ctx context.Context, alias, ns, table string) ([]ColumnMeta, error) {
	rows, err := e.db.QueryContext(ctx, "DESCRIBE "+qualified(alias, ns, table))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ColumnMeta
	for rows.Next() {
		var name, typ string
		var null, key, def, extra sql.NullString
		if err := rows.Scan(&name, &typ, &null, &key, &def, &extra); err != nil {
			return nil, err
		}
		out = append(out, ColumnMeta{Name: name, Type: typ})
	}
	return out, rows.Err()
}

// Rows runs one viewer page with keyset pagination (sort column + _cdc_key).
func (e *Engine) Rows(ctx context.Context, req RowsRequest) (*RowsResult, error) {
	start := time.Now()
	if req.Limit <= 0 || req.Limit > 1000 {
		req.Limit = 100
	}
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}
	cols, err := e.Columns(ctx, req.Alias, req.Namespace, req.Table)
	if err != nil {
		return nil, err
	}
	colType := map[string]string{}
	for _, c := range cols {
		colType[c.Name] = c.Type
	}
	if _, ok := colType[core.ColKey]; !ok {
		return nil, fmt.Errorf("table %s.%s has no %s column; the viewer only reads lakesync tables", req.Namespace, req.Table, core.ColKey)
	}
	// projection
	selected := cols
	if len(req.Columns) > 0 {
		selected = nil
		seen := map[string]bool{}
		for _, c := range req.Columns {
			if _, ok := colType[c]; ok && !seen[c] {
				selected = append(selected, ColumnMeta{Name: c, Type: colType[c]})
				seen[c] = true
			}
		}
		if !seen[core.ColKey] {
			selected = append([]ColumnMeta{{Name: core.ColKey, Type: colType[core.ColKey]}}, selected...)
		}
	}
	sortCol, sortDesc := "", false
	if req.Sort != nil && req.Sort.Column != "" && req.Sort.Column != core.ColKey {
		if _, ok := colType[req.Sort.Column]; !ok {
			return nil, fmt.Errorf("unknown sort column %q", req.Sort.Column)
		}
		sortCol, sortDesc = req.Sort.Column, req.Sort.Desc
	}
	where, args, err := buildWhere(req.Filters, req.Search, cols, colType)
	if err != nil {
		return nil, err
	}
	// keyset predicate
	if req.Cursor != "" {
		c, err := decodeCursor(req.Cursor)
		if err != nil {
			return nil, err
		}
		if c.SortCol != sortCol || c.SortDesc != sortDesc {
			return nil, fmt.Errorf("cursor does not match the sort; restart from the first page")
		}
		if sortCol == "" {
			where = append(where, quoteIdent(core.ColKey)+" > ?")
			args = append(args, c.Key)
		} else if c.SortNull {
			where = append(where, fmt.Sprintf("(%s IS NULL AND %s > ?)", quoteIdent(sortCol), quoteIdent(core.ColKey)))
			args = append(args, c.Key)
		} else {
			op := ">"
			if sortDesc {
				op = "<"
			}
			sc := quoteIdent(sortCol)
			where = append(where, fmt.Sprintf("(%s %s ? OR (%s = ? AND %s > ?) OR %s IS NULL)", sc, op, sc, quoteIdent(core.ColKey), sc))
			args = append(args, c.SortVal, c.SortVal, c.Key)
		}
	}
	var sb strings.Builder
	sb.WriteString("SELECT ")
	for i, c := range selected {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString(quoteIdent(c.Name))
	}
	// always fetch the sort column for the cursor, even if not selected
	sortIdx := -1
	for i, c := range selected {
		if c.Name == sortCol {
			sortIdx = i
		}
	}
	if sortCol != "" && sortIdx == -1 {
		sb.WriteString(", " + quoteIdent(sortCol))
		sortIdx = len(selected)
	}
	keyIdx := 0
	for i, c := range selected {
		if c.Name == core.ColKey {
			keyIdx = i
		}
	}
	sb.WriteString(" FROM " + qualified(req.Alias, req.Namespace, req.Table))
	if len(where) > 0 {
		sb.WriteString(" WHERE " + strings.Join(where, " AND "))
	}
	sb.WriteString(" ORDER BY ")
	if sortCol != "" {
		dir := "ASC"
		if sortDesc {
			dir = "DESC"
		}
		sb.WriteString(quoteIdent(sortCol) + " " + dir + " NULLS LAST, ")
	}
	sb.WriteString(quoteIdent(core.ColKey) + " ASC")
	fmt.Fprintf(&sb, " LIMIT %d", req.Limit+1)

	rows, err := e.db.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, friendlyErr(err)
	}
	defer rows.Close()
	ncols := len(selected)
	if sortIdx >= ncols {
		ncols++
	}
	res := &RowsResult{Columns: selected}
	for rows.Next() {
		vals := make([]any, ncols)
		ptrs := make([]any, ncols)
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		if len(res.Rows) == req.Limit {
			res.HasMore = true
			break
		}
		out := make([]any, len(selected))
		for i := range selected {
			out[i] = normalizeValue(vals[i])
		}
		res.Rows = append(res.Rows, out)
		if len(res.Rows) == req.Limit {
			c := cursor{SortCol: sortCol, SortDesc: sortDesc, Key: fmt.Sprint(vals[keyIdx])}
			if sortCol != "" {
				if vals[sortIdx] == nil {
					c.SortNull = true
				} else {
					c.SortVal = normalizeValue(vals[sortIdx])
				}
			}
			res.NextCursor = encodeCursor(c)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, friendlyErr(err)
	}
	if !res.HasMore {
		res.NextCursor = ""
	}
	if req.WithTotal {
		cwhere, cargs, _ := buildWhere(req.Filters, req.Search, cols, colType)
		q := "SELECT count(*) FROM " + qualified(req.Alias, req.Namespace, req.Table)
		if len(cwhere) > 0 {
			q += " WHERE " + strings.Join(cwhere, " AND ")
		}
		var n int64
		if err := e.db.QueryRowContext(ctx, q, cargs...).Scan(&n); err == nil {
			res.Total = &n
		}
	}
	res.DurationMs = time.Since(start).Milliseconds()
	return res, nil
}

// buildWhere turns filters and the search box into SQL with placeholders.
func buildWhere(filters []Filter, search string, cols []ColumnMeta, colType map[string]string) ([]string, []any, error) {
	var where []string
	var args []any
	for _, f := range filters {
		t, ok := colType[f.Column]
		if !ok {
			return nil, nil, fmt.Errorf("unknown filter column %q", f.Column)
		}
		c := quoteIdent(f.Column)
		switch f.Op {
		case "eq":
			where = append(where, c+" = ?")
			args = append(args, castArg(t, f.Value))
		case "neq":
			where = append(where, "("+c+" <> ? OR "+c+" IS NULL)")
			args = append(args, castArg(t, f.Value))
		case "contains":
			where = append(where, "CAST("+c+" AS VARCHAR) ILIKE ?")
			args = append(args, "%"+escapeLike(fmt.Sprint(f.Value))+"%")
		case "starts":
			where = append(where, "CAST("+c+" AS VARCHAR) ILIKE ?")
			args = append(args, escapeLike(fmt.Sprint(f.Value))+"%")
		case "gt", "gte", "lt", "lte":
			op := map[string]string{"gt": ">", "gte": ">=", "lt": "<", "lte": "<="}[f.Op]
			where = append(where, c+" "+op+" ?")
			args = append(args, castArg(t, f.Value))
		case "between":
			where = append(where, c+" BETWEEN ? AND ?")
			args = append(args, castArg(t, f.Value), castArg(t, f.Value2))
		case "isnull":
			where = append(where, c+" IS NULL")
		case "notnull":
			where = append(where, c+" IS NOT NULL")
		case "in":
			list, ok := f.Value.([]any)
			if !ok || len(list) == 0 {
				return nil, nil, fmt.Errorf("filter 'in' on %q needs a list", f.Column)
			}
			ph := make([]string, len(list))
			for i, v := range list {
				ph[i] = "?"
				args = append(args, castArg(t, v))
			}
			where = append(where, c+" IN ("+strings.Join(ph, ",")+")")
		default:
			return nil, nil, fmt.Errorf("unknown filter op %q", f.Op)
		}
	}
	if s := strings.TrimSpace(search); s != "" {
		var ors []string
		for _, c := range cols {
			if strings.HasPrefix(c.Name, core.MetaPrefix) && c.Name != core.ColKey && c.Name != core.ColDoc {
				continue
			}
			if strings.EqualFold(c.Type, "BLOB") {
				continue
			}
			ors = append(ors, "CAST("+quoteIdent(c.Name)+" AS VARCHAR) ILIKE ?")
			args = append(args, "%"+escapeLike(s)+"%")
		}
		if len(ors) > 0 {
			where = append(where, "("+strings.Join(ors, " OR ")+")")
		}
	}
	return where, args, nil
}

func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// castArg converts a JSON-decoded filter value into something DuckDB can bind
// for the column type.
func castArg(colType string, v any) any {
	s, isStr := v.(string)
	t := strings.ToUpper(colType)
	switch {
	case strings.Contains(t, "INT"):
		if isStr {
			var n int64
			if _, err := fmt.Sscan(s, &n); err == nil {
				return n
			}
		}
		if f, ok := v.(float64); ok {
			return int64(f)
		}
	case strings.Contains(t, "DOUBLE") || strings.Contains(t, "FLOAT") || strings.Contains(t, "DECIMAL"):
		if isStr {
			var f float64
			if _, err := fmt.Sscan(s, &f); err == nil {
				return f
			}
		}
	case strings.Contains(t, "BOOL"):
		if isStr {
			return strings.EqualFold(s, "true")
		}
	case strings.Contains(t, "TIMESTAMP") || strings.Contains(t, "DATE"):
		if isStr {
			for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04", "2006-01-02"} {
				if ts, err := time.Parse(layout, s); err == nil {
					return ts
				}
			}
		}
	}
	return v
}

// normalizeValue converts DuckDB Go values into JSON-friendly ones.
func normalizeValue(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case []byte:
		return base64.StdEncoding.EncodeToString(x)
	case time.Time:
		return x.UTC().Format(time.RFC3339Nano)
	case *big.Int:
		return x.String()
	case big.Int:
		return x.String()
	case int8, int16, int32, int64, uint8, uint16, uint32, uint64, int:
		return x
	case float32:
		return float64(x)
	}
	return v
}

func friendlyErr(err error) error {
	msg := err.Error()
	if i := strings.Index(msg, "\n"); i > 0 {
		msg = msg[:i]
	}
	return fmt.Errorf("%s", msg)
}

// HistoryEvent is one raw event for a document, newest first.
type HistoryEvent struct {
	Seq        int64            `json:"seq"`
	Op         string           `json:"op"`
	Generation int64            `json:"generation"`
	SourceTS   *string          `json:"source_ts"`
	IngestedAt string           `json:"ingested_at"`
	CastErrors *string          `json:"cast_errors"`
	Doc        json.RawMessage  `json:"doc"` // full document, null on delete
	Changed    []string         `json:"changed"`
	Diff       map[string]Delta `json:"diff"` // field -> before/after (top-level)
}

// Delta is a field change between two versions.
type Delta struct {
	Before json.RawMessage `json:"before"`
	After  json.RawMessage `json:"after"`
}

// History returns every raw event for a key, newest first, with top-level diffs
// against the previous version.
func (e *Engine) History(ctx context.Context, alias, ns, rawTable, key string, limit int) ([]HistoryEvent, error) {
	if limit <= 0 || limit > 5000 {
		limit = 1000
	}
	q := fmt.Sprintf("SELECT %s, %s, %s, %s, %s, %s, %s FROM %s WHERE %s = ? ORDER BY %s DESC LIMIT %d",
		quoteIdent(core.ColSeq), quoteIdent(core.ColOp), quoteIdent(core.ColGeneration), quoteIdent(core.ColSourceTS), quoteIdent(core.ColIngestedAt), quoteIdent(core.ColCastErrors), quoteIdent(core.ColDoc),
		qualified(alias, ns, rawTable), quoteIdent(core.ColKey), quoteIdent(core.ColSeq), limit)
	rows, err := e.db.QueryContext(ctx, q, key)
	if err != nil {
		return nil, friendlyErr(err)
	}
	defer rows.Close()
	var out []HistoryEvent
	for rows.Next() {
		var h HistoryEvent
		var src, ing sql.NullTime
		var castErr, doc sql.NullString
		if err := rows.Scan(&h.Seq, &h.Op, &h.Generation, &src, &ing, &castErr, &doc); err != nil {
			return nil, err
		}
		if src.Valid {
			s := src.Time.UTC().Format(time.RFC3339Nano)
			h.SourceTS = &s
		}
		if ing.Valid {
			h.IngestedAt = ing.Time.UTC().Format(time.RFC3339Nano)
		}
		if castErr.Valid && castErr.String != "" {
			h.CastErrors = &castErr.String
		}
		if doc.Valid && doc.String != "" && json.Valid([]byte(doc.String)) {
			h.Doc = json.RawMessage(doc.String)
		} else {
			h.Doc = json.RawMessage("null")
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// diffs: compare each event with the next-older one
	for i := range out {
		var prev json.RawMessage
		if i+1 < len(out) {
			prev = out[i+1].Doc
		}
		out[i].Changed, out[i].Diff = diffDocs(prev, out[i].Doc)
	}
	return out, nil
}

// diffDocs compares two JSON documents at the top level.
func diffDocs(before, after json.RawMessage) ([]string, map[string]Delta) {
	var b, a map[string]json.RawMessage
	_ = json.Unmarshal(before, &b)
	_ = json.Unmarshal(after, &a)
	diff := map[string]Delta{}
	var changed []string
	keys := map[string]bool{}
	for k := range a {
		keys[k] = true
	}
	for k := range b {
		keys[k] = true
	}
	for k := range keys {
		bv, aok := b[k]
		av, ok := a[k]
		if aok == ok && string(normalizeJSON(bv)) == string(normalizeJSON(av)) {
			continue
		}
		changed = append(changed, k)
		diff[k] = Delta{Before: nullIfEmpty(bv), After: nullIfEmpty(av)}
	}
	sortStrings(changed)
	return changed, diff
}

func normalizeJSON(r json.RawMessage) json.RawMessage {
	if len(r) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(r, &v); err != nil {
		return r
	}
	b, _ := json.Marshal(v)
	return b
}

func nullIfEmpty(r json.RawMessage) json.RawMessage {
	if len(r) == 0 {
		return json.RawMessage("null")
	}
	return r
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
