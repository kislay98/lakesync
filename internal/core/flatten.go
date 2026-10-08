package core

import (
	"encoding/json"
	"time"
)

// Row is one flattened raw-table row: metadata plus one value per user column,
// in schema order. A nil Value pointer in Values means NULL.
type Row struct {
	Key        string
	Op         Op
	Seq        int64
	Generation int64
	SourceTS   time.Time
	IngestedAt time.Time
	CastErrors string // JSON array of field names, "" when none
	Doc        string // full document JSON, "" on delete
	Values     []Value
}

// Flatten converts an event into a row for the given schema (section 6.3 rule 3):
// a value that does not fit its column becomes NULL, the field is listed in
// _cdc_cast_errors and the true value survives in _cdc_doc. It returns the
// conflicting source fields so callers can count them.
func Flatten(s Schema, e ChangeEvent, seq, generation int64, ingestedAt time.Time) (Row, []string) {
	r := Row{
		Key:        e.Key,
		Op:         e.Op,
		Seq:        seq,
		Generation: generation,
		SourceTS:   e.SourceTS,
		IngestedAt: ingestedAt,
		Values:     make([]Value, len(s.Columns)),
	}
	var conflicts []string
	for i := range r.Values {
		r.Values[i] = NullValue()
	}
	if e.Doc == nil {
		return r, nil
	}
	r.Doc = e.Doc.JSON
	byField := make(map[string]int, len(s.Columns))
	for i, c := range s.Columns {
		byField[c.SourceField] = i
	}
	for _, f := range e.Doc.Fields {
		i, ok := byField[f.Name]
		if !ok {
			continue // unknown field: schema evolution adds it before the next flush
		}
		v := f.Value
		if !Fits(s.Columns[i].Type, v) {
			conflicts = append(conflicts, f.Name)
			continue
		}
		if s.Columns[i].Type == TypeDouble && v.Kind == KindInt {
			v = FloatValue(float64(v.Int))
		}
		r.Values[i] = v
	}
	if len(conflicts) > 0 {
		b, _ := json.Marshal(conflicts)
		r.CastErrors = string(b)
	}
	return r, conflicts
}

// Batch is what the engine hands to a destination: the events, their flattened
// rows (same order) and the schema the rows follow.
type Batch struct {
	Schema Schema
	Events []ChangeEvent
	Rows   []Row
}

// FlattenBatch flattens events, assigning consecutive sequence numbers starting at
// firstSeq. It returns the batch and the per-field conflict counts.
func FlattenBatch(s Schema, events []ChangeEvent, firstSeq, generation int64, ingestedAt time.Time) (*Batch, map[string]int) {
	b := &Batch{Schema: s, Events: events, Rows: make([]Row, len(events))}
	conflicts := map[string]int{}
	for i, e := range events {
		row, c := Flatten(s, e, firstSeq+int64(i), generation, ingestedAt)
		b.Rows[i] = row
		for _, f := range c {
			conflicts[f]++
		}
	}
	return b, conflicts
}

// LastSeq returns the sequence number of the last row, or firstSeq-1 when empty.
func (b *Batch) LastSeq() int64 {
	if len(b.Rows) == 0 {
		return -1
	}
	return b.Rows[len(b.Rows)-1].Seq
}
