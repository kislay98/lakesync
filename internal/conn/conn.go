// Package conn opens sources and destinations from saved connections: it unseals
// their secrets and hands the generic config to the registered type. It also
// derives the DuckDB attach configuration for a destination.
package conn

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"lakesync/internal/dest"
	"lakesync/internal/query"
	"lakesync/internal/secrets"
	"lakesync/internal/source"
	"lakesync/internal/store"
)

// Secrets unseals the secret fields of a connection.
func Secrets(sec secrets.SecretStore, c *store.Connection) (map[string]string, error) {
	out := map[string]string{}
	if len(c.SecretCiphertext) == 0 {
		return out, nil
	}
	pt, err := sec.Open(c.SecretCiphertext)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(pt, &out); err != nil {
		return nil, fmt.Errorf("secrets of connection %s are corrupt", c.ID)
	}
	return out, nil
}

// SealSecrets seals a map of secret fields.
func SealSecrets(sec secrets.SecretStore, m map[string]string) ([]byte, error) {
	if len(m) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return sec.Seal(b)
}

// OpenSource instantiates the source behind a connection row.
func OpenSource(ctx context.Context, sec secrets.SecretStore, c *store.Connection) (source.Source, error) {
	if c.Kind != "source" {
		return nil, fmt.Errorf("connection %s is not a source", c.Name)
	}
	s, err := Secrets(sec, c)
	if err != nil {
		return nil, err
	}
	return source.Open(ctx, c.Type, c.Config, s)
}

// OpenDestination instantiates the destination behind a connection row.
func OpenDestination(ctx context.Context, sec secrets.SecretStore, c *store.Connection) (dest.Destination, error) {
	if c.Kind != "destination" {
		return nil, fmt.Errorf("connection %s is not a destination", c.Name)
	}
	s, err := Secrets(sec, c)
	if err != nil {
		return nil, err
	}
	return dest.Open(ctx, c.Type, c.Config, s)
}

// CatalogConfig derives the DuckDB attach configuration for an Iceberg REST
// destination from its generic config and secrets.
func CatalogConfig(c *store.Connection, s map[string]string) (query.CatalogConfig, error) {
	if c.Type != "iceberg_rest" {
		return query.CatalogConfig{}, fmt.Errorf("destination type %s cannot be attached to DuckDB", c.Type)
	}
	str := func(k string) string {
		if v, ok := c.Config[k]; ok && v != nil {
			return strings.TrimSpace(fmt.Sprint(v))
		}
		return ""
	}
	boolean := func(k string, def bool) bool {
		v, ok := c.Config[k]
		if !ok || v == nil {
			return def
		}
		switch x := v.(type) {
		case bool:
			return x
		case string:
			return strings.EqualFold(x, "true")
		}
		return def
	}
	cfg := query.CatalogConfig{
		URI:               str("catalog_uri"),
		Warehouse:         str("warehouse"),
		Prefix:            str("prefix"),
		AuthType:          str("auth_type"),
		Token:             s["token"],
		ClientID:          str("client_id"),
		ClientSecret:      s["client_secret"],
		OAuth2ServerURI:   str("oauth2_server_uri"),
		Scope:             str("scope"),
		S3Endpoint:        str("s3_endpoint"),
		S3Region:          str("s3_region"),
		S3AccessKey:       s["s3_access_key_id"],
		S3SecretKey:       s["s3_secret_access_key"],
		S3PathStyle:       boolean("s3_path_style", str("s3_endpoint") != ""),
		VendedCredentials: boolean("vended_credentials", false),
	}
	if cfg.URI == "" {
		return cfg, fmt.Errorf("destination %s has no catalog URI", c.Name)
	}
	if cfg.AuthType == "" {
		cfg.AuthType = "none"
	}
	return cfg, nil
}

// Attach makes the destination available in DuckDB and returns its alias.
func Attach(ctx context.Context, eng *query.Engine, sec secrets.SecretStore, c *store.Connection, staleness ...int) (string, error) {
	s, err := Secrets(sec, c)
	if err != nil {
		return "", err
	}
	cfg, err := CatalogConfig(c, s)
	if err != nil {
		return "", err
	}
	alias := query.Alias(c.ID)
	if err := eng.Attach(ctx, alias, cfg); err != nil {
		return "", err
	}
	return alias, nil
}
