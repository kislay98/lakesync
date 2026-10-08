package core

import (
	"sort"
	"strings"
)

// ColumnType is the destination-neutral column type. The Iceberg destination maps
// it to Iceberg types; a future destination maps it to its own.
type ColumnType string

const (
	TypeString      ColumnType = "string"
	TypeLong        ColumnType = "long"
	TypeDouble      ColumnType = "double"
	TypeBoolean     ColumnType = "boolean"
	TypeTimestampTz ColumnType = "timestamptz"
	TypeBinary      ColumnType = "binary"
)

// Column is one user column (a top-level source field).
type Column struct {
	Name        string     `json:"name"`         // column name in the destination
	SourceField string     `json:"source_field"` // top-level field name in the source document
	Type        ColumnType `json:"type"`
}

// Schema is the ordered list of user columns of a synced entity. Metadata columns
// (prefix _cdc_) are not part of it; destinations add them.
type Schema struct {
	Columns []Column `json:"columns"`
}

// Column finds a column by source field name.
func (s Schema) Column(sourceField string) (Column, bool) {
	for _, c := range s.Columns {
		if c.SourceField == sourceField {
			return c, true
		}
	}
	return Column{}, false
}

// Names returns the column names in order.
func (s Schema) Names() []string {
	out := make([]string, len(s.Columns))
	for i, c := range s.Columns {
		out[i] = c.Name
	}
	return out
}

// Clone returns a deep copy.
func (s Schema) Clone() Schema {
	out := Schema{Columns: make([]Column, len(s.Columns))}
	copy(out.Columns, s.Columns)
	return out
}

// MetaPrefix is reserved for metadata columns.
const MetaPrefix = "_cdc_"

// Metadata column names, shared by every destination.
const (
	ColKey        = MetaPrefix + "key"
	ColOp         = MetaPrefix + "op"
	ColSeq        = MetaPrefix + "seq"
	ColGeneration = MetaPrefix + "generation"
	ColSourceTS   = MetaPrefix + "source_ts"
	ColIngestedAt = MetaPrefix + "ingested_at"
	ColCastErrors = MetaPrefix + "cast_errors"
	ColDoc        = MetaPrefix + "doc"
)

// MetaColumn describes a metadata column.
type MetaColumn struct {
	Name  string
	Type  ColumnType
	Raw   bool // present in the raw table
	Clean bool // present in the clean table
}

// MetaColumns in table order.
var MetaColumns = []MetaColumn{
	{ColKey, TypeString, true, true},
	{ColOp, TypeString, true, false},
	{ColSeq, TypeLong, true, true},
	{ColGeneration, TypeLong, true, true},
	{ColSourceTS, TypeTimestampTz, true, true},
	{ColIngestedAt, TypeTimestampTz, true, true},
	{ColCastErrors, TypeString, true, true},
	{ColDoc, TypeString, true, false},
}

// IsMetaColumn reports whether a column name is reserved.
func IsMetaColumn(name string) bool { return strings.HasPrefix(name, MetaPrefix) }

// CleanColumns returns the metadata column names present in the clean table.
func CleanMetaColumns() []string {
	var out []string
	for _, m := range MetaColumns {
		if m.Clean {
			out = append(out, m.Name)
		}
	}
	return out
}

// kindType maps a value kind to the column type it needs.
func kindType(k Kind) (ColumnType, bool) {
	switch k {
	case KindString, KindDecimal, KindJSON, KindOther:
		return TypeString, true
	case KindInt:
		return TypeLong, true
	case KindFloat:
		return TypeDouble, true
	case KindBool:
		return TypeBoolean, true
	case KindTime:
		return TypeTimestampTz, true
	case KindBinary:
		return TypeBinary, true
	}
	return "", false // null: no opinion
}

// Fits reports whether a value can be stored in a column of the given type
// without changing its meaning. Null always fits.
func Fits(t ColumnType, v Value) bool {
	if v.Kind == KindNull {
		return true
	}
	vt, _ := kindType(v.Kind)
	if vt == t {
		return true
	}
	// an integer fits a double column exactly enough (section 6.2)
	return t == TypeDouble && v.Kind == KindInt
}

// fieldStats accumulates the kinds seen for one field during inference.
type fieldStats struct {
	first int // position of first appearance, to keep document order
	seen  map[ColumnType]int
	nulls int
}

// InferSchema derives column types from sampled documents (section 6.3).
//
// Rules: each field's type is the type needed by the majority of its non-null
// values; int and double together become double; a tie prefers string; a field
// only ever seen as null becomes string. Column names are sanitised and made
// unique with SanitizeColumns. Fields appear in order of first appearance.
func InferSchema(docs []*Document) (Schema, []NameMapping) {
	stats := map[string]*fieldStats{}
	order := 0
	for _, d := range docs {
		if d == nil {
			continue
		}
		for _, f := range d.Fields {
			st, ok := stats[f.Name]
			if !ok {
				st = &fieldStats{first: order, seen: map[ColumnType]int{}}
				stats[f.Name] = st
				order++
			}
			if f.Value.Kind == KindNull {
				st.nulls++
				continue
			}
			if t, ok := kindType(f.Value.Kind); ok {
				st.seen[t]++
			}
		}
	}
	names := make([]string, 0, len(stats))
	for n := range stats {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool { return stats[names[i]].first < stats[names[j]].first })
	cols := make([]Column, 0, len(names))
	for _, n := range names {
		cols = append(cols, Column{SourceField: n, Type: decideType(stats[n].seen)})
	}
	return SanitizeColumns(cols)
}

// decideType applies the majority rule with the int/double exception.
func decideType(seen map[ColumnType]int) ColumnType {
	if len(seen) == 0 {
		return TypeString
	}
	// int and double mixed -> double (counted together)
	if seen[TypeLong] > 0 && seen[TypeDouble] > 0 {
		seen = cloneCounts(seen)
		seen[TypeDouble] += seen[TypeLong]
		delete(seen, TypeLong)
	}
	best, bestN := TypeString, -1
	for _, t := range []ColumnType{TypeString, TypeDouble, TypeLong, TypeBoolean, TypeTimestampTz, TypeBinary} {
		if n := seen[t]; n > bestN {
			best, bestN = t, n
		} else if n == bestN && t == TypeString {
			best = TypeString
		}
	}
	if seen[TypeString] == bestN {
		return TypeString // tie -> string
	}
	return best
}

func cloneCounts(m map[ColumnType]int) map[ColumnType]int {
	out := make(map[ColumnType]int, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// TypeForValue returns the column type a brand-new field should get from its first
// non-null value. Null yields string.
func TypeForValue(v Value) ColumnType {
	if t, ok := kindType(v.Kind); ok {
		return t
	}
	return TypeString
}

// NewFields returns the source fields present in the documents of the batch that
// the schema does not know yet, in order of first appearance, typed by their first
// non-null value (or string when only nulls were seen).
func NewFields(s Schema, events []ChangeEvent) []Column {
	known := make(map[string]bool, len(s.Columns))
	for _, c := range s.Columns {
		known[c.SourceField] = true
	}
	var out []Column
	index := map[string]int{}
	typed := map[string]bool{} // whether out[i].Type came from a non-null value
	for _, e := range events {
		if e.Doc == nil {
			continue
		}
		for _, f := range e.Doc.Fields {
			if known[f.Name] {
				continue
			}
			i, seen := index[f.Name]
			if !seen {
				index[f.Name] = len(out)
				typed[f.Name] = f.Value.Kind != KindNull
				out = append(out, Column{SourceField: f.Name, Type: TypeForValue(f.Value)})
				continue
			}
			if !typed[f.Name] && f.Value.Kind != KindNull {
				out[i].Type = TypeForValue(f.Value)
				typed[f.Name] = true
			}
		}
	}
	return out
}
