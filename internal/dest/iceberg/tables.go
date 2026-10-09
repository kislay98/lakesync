package iceberg

import (
	"context"
	"errors"
	"fmt"

	"github.com/apache/iceberg-go"
	"github.com/apache/iceberg-go/catalog"

	"lakesync/internal/core"
)

// Table property keys written by this destination.
const (
	PropRole           = core.CheckpointPrefix + "role"   // raw | clean
	PropEntity         = core.CheckpointPrefix + "entity" // source entity
	PropLastCompaction = core.CheckpointPrefix + "last_compaction"
	PropMaintenance    = core.CheckpointPrefix + "maintenance" // snapshot summary: job kind
	RoleRaw            = "raw"
	RoleClean          = "clean"
	partitionFieldName = core.ColIngestedAt + "_day"
	formatVersion      = "2"
)

// buildSchema assembles the Iceberg schema of a raw or clean table.
func buildSchema(role string, user core.Schema) (*iceberg.Schema, error) {
	var fields []iceberg.NestedField
	id := 1
	for _, m := range core.MetaColumns {
		if (role == RoleRaw && !m.Raw) || (role == RoleClean && !m.Clean) {
			continue
		}
		t, err := icebergType(m.Type)
		if err != nil {
			return nil, err
		}
		required := false
		switch m.Name {
		case core.ColKey, core.ColOp, core.ColSeq, core.ColGeneration, core.ColIngestedAt:
			required = true
		}
		fields = append(fields, iceberg.NestedField{ID: id, Name: m.Name, Type: t, Required: required, Doc: "lakesync metadata"})
		id++
	}
	for _, c := range user.Columns {
		t, err := icebergType(c.Type)
		if err != nil {
			return nil, fmt.Errorf("column %s: %w", c.Name, err)
		}
		fields = append(fields, iceberg.NestedField{ID: id, Name: c.Name, Type: t, Required: false, Doc: c.SourceField})
		id++
	}
	return iceberg.NewSchema(0, fields...), nil
}

func tableProps(role, entity string) iceberg.Properties {
	return iceberg.Properties{
		"format-version":                  formatVersion,
		"write.parquet.compression-codec": "zstd",
		PropRole:                          role,
		PropEntity:                        entity,
	}
}

// EnsureTables creates the namespace and both tables when missing.
func (d *Destination) EnsureTables(ctx context.Context, t core.TableSpec, schema core.Schema) error {
	cat, err := d.catalog(ctx)
	if err != nil {
		return err
	}
	ns := namespaceIdent(t.Namespace)
	exists, err := cat.CheckNamespaceExists(ctx, ns)
	if err != nil {
		return fmt.Errorf("check namespace %s: %w", t.Namespace, sanitizeErr(err, d.cfg))
	}
	if !exists {
		if err := cat.CreateNamespace(ctx, ns, nil); err != nil && !errors.Is(err, catalog.ErrNamespaceAlreadyExists) {
			return fmt.Errorf("create namespace %s: %w", t.Namespace, sanitizeErr(err, d.cfg))
		}
	}
	if err := d.ensureTable(ctx, cat, t.Namespace, t.Raw, RoleRaw, t.Entity, schema); err != nil {
		return err
	}
	return d.ensureTable(ctx, cat, t.Namespace, t.Clean, RoleClean, t.Entity, schema)
}

func (d *Destination) ensureTable(ctx context.Context, cat catalog.Catalog, namespace, name, role, entity string, schema core.Schema) error {
	ident := identifier(namespace, name)
	exists, err := cat.CheckTableExists(ctx, ident)
	if err != nil {
		return fmt.Errorf("check table %s.%s: %w", namespace, name, sanitizeErr(err, d.cfg))
	}
	if exists {
		return nil
	}
	sc, err := buildSchema(role, schema)
	if err != nil {
		return err
	}
	opts := []catalog.CreateTableOpt{catalog.WithProperties(tableProps(role, entity))}
	if role == RoleRaw {
		var srcID int
		for _, f := range sc.Fields() {
			if f.Name == core.ColIngestedAt {
				srcID = f.ID
			}
		}
		spec := iceberg.NewPartitionSpec(iceberg.PartitionField{SourceIDs: []int{srcID}, FieldID: 1000, Transform: iceberg.DayTransform{}, Name: partitionFieldName})
		opts = append(opts, catalog.WithPartitionSpec(&spec))
	}
	if _, err := cat.CreateTable(ctx, ident, sc, opts...); err != nil {
		if errors.Is(err, catalog.ErrTableAlreadyExists) {
			return nil
		}
		return fmt.Errorf("create table %s.%s: %w", namespace, name, sanitizeErr(err, d.cfg))
	}
	d.log.Info("created table", "namespace", namespace, "table", name, "role", role, "columns", len(sc.Fields()))
	return nil
}

// CurrentSchema returns the user columns the raw table has now.
func (d *Destination) CurrentSchema(ctx context.Context, t core.TableSpec) (core.Schema, error) {
	tbl, err := d.loadTable(ctx, t.Namespace, t.Raw)
	if err != nil {
		if isNoSuchTable(err) {
			return core.Schema{}, nil // no table yet: the engine infers the schema
		}
		return core.Schema{}, err
	}
	return userColumns(tbl.Schema(), d.log), nil
}

// EvolveSchema adds columns to the raw table, then the clean table. Columns that
// already exist are skipped, so the call is idempotent and safe to retry.
func (d *Destination) EvolveSchema(ctx context.Context, t core.TableSpec, add []core.Column) error {
	if len(add) == 0 {
		return nil
	}
	for _, name := range []string{t.Raw, t.Clean} {
		err := withRetry(ctx, 5, func() error {
			tbl, err := d.loadTable(ctx, t.Namespace, name)
			if err != nil {
				return err
			}
			existing := map[string]bool{}
			for _, f := range tbl.Schema().Fields() {
				existing[f.Name] = true
			}
			tx := tbl.NewTransaction()
			us := tx.UpdateSchema(true, false)
			n := 0
			for _, c := range add {
				if existing[c.Name] {
					continue
				}
				it, err := icebergType(c.Type)
				if err != nil {
					return fmt.Errorf("column %s: %w", c.Name, err)
				}
				us = us.AddColumn([]string{c.Name}, it, c.SourceField, false, nil)
				n++
			}
			if n == 0 {
				return nil
			}
			if err := us.Commit(); err != nil {
				return fmt.Errorf("stage schema update on %s.%s: %w", t.Namespace, name, sanitizeErr(err, d.cfg))
			}
			if _, err := tx.Commit(ctx); err != nil {
				return fmt.Errorf("commit schema update on %s.%s: %w", t.Namespace, name, mapCommitErr(err, d.cfg))
			}
			d.log.Info("schema evolved", "namespace", t.Namespace, "table", name, "added", n)
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// DropTables removes both tables; missing tables are not an error.
func (d *Destination) DropTables(ctx context.Context, t core.TableSpec) error {
	cat, err := d.catalog(ctx)
	if err != nil {
		return err
	}
	for _, name := range []string{t.Raw, t.Clean} {
		if err := cat.DropTable(ctx, identifier(t.Namespace, name)); err != nil && !errors.Is(err, catalog.ErrNoSuchTable) {
			return fmt.Errorf("drop table %s.%s: %w", t.Namespace, name, sanitizeErr(err, d.cfg))
		}
	}
	return nil
}
