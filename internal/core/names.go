package core

import (
	"fmt"
	"strings"
	"unicode"
)

// NameMapping records a source field whose column name had to change.
type NameMapping struct {
	SourceField string `json:"source_field"`
	ColumnName  string `json:"column_name"`
	Reason      string `json:"reason"`
}

// SanitizeTableName makes a safe destination table name from an entity name:
// lower-case letters, digits and underscores, starting with a letter or underscore.
func SanitizeTableName(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" {
		out = "_"
	}
	if out[0] >= '0' && out[0] <= '9' {
		out = "_" + out
	}
	return out
}

// SanitizeColumns assigns column names to source fields (section 6.3 rule 4):
// names are kept as-is when valid; names that collide case-insensitively get a
// numeric suffix (`name`, `Name__2`); empty names and names starting with the
// reserved metadata prefix are renamed. Every renamed field is reported.
func SanitizeColumns(cols []Column) (Schema, []NameMapping) {
	used := map[string]bool{}
	var maps []NameMapping
	out := make([]Column, len(cols))
	for i, c := range cols {
		name, reason := baseColumnName(c.SourceField)
		candidate := name
		for n := 2; used[strings.ToLower(candidate)]; n++ {
			candidate = fmt.Sprintf("%s__%d", name, n)
			if reason == "" {
				reason = "collides case-insensitively with another field"
			}
		}
		used[strings.ToLower(candidate)] = true
		if candidate != c.SourceField {
			maps = append(maps, NameMapping{SourceField: c.SourceField, ColumnName: candidate, Reason: reason})
		}
		out[i] = Column{Name: candidate, SourceField: c.SourceField, Type: c.Type}
	}
	return Schema{Columns: out}, maps
}

// AddColumnNames assigns names to new columns given the existing schema, applying
// the same rules, and returns the named columns and mappings.
func AddColumnNames(existing Schema, add []Column) ([]Column, []NameMapping) {
	used := map[string]bool{}
	for _, c := range existing.Columns {
		used[strings.ToLower(c.Name)] = true
	}
	for _, m := range MetaColumns {
		used[strings.ToLower(m.Name)] = true
	}
	var maps []NameMapping
	out := make([]Column, len(add))
	for i, c := range add {
		name, reason := baseColumnName(c.SourceField)
		candidate := name
		for n := 2; used[strings.ToLower(candidate)]; n++ {
			candidate = fmt.Sprintf("%s__%d", name, n)
			if reason == "" {
				reason = "collides case-insensitively with another field"
			}
		}
		used[strings.ToLower(candidate)] = true
		if candidate != c.SourceField {
			maps = append(maps, NameMapping{SourceField: c.SourceField, ColumnName: candidate, Reason: reason})
		}
		out[i] = Column{Name: candidate, SourceField: c.SourceField, Type: c.Type}
	}
	return out, maps
}

// baseColumnName returns a valid base name and the reason it differs from the field.
func baseColumnName(field string) (string, string) {
	if field == "" {
		return "_empty", "empty field name"
	}
	if strings.HasPrefix(field, MetaPrefix) {
		return "f" + field, "starts with the reserved prefix " + MetaPrefix
	}
	var b strings.Builder
	changed := false
	for i, r := range field {
		ok := r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
		if i == 0 && unicode.IsDigit(r) {
			b.WriteByte('_')
			changed = true
		}
		if ok {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
			changed = true
		}
	}
	if changed {
		return b.String(), "contains characters that are not letters, digits or underscores"
	}
	return field, ""
}
