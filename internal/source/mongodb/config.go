// Package mongodb implements the lakesync Source interface for MongoDB using
// the official Go driver and change streams.
package mongodb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"

	"lakesync/internal/core"
	"lakesync/internal/source"
)

// TypeName is the registered source type.
const TypeName = "mongodb"

// Unchanged is the sentinel the UI may send for a secret field to keep the
// stored value. The API resolves it before secrets reach this package.
const Unchanged = "__unchanged__"

// Config is the non-secret part of a MongoDB connection.
type Config struct {
	URI            string `json:"uri"`                       // connection string without credentials
	Username       string `json:"username,omitempty"`        //
	AuthSource     string `json:"auth_source,omitempty"`     // default admin
	Database       string `json:"database,omitempty"`        // restrict browsing to one database
	ReadPreference string `json:"read_preference,omitempty"` // primary (default) ...
}

// Secrets are the sealed fields.
type Secrets struct {
	Password string `json:"password,omitempty"`
}

// Schema is the JSON schema of the connection form.
var Schema = json.RawMessage(`{
  "type": "object",
  "title": "MongoDB",
  "x-parse": {
    "title": "Paste a connection string",
    "description": "Paste the full mongodb:// or mongodb+srv:// URI from Atlas. Credentials are moved into the masked fields below.",
    "placeholder": "mongodb+srv://user:password@cluster0.abc123.mongodb.net/?retryWrites=true&w=majority"
  },
  "properties": {
    "uri": {
      "type": "string",
      "title": "Connection URI",
      "description": "mongodb:// or mongodb+srv:// URI without credentials. Hosts, replica set, TLS and other options go here.",
      "format": "uri",
      "x-order": 1,
      "x-group": "Connection",
      "x-placeholder": "mongodb+srv://cluster0.abc123.mongodb.net/?retryWrites=true"
    },
    "username": {
      "type": "string",
      "title": "Username",
      "description": "Database user. Leave empty for servers without authentication.",
      "x-order": 2,
      "x-group": "Connection"
    },
    "password": {
      "type": "string",
      "title": "Password",
      "description": "Stored encrypted. Never shown again.",
      "format": "password",
      "x-secret": true,
      "x-order": 3,
      "x-group": "Connection"
    },
    "auth_source": {
      "type": "string",
      "title": "Auth database",
      "description": "Database the user is defined in. Atlas users live in admin.",
      "default": "admin",
      "x-order": 4,
      "x-group": "Advanced"
    },
    "database": {
      "type": "string",
      "title": "Database",
      "description": "Only browse this database. Leave empty to list every database the user can see.",
      "x-order": 5,
      "x-group": "Advanced"
    },
    "read_preference": {
      "type": "string",
      "title": "Read preference",
      "description": "Where full copies read from. Change streams always follow the replica set.",
      "enum": ["primary", "primaryPreferred", "secondary", "secondaryPreferred", "nearest"],
      "default": "primary",
      "x-order": 6,
      "x-group": "Advanced"
    }
  },
  "required": ["uri"]
}`)

func init() {
	source.Register(source.Descriptor{
		Type:        TypeName,
		Title:       "MongoDB",
		Description: "MongoDB replica set or Atlas cluster, read through change streams.",
		Schema:      Schema,
		New:         newFromMaps,
		Redact:      Redact,
		Parse:       Parse,
	})
}

// configFromMaps decodes the generic maps the registry passes in.
func configFromMaps(config map[string]any, secrets map[string]string) (Config, Secrets, error) {
	var cfg Config
	b, err := json.Marshal(config)
	if err != nil {
		return cfg, Secrets{}, fmt.Errorf("mongodb config: %w", err)
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, Secrets{}, fmt.Errorf("mongodb config: %w", err)
	}
	sec := Secrets{Password: secrets["password"]}
	cfg.URI = strings.TrimSpace(cfg.URI)
	if cfg.URI == "" {
		return cfg, sec, errors.New("mongodb config: uri is required")
	}
	if _, err := ParseURI(cfg.URI); err != nil {
		return cfg, sec, err
	}
	return cfg, sec, nil
}

func newFromMaps(ctx context.Context, config map[string]any, secrets map[string]string) (source.Source, error) {
	cfg, sec, err := configFromMaps(config, secrets)
	if err != nil {
		return nil, err
	}
	return New(ctx, cfg, sec)
}

// Redact returns a copy of config with credentials hidden.
func Redact(config map[string]any) map[string]any {
	out := make(map[string]any, len(config))
	for k, v := range config {
		out[k] = v
	}
	if u, ok := out["uri"].(string); ok {
		out["uri"] = core.RedactURI(u)
	}
	for _, k := range []string{"password", "pwd", "secret", "token"} {
		if _, ok := out[k]; ok {
			out[k] = "***"
		}
	}
	return out
}

// Parse turns a pasted connection string into form values. Credentials are
// removed from the URI and returned separately (password as a secret).
func Parse(text string) (map[string]any, map[string]string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, nil, errors.New("paste a mongodb:// or mongodb+srv:// connection string")
	}
	p, err := ParseURI(text)
	if err != nil {
		return nil, nil, err
	}
	config := map[string]any{"uri": p.WithoutCredentials()}
	secrets := map[string]string{}
	if p.Username != "" {
		config["username"] = p.Username
	}
	if p.PasswordSet {
		secrets["password"] = p.Password
	}
	if as := p.Options.Get("authSource"); as != "" {
		config["auth_source"] = as
	}
	if rp := p.Options.Get("readPreference"); rp != "" {
		config["read_preference"] = rp
	}
	return config, secrets, nil
}

// ParsedURI is the result of ParseURI.
type ParsedURI struct {
	Scheme      string // mongodb or mongodb+srv
	SRV         bool
	Username    string
	Password    string
	PasswordSet bool
	Hosts       []string // host[:port]
	Database    string   // path component, may be empty
	Options     url.Values
	RawOptions  string // the original query string
}

// TLS reports whether the URI implies TLS (SRV implies it; tls/ssl options override).
func (p ParsedURI) TLS() bool {
	for _, k := range []string{"tls", "ssl"} {
		if v := strings.ToLower(p.Options.Get(k)); v != "" {
			return v == "true"
		}
	}
	return p.SRV
}

// IsAtlas reports whether the hosts look like MongoDB Atlas.
func (p ParsedURI) IsAtlas() bool {
	for _, h := range p.Hosts {
		if strings.Contains(strings.ToLower(h), ".mongodb.net") {
			return true
		}
	}
	return false
}

// WithoutCredentials rebuilds the URI with the userinfo removed.
func (p ParsedURI) WithoutCredentials() string {
	var b strings.Builder
	b.WriteString(p.Scheme)
	b.WriteString("://")
	b.WriteString(strings.Join(p.Hosts, ","))
	if p.Database != "" || p.RawOptions != "" {
		b.WriteByte('/')
		b.WriteString(url.PathEscape(p.Database))
	}
	if p.RawOptions != "" {
		b.WriteByte('?')
		b.WriteString(p.RawOptions)
	}
	return b.String()
}

// ParseURI parses a MongoDB connection string. It is lenient about an
// un-encoded '@' in passwords (the last '@' before the path separates the
// credentials) and accepts multiple comma-separated hosts, which net/url does not.
func ParseURI(s string) (*ParsedURI, error) {
	s = strings.TrimSpace(s)
	i := strings.Index(s, "://")
	if i < 0 {
		return nil, errors.New("not a MongoDB connection string: it must start with mongodb:// or mongodb+srv://")
	}
	p := &ParsedURI{Scheme: strings.ToLower(s[:i]), Options: url.Values{}}
	switch p.Scheme {
	case "mongodb":
	case "mongodb+srv":
		p.SRV = true
	default:
		return nil, fmt.Errorf("not a MongoDB connection string: unsupported scheme %q (want mongodb:// or mongodb+srv://)", p.Scheme)
	}
	rest := s[i+3:]
	// authority ends at the first '/' or '?' that is not inside the userinfo.
	authEnd := len(rest)
	for j := 0; j < len(rest); j++ {
		if rest[j] == '/' || rest[j] == '?' {
			authEnd = j
			break
		}
	}
	authority := rest[:authEnd]
	tail := rest[authEnd:]
	if at := strings.LastIndex(authority, "@"); at >= 0 {
		userinfo := authority[:at]
		authority = authority[at+1:]
		user, pass, hasPass := strings.Cut(userinfo, ":")
		u, err := url.PathUnescape(user)
		if err != nil {
			return nil, fmt.Errorf("invalid percent-encoding in username: %w", err)
		}
		p.Username = u
		if hasPass {
			pw, err := url.PathUnescape(pass)
			if err != nil {
				return nil, fmt.Errorf("invalid percent-encoding in password: %w", err)
			}
			p.Password = pw
			p.PasswordSet = true
		}
	}
	if authority == "" {
		return nil, errors.New("connection string has no host")
	}
	for _, h := range strings.Split(authority, ",") {
		h = strings.TrimSpace(h)
		if h == "" {
			return nil, errors.New("connection string has an empty host")
		}
		p.Hosts = append(p.Hosts, h)
	}
	if p.SRV && len(p.Hosts) != 1 {
		return nil, errors.New("mongodb+srv:// URIs must have exactly one host")
	}
	if p.SRV && strings.Contains(p.Hosts[0], ":") {
		return nil, errors.New("mongodb+srv:// URIs must not include a port")
	}
	if strings.HasPrefix(tail, "/") {
		tail = tail[1:]
		path, query, _ := strings.Cut(tail, "?")
		db, err := url.PathUnescape(path)
		if err != nil {
			return nil, fmt.Errorf("invalid database name: %w", err)
		}
		p.Database = db
		p.RawOptions = query
	} else if strings.HasPrefix(tail, "?") {
		p.RawOptions = tail[1:]
	}
	if p.RawOptions != "" {
		v, err := url.ParseQuery(p.RawOptions)
		if err != nil {
			return nil, fmt.Errorf("invalid connection options: %w", err)
		}
		// option names are case-insensitive; index them lower-cased for lookups
		for k, vals := range v {
			for _, val := range vals {
				p.Options.Add(canonicalOption(k), val)
			}
		}
	}
	return p, nil
}

// canonicalOption maps a URI option to its camelCase spelling for lookups.
func canonicalOption(k string) string {
	known := []string{"authSource", "authMechanism", "replicaSet", "directConnection", "tls", "ssl", "readPreference", "retryWrites", "w", "appName", "tlsInsecure", "tlsAllowInvalidCertificates", "connectTimeoutMS", "serverSelectionTimeoutMS", "maxPoolSize"}
	for _, want := range known {
		if strings.EqualFold(k, want) {
			return want
		}
	}
	return k
}

// Defaults for client behaviour.
const (
	connectTimeout         = 10 * time.Second
	operationTimeout       = 2 * time.Minute // bounds each driver operation; change streams wait at most MaxAwaitTime
	serverSelectionTimeout = 10 * time.Second
	maxPoolSize            = 50 // one client per source; Atlas Free allows 500 connections
)

// clientOptions builds the driver options from config and secrets.
func clientOptions(cfg Config, sec Secrets) (*options.ClientOptions, error) {
	opts := options.Client().
		ApplyURI(cfg.URI).
		SetAppName(core.AppName).
		SetConnectTimeout(connectTimeout).
		SetServerSelectionTimeout(serverSelectionTimeout).
		SetMaxPoolSize(maxPoolSize).
		// every operation is bounded, so a connection that silently dies surfaces as a
		// transient error (and a retry) instead of a stalled pipeline
		SetTimeout(operationTimeout)
	if cfg.Username != "" {
		cred := options.Credential{Username: cfg.Username, Password: sec.Password, PasswordSet: true}
		if cfg.AuthSource != "" {
			cred.AuthSource = cfg.AuthSource
		}
		opts.SetAuth(cred)
	}
	if cfg.ReadPreference != "" {
		mode, err := readpref.ModeFromString(cfg.ReadPreference)
		if err != nil {
			return nil, fmt.Errorf("read_preference: %w", err)
		}
		rp, err := readpref.New(mode)
		if err != nil {
			return nil, fmt.Errorf("read_preference: %w", err)
		}
		opts.SetReadPreference(rp)
	}
	if err := opts.Validate(); err != nil {
		return nil, fmt.Errorf("invalid connection settings: %w", err)
	}
	return opts, nil
}

// connect opens a client. The caller must Disconnect it.
func connect(cfg Config, sec Secrets) (*mongo.Client, error) {
	opts, err := clientOptions(cfg, sec)
	if err != nil {
		return nil, err
	}
	cl, err := mongo.Connect(opts)
	if err != nil {
		return nil, fmt.Errorf("mongodb connect: %w", redactErr(err, sec))
	}
	return cl, nil
}

// redactErr strips the password from an error message.
func redactErr(err error, sec Secrets) error {
	if err == nil {
		return nil
	}
	msg := core.RedactURI(err.Error())
	if sec.Password != "" {
		msg = core.RedactText(msg, sec.Password)
	}
	if msg == err.Error() {
		return err
	}
	return errors.New(msg)
}
