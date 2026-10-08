package core

import (
	"encoding/base64"
	"strconv"
	"time"
)

// Kind is the neutral value model every source converts into. It deliberately
// has fewer kinds than BSON: nested structures are carried as JSON text.
type Kind uint8

const (
	KindNull    Kind = iota
	KindString       // text (also ObjectId hex, Symbol, ...)
	KindInt          // 32/64-bit integers
	KindFloat        // IEEE double
	KindBool         //
	KindTime         // instant with millisecond or better precision
	KindBinary       // raw bytes
	KindDecimal      // exact decimal carried as text
	KindJSON         // embedded object or array, as Relaxed Extended JSON text
	KindOther        // regex, code, min/max key, ... as Relaxed Extended JSON text
)

func (k Kind) String() string {
	switch k {
	case KindNull:
		return "null"
	case KindString:
		return "string"
	case KindInt:
		return "int"
	case KindFloat:
		return "double"
	case KindBool:
		return "bool"
	case KindTime:
		return "date"
	case KindBinary:
		return "binary"
	case KindDecimal:
		return "decimal"
	case KindJSON:
		return "object"
	case KindOther:
		return "other"
	}
	return "unknown"
}

// Value is one field value in the neutral model.
type Value struct {
	Kind  Kind
	Str   string // KindString, KindDecimal, KindJSON, KindOther
	Int   int64
	Float float64
	Bool  bool
	Time  time.Time
	Bytes []byte
}

func NullValue() Value            { return Value{Kind: KindNull} }
func StringValue(s string) Value  { return Value{Kind: KindString, Str: s} }
func IntValue(i int64) Value      { return Value{Kind: KindInt, Int: i} }
func FloatValue(f float64) Value  { return Value{Kind: KindFloat, Float: f} }
func BoolValue(b bool) Value      { return Value{Kind: KindBool, Bool: b} }
func TimeValue(t time.Time) Value { return Value{Kind: KindTime, Time: t} }
func BinaryValue(b []byte) Value  { return Value{Kind: KindBinary, Bytes: b} }
func DecimalValue(s string) Value { return Value{Kind: KindDecimal, Str: s} }
func JSONValue(s string) Value    { return Value{Kind: KindJSON, Str: s} }
func OtherValue(s string) Value   { return Value{Kind: KindOther, Str: s} }

// IsNull reports whether the value is null.
func (v Value) IsNull() bool { return v.Kind == KindNull }

// Text renders any value as text (used for display and for the string form of IDs).
func (v Value) Text() string {
	switch v.Kind {
	case KindNull:
		return ""
	case KindString, KindDecimal, KindJSON, KindOther:
		return v.Str
	case KindInt:
		return strconv.FormatInt(v.Int, 10)
	case KindFloat:
		return strconv.FormatFloat(v.Float, 'g', -1, 64)
	case KindBool:
		return strconv.FormatBool(v.Bool)
	case KindTime:
		return v.Time.UTC().Format(time.RFC3339Nano)
	case KindBinary:
		return base64.StdEncoding.EncodeToString(v.Bytes)
	}
	return ""
}

// Field is a top-level document field.
type Field struct {
	Name  string
	Value Value
}

// Document is an ordered set of top-level fields plus the full document as
// Relaxed Extended JSON (what lands in _cdc_doc).
type Document struct {
	Fields []Field
	JSON   string
}

// Get returns the value of a top-level field.
func (d *Document) Get(name string) (Value, bool) {
	if d == nil {
		return Value{}, false
	}
	for _, f := range d.Fields {
		if f.Name == name {
			return f.Value, true
		}
	}
	return Value{}, false
}
