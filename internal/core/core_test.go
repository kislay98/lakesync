package core

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func doc(fields ...Field) *Document {
	m := map[string]any{}
	for _, f := range fields {
		m[f.Name] = f.Value.Text()
	}
	b, _ := json.Marshal(m)
	return &Document{Fields: fields, JSON: string(b)}
}

func TestInferSchema(t *testing.T) {
	now := time.Now()
	docs := []*Document{
		doc(Field{"_id", StringValue("a")}, Field{"n", IntValue(1)}, Field{"phone", StringValue("+1")}, Field{"ok", BoolValue(true)}, Field{"when", TimeValue(now)}, Field{"onlynull", NullValue()}),
		doc(Field{"_id", StringValue("b")}, Field{"n", FloatValue(1.5)}, Field{"phone", IntValue(5)}, Field{"price", DecimalValue("1.50")}, Field{"addr", JSONValue(`{"x":1}`)}, Field{"blob", BinaryValue([]byte{1})}),
		doc(Field{"_id", StringValue("c")}, Field{"phone", IntValue(6)}, Field{"onlynull", NullValue()}),
	}
	s, maps := InferSchema(docs)
	want := map[string]ColumnType{"_id": TypeString, "n": TypeDouble, "phone": TypeLong, "ok": TypeBoolean, "when": TypeTimestampTz, "onlynull": TypeString, "price": TypeString, "addr": TypeString, "blob": TypeBinary}
	if len(s.Columns) != len(want) {
		t.Fatalf("got %d columns want %d: %+v", len(s.Columns), len(want), s.Columns)
	}
	for _, c := range s.Columns {
		if want[c.SourceField] != c.Type {
			t.Errorf("%s: got %s want %s", c.SourceField, c.Type, want[c.SourceField])
		}
	}
	if s.Columns[0].SourceField != "_id" || s.Columns[1].SourceField != "n" {
		t.Errorf("order of first appearance not kept: %v", s.Names())
	}
	if len(maps) != 0 {
		t.Errorf("unexpected mappings %+v", maps)
	}
}

func TestInferSchemaTieIsString(t *testing.T) {
	docs := []*Document{
		doc(Field{"v", StringValue("x")}),
		doc(Field{"v", IntValue(1)}),
	}
	s, _ := InferSchema(docs)
	if s.Columns[0].Type != TypeString {
		t.Fatalf("tie should be string, got %s", s.Columns[0].Type)
	}
}

func TestSanitizeColumns(t *testing.T) {
	cols := []Column{
		{SourceField: "name", Type: TypeString},
		{SourceField: "Name", Type: TypeString},
		{SourceField: "NAME", Type: TypeString},
		{SourceField: "", Type: TypeString},
		{SourceField: "_cdc_seq", Type: TypeLong},
		{SourceField: "first name", Type: TypeString},
		{SourceField: "1st", Type: TypeString},
		{SourceField: "émail", Type: TypeString},
	}
	s, maps := SanitizeColumns(cols)
	got := s.Names()
	want := []string{"name", "Name__2", "NAME__3", "_empty", "f_cdc_seq", "first_name", "_1st", "émail"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("col %d: got %q want %q", i, got[i], want[i])
		}
	}
	if len(maps) != 6 {
		t.Errorf("want 6 mappings got %d: %+v", len(maps), maps)
	}
	// adding later must not collide with existing or metadata columns
	add, m2 := AddColumnNames(s, []Column{{SourceField: "name", Type: TypeString}, {SourceField: "_cdc_key"}})
	if add[0].Name != "name__4" || add[1].Name != "f_cdc_key" || len(m2) != 2 {
		t.Errorf("AddColumnNames: %+v %+v", add, m2)
	}
}

func TestFlattenCastErrors(t *testing.T) {
	s := Schema{Columns: []Column{
		{Name: "_id", SourceField: "_id", Type: TypeString},
		{Name: "n", SourceField: "n", Type: TypeLong},
		{Name: "d", SourceField: "d", Type: TypeDouble},
		{Name: "b", SourceField: "b", Type: TypeBoolean},
		{Name: "t", SourceField: "t", Type: TypeTimestampTz},
	}}
	now := time.Now()
	e := ChangeEvent{Key: `{"$oid":"x"}`, Op: OpInsert, SourceTS: now, Doc: doc(
		Field{"_id", StringValue("x")}, Field{"n", StringValue("not a number")}, Field{"d", IntValue(3)}, Field{"b", NullValue()}, Field{"t", FloatValue(1)}, Field{"extra", IntValue(9)})}
	row, conflicts := Flatten(s, e, 7, 2, now)
	if row.Seq != 7 || row.Generation != 2 || row.Key != e.Key || row.Doc == "" {
		t.Fatalf("row metadata wrong: %+v", row)
	}
	if !row.Values[1].IsNull() || row.Values[2].Kind != KindFloat || row.Values[2].Float != 3 || !row.Values[3].IsNull() || !row.Values[4].IsNull() {
		t.Errorf("values: %+v", row.Values)
	}
	if strings.Join(conflicts, ",") != "n,t" || row.CastErrors != `["n","t"]` {
		t.Errorf("conflicts %v cast_errors %s", conflicts, row.CastErrors)
	}
	del := ChangeEvent{Key: "k", Op: OpDelete}
	drow, dc := Flatten(s, del, 8, 2, now)
	if len(dc) != 0 || drow.Doc != "" || !drow.Values[0].IsNull() {
		t.Errorf("delete row: %+v", drow)
	}
	nf := NewFields(s, []ChangeEvent{e, {Op: OpInsert, Doc: doc(Field{"later", NullValue()})}, {Op: OpInsert, Doc: doc(Field{"later", BoolValue(true)})}})
	if len(nf) != 2 || nf[0].SourceField != "extra" || nf[0].Type != TypeLong || nf[1].SourceField != "later" || nf[1].Type != TypeBoolean {
		t.Errorf("NewFields: %+v", nf)
	}
}

func TestCheckpointRoundTrip(t *testing.T) {
	cp := Checkpoint{Bookmark: []byte(`{"_data":"82"}`), Seq: 42, Generation: 3, Phase: PhaseSnapshot, SnapshotCursor: `{"$oid":"abc"}`, RunID: "r1"}
	p := cp.Properties()
	got, ok, err := CheckpointFromProperties(p)
	if err != nil || !ok {
		t.Fatalf("decode: ok=%v err=%v", ok, err)
	}
	if string(got.Bookmark) != string(cp.Bookmark) || got.Seq != 42 || got.Generation != 3 || got.Phase != PhaseSnapshot || got.SnapshotCursor != cp.SnapshotCursor || got.RunID != "r1" {
		t.Errorf("round trip mismatch: %+v", got)
	}
	if _, ok, _ := CheckpointFromProperties(map[string]string{"other": "x"}); ok {
		t.Error("expected no checkpoint")
	}
	if _, _, err := CheckpointFromProperties(map[string]string{PropSeq: "nope"}); err == nil {
		t.Error("expected error on bad seq")
	}
}

func TestRedactURI(t *testing.T) {
	cases := map[string]string{
		"mongodb+srv://alice:s3cr%40t@cluster0.x.mongodb.net/db?retryWrites=true": "mongodb+srv://alice:***@cluster0.x.mongodb.net/db?retryWrites=true",
		"mongodb://localhost:27017/?replicaSet=rs0":                               "mongodb://localhost:27017/?replicaSet=rs0",
		"postgres://u:p@h/db?password=zzz":                                        "postgres://u:***@h/db?password=***",
		"https://catalog.example.com/v1?token=abc123":                             "https://catalog.example.com/v1?token=***",
		"mongodb://onlyuser@host":                                                 "mongodb://***@host",
	}
	for in, want := range cases {
		if got := RedactURI(in); got != want {
			t.Errorf("RedactURI(%q) = %q want %q", in, got, want)
		}
	}
	if got := RedactText("key is hunter22 and hunter22 again", "hunter22"); got != "key is *** and *** again" {
		t.Errorf("RedactText: %q", got)
	}
}

func TestParseEntity(t *testing.T) {
	e, err := ParseEntity("shop.orders.v2")
	if err != nil || e.Namespace != "shop" || e.Name != "orders.v2" {
		t.Fatalf("got %+v %v", e, err)
	}
	for _, bad := range []string{"", "shop", ".x", "x."} {
		if _, err := ParseEntity(bad); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
	spec := NewTableSpec("lake", Entity{Namespace: "shop", Name: "Order-Items"})
	if spec.Raw != "order_items__raw" || spec.Clean != "order_items" || spec.Entity != "shop.Order-Items" {
		t.Errorf("spec %+v", spec)
	}
}
