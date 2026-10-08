package mongodb

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/mongo"

	"lakesync/internal/core"
	"lakesync/internal/source"
)

func TestParseURI(t *testing.T) {
	cases := []struct {
		in              string
		user, pass      string
		hosts           []string
		db              string
		srv, tls, atlas bool
		authSource      string
		stripped        string
		wantErr         string
	}{
		{
			in:   "mongodb+srv://alice:p%40ss%3Aword@cluster0.abc12.mongodb.net/?retryWrites=true&w=majority&authSource=admin",
			user: "alice", pass: "p@ss:word", hosts: []string{"cluster0.abc12.mongodb.net"}, srv: true, tls: true, atlas: true, authSource: "admin",
			stripped: "mongodb+srv://cluster0.abc12.mongodb.net/?retryWrites=true&w=majority&authSource=admin",
		},
		{
			in:       "mongodb://localhost:27017/?directConnection=true",
			hosts:    []string{"localhost:27017"},
			stripped: "mongodb://localhost:27017/?directConnection=true",
		},
		{
			in:   "mongodb://u:pw@h1:27017,h2:27018,h3/mydb?replicaSet=rs0&tls=true",
			user: "u", pass: "pw", hosts: []string{"h1:27017", "h2:27018", "h3"}, db: "mydb", tls: true,
			stripped: "mongodb://h1:27017,h2:27018,h3/mydb?replicaSet=rs0&tls=true",
		},
		{
			in:   "mongodb://user:p@ss@host:27017/db", // un-encoded @ in the password
			user: "user", pass: "p@ss", hosts: []string{"host:27017"}, db: "db",
			stripped: "mongodb://host:27017/db",
		},
		{
			in:   "mongodb://onlyuser@host",
			user: "onlyuser", hosts: []string{"host"},
			stripped: "mongodb://host",
		},
		{in: "postgres://x", wantErr: "unsupported scheme"},
		{in: "not a uri", wantErr: "must start with"},
		{in: "mongodb://", wantErr: "no host"},
		{in: "mongodb+srv://a.example.com,b.example.com", wantErr: "exactly one host"},
		{in: "mongodb+srv://a.example.com:27017", wantErr: "must not include a port"},
	}
	for _, c := range cases {
		p, err := ParseURI(c.in)
		if c.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("%s: want error containing %q, got %v", c.in, c.wantErr, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.in, err)
			continue
		}
		if p.Username != c.user || p.Password != c.pass || p.PasswordSet != (c.pass != "") {
			t.Errorf("%s: credentials %q/%q (set=%v)", c.in, p.Username, p.Password, p.PasswordSet)
		}
		if strings.Join(p.Hosts, ",") != strings.Join(c.hosts, ",") || p.Database != c.db || p.SRV != c.srv || p.TLS() != c.tls || p.IsAtlas() != c.atlas {
			t.Errorf("%s: parsed %+v", c.in, p)
		}
		if p.Options.Get("authSource") != c.authSource {
			t.Errorf("%s: authSource %q", c.in, p.Options.Get("authSource"))
		}
		if got := p.WithoutCredentials(); got != c.stripped {
			t.Errorf("%s: stripped %q want %q", c.in, got, c.stripped)
		}
	}
}

func TestParseAndRedact(t *testing.T) {
	cfg, sec, err := Parse("  mongodb+srv://bob:s3cret@cluster0.x.mongodb.net/shop?retryWrites=true&readPreference=secondaryPreferred ")
	if err != nil {
		t.Fatal(err)
	}
	if cfg["uri"] != "mongodb+srv://cluster0.x.mongodb.net/shop?retryWrites=true&readPreference=secondaryPreferred" {
		t.Errorf("uri: %v", cfg["uri"])
	}
	if cfg["username"] != "bob" || sec["password"] != "s3cret" || cfg["read_preference"] != "secondaryPreferred" {
		t.Errorf("cfg %v sec %v", cfg, sec)
	}
	if _, ok := cfg["auth_source"]; ok {
		t.Errorf("auth_source should be absent when the URI has none")
	}
	if _, _, err := Parse("postgres://u:p@h/db"); err == nil {
		t.Error("non-mongodb text accepted")
	}
	if _, _, err := Parse(""); err == nil {
		t.Error("empty text accepted")
	}
	red := Redact(map[string]any{"uri": "mongodb://u:pw@h/db", "username": "u", "password": "pw"})
	if red["uri"] != "mongodb://u:***@h/db" || red["password"] != "***" || red["username"] != "u" {
		t.Errorf("redact: %v", red)
	}
}

func TestSchemaAndRegistry(t *testing.T) {
	var sch map[string]any
	if err := json.Unmarshal(Schema, &sch); err != nil {
		t.Fatal(err)
	}
	props := sch["properties"].(map[string]any)
	pw := props["password"].(map[string]any)
	if pw["x-secret"] != true || pw["format"] != "password" {
		t.Errorf("password must be a secret: %v", pw)
	}
	if sch["required"].([]any)[0] != "uri" {
		t.Errorf("uri must be required")
	}
	d, err := source.Lookup(TypeName)
	if err != nil || d.Parse == nil || d.Redact == nil || d.New == nil || d.Title != "MongoDB" {
		t.Fatalf("registry: %+v %v", d, err)
	}
	if _, err := newFromMaps(t.Context(), map[string]any{}, nil); err == nil {
		t.Error("missing uri accepted")
	}
	if _, err := newFromMaps(t.Context(), map[string]any{"uri": "http://x"}, nil); err == nil {
		t.Error("bad scheme accepted")
	}
}

func TestClassify(t *testing.T) {
	if !isAuthError(errString2("connection() error occurred during connection handshake: auth error: sasl conversation error: unable to authenticate using mechanism \"SCRAM-SHA-256\"")) {
		t.Error("auth error not detected")
	}
	if !isUnauthorized(errString2("(Unauthorized) not authorized on shop to execute command")) {
		t.Error("unauthorized not detected")
	}
}

type errString2 string

func (e errString2) Error() string { return string(e) }

func TestClassifyServerErrors(t *testing.T) {
	cases := []struct {
		err  error
		lost bool
	}{
		{mongo.CommandError{Code: 286, Name: "ChangeStreamHistoryLost", Message: "Resume of change stream was not possible, as the resume point may no longer be in the oplog."}, true},
		{mongo.CommandError{Code: 280, Name: "ChangeStreamFatalError", Message: "cannot resume stream; the resume token was not found"}, true},
		{mongo.CommandError{Code: 136, Name: "CappedPositionLost", Message: "CollectionScan died due to position in capped collection being deleted"}, true},
		{mongo.CommandError{Code: 9999, Labels: []string{"NonResumableChangeStreamError"}, Message: "x"}, true},
		{mongo.CommandError{Code: 10107, Name: "NotWritablePrimary", Message: "not primary"}, false},
		{errors.New("read tcp: connection reset by peer"), false},
	}
	for _, c := range cases {
		got := classify(c.err)
		if errors.Is(got, core.ErrHistoryLost) != c.lost {
			t.Errorf("%v: history lost = %v, want %v", c.err, !c.lost, c.lost)
		}
		if !c.lost && !core.IsTransient(got) {
			t.Errorf("%v: expected transient classification, got %v", c.err, got)
		}
	}
	if classify(nil) != nil {
		t.Error("classify(nil) must be nil")
	}
	sr := &core.SourceRemovedError{Reason: "x"}
	if classify(sr) != sr {
		t.Error("source removed must pass through")
	}
}
