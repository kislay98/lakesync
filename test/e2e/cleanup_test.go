//go:build e2e

package e2e

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/apache/iceberg-go"
	"github.com/apache/iceberg-go/catalog/rest"
	"github.com/apache/iceberg-go/table"
)

// testNamespacePrefixes are the catalog namespaces the suites create.
var testNamespacePrefixes = []string{"e2e_", "bench_", "lakesync_it_", "lakesync_test", "qit_", "phase0"}

// TestMain drops leftover test namespaces from the shared catalog before and
// after the suites, so aborted runs do not pile up tables in the catalog browser.
func TestMain(m *testing.M) {
	cleanupCatalog()
	code := m.Run()
	cleanupCatalog()
	os.Exit(code)
}

func cleanupCatalog() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cat, err := rest.NewCatalog(ctx, "cleanup", catalogURI, rest.WithWarehouseLocation("warehouse"),
		rest.WithAdditionalProps(iceberg.Properties{"s3.endpoint": s3Endpoint, "s3.access-key-id": s3Key, "s3.secret-access-key": s3Secret, "s3.region": "us-east-1", "s3.force-virtual-addressing": "false"}))
	if err != nil {
		return
	}
	nss, err := cat.ListNamespaces(ctx, nil)
	if err != nil {
		return
	}
	for _, ns := range nss {
		name := strings.Join(ns, ".")
		drop := false
		for _, p := range testNamespacePrefixes {
			if strings.HasPrefix(name, p) {
				drop = true
			}
		}
		if !drop {
			continue
		}
		for ident, err := range cat.ListTables(ctx, ns) {
			if err != nil {
				break
			}
			_ = cat.DropTable(ctx, table.Identifier(ident))
		}
		_ = cat.DropNamespace(ctx, ns)
	}
}
