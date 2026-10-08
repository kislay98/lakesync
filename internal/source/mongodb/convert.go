package mongodb

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"lakesync/internal/core"
)

// toDocument converts a raw BSON document into the neutral model, keeping field
// order. Nested documents and arrays become Relaxed Extended JSON text; the full
// document is kept as Relaxed Extended JSON in Document.JSON.
func toDocument(raw bson.Raw) (*core.Document, error) {
	if err := raw.Validate(); err != nil {
		return nil, fmt.Errorf("invalid BSON document: %w", err)
	}
	elems, err := raw.Elements()
	if err != nil {
		return nil, fmt.Errorf("invalid BSON document: %w", err)
	}
	doc := &core.Document{Fields: make([]core.Field, 0, len(elems))}
	for _, el := range elems {
		key, err := el.KeyErr()
		if err != nil {
			return nil, fmt.Errorf("invalid BSON element: %w", err)
		}
		rv, err := el.ValueErr()
		if err != nil {
			return nil, fmt.Errorf("invalid BSON value for %q: %w", key, err)
		}
		v, err := toValue(rv)
		if err != nil {
			return nil, fmt.Errorf("field %q: %w", key, err)
		}
		doc.Fields = append(doc.Fields, core.Field{Name: key, Value: v})
	}
	js, err := bson.MarshalExtJSON(raw, false, false)
	if err != nil {
		return nil, fmt.Errorf("render document as JSON: %w", err)
	}
	doc.JSON = string(js)
	return doc, nil
}

// toValue maps one BSON value to the neutral model (brief section 6.2).
func toValue(rv bson.RawValue) (core.Value, error) {
	switch rv.Type {
	case bson.TypeNull, bson.TypeUndefined:
		return core.NullValue(), nil
	case bson.TypeObjectID:
		return core.StringValue(rv.ObjectID().Hex()), nil
	case bson.TypeString:
		return core.StringValue(rv.StringValue()), nil
	case bson.TypeInt32:
		return core.IntValue(int64(rv.Int32())), nil
	case bson.TypeInt64:
		return core.IntValue(rv.Int64()), nil
	case bson.TypeDouble:
		return core.FloatValue(rv.Double()), nil
	case bson.TypeDecimal128:
		return core.DecimalValue(rv.Decimal128().String()), nil
	case bson.TypeBoolean:
		return core.BoolValue(rv.Boolean()), nil
	case bson.TypeDateTime:
		return core.TimeValue(rv.Time().UTC()), nil
	case bson.TypeTimestamp:
		t, _ := rv.Timestamp()
		return core.TimeValue(time.Unix(int64(t), 0).UTC()), nil
	case bson.TypeBinary:
		_, data := rv.Binary()
		cp := make([]byte, len(data))
		copy(cp, data)
		return core.BinaryValue(cp), nil
	case bson.TypeEmbeddedDocument, bson.TypeArray:
		s, err := extJSON(rv, false)
		if err != nil {
			return core.Value{}, err
		}
		return core.JSONValue(s), nil
	case bson.TypeRegex, bson.TypeJavaScript, bson.TypeCodeWithScope, bson.TypeSymbol, bson.TypeDBPointer, bson.TypeMinKey, bson.TypeMaxKey:
		s, err := extJSON(rv, false)
		if err != nil {
			return core.Value{}, err
		}
		return core.OtherValue(s), nil
	}
	return core.Value{}, fmt.Errorf("unsupported BSON type 0x%02x", byte(rv.Type))
}

// extJSON renders a single value as Extended JSON by wrapping it in a document
// and stripping the wrapper, which works for every BSON type.
func extJSON(rv bson.RawValue, canonical bool) (string, error) {
	b, err := bson.MarshalExtJSON(bson.D{{Key: "v", Value: rv}}, canonical, false)
	if err != nil {
		return "", fmt.Errorf("render value as JSON: %w", err)
	}
	s := string(b)
	if !strings.HasPrefix(s, `{"v":`) || !strings.HasSuffix(s, "}") {
		return "", errors.New("unexpected Extended JSON shape")
	}
	return s[5 : len(s)-1], nil
}

// keyOf returns the canonical, type-unique key of an _id value, plus its display
// form. Integral numbers of any BSON numeric type map to the same key because
// MongoDB treats them as the same _id.
func keyOf(id bson.RawValue) (key, display string, err error) {
	switch id.Type {
	case bson.TypeObjectID:
		h := id.ObjectID().Hex()
		return `{"$oid":"` + h + `"}`, h, nil
	case bson.TypeString:
		s := id.StringValue()
		b, err := bson.MarshalExtJSON(bson.D{{Key: "v", Value: s}}, true, false)
		if err != nil {
			return "", "", err
		}
		js := string(b)
		return js[5 : len(js)-1], s, nil
	case bson.TypeInt32:
		n := int64(id.Int32())
		return `{"$numberLong":"` + strconv.FormatInt(n, 10) + `"}`, strconv.FormatInt(n, 10), nil
	case bson.TypeInt64:
		n := id.Int64()
		return `{"$numberLong":"` + strconv.FormatInt(n, 10) + `"}`, strconv.FormatInt(n, 10), nil
	case bson.TypeDouble:
		f := id.Double()
		if f == math.Trunc(f) && !math.IsInf(f, 0) && math.Abs(f) < 1<<53 {
			n := int64(f)
			return `{"$numberLong":"` + strconv.FormatInt(n, 10) + `"}`, strconv.FormatInt(n, 10), nil
		}
		k, err := extJSON(id, true)
		return k, strconv.FormatFloat(f, 'g', -1, 64), err
	}
	k, err := extJSON(id, true)
	if err != nil {
		return "", "", err
	}
	d, err := extJSON(id, false)
	if err != nil {
		return "", "", err
	}
	return k, d, nil
}

// idFromKey parses a canonical key (as produced by keyOf) back into a BSON value
// usable in a query filter.
func idFromKey(key string) (bson.RawValue, error) {
	var raw bson.Raw
	if err := bson.UnmarshalExtJSON([]byte(`{"v":`+key+`}`), true, &raw); err != nil {
		return bson.RawValue{}, fmt.Errorf("invalid key %q: %w", key, err)
	}
	rv, err := raw.LookupErr("v")
	if err != nil {
		return bson.RawValue{}, fmt.Errorf("invalid key %q: %w", key, err)
	}
	return rv, nil
}

// lookupID returns the _id of a raw document.
func lookupID(raw bson.Raw) (bson.RawValue, error) {
	rv, err := raw.LookupErr("_id")
	if err != nil {
		return bson.RawValue{}, errors.New("document has no _id")
	}
	return rv, nil
}
