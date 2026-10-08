package iceberg

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/iceberg-go"
	"github.com/apache/iceberg-go/catalog"
	icebergio "github.com/apache/iceberg-go/io"
	"github.com/apache/iceberg-go/table"

	"lakesync/internal/core"
)

const testNamespace = "lakesync_test"

func randomSuffix() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Test runs the destination checklist (brief 10.1). It never panics and always
// returns every step it managed to run, in order.
func (d *Destination) Test(ctx context.Context) core.TestReport {
	rep := core.TestReport{Info: map[string]string{}}
	step := func(name string, fn func() (string, string, error)) bool {
		start := time.Now()
		msg, warn, err := fn()
		s := core.TestStep{Name: name, Duration: time.Since(start), Message: msg, Status: core.TestOK}
		if warn != "" {
			s.Status = core.TestWarn
			s.Message = warn
		}
		if err != nil {
			s.Status = core.TestFail
			s.Message = sanitizeErr(err, d.cfg).Error()
			s.Hint = hintFor(err, name, d.cfg)
		}
		rep.Add(s)
		return err == nil
	}

	// 1. reach the catalog
	var warehouseLoc string
	if !step("Reach the catalog", func() (string, string, error) {
		loc, msg, err := d.probeConfig(ctx)
		warehouseLoc = loc
		if loc != "" {
			rep.Info["warehouse_location"] = loc
		}
		return msg, "", err
	}) {
		return rep
	}

	// 2. authenticate (builds the client; oauth2 obtains a token here)
	var cat catalogAPI
	if !step("Authenticate", func() (string, string, error) {
		c, err := d.catalog(ctx)
		if err != nil {
			d.resetCatalog()
			return "", "", err
		}
		cat = c
		switch d.cfg.AuthType {
		case AuthNone:
			return "No authentication configured; the catalog accepted anonymous requests", "", nil
		case AuthBearer:
			return "Bearer token accepted", "", nil
		default:
			return "OAuth2 token obtained", "", nil
		}
	}) {
		return rep
	}

	// 3. namespace
	createdNS := false
	nsIdent := namespaceIdent(testNamespace)
	if !step("List or create a test namespace", func() (string, string, error) {
		exists, err := cat.CheckNamespaceExists(ctx, nsIdent)
		if err != nil {
			return "", "", err
		}
		if exists {
			return fmt.Sprintf("Namespace %s exists", testNamespace), "", nil
		}
		if err := cat.CreateNamespace(ctx, nsIdent, nil); err != nil && !errors.Is(err, catalog.ErrNamespaceAlreadyExists) {
			return "", "", err
		}
		createdNS = true
		return fmt.Sprintf("Created namespace %s", testNamespace), "", nil
	}) {
		return rep
	}

	// 4. storage probe (deferred to after table creation when we depend on vended settings)
	storageDone := false
	if warehouseLoc != "" && d.cfg.HasS3() {
		storageDone = true
		if !step("Write and delete an object in storage", func() (string, string, error) {
			fs, err := icebergio.LoadFS(ctx, d.cfg.S3Props(), warehouseLoc)
			if err != nil {
				return "", "", err
			}
			path := strings.TrimRight(warehouseLoc, "/") + "/" + testNamespace + "/.probe-" + randomSuffix()
			return probeStorage(fs, path)
		}) {
			if createdNS {
				_ = cat.DropNamespace(ctx, nsIdent)
			}
			return rep
		}
	}

	// 5. create temporary table
	tblName := "probe_" + randomSuffix()
	ident := identifier(testNamespace, tblName)
	var tbl *table.Table
	cleanup := func() {
		if tbl != nil {
			_ = cat.DropTable(ctx, ident)
		}
		if createdNS {
			_ = cat.DropNamespace(ctx, nsIdent)
		}
	}
	sc := iceberg.NewSchema(0,
		iceberg.NestedField{ID: 1, Name: core.ColKey, Type: iceberg.PrimitiveTypes.String, Required: true},
		iceberg.NestedField{ID: 2, Name: "v", Type: iceberg.PrimitiveTypes.Int64, Required: false},
	)
	if !step("Create a temporary table", func() (string, string, error) {
		t, err := cat.CreateTable(ctx, ident, sc, catalog.WithProperties(iceberg.Properties{"format-version": formatVersion}))
		if err != nil {
			return "", "", err
		}
		tbl = t
		rep.Info["table_location_example"] = t.Location()
		return fmt.Sprintf("Created %s.%s at %s", testNamespace, tblName, t.Location()), "", nil
	}) {
		cleanup()
		return rep
	}
	if !storageDone {
		if !step("Write and delete an object in storage", func() (string, string, error) {
			fs, err := tbl.FS(ctx)
			if err != nil {
				return "", "", err
			}
			return probeStorage(fs, strings.TrimRight(tbl.Location(), "/")+"/.probe-"+randomSuffix())
		}) {
			cleanup()
			return rep
		}
	}

	// 6. append and read one row
	if !step("Append a row and read it back", func() (string, string, error) {
		asc := arrow.NewSchema([]arrow.Field{{Name: core.ColKey, Type: arrow.BinaryTypes.String}, {Name: "v", Type: arrow.PrimitiveTypes.Int64, Nullable: true}}, nil)
		b := array.NewRecordBuilder(memory.DefaultAllocator, asc)
		b.Field(0).(*array.StringBuilder).Append("probe")
		b.Field(1).(*array.Int64Builder).Append(1)
		rec := b.NewRecordBatch()
		b.Release()
		defer rec.Release()
		rdr, err := array.NewRecordReader(asc, []arrow.RecordBatch{rec})
		if err != nil {
			return "", "", err
		}
		defer rdr.Release()
		tx := tbl.NewTransaction()
		if err := tx.Append(ctx, rdr, iceberg.Properties{core.CheckpointPrefix + "probe": "true"}); err != nil {
			return "", "", err
		}
		committed, err := tx.Commit(ctx)
		if err != nil {
			return "", "", err
		}
		tbl = committed
		s := committed.Scan()
		tasks, err := s.PlanFiles(ctx)
		if err != nil {
			return "", "", err
		}
		_, it, err := s.ReadTasks(ctx, normalizeTasks(tasks))
		if err != nil {
			return "", "", err
		}
		var n int64
		for r, err := range it {
			if err != nil {
				return "", "", err
			}
			n += r.NumRows()
			r.Release()
		}
		if n != 1 {
			return "", "", fmt.Errorf("expected 1 row after append, read %d", n)
		}
		return "Wrote one Parquet file, committed one snapshot, read 1 row back", "", nil
	}) {
		cleanup()
		return rep
	}

	// 7. drop
	step("Drop the temporary table", func() (string, string, error) {
		if err := cat.DropTable(ctx, ident); err != nil {
			return "", "", err
		}
		tbl = nil
		return "Dropped " + testNamespace + "." + tblName, "", nil
	})
	if createdNS {
		step("Remove the test namespace", func() (string, string, error) {
			if err := cat.DropNamespace(ctx, nsIdent); err != nil {
				return "", "Could not drop " + testNamespace + ": " + sanitizeErr(err, d.cfg).Error(), nil
			}
			return "Dropped namespace " + testNamespace, "", nil
		})
	}
	return rep
}

// catalogAPI is the subset of the REST catalog used by Test.
type catalogAPI interface {
	CheckNamespaceExists(context.Context, table.Identifier) (bool, error)
	CreateNamespace(context.Context, table.Identifier, iceberg.Properties) error
	DropNamespace(context.Context, table.Identifier) error
	CreateTable(context.Context, table.Identifier, *iceberg.Schema, ...catalog.CreateTableOpt) (*table.Table, error)
	DropTable(context.Context, table.Identifier) error
}

func (d *Destination) resetCatalog() {
	d.mu.Lock()
	d.cat = nil
	d.mu.Unlock()
}

// probeConfig fetches /v1/config over plain HTTP and returns the warehouse
// location when the catalog reports one.
func (d *Destination) probeConfig(ctx context.Context) (location, msg string, err error) {
	u := d.cfg.CatalogURI + "/v1/config"
	if d.cfg.Warehouse != "" {
		u += "?warehouse=" + strings.ReplaceAll(d.cfg.Warehouse, "&", "%26")
	}
	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, u, nil)
	if err != nil {
		return "", "", err
	}
	if d.cfg.AuthType == AuthBearer {
		req.Header.Set("Authorization", "Bearer "+d.cfg.Token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		if d.cfg.AuthType == AuthOAuth2 {
			return "", fmt.Sprintf("Catalog reachable; /v1/config requires authentication (HTTP %d), checked in the next step", resp.StatusCode), nil
		}
		return "", "", fmt.Errorf("HTTP %d from %s", resp.StatusCode, u)
	}
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("HTTP %d from %s: %s", resp.StatusCode, u, strings.TrimSpace(string(body[:min(len(body), 200)])))
	}
	var cfg struct {
		Defaults  map[string]string `json:"defaults"`
		Overrides map[string]string `json:"overrides"`
	}
	if err := json.Unmarshal(body, &cfg); err != nil {
		return "", "", fmt.Errorf("%s did not return the Iceberg REST config document: %w", u, err)
	}
	wh := cfg.Overrides["warehouse"]
	if wh == "" {
		wh = cfg.Defaults["warehouse"]
	}
	if strings.Contains(wh, "://") {
		location = wh
	}
	msg = "Catalog reachable (Iceberg REST config received"
	if wh != "" {
		msg += ", warehouse " + wh
	}
	return location, msg + ")", nil
}

// probeStorage writes, reads and removes one small object.
func probeStorage(fs icebergio.IO, path string) (string, string, error) {
	wfs, ok := fs.(icebergio.WriteFileIO)
	if !ok {
		return "", "", errors.New("storage does not support writes")
	}
	payload := []byte("lakesync storage probe " + time.Now().UTC().Format(time.RFC3339))
	if err := wfs.WriteFile(path, payload); err != nil {
		return "", "", fmt.Errorf("write %s: %w", path, err)
	}
	f, err := fs.Open(path)
	if err != nil {
		return "", "", fmt.Errorf("read back %s: %w", path, err)
	}
	got, err := io.ReadAll(f)
	f.Close()
	if err != nil || string(got) != string(payload) {
		return "", "", fmt.Errorf("read back %s: content mismatch", path)
	}
	if err := fs.Remove(path); err != nil {
		return "", "", fmt.Errorf("delete %s: %w", path, err)
	}
	return "Wrote, read and deleted " + path, "", nil
}

// hintFor maps common failures to plain-English next steps.
func hintFor(err error, stepName string, cfg Config) string {
	if err == nil {
		return ""
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "nosuchbucket") || strings.Contains(msg, "no such bucket") || strings.Contains(msg, "bucket does not exist"):
		return "The bucket in the warehouse location does not exist. Create it in your object store (and make sure the warehouse path points at it)."
	case strings.Contains(msg, "no such host") || strings.Contains(msg, "lookup ") || strings.Contains(msg, "could not resolve"):
		if strings.Contains(stepName, "storage") || strings.Contains(stepName, "row") {
			if !cfg.HasS3() || cfg.VendedCredentials {
				return "The catalog vends a storage endpoint this server cannot resolve. Enter the S3 endpoint explicitly (as reachable from this server) or run the app inside the same network as the storage."
			}
			return "The S3 endpoint hostname cannot be resolved from this server. Check the hostname and DNS."
		}
		return "The catalog hostname cannot be resolved from this server. Check the catalog URI and DNS."
	case strings.Contains(msg, "connection refused") || strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline exceeded") || strings.Contains(msg, "i/o timeout"):
		if strings.Contains(stepName, "storage") || strings.Contains(stepName, "row") {
			return "Is the S3 endpoint reachable from this server? Check the endpoint URL, port and firewall rules."
		}
		return "Is the catalog URL reachable from this server? Check the URL, port and firewall rules."
	case strings.Contains(msg, "401") || strings.Contains(msg, "unauthorized") || strings.Contains(msg, "invalid_client") || strings.Contains(msg, "oauth"):
		return "The catalog rejected the credentials. Check the authentication type, token or client ID/secret, and the OAuth2 token endpoint."
	case strings.Contains(msg, "403") || strings.Contains(msg, "forbidden") || strings.Contains(msg, "accessdenied") || strings.Contains(msg, "access denied") || strings.Contains(msg, "signaturedoesnotmatch") || strings.Contains(msg, "invalidaccesskeyid"):
		if strings.Contains(stepName, "storage") || strings.Contains(stepName, "row") {
			return "Storage refused the request. Check the S3 access key, secret key, region and the bucket policy (the key needs read, write and delete on the warehouse prefix)."
		}
		return "The catalog refused the request. Check that the credentials are allowed to create namespaces and tables in this warehouse."
	case strings.Contains(msg, "404") || strings.Contains(msg, "not found"):
		if strings.Contains(stepName, "catalog") {
			return "The catalog URI must point at the base of the Iceberg REST API (the path before /v1/config). For Nessie that is http://host:19120/iceberg."
		}
		return "The catalog returned 404. Check the warehouse name and the prefix."
	case strings.Contains(msg, "certificate") || strings.Contains(msg, "tls") || strings.Contains(msg, "x509"):
		return "TLS handshake failed. Check the URL scheme (http vs https) and the server certificate."
	case strings.Contains(msg, "config document") || strings.Contains(msg, "invalid character"):
		return "The URL answered but not with an Iceberg REST catalog response. Check the catalog URI (for Nessie use the /iceberg path)."
	}
	return "Check the destination settings and the server logs for details."
}
