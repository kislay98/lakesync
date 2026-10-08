package mongodb

import (
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"lakesync/internal/core"
)

func mustRaw(t *testing.T, d bson.D) bson.Raw {
	t.Helper()
	b, err := bson.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	return bson.Raw(b)
}

func TestToValueEveryBSONType(t *testing.T) {
	oid, _ := bson.ObjectIDFromHex("507f1f77bcf86cd799439011")
	dec, _ := bson.ParseDecimal128("12345.678900")
	when := time.Date(2026, 10, 9, 1, 2, 3, 456_000_000, time.UTC)
	raw := mustRaw(t, bson.D{
		{Key: "oid", Value: oid},
		{Key: "str", Value: "héllo"},
		{Key: "i32", Value: int32(7)},
		{Key: "i64", Value: int64(1) << 40},
		{Key: "dbl", Value: 2.5},
		{Key: "dec", Value: dec},
		{Key: "bool", Value: true},
		{Key: "date", Value: bson.NewDateTimeFromTime(when)},
		{Key: "ts", Value: bson.Timestamp{T: 1700000000, I: 5}},
		{Key: "bin", Value: bson.Binary{Subtype: 0x00, Data: []byte{1, 2, 3}}},
		{Key: "uuid", Value: bson.Binary{Subtype: 0x04, Data: []byte("0123456789abcdef")}},
		{Key: "obj", Value: bson.D{{Key: "a", Value: int32(1)}, {Key: "b", Value: "x"}}},
		{Key: "arr", Value: bson.A{int32(1), "two", bson.D{{Key: "three", Value: 3.0}}}},
		{Key: "null", Value: nil},
		{Key: "undef", Value: bson.Undefined{}},
		{Key: "re", Value: bson.Regex{Pattern: "^a.*", Options: "i"}},
		{Key: "js", Value: bson.JavaScript("function(){}")},
		{Key: "jss", Value: bson.CodeWithScope{Code: "x", Scope: bson.D{{Key: "y", Value: int32(1)}}}},
		{Key: "sym", Value: bson.Symbol("sym")},
		{Key: "dbp", Value: bson.DBPointer{DB: "db.coll", Pointer: oid}},
		{Key: "min", Value: bson.MinKey{}},
		{Key: "max", Value: bson.MaxKey{}},
	})
	doc, err := toDocument(raw)
	if err != nil {
		t.Fatal(err)
	}
	type want struct {
		kind core.Kind
		text string
	}
	wants := map[string]want{
		"oid":   {core.KindString, "507f1f77bcf86cd799439011"},
		"str":   {core.KindString, "héllo"},
		"i32":   {core.KindInt, "7"},
		"i64":   {core.KindInt, "1099511627776"},
		"dbl":   {core.KindFloat, "2.5"},
		"dec":   {core.KindDecimal, "12345.678900"},
		"bool":  {core.KindBool, "true"},
		"date":  {core.KindTime, "2026-10-09T01:02:03.456Z"},
		"ts":    {core.KindTime, "2023-11-14T22:13:20Z"},
		"bin":   {core.KindBinary, "AQID"},
		"uuid":  {core.KindBinary, "MDEyMzQ1Njc4OWFiY2RlZg=="},
		"obj":   {core.KindJSON, `{"a":1,"b":"x"}`},
		"arr":   {core.KindJSON, `[1,"two",{"three":3.0}]`},
		"null":  {core.KindNull, ""},
		"undef": {core.KindNull, ""},
		"re":    {core.KindOther, `{"$regularExpression":{"pattern":"^a.*","options":"i"}}`},
		"js":    {core.KindOther, `{"$code":"function(){}"}`},
		"jss":   {core.KindOther, `{"$code":"x","$scope":{"y":1}}`},
		"sym":   {core.KindOther, `{"$symbol":"sym"}`},
		"dbp":   {core.KindOther, `{"$dbPointer":{"$ref":"db.coll","$id":{"$oid":"507f1f77bcf86cd799439011"}}}`},
		"min":   {core.KindOther, `{"$minKey":1}`},
		"max":   {core.KindOther, `{"$maxKey":1}`},
	}
	if len(doc.Fields) != len(wants) {
		t.Fatalf("got %d fields want %d", len(doc.Fields), len(wants))
	}
	order := []string{"oid", "str", "i32", "i64", "dbl", "dec", "bool", "date", "ts", "bin", "uuid", "obj", "arr", "null", "undef", "re", "js", "jss", "sym", "dbp", "min", "max"}
	for i, f := range doc.Fields {
		if f.Name != order[i] {
			t.Errorf("field %d: order broken, got %q want %q", i, f.Name, order[i])
		}
		w := wants[f.Name]
		if f.Value.Kind != w.kind {
			t.Errorf("%s: kind %s want %s", f.Name, f.Value.Kind, w.kind)
		}
		if got := f.Value.Text(); got != w.text {
			t.Errorf("%s: text %q want %q", f.Name, got, w.text)
		}
	}
	if !strings.HasPrefix(doc.JSON, `{"oid":{"$oid":"507f1f77bcf86cd799439011"},"str":"héllo","i32":7,`) {
		t.Errorf("relaxed JSON: %s", doc.JSON)
	}
	if !strings.Contains(doc.JSON, `"date":{"$date":"2026-10-09T01:02:03.456Z"}`) {
		t.Errorf("relaxed JSON date: %s", doc.JSON)
	}
}

func TestKeyOf(t *testing.T) {
	oid, _ := bson.ObjectIDFromHex("507f1f77bcf86cd799439011")
	dec, _ := bson.ParseDecimal128("1.5")
	cases := []struct {
		name    string
		id      any
		key     string
		display string
	}{
		{"objectid", oid, `{"$oid":"507f1f77bcf86cd799439011"}`, "507f1f77bcf86cd799439011"},
		{"string", "user-1", `"user-1"`, "user-1"},
		{"string with quote", `a"b`, `"a\"b"`, `a"b`},
		{"int32", int32(5), `{"$numberLong":"5"}`, "5"},
		{"int64", int64(5), `{"$numberLong":"5"}`, "5"},
		{"double integral", 5.0, `{"$numberLong":"5"}`, "5"},
		{"double fraction", 5.25, `{"$numberDouble":"5.25"}`, "5.25"},
		{"decimal", dec, `{"$numberDecimal":"1.5"}`, `{"$numberDecimal":"1.5"}`},
		{"bool", true, `true`, `true`},
		{"doc", bson.D{{Key: "a", Value: int32(1)}, {Key: "b", Value: "x"}}, `{"a":{"$numberInt":"1"},"b":"x"}`, `{"a":1,"b":"x"}`},
		{"binary", bson.Binary{Subtype: 0, Data: []byte{1}}, `{"$binary":{"base64":"AQ==","subType":"00"}}`, `{"$binary":{"base64":"AQ==","subType":"00"}}`},
		{"date", bson.NewDateTimeFromTime(time.Unix(0, 0)), `{"$date":{"$numberLong":"0"}}`, `{"$date":"1970-01-01T00:00:00Z"}`},
	}
	seen := map[string]bool{}
	for _, c := range cases {
		raw := mustRaw(t, bson.D{{Key: "_id", Value: c.id}})
		rv, err := lookupID(raw)
		if err != nil {
			t.Fatal(err)
		}
		key, disp, err := keyOf(rv)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if key != c.key || disp != c.display {
			t.Errorf("%s: key %q display %q; want %q %q", c.name, key, disp, c.key, c.display)
		}
		// key round-trips into a filter value
		back, err := idFromKey(key)
		if err != nil {
			t.Errorf("%s: idFromKey: %v", c.name, err)
		}
		k2, _, _ := keyOf(back)
		if k2 != key {
			t.Errorf("%s: round trip changed key: %q -> %q", c.name, key, k2)
		}
		seen[key] = true
	}
	// numeric 5 in three BSON types collapses to one key; everything else is unique
	if len(seen) != len(cases)-2 {
		t.Errorf("expected %d distinct keys, got %d", len(cases)-2, len(seen))
	}
	if _, err := idFromKey(`{"$oid":"nothex"}`); err == nil {
		t.Error("expected error for bad key")
	}
	if _, err := lookupID(mustRaw(t, bson.D{{Key: "x", Value: 1}})); err == nil {
		t.Error("expected error for missing _id")
	}
}
