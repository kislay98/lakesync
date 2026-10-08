package iceberg

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/iceberg-go"
	"github.com/apache/iceberg-go/table"

	"lakesync/internal/core"
	"lakesync/internal/dest"
)

func TestSchemaIsValidJSON(t *testing.T) {
	var m map[string]any
	if err := json.Unmarshal([]byte(Schema), &m); err != nil {
		t.Fatal(err)
	}
	props := m["properties"].(map[string]any)
	for _, secret := range []string{"token", "client_secret", "s3_access_key_id", "s3_secret_access_key"} {
		p := props[secret].(map[string]any)
		if p["x-secret"] != true {
			t.Errorf("%s must be x-secret", secret)
		}
	}
	d, err := dest.Lookup(TypeName)
	if err != nil || d.Title == "" || d.New == nil || d.Redact == nil {
		t.Fatalf("registration incomplete: %+v %v", d, err)
	}
}

func TestParseConfig(t *testing.T) {
	cfg, err := ParseConfig(map[string]any{"catalog_uri": "http://nessie:19120/iceberg/", "warehouse": "warehouse", "s3_endpoint": "http://s3:9000/"},
		map[string]string{"s3_access_key_id": "k", "s3_secret_access_key": "s"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CatalogURI != "http://nessie:19120/iceberg" || cfg.S3Endpoint != "http://s3:9000" || !cfg.S3PathStyle || cfg.AuthType != AuthNone || cfg.S3Region != "us-east-1" {
		t.Errorf("defaults wrong: %+v", cfg)
	}
	if !cfg.StripVended() || !cfg.HasS3() {
		t.Error("explicit S3 settings must strip vended config")
	}
	p := cfg.S3Props()
	if p["s3.endpoint"] != "http://s3:9000" || p["s3.force-virtual-addressing"] != "false" || p["s3.access-key-id"] != "k" || p["s3.secret-access-key"] != "s" {
		t.Errorf("S3 props: %v", p)
	}
	if got := cfg.Endpoints(); len(got) != 2 || got[0] != cfg.CatalogURI || got[1] != cfg.S3Endpoint {
		t.Errorf("Endpoints: %v", got)
	}

	cases := []struct {
		name    string
		config  map[string]any
		secrets map[string]string
		want    string
	}{
		{"missing uri", map[string]any{"warehouse": "w"}, nil, "catalog_uri is required"},
		{"bad uri", map[string]any{"catalog_uri": "nessie:19120", "warehouse": "w"}, nil, "http(s) URL"},
		{"missing warehouse", map[string]any{"catalog_uri": "http://x"}, nil, "warehouse is required"},
		{"bearer without token", map[string]any{"catalog_uri": "http://x", "warehouse": "w", "auth_type": "bearer"}, nil, "token is required"},
		{"oauth without secret", map[string]any{"catalog_uri": "http://x", "warehouse": "w", "auth_type": "oauth2", "client_id": "id"}, nil, "client_secret"},
		{"bad auth", map[string]any{"catalog_uri": "http://x", "warehouse": "w", "auth_type": "magic"}, nil, "auth_type must be"},
		{"half keys", map[string]any{"catalog_uri": "http://x", "warehouse": "w"}, map[string]string{"s3_access_key_id": "k"}, "given together"},
		{"endpoint without keys", map[string]any{"catalog_uri": "http://x", "warehouse": "w", "s3_endpoint": "http://s3:9000"}, nil, "required when an S3 endpoint"},
	}
	for _, c := range cases {
		_, err := ParseConfig(c.config, c.secrets)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v want %q", c.name, err, c.want)
		}
	}
	ok, err := ParseConfig(map[string]any{"catalog_uri": "https://cat", "warehouse": "w", "auth_type": "oauth2", "client_id": "id", "vended_credentials": "true"}, map[string]string{"client_secret": "sec"})
	if err != nil || !ok.VendedCredentials || ok.StripVended() {
		t.Errorf("oauth2 + vended: %+v %v", ok, err)
	}
}

func TestRedact(t *testing.T) {
	out := Redact(map[string]any{"catalog_uri": "https://user:pw@cat/iceberg", "token": "t", "warehouse": "w", "s3_endpoint": "http://s3"})
	if _, has := out["token"]; has {
		t.Error("secret leaked")
	}
	if out["catalog_uri"] != "https://user:***@cat/iceberg" || out["warehouse"] != "w" {
		t.Errorf("redact: %v", out)
	}
}

func TestTypeMapping(t *testing.T) {
	for _, ct := range []core.ColumnType{core.TypeString, core.TypeLong, core.TypeDouble, core.TypeBoolean, core.TypeTimestampTz, core.TypeBinary} {
		it, err := icebergType(ct)
		if err != nil {
			t.Fatalf("%s: %v", ct, err)
		}
		back, ok := coreType(it)
		if !ok || back != ct {
			t.Errorf("%s -> %s -> %s", ct, it, back)
		}
		if _, err := arrowType(it); err != nil {
			t.Errorf("arrow for %s: %v", ct, err)
		}
	}
	if _, err := icebergType("decimal"); err == nil {
		t.Error("unknown type accepted")
	}
	if back, ok := coreType(iceberg.PrimitiveTypes.Date); ok || back != core.TypeString {
		t.Errorf("unsupported Iceberg type should degrade to string: %s %v", back, ok)
	}
}

func TestBuildSchemaAndUserColumns(t *testing.T) {
	user := core.Schema{Columns: []core.Column{{Name: "_id", SourceField: "_id", Type: core.TypeString}, {Name: "Name__2", SourceField: "Name", Type: core.TypeLong}}}
	raw, err := buildSchema(RoleRaw, user)
	if err != nil {
		t.Fatal(err)
	}
	clean, err := buildSchema(RoleClean, user)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw.Fields()) != 8+2 || len(clean.Fields()) != 6+2 {
		t.Errorf("field counts raw=%d clean=%d", len(raw.Fields()), len(clean.Fields()))
	}
	ids := map[int]bool{}
	for _, f := range raw.Fields() {
		if ids[f.ID] {
			t.Errorf("duplicate field id %d", f.ID)
		}
		ids[f.ID] = true
	}
	got := userColumns(raw, nil)
	if len(got.Columns) != 2 || got.Columns[1].Name != "Name__2" || got.Columns[1].SourceField != "Name" || got.Columns[1].Type != core.TypeLong {
		t.Errorf("userColumns: %+v", got.Columns)
	}
}

func TestRowsToRecord(t *testing.T) {
	user := core.Schema{Columns: []core.Column{
		{Name: "s", SourceField: "s", Type: core.TypeString},
		{Name: "l", SourceField: "l", Type: core.TypeLong},
		{Name: "d", SourceField: "d", Type: core.TypeDouble},
		{Name: "b", SourceField: "b", Type: core.TypeBoolean},
		{Name: "t", SourceField: "t", Type: core.TypeTimestampTz},
		{Name: "bin", SourceField: "bin", Type: core.TypeBinary},
	}}
	sc, _ := buildSchema(RoleRaw, user)
	now := time.Date(2026, 10, 9, 1, 2, 3, 456000, time.UTC)
	rows := []core.Row{
		{Key: "k1", Op: core.OpInsert, Seq: 1, Generation: 1, SourceTS: now, IngestedAt: now, Doc: `{"s":"x"}`, Values: []core.Value{
			core.StringValue("x"), core.IntValue(7), core.IntValue(3), core.BoolValue(true), core.TimeValue(now), core.BinaryValue([]byte{1, 2})}},
		{Key: "k2", Op: core.OpDelete, Seq: 2, Generation: 1, IngestedAt: now, CastErrors: `["l"]`, Values: []core.Value{
			core.NullValue(), core.NullValue(), core.FloatValue(1.5), core.NullValue(), core.NullValue(), core.NullValue()}},
	}
	rec, err := rowsToRecord(sc, &core.Batch{Schema: user, Rows: rows})
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Release()
	if rec.NumRows() != 2 || rec.NumCols() != int64(len(sc.Fields())) {
		t.Fatalf("shape %dx%d", rec.NumRows(), rec.NumCols())
	}
	col := func(name string) arrow.Array {
		for i, f := range rec.Schema().Fields() {
			if f.Name == name {
				return rec.Column(i)
			}
		}
		t.Fatalf("no column %s", name)
		return nil
	}
	if col(core.ColKey).(*array.String).Value(1) != "k2" || col(core.ColOp).(*array.String).Value(1) != "d" {
		t.Error("meta columns wrong")
	}
	if !col(core.ColDoc).IsNull(1) || col(core.ColDoc).(*array.String).Value(0) != `{"s":"x"}` {
		t.Error("_cdc_doc handling wrong")
	}
	if !col(core.ColCastErrors).IsNull(0) || col(core.ColCastErrors).(*array.String).Value(1) != `["l"]` {
		t.Error("_cdc_cast_errors handling wrong")
	}
	if !col(core.ColSourceTS).IsNull(1) || col(core.ColSourceTS).(*array.Timestamp).Value(0) != arrow.Timestamp(now.UnixMicro()) {
		t.Error("_cdc_source_ts wrong")
	}
	if col("l").(*array.Int64).Value(0) != 7 || col("d").(*array.Float64).Value(0) != 3 || col("d").(*array.Float64).Value(1) != 1.5 {
		t.Error("numeric values wrong")
	}
	if !col("b").(*array.Boolean).Value(0) || col("t").(*array.Timestamp).Value(0) != arrow.Timestamp(now.UnixMicro()) || string(col("bin").(*array.Binary).Value(0)) != "\x01\x02" {
		t.Error("typed values wrong")
	}
	for _, n := range []string{"s", "l", "b", "t", "bin"} {
		if !col(n).IsNull(1) {
			t.Errorf("%s should be null on row 2", n)
		}
	}
	// a batch column the table lacks is an error (schema must be evolved first)
	bad := core.Schema{Columns: append(user.Clone().Columns, core.Column{Name: "new", SourceField: "new", Type: core.TypeString})}
	if _, err := rowsToRecord(sc, &core.Batch{Schema: bad, Rows: []core.Row{{Values: make([]core.Value, 7)}}}); err == nil || !strings.Contains(err.Error(), "evolve") {
		t.Errorf("expected evolve error, got %v", err)
	}
	// table columns missing from the batch become NULL
	less := core.Schema{Columns: user.Columns[:2]}
	rec2, err := rowsToRecord(sc, &core.Batch{Schema: less, Rows: []core.Row{{Key: "k", Op: core.OpInsert, IngestedAt: now, Values: []core.Value{core.StringValue("a"), core.IntValue(1)}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer rec2.Release()
	if !rec2.Column(int(rec2.NumCols()) - 1).IsNull(0) {
		t.Error("missing batch column should be NULL")
	}
}

func TestCommitErrorMapping(t *testing.T) {
	cfg := Config{Token: "supersecrettoken"}
	err := mapCommitErr(errors.New("commit failed: Requirement failed: snapshot id changed"), cfg)
	if !core.IsCommitConflict(err) {
		t.Errorf("requirement failure should be a conflict: %v", err)
	}
	if !core.IsCommitConflict(mapCommitErr(table.ErrCommitFailed, cfg)) {
		t.Error("ErrCommitFailed should be a conflict")
	}
	plain := mapCommitErr(errors.New("boom with supersecrettoken inside"), cfg)
	if core.IsCommitConflict(plain) || strings.Contains(plain.Error(), "supersecrettoken") {
		t.Errorf("plain error mishandled: %v", plain)
	}
	if mapCommitErr(nil, cfg) != nil {
		t.Error("nil must stay nil")
	}
}

func TestCheckpointPropertiesRoundTrip(t *testing.T) {
	cp := core.Checkpoint{Bookmark: []byte{0, 1, 2, 255}, Seq: 10, Generation: 2, Phase: core.PhaseStream, RunID: "r"}
	props := iceberg.Properties(cp.Properties())
	got, ok, err := core.CheckpointFromProperties(props)
	if err != nil || !ok || got.Seq != 10 || string(got.Bookmark) != string(cp.Bookmark) || got.Phase != core.PhaseStream {
		t.Fatalf("round trip: %+v %v %v", got, ok, err)
	}
}

func TestHintFor(t *testing.T) {
	if h := hintFor(errors.New("dial tcp: connection refused"), "Reach the catalog", Config{}); !strings.Contains(h, "catalog URL reachable") {
		t.Error(h)
	}
	if h := hintFor(errors.New("lookup s3: no such host"), "Write and delete an object in storage", Config{}); !strings.Contains(h, "vends") {
		t.Error(h)
	}
	if h := hintFor(errors.New("HTTP 401"), "Authenticate", Config{}); !strings.Contains(h, "credentials") {
		t.Error(h)
	}
	if h := hintFor(errors.New("NoSuchBucket"), "x", Config{}); !strings.Contains(h, "bucket") {
		t.Error(h)
	}
}

func TestIdentifier(t *testing.T) {
	id := identifier("a.b", "t")
	if len(id) != 3 || id[2] != "t" || joinNamespace(id[:2]) != "a.b" {
		t.Errorf("identifier: %v", id)
	}
}
