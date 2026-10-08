// Package iceberg is the Apache Iceberg destination: tables in any Iceberg REST
// catalog with data in any S3-compatible object store, written with iceberg-go.
//
// The package only speaks the Iceberg REST catalog spec and the S3 API; it
// imports no cloud provider SDK directly (iceberg-go's S3 file IO uses the AWS
// SDK internally, see DECISIONS.md).
package iceberg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	icebergio "github.com/apache/iceberg-go/io"

	"lakesync/internal/core"
	"lakesync/internal/dest"
)

// TypeName is the registered destination type.
const TypeName = "iceberg_rest"

// Auth types.
const (
	AuthNone   = "none"
	AuthBearer = "bearer"
	AuthOAuth2 = "oauth2"
)

// Config is the parsed destination configuration. Secret fields are only ever
// populated from the opened secrets map, never from the stored config.
type Config struct {
	CatalogURI string
	Warehouse  string
	Prefix     string

	AuthType        string
	Token           string // secret
	ClientID        string
	ClientSecret    string // secret
	OAuth2ServerURI string
	Scope           string

	S3Endpoint        string
	S3Region          string
	S3AccessKeyID     string // secret
	S3SecretAccessKey string // secret
	S3PathStyle       bool
	VendedCredentials bool
}

// Schema is the JSON schema that drives the destination form in the UI.
const Schema = `{
  "type": "object",
  "title": "Apache Iceberg (REST catalog + S3)",
  "required": ["catalog_uri", "warehouse"],
  "properties": {
    "catalog_uri": {"type": "string", "format": "uri", "title": "Catalog URI", "description": "Base URL of the Iceberg REST catalog (the part before /v1/config).", "x-order": 1, "x-group": "Catalog", "x-placeholder": "http://nessie:19120/iceberg"},
    "warehouse": {"type": "string", "title": "Warehouse", "description": "Warehouse name or location the catalog expects (Nessie: the warehouse name; R2: the bucket; Glue/S3 Tables: the ARN).", "x-order": 2, "x-group": "Catalog", "x-placeholder": "warehouse"},
    "prefix": {"type": "string", "title": "Prefix", "description": "Optional path prefix some catalogs require (leave empty unless the catalog documents one).", "x-order": 3, "x-group": "Advanced"},
    "auth_type": {"type": "string", "title": "Authentication", "enum": ["none", "bearer", "oauth2"], "default": "none", "description": "How to authenticate against the catalog.", "x-order": 4, "x-group": "Catalog"},
    "token": {"type": "string", "format": "password", "title": "Bearer token", "description": "Static token sent as Authorization: Bearer.", "x-secret": true, "x-order": 5, "x-group": "Catalog", "x-depends-on": {"auth_type": "bearer"}},
    "client_id": {"type": "string", "title": "OAuth2 client ID", "x-order": 6, "x-group": "Catalog", "x-depends-on": {"auth_type": "oauth2"}},
    "client_secret": {"type": "string", "format": "password", "title": "OAuth2 client secret", "x-secret": true, "x-order": 7, "x-group": "Catalog", "x-depends-on": {"auth_type": "oauth2"}},
    "oauth2_server_uri": {"type": "string", "format": "uri", "title": "OAuth2 token endpoint", "description": "Defaults to <catalog>/v1/oauth/tokens when empty.", "x-order": 8, "x-group": "Catalog", "x-depends-on": {"auth_type": "oauth2"}},
    "scope": {"type": "string", "title": "OAuth2 scope", "default": "", "x-order": 9, "x-group": "Catalog", "x-depends-on": {"auth_type": "oauth2"}},
    "s3_endpoint": {"type": "string", "format": "uri", "title": "S3 endpoint", "description": "S3 API endpoint of the object store (leave empty for AWS S3). Inside Docker use the service hostname, e.g. http://s3:9000.", "x-order": 10, "x-group": "Storage", "x-placeholder": "http://s3:9000"},
    "s3_region": {"type": "string", "title": "S3 region", "default": "us-east-1", "x-order": 11, "x-group": "Storage"},
    "s3_access_key_id": {"type": "string", "format": "password", "title": "S3 access key ID", "x-secret": true, "x-order": 12, "x-group": "Storage"},
    "s3_secret_access_key": {"type": "string", "format": "password", "title": "S3 secret access key", "x-secret": true, "x-order": 13, "x-group": "Storage"},
    "s3_path_style": {"type": "boolean", "title": "Path-style addressing", "description": "Required by MinIO-compatible stores; off for AWS S3 and R2.", "default": true, "x-order": 14, "x-group": "Storage"},
    "vended_credentials": {"type": "boolean", "title": "Use catalog-vended storage settings", "description": "Let the catalog hand out storage endpoints and credentials instead of the settings above (Polaris, R2 Data Catalog, S3 Tables).", "default": false, "x-order": 15, "x-group": "Advanced"}
  }
}`

// ParseConfig validates the stored config plus opened secrets.
func ParseConfig(config map[string]any, secrets map[string]string) (Config, error) {
	c := Config{
		CatalogURI:        strings.TrimRight(str(config, "catalog_uri"), "/"),
		Warehouse:         str(config, "warehouse"),
		Prefix:            strings.Trim(str(config, "prefix"), "/"),
		AuthType:          strings.ToLower(str(config, "auth_type")),
		ClientID:          str(config, "client_id"),
		OAuth2ServerURI:   str(config, "oauth2_server_uri"),
		Scope:             str(config, "scope"),
		S3Endpoint:        strings.TrimRight(str(config, "s3_endpoint"), "/"),
		S3Region:          str(config, "s3_region"),
		VendedCredentials: boolean(config, "vended_credentials", false),
		Token:             secrets["token"],
		ClientSecret:      secrets["client_secret"],
		S3AccessKeyID:     secrets["s3_access_key_id"],
		S3SecretAccessKey: secrets["s3_secret_access_key"],
	}
	// backwards tolerant: allow secrets to be supplied in config when callers
	// have already opened them (never stored that way).
	if c.Token == "" {
		c.Token = str(config, "token")
	}
	if c.ClientSecret == "" {
		c.ClientSecret = str(config, "client_secret")
	}
	if c.S3AccessKeyID == "" {
		c.S3AccessKeyID = str(config, "s3_access_key_id")
	}
	if c.S3SecretAccessKey == "" {
		c.S3SecretAccessKey = str(config, "s3_secret_access_key")
	}
	c.S3PathStyle = boolean(config, "s3_path_style", c.S3Endpoint != "")
	if c.AuthType == "" {
		c.AuthType = AuthNone
	}
	if c.S3Region == "" {
		c.S3Region = "us-east-1"
	}
	var errs []error
	if c.CatalogURI == "" {
		errs = append(errs, errors.New("catalog_uri is required"))
	} else if u, err := url.Parse(c.CatalogURI); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		errs = append(errs, errors.New("catalog_uri must be an http(s) URL"))
	}
	if c.Warehouse == "" {
		errs = append(errs, errors.New("warehouse is required"))
	}
	switch c.AuthType {
	case AuthNone:
	case AuthBearer:
		if c.Token == "" {
			errs = append(errs, errors.New("token is required for bearer authentication"))
		}
	case AuthOAuth2:
		if c.ClientID == "" || c.ClientSecret == "" {
			errs = append(errs, errors.New("client_id and client_secret are required for oauth2 authentication"))
		}
		if c.OAuth2ServerURI != "" {
			if u, err := url.Parse(c.OAuth2ServerURI); err != nil || u.Host == "" {
				errs = append(errs, errors.New("oauth2_server_uri must be a URL"))
			}
		}
	default:
		errs = append(errs, fmt.Errorf("auth_type must be one of none, bearer, oauth2 (got %q)", c.AuthType))
	}
	if c.S3Endpoint != "" {
		if u, err := url.Parse(c.S3Endpoint); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, errors.New("s3_endpoint must be an http(s) URL"))
		}
	}
	if (c.S3AccessKeyID == "") != (c.S3SecretAccessKey == "") {
		errs = append(errs, errors.New("s3_access_key_id and s3_secret_access_key must be given together"))
	}
	if !c.VendedCredentials && c.S3AccessKeyID == "" && c.S3Endpoint != "" {
		errs = append(errs, errors.New("s3_access_key_id and s3_secret_access_key are required when an S3 endpoint is set (or enable vended credentials)"))
	}
	if len(errs) > 0 {
		return c, errors.Join(errs...)
	}
	return c, nil
}

// HasS3 reports whether explicit storage settings were given.
func (c Config) HasS3() bool { return c.S3Endpoint != "" || c.S3AccessKeyID != "" }

// StripVended reports whether catalog-vended storage settings must be ignored
// in favour of the explicit ones.
func (c Config) StripVended() bool { return c.HasS3() && !c.VendedCredentials }

// S3Props returns the iceberg-go file IO properties for the explicit storage
// settings (empty when none are configured).
func (c Config) S3Props() map[string]string {
	p := map[string]string{}
	if !c.HasS3() {
		return p
	}
	if c.S3Endpoint != "" {
		p[icebergio.S3EndpointURL] = c.S3Endpoint
		p[icebergio.S3ForceVirtualAddressing] = strconv.FormatBool(!c.S3PathStyle)
	}
	if c.S3AccessKeyID != "" {
		p[icebergio.S3AccessKeyID] = c.S3AccessKeyID
		p[icebergio.S3SecretAccessKey] = c.S3SecretAccessKey
	}
	p[icebergio.S3Region] = c.S3Region
	p[icebergio.S3ClientRegion] = c.S3Region
	return p
}

// Endpoints lists the user-supplied URLs so the API can apply its outbound
// endpoint policy before anything connects.
func (c Config) Endpoints() []string {
	out := []string{c.CatalogURI}
	if c.S3Endpoint != "" {
		out = append(out, c.S3Endpoint)
	}
	if c.OAuth2ServerURI != "" {
		out = append(out, c.OAuth2ServerURI)
	}
	return out
}

// Redact returns a copy of the stored config safe to display. Secrets never
// live in config, but URLs may embed credentials.
func Redact(config map[string]any) map[string]any {
	out := make(map[string]any, len(config))
	for k, v := range config {
		switch k {
		case "token", "client_secret", "s3_access_key_id", "s3_secret_access_key":
			continue
		case "catalog_uri", "s3_endpoint", "oauth2_server_uri":
			if s, ok := v.(string); ok {
				out[k] = core.RedactURI(s)
				continue
			}
		}
		out[k] = v
	}
	return out
}

func str(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	switch v := m[key].(type) {
	case string:
		return strings.TrimSpace(v)
	case fmt.Stringer:
		return v.String()
	case nil:
		return ""
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

func boolean(m map[string]any, key string, def bool) bool {
	if m == nil {
		return def
	}
	switch v := m[key].(type) {
	case bool:
		return v
	case string:
		if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
			return b
		}
	case float64:
		return v != 0
	}
	return def
}

func init() {
	dest.Register(dest.Descriptor{
		Type:        TypeName,
		Title:       "Apache Iceberg (REST catalog + S3)",
		Description: "Any Iceberg REST catalog (Nessie, Lakekeeper, Polaris, R2 Data Catalog, Glue) with S3-compatible storage.",
		Schema:      json.RawMessage(Schema),
		New: func(ctx context.Context, config map[string]any, secrets map[string]string) (dest.Destination, error) {
			cfg, err := ParseConfig(config, secrets)
			if err != nil {
				return nil, err
			}
			return New(ctx, cfg)
		},
		Redact: Redact,
	})
}
