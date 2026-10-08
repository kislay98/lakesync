// Package verify compares MongoDB collections with their clean Iceberg tables
// (brief 13.2): same set of keys and every document equal field by field after
// the same flattening rules. It is what the end-to-end tests assert with.
package verify

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"

	"lakesync/internal/conn"
	"lakesync/internal/core"
	"lakesync/internal/query"
	"lakesync/internal/secrets"
	"lakesync/internal/source"
	"lakesync/internal/store"
)

// Options select what to verify.
type Options struct {
	Store      *store.Store
	Secrets    secrets.SecretStore
	Query      *query.Engine
	Pipeline   string // name or id; "" = all
	Collection string // "" = all
	Show       int
	JSON       bool
	Out        io.Writer
}

// Mismatch is one difference.
type Mismatch struct {
	Key    string `json:"key"`
	Kind   string `json:"kind"` // missing_in_table, extra_in_table, value
	Column string `json:"column,omitempty"`
	Source string `json:"source,omitempty"`
	Table  string `json:"table,omitempty"`
}

// Report is the result for one collection.
type Report struct {
	Pipeline      string     `json:"pipeline"`
	Collection    string     `json:"collection"`
	Table         string     `json:"table"`
	SourceDocs    int64      `json:"source_docs"`
	TableRows     int64      `json:"table_rows"`
	DuplicateKeys int64      `json:"duplicate_keys"`
	Mismatches    int64      `json:"mismatches"`
	Sample        []Mismatch `json:"sample"`
	DurationMs    int64      `json:"duration_ms"`
	Error         string     `json:"error,omitempty"`
}

// OK reports whether the collection matches exactly.
func (r Report) OK() bool { return r.Error == "" && r.Mismatches == 0 && r.DuplicateKeys == 0 }

// Run verifies and prints; it returns false when any difference exists.
func Run(ctx context.Context, o Options) (bool, error) {
	if o.Show <= 0 {
		o.Show = 20
	}
	pipelines, err := o.Store.ListPipelines(ctx)
	if err != nil {
		return false, err
	}
	var selected []*store.Pipeline
	for _, p := range pipelines {
		if o.Pipeline == "" || p.ID == o.Pipeline || strings.EqualFold(p.Name, o.Pipeline) {
			selected = append(selected, p)
		}
	}
	if len(selected) == 0 {
		return false, fmt.Errorf("no pipeline matches %q", o.Pipeline)
	}
	var reports []Report
	allOK := true
	for _, p := range selected {
		cols, err := o.Store.ListCollections(ctx, p.ID)
		if err != nil {
			return false, err
		}
		for _, c := range cols {
			if !c.Enabled || (o.Collection != "" && c.Collection != o.Collection) {
				continue
			}
			rep := VerifyCollection(ctx, o, p, c)
			reports = append(reports, rep)
			if !rep.OK() {
				allOK = false
			}
			if !o.JSON {
				printReport(o.Out, rep, o.Show)
			}
		}
	}
	if o.JSON {
		enc := json.NewEncoder(o.Out)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{"ok": allOK, "collections": reports})
	} else if allOK {
		fmt.Fprintf(o.Out, "\nOK: %d collection(s) match exactly, zero duplicate keys.\n", len(reports))
	} else {
		fmt.Fprintf(o.Out, "\nFAILED: differences found.\n")
	}
	return allOK, nil
}

func printReport(w io.Writer, r Report, show int) {
	status := "OK  "
	if !r.OK() {
		status = "FAIL"
	}
	fmt.Fprintf(w, "%s %s / %s -> %s: source=%d table=%d duplicates=%d mismatches=%d (%dms)\n", status, r.Pipeline, r.Collection, r.Table, r.SourceDocs, r.TableRows, r.DuplicateKeys, r.Mismatches, r.DurationMs)
	if r.Error != "" {
		fmt.Fprintf(w, "     error: %s\n", r.Error)
	}
	for i, m := range r.Sample {
		if i >= show {
			break
		}
		switch m.Kind {
		case "value":
			fmt.Fprintf(w, "     %s %s: source=%s table=%s\n", m.Key, m.Column, m.Source, m.Table)
		default:
			fmt.Fprintf(w, "     %s %s\n", m.Key, m.Kind)
		}
	}
}

// VerifyCollection compares one collection.
func VerifyCollection(ctx context.Context, o Options, p *store.Pipeline, c *store.PipelineCollection) Report {
	start := time.Now()
	rep := Report{Pipeline: p.Name, Collection: c.Collection, Table: p.Namespace + "." + c.CleanTable}
	fail := func(err error) Report {
		rep.Error = core.RedactURI(err.Error())
		rep.DurationMs = time.Since(start).Milliseconds()
		return rep
	}
	srcConn, err := o.Store.GetConnection(ctx, p.SourceID)
	if err != nil {
		return fail(err)
	}
	dstConn, err := o.Store.GetConnection(ctx, p.DestinationID)
	if err != nil {
		return fail(err)
	}
	src, err := conn.OpenSource(ctx, o.Secrets, srcConn)
	if err != nil {
		return fail(err)
	}
	defer src.Close()
	dst, err := conn.OpenDestination(ctx, o.Secrets, dstConn)
	if err != nil {
		return fail(err)
	}
	defer dst.Close()
	ent, err := core.ParseEntity(c.Collection)
	if err != nil {
		return fail(err)
	}
	spec := core.TableSpec{Namespace: p.Namespace, Raw: c.RawTable, Clean: c.CleanTable, Entity: c.Collection}
	schema, err := dst.CurrentSchema(ctx, spec)
	if err != nil {
		return fail(fmt.Errorf("read table schema: %w", err))
	}
	alias, err := conn.Attach(ctx, o.Query, o.Secrets, dstConn)
	if err != nil {
		return fail(err)
	}
	// 1. table rows keyed by _cdc_key
	tableRows := map[string][]any{}
	var tableCols []string
	colList := []string{query.QuoteIdent(core.ColKey)}
	for _, col := range schema.Columns {
		colList = append(colList, query.QuoteIdent(col.Name))
	}
	q := fmt.Sprintf("SELECT %s FROM %s", strings.Join(colList, ", "), query.Qualified(alias, p.Namespace, c.CleanTable))
	err = o.Query.StreamRows(ctx, q, func(cols []string, vals []any) error {
		tableCols = cols
		key, _ := vals[0].(string)
		if _, dup := tableRows[key]; dup {
			rep.DuplicateKeys++
		}
		tableRows[key] = vals[1:]
		return nil
	})
	if err != nil {
		return fail(fmt.Errorf("read clean table: %w", err))
	}
	_ = tableCols
	rep.TableRows = int64(len(tableRows)) + rep.DuplicateKeys
	if d, err := o.Query.DuplicateKeys(ctx, alias, p.Namespace, c.CleanTable); err == nil {
		rep.DuplicateKeys = d
	}
	// 2. source documents, flattened the same way
	it, err := src.Snapshot(ctx, ent, "", source.SnapshotOptions{ChunkSize: 5000})
	if err != nil {
		return fail(fmt.Errorf("read source: %w", err))
	}
	defer it.Close()
	seen := map[string]bool{}
	now := time.Now()
	for {
		ev, err := it.Next(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return fail(fmt.Errorf("read source: %w", err))
		}
		rep.SourceDocs++
		seen[ev.Key] = true
		row, _ := core.Flatten(schema, *ev, 0, 0, now)
		tv, ok := tableRows[ev.Key]
		if !ok {
			rep.Mismatches++
			if len(rep.Sample) < o.Show {
				rep.Sample = append(rep.Sample, Mismatch{Key: ev.Key, Kind: "missing_in_table"})
			}
			continue
		}
		for i, col := range schema.Columns {
			if i >= len(tv) {
				break
			}
			if !equal(col.Type, row.Values[i], tv[i]) {
				rep.Mismatches++
				if len(rep.Sample) < o.Show {
					rep.Sample = append(rep.Sample, Mismatch{Key: ev.Key, Kind: "value", Column: col.Name, Source: row.Values[i].Text(), Table: fmt.Sprint(tv[i])})
				}
			}
		}
	}
	// 3. extra rows in the table
	var extra []string
	for key := range tableRows {
		if !seen[key] {
			extra = append(extra, key)
		}
	}
	sort.Strings(extra)
	for _, key := range extra {
		rep.Mismatches++
		if len(rep.Sample) < o.Show {
			rep.Sample = append(rep.Sample, Mismatch{Key: key, Kind: "extra_in_table"})
		}
	}
	rep.DurationMs = time.Since(start).Milliseconds()
	return rep
}

// equal compares a flattened source value with a DuckDB value.
func equal(t core.ColumnType, v core.Value, tv any) bool {
	if v.IsNull() {
		return tv == nil
	}
	if tv == nil {
		return false
	}
	switch t {
	case core.TypeString:
		s, ok := tv.(string)
		return ok && s == v.Str
	case core.TypeLong:
		return toInt64(tv) == v.Int
	case core.TypeDouble:
		f, ok := toFloat(tv)
		if !ok {
			return false
		}
		if f == v.Float {
			return true
		}
		return math.Abs(f-v.Float) <= 1e-9*math.Max(math.Abs(f), math.Abs(v.Float))
	case core.TypeBoolean:
		b, ok := tv.(bool)
		return ok && b == v.Bool
	case core.TypeTimestampTz:
		s, ok := tv.(string)
		if !ok {
			return false
		}
		ts, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			return false
		}
		return ts.UnixMicro() == v.Time.UnixMicro()
	case core.TypeBinary:
		s, ok := tv.(string)
		return ok && s == base64.StdEncoding.EncodeToString(v.Bytes)
	}
	return fmt.Sprint(tv) == v.Text()
}

func toInt64(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int32:
		return int64(x)
	case int:
		return int64(x)
	case float64:
		return int64(x)
	case string:
		var n int64
		fmt.Sscan(x, &n)
		return n
	}
	return math.MinInt64
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int64:
		return float64(x), true
	case int32:
		return float64(x), true
	case int:
		return float64(x), true
	}
	return 0, false
}
