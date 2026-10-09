package query

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"lakesync/internal/core"
)

// FieldDrift describes the types one source field has taken across documents.
type FieldDrift struct {
	Field     string      `json:"field"`
	Documents int64       `json:"documents"` // latest versions that have the field
	Types     []TypeShare `json:"types"`
	FirstSeen *time.Time  `json:"first_seen"`
	LastSeen  *time.Time  `json:"last_seen"`
	Conflicts []Conflict  `json:"conflicts"` // sample values that did not fit the column
}

// TypeShare is one type's share of a field's values.
type TypeShare struct {
	Type      string     `json:"type"`
	Count     int64      `json:"count"`
	Percent   float64    `json:"percent"`
	FirstSeen *time.Time `json:"first_seen"`
	LastSeen  *time.Time `json:"last_seen"`
	Sample    string     `json:"sample"`
}

// Conflict is one value that was stored as null because it did not fit.
type Conflict struct {
	Key   string `json:"key"`
	Seq   int64  `json:"seq"`
	Value string `json:"value"`
	At    string `json:"at"`
}

// SchemaDrift computes, from the raw table's _cdc_doc column, which types each
// top-level field has taken across the latest version of every document, with
// first/last seen times and samples of values that caused cast errors.
// Relaxed Extended JSON wrappers ({"$date": ...}, {"$numberDecimal": ...}, ...)
// are reported by their MongoDB meaning.
func (e *Engine) SchemaDrift(ctx context.Context, alias, ns, rawTable string, limit int) ([]FieldDrift, error) {
	if limit <= 0 {
		limit = 200
	}
	raw := qualified(alias, ns, rawTable)
	q := fmt.Sprintf(`
WITH latest_event AS (
  SELECT %[1]s AS key, %[2]s AS seq, %[3]s AS ts, %[4]s AS doc
  FROM %[5]s
  QUALIFY row_number() OVER (PARTITION BY %[1]s ORDER BY %[2]s DESC) = 1
),
latest AS (
  SELECT * FROM latest_event WHERE doc IS NOT NULL
),
fields AS (
  SELECT latest.key AS key, latest.seq AS seq, latest.ts AS ts, j.key AS field, j.value AS value,
    CASE json_type(j.value)
      WHEN 'OBJECT' THEN COALESCE(
        CASE WHEN json_extract(j.value, '$.$date') IS NOT NULL THEN 'date'
             WHEN json_extract(j.value, '$.$numberDecimal') IS NOT NULL THEN 'decimal'
             WHEN json_extract(j.value, '$.$oid') IS NOT NULL THEN 'objectId'
             WHEN json_extract(j.value, '$.$binary') IS NOT NULL THEN 'binary'
             WHEN json_extract(j.value, '$.$timestamp') IS NOT NULL THEN 'timestamp'
             WHEN json_extract(j.value, '$.$numberLong') IS NOT NULL THEN 'long'
             WHEN json_extract(j.value, '$.$regularExpression') IS NOT NULL THEN 'regex'
             WHEN json_extract(j.value, '$.$code') IS NOT NULL THEN 'javascript'
             WHEN json_extract(j.value, '$.$minKey') IS NOT NULL OR json_extract(j.value, '$.$maxKey') IS NOT NULL THEN 'minmaxkey'
        END, 'object')
      WHEN 'ARRAY' THEN 'array'
      WHEN 'VARCHAR' THEN 'string'
      WHEN 'BIGINT' THEN 'int'
      WHEN 'UBIGINT' THEN 'int'
      WHEN 'DOUBLE' THEN 'double'
      WHEN 'BOOLEAN' THEN 'bool'
      WHEN 'NULL' THEN 'null'
      ELSE lower(json_type(j.value))
    END AS t
  FROM latest, json_each(latest.doc) AS j
)
SELECT field, t, count(*) AS n, min(ts) AS first_seen, max(ts) AS last_seen, min(CAST(value AS VARCHAR)) AS sample
FROM fields
GROUP BY field, t
ORDER BY field, n DESC`, quoteIdent(core.ColKey), quoteIdent(core.ColSeq), quoteIdent(core.ColSourceTS), quoteIdent(core.ColDoc), raw)
	rows, err := e.db.QueryContext(ctx, q)
	if err != nil {
		return nil, friendlyErr(err)
	}
	defer rows.Close()
	byField := map[string]*FieldDrift{}
	var order []string
	for rows.Next() {
		var field, typ string
		var n int64
		var first, last *time.Time
		var sample *string
		if err := rows.Scan(&field, &typ, &n, &first, &last, &sample); err != nil {
			return nil, err
		}
		fd, ok := byField[field]
		if !ok {
			fd = &FieldDrift{Field: field}
			byField[field] = fd
			order = append(order, field)
		}
		fd.Documents += n
		ts := TypeShare{Type: typ, Count: n, FirstSeen: first, LastSeen: last}
		if sample != nil {
			ts.Sample = truncateSample(*sample, 80)
		}
		fd.Types = append(fd.Types, ts)
		if first != nil && (fd.FirstSeen == nil || first.Before(*fd.FirstSeen)) {
			fd.FirstSeen = first
		}
		if last != nil && (fd.LastSeen == nil || last.After(*fd.LastSeen)) {
			fd.LastSeen = last
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// conflict samples: events whose cast errors mention the field
	cq := fmt.Sprintf(`SELECT %s, %s, %s, %s FROM %s WHERE %s IS NOT NULL AND %s <> '' ORDER BY %s DESC LIMIT 500`,
		quoteIdent(core.ColKey), quoteIdent(core.ColSeq), quoteIdent(core.ColCastErrors), quoteIdent(core.ColDoc), raw, quoteIdent(core.ColCastErrors), quoteIdent(core.ColCastErrors), quoteIdent(core.ColSeq))
	crows, err := e.db.QueryContext(ctx, cq)
	if err == nil {
		defer crows.Close()
		for crows.Next() {
			var key, errs string
			var seq int64
			var doc *string
			if err := crows.Scan(&key, &seq, &errs, &doc); err != nil {
				break
			}
			var fields []string
			if json.Unmarshal([]byte(errs), &fields) != nil || doc == nil {
				continue
			}
			var m map[string]json.RawMessage
			_ = json.Unmarshal([]byte(*doc), &m)
			for _, f := range fields {
				fd, ok := byField[f]
				if !ok || len(fd.Conflicts) >= 5 {
					continue
				}
				val := "?"
				if v, ok := m[f]; ok {
					val = truncateSample(string(v), 80)
				}
				fd.Conflicts = append(fd.Conflicts, Conflict{Key: key, Seq: seq, Value: val})
			}
		}
	}
	out := make([]FieldDrift, 0, len(order))
	for _, f := range order {
		fd := byField[f]
		for i := range fd.Types {
			if fd.Documents > 0 {
				fd.Types[i].Percent = float64(fd.Types[i].Count) * 100 / float64(fd.Documents)
			}
		}
		sort.SliceStable(fd.Types, func(i, j int) bool { return fd.Types[i].Count > fd.Types[j].Count })
		if fd.Conflicts == nil {
			fd.Conflicts = []Conflict{}
		}
		out = append(out, *fd)
	}
	// fields with several types first, then by name
	sort.SliceStable(out, func(i, j int) bool {
		ti, tj := len(out[i].Types), len(out[j].Types)
		if ti != tj {
			return ti > tj
		}
		return out[i].Field < out[j].Field
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func truncateSample(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n-1] + "…"
	}
	return s
}
