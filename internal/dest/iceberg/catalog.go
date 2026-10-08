package iceberg

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/apache/iceberg-go"
	"github.com/apache/iceberg-go/catalog/rest"
	_ "github.com/apache/iceberg-go/io/gocloud/s3" // registers the s3:// file IO
	"github.com/apache/iceberg-go/table"

	"lakesync/internal/core"
	"lakesync/internal/dest"
)

// Destination implements dest.Destination, dest.Inspector and dest.Maintainer
// for an Iceberg REST catalog.
type Destination struct {
	cfg Config
	log *slog.Logger

	mu  sync.Mutex
	cat *rest.Catalog
}

var (
	_ dest.Destination = (*Destination)(nil)
	_ dest.Inspector   = (*Destination)(nil)
	_ dest.Maintainer  = (*Destination)(nil)
)

// New returns a destination. It does not connect until first use, so that
// Test can report a reachability failure step by step.
func New(_ context.Context, cfg Config) (*Destination, error) {
	return &Destination{cfg: cfg, log: slog.Default().With("component", "dest.iceberg")}, nil
}

// Config returns the configuration (secrets included; never log it).
func (d *Destination) Config() Config { return d.cfg }

// catalog returns the shared REST catalog, connecting on first use.
func (d *Destination) catalog(ctx context.Context) (*rest.Catalog, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cat != nil {
		return d.cat, nil
	}
	cat, err := d.cfg.openCatalog(ctx)
	if err != nil {
		return nil, err
	}
	d.cat = cat
	return cat, nil
}

// openCatalog builds a fresh REST catalog client from the configuration.
func (c Config) openCatalog(ctx context.Context) (*rest.Catalog, error) {
	opts := []rest.Option{rest.WithWarehouseLocation(c.Warehouse)}
	if c.Prefix != "" {
		opts = append(opts, rest.WithPrefix(c.Prefix))
	}
	switch c.AuthType {
	case AuthBearer:
		opts = append(opts, rest.WithOAuthToken(c.Token))
	case AuthOAuth2:
		opts = append(opts, rest.WithCredential(c.ClientID+":"+c.ClientSecret))
		if c.OAuth2ServerURI != "" {
			u, err := url.Parse(c.OAuth2ServerURI)
			if err != nil {
				return nil, fmt.Errorf("oauth2_server_uri: %w", err)
			}
			opts = append(opts, rest.WithAuthURI(u))
		}
		if c.Scope != "" {
			opts = append(opts, rest.WithScope(c.Scope))
		}
	}
	if c.HasS3() {
		opts = append(opts, rest.WithAdditionalProps(iceberg.Properties(c.S3Props())))
	}
	transport := http.RoundTripper(http.DefaultTransport)
	if c.StripVended() {
		transport = stripVendedStorage{next: transport}
	}
	opts = append(opts, rest.WithCustomTransport(transport))
	cat, err := rest.NewCatalog(ctx, core.AppName, c.CatalogURI, opts...)
	if err != nil {
		return nil, fmt.Errorf("connect to catalog: %w", sanitizeErr(err, c))
	}
	return cat, nil
}

// stripVendedStorage removes catalog-vended storage settings (s3.*, client.region
// and storage-credentials) from table responses so that the user's explicit S3
// settings win. iceberg-go merges response config over client properties.
type stripVendedStorage struct{ next http.RoundTripper }

func (s stripVendedStorage) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := s.next.RoundTrip(req)
	if err != nil || resp == nil || resp.Body == nil || resp.StatusCode != http.StatusOK || !strings.Contains(req.URL.Path, "/tables") {
		return resp, err
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) == nil {
		changed := false
		if raw, ok := m["config"]; ok {
			var cfg map[string]string
			if json.Unmarshal(raw, &cfg) == nil {
				for k := range cfg {
					if strings.HasPrefix(k, "s3.") || k == "client.region" {
						delete(cfg, k)
						changed = true
					}
				}
				if changed {
					m["config"], _ = json.Marshal(cfg)
				}
			}
		}
		if _, ok := m["storage-credentials"]; ok {
			delete(m, "storage-credentials")
			changed = true
		}
		if changed {
			if b, err := json.Marshal(m); err == nil {
				body = b
			}
		}
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Del("Content-Length")
	return resp, nil
}

// identifier splits a dotted namespace into levels and appends the table name.
func identifier(namespace, name string) table.Identifier {
	parts := strings.Split(namespace, ".")
	return append(parts, name)
}

func namespaceIdent(namespace string) table.Identifier { return strings.Split(namespace, ".") }

func joinNamespace(id table.Identifier) string { return strings.Join(id, ".") }

// loadTable always fetches fresh metadata.
func (d *Destination) loadTable(ctx context.Context, namespace, name string) (*table.Table, error) {
	cat, err := d.catalog(ctx)
	if err != nil {
		return nil, err
	}
	tbl, err := cat.LoadTable(ctx, identifier(namespace, name))
	if err != nil {
		return nil, fmt.Errorf("load table %s.%s: %w", namespace, name, sanitizeErr(err, d.cfg))
	}
	return tbl, nil
}

// isConflict reports whether a commit error is a retryable conflict.
func isConflict(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, table.ErrCommitFailed) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "Requirement failed") || strings.Contains(msg, "409")
}

// ErrCommitStateUnknown is returned when the catalog may or may not have applied
// a commit. Callers must read LastCheckpoint before retrying.
var ErrCommitStateUnknown = errors.New("commit state unknown: read the checkpoint back before retrying")

// mapCommitErr converts iceberg-go commit failures into the core error model.
func mapCommitErr(err error, cfg Config) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, rest.ErrCommitStateUnknown) {
		return fmt.Errorf("%w: %v", ErrCommitStateUnknown, sanitizeErr(err, cfg))
	}
	if isConflict(err) {
		return &core.CommitConflictError{Err: sanitizeErr(err, cfg)}
	}
	return sanitizeErr(err, cfg)
}

// sanitizeErr strips secrets that libraries may echo in error text.
func sanitizeErr(err error, cfg Config) error {
	if err == nil {
		return nil
	}
	msg := core.RedactText(err.Error(), cfg.Token, cfg.ClientSecret, cfg.S3SecretAccessKey, cfg.S3AccessKeyID)
	if msg == err.Error() {
		return err
	}
	return errors.New(msg)
}

// withRetry runs fn until it succeeds, the attempts are exhausted or a
// non-conflict error occurs. fn must reload table metadata on every call.
func withRetry(ctx context.Context, attempts int, fn func() error) error {
	var err error
	for i := 0; i < attempts; i++ {
		if err = fn(); err == nil || !core.IsCommitConflict(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(50*(i+1)) * time.Millisecond):
		}
	}
	return err
}

// Close releases the catalog client.
func (d *Destination) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cat != nil {
		err := d.cat.Close()
		d.cat = nil
		return err
	}
	return nil
}
