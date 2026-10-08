package iceberg

import (
	"context"
	"fmt"
	"hash/fnv"
	"sort"
	"strconv"
	"time"

	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/iceberg-go"
	"github.com/apache/iceberg-go/table"

	"lakesync/internal/core"
	"lakesync/internal/dest"
)

// parquetFile normalises the file format of manifest entries written by engines
// that spell it in lowercase (DuckDB writes "parquet"); iceberg-go compares the
// value case-sensitively against "PARQUET" before opening a file.
type parquetFile struct{ iceberg.DataFile }

func (parquetFile) FileFormat() iceberg.FileFormat { return iceberg.ParquetFile }

// normalizeTasks wraps every data and delete file of the planned tasks.
func normalizeTasks(tasks []table.FileScanTask) []table.FileScanTask {
	out := make([]table.FileScanTask, len(tasks))
	for i, t := range tasks {
		t.File = parquetFile{t.File}
		if len(t.DeleteFiles) > 0 {
			dels := make([]iceberg.DataFile, len(t.DeleteFiles))
			for j, df := range t.DeleteFiles {
				dels[j] = parquetFile{df}
			}
			t.DeleteFiles = dels
		}
		out[i] = t
	}
	return out
}

// ListNamespaces lists top-level namespaces as dotted names.
func (d *Destination) ListNamespaces(ctx context.Context) ([]string, error) {
	cat, err := d.catalog(ctx)
	if err != nil {
		return nil, err
	}
	ids, err := cat.ListNamespaces(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("list namespaces: %w", sanitizeErr(err, d.cfg))
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, joinNamespace(id))
	}
	sort.Strings(out)
	return out, nil
}

// ListTables describes every table of a namespace.
func (d *Destination) ListTables(ctx context.Context, namespace string) ([]dest.TableInfo, error) {
	cat, err := d.catalog(ctx)
	if err != nil {
		return nil, err
	}
	var out []dest.TableInfo
	for id, err := range cat.ListTables(ctx, namespaceIdent(namespace)) {
		if err != nil {
			return nil, fmt.Errorf("list tables in %s: %w", namespace, sanitizeErr(err, d.cfg))
		}
		name := id[len(id)-1]
		tbl, err := d.loadTable(ctx, namespace, name)
		if err != nil {
			d.log.Warn("skipping table that failed to load", "namespace", namespace, "table", name, "error", err)
			continue
		}
		out = append(out, d.tableInfo(ctx, namespace, name, tbl))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (d *Destination) tableInfo(ctx context.Context, namespace, name string, tbl *table.Table) dest.TableInfo {
	info := dest.TableInfo{
		Namespace:   namespace,
		Name:        name,
		Location:    tbl.Location(),
		LastUpdated: snapshotTime(tbl.Metadata().LastUpdatedMillis()),
		Snapshots:   len(tbl.Metadata().Snapshots()),
	}
	snap := tbl.CurrentSnapshot()
	if snap == nil {
		return info
	}
	info.LastUpdated = snapshotTime(snap.TimestampMs)
	haveSummary := false
	if snap.Summary != nil {
		if v, ok := snap.Summary.Properties["total-records"]; ok {
			// records in data files minus rows removed by delete files (merge-on-read)
			info.RowCount = atoi(v) - atoi(snap.Summary.Properties["total-position-deletes"]) - atoi(snap.Summary.Properties["total-equality-deletes"])
			if info.RowCount < 0 {
				info.RowCount = 0
			}
			haveSummary = true
		}
		if v, ok := snap.Summary.Properties["total-files-size"]; ok {
			info.SizeBytes = atoi(v)
		}
	}
	if !haveSummary || info.SizeBytes == 0 {
		if tasks, err := tbl.Scan().PlanFiles(ctx); err == nil {
			var rows, size int64
			for _, t := range tasks {
				rows += t.File.Count()
				size += t.File.FileSizeBytes()
			}
			if !haveSummary {
				info.RowCount = rows
			}
			info.SizeBytes = size
		}
	}
	return info
}

// TableColumns lists every column including metadata columns.
func (d *Destination) TableColumns(ctx context.Context, namespace, name string) ([]dest.ColumnInfo, error) {
	tbl, err := d.loadTable(ctx, namespace, name)
	if err != nil {
		return nil, err
	}
	fields := tbl.Schema().Fields()
	out := make([]dest.ColumnInfo, 0, len(fields))
	for _, f := range fields {
		out = append(out, dest.ColumnInfo{Name: f.Name, Type: f.Type.String(), Required: f.Required, ID: f.ID})
	}
	return out, nil
}

// fileStats summarises the planned files of a table.
type fileStats struct {
	dataFiles   int
	smallFiles  int
	totalBytes  int64
	rows        int64
	deleteFiles int
	partitions  int
	byPartition map[string][]table.FileScanTask
}

func planStats(tasks []table.FileScanTask, smallBytes int64) fileStats {
	st := fileStats{byPartition: map[string][]table.FileScanTask{}}
	seenDel := map[string]bool{}
	for _, t := range tasks {
		st.dataFiles++
		st.rows += t.File.Count()
		st.totalBytes += t.File.FileSizeBytes()
		if t.File.FileSizeBytes() < smallBytes {
			st.smallFiles++
		}
		for _, lists := range [][]iceberg.DataFile{t.DeleteFiles, t.EqualityDeleteFiles, t.DeletionVectorFiles} {
			for _, df := range lists {
				if !seenDel[df.FilePath()] {
					seenDel[df.FilePath()] = true
					st.deleteFiles++
				}
			}
		}
		key := partitionKey(t)
		st.byPartition[key] = append(st.byPartition[key], t)
	}
	st.partitions = len(st.byPartition)
	return st
}

func partitionKey(t table.FileScanTask) string { return fmt.Sprint(t.File.Partition()) }

// TableHealth computes the health metrics of a table. Orphan statistics are
// not included here because scanning storage can be slow; call OrphanStats.
func (d *Destination) TableHealth(ctx context.Context, namespace, name string, cfg dest.MaintenanceConfig) (dest.Health, error) {
	tbl, err := d.loadTable(ctx, namespace, name)
	if err != nil {
		return dest.Health{}, err
	}
	tasks, err := tbl.Scan().PlanFiles(ctx)
	if err != nil {
		return dest.Health{}, fmt.Errorf("plan files of %s.%s: %w", namespace, name, sanitizeErr(err, d.cfg))
	}
	st := planStats(tasks, cfg.SmallFileBytes)
	h := dest.Health{
		DataFiles:     st.dataFiles,
		SmallFiles:    st.smallFiles,
		DeleteFiles:   st.deleteFiles,
		Snapshots:     len(tbl.Metadata().Snapshots()),
		TotalBytes:    st.totalBytes,
		RowCount:      st.rows,
		Partitions:    st.partitions,
		GCEnabled:     tbl.Properties()["gc.enabled"] != "false",
		FormatVersion: tbl.Metadata().Version(),
		Properties:    publicProps(tbl.Properties()),
	}
	if st.dataFiles > 0 {
		h.AvgFileBytes = st.totalBytes / int64(st.dataFiles)
	}
	if snap := tbl.CurrentSnapshot(); snap != nil {
		h.LastCommit = snapshotTime(snap.TimestampMs)
	} else {
		h.LastCommit = snapshotTime(tbl.Metadata().LastUpdatedMillis())
	}
	if v := tbl.Properties()[PropLastCompaction]; v != "" {
		if ts, err := time.Parse(time.RFC3339, v); err == nil {
			h.LastCompaction = ts
		}
	}
	if h.LastCompaction.IsZero() {
		for _, s := range tbl.Metadata().Snapshots() {
			if s.Summary != nil && s.Summary.Properties[PropMaintenance] != "" {
				if ts := snapshotTime(s.TimestampMs); ts.After(h.LastCompaction) {
					h.LastCompaction = ts
				}
			}
		}
	}
	return h, nil
}

// OrphanStats scans storage for files not referenced by the table, older than
// the configured age. It can be slow on large warehouses; call it lazily.
func (d *Destination) OrphanStats(ctx context.Context, namespace, name string, olderThan time.Duration) (files int, bytes int64, err error) {
	tbl, err := d.loadTable(ctx, namespace, name)
	if err != nil {
		return 0, 0, err
	}
	plan, err := tbl.PlanOrphanFiles(ctx, table.WithFilesOlderThan(olderThan))
	if err != nil {
		return 0, 0, fmt.Errorf("plan orphan files of %s.%s: %w", namespace, name, sanitizeErr(err, d.cfg))
	}
	return len(plan.Files()), plan.TotalSizeBytes(), nil
}

// publicProps hides nothing today but keeps the door open for filtering.
func publicProps(p iceberg.Properties) map[string]string {
	out := make(map[string]string, len(p))
	for k, v := range p {
		out[k] = v
	}
	return out
}

// Snapshots lists the snapshots the catalog exposes for a table.
func (d *Destination) Snapshots(ctx context.Context, namespace, name string) ([]dest.SnapshotInfo, error) {
	tbl, err := d.loadTable(ctx, namespace, name)
	if err != nil {
		return nil, err
	}
	var currentID int64
	if cur := tbl.CurrentSnapshot(); cur != nil {
		currentID = cur.SnapshotID
	}
	snaps := tbl.Metadata().Snapshots()
	out := make([]dest.SnapshotInfo, 0, len(snaps))
	for _, s := range snaps {
		info := dest.SnapshotInfo{ID: s.SnapshotID, ParentID: s.ParentSnapshotID, Timestamp: snapshotTime(s.TimestampMs), SchemaID: s.SchemaID, IsCurrent: s.SnapshotID == currentID}
		if s.Summary != nil {
			info.Operation = string(s.Summary.Operation)
			info.Summary = publicProps(s.Summary.Properties)
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Timestamp.After(out[j].Timestamp) })
	return out, nil
}

// contentChecksum reads (_cdc_key, _cdc_seq) of every row and returns the row
// count and an order-independent checksum.
func contentChecksum(ctx context.Context, tbl *table.Table) (rows int64, sum uint64, err error) {
	sc := tbl.Scan(table.WithSelectedFields(core.ColKey, core.ColSeq))
	tasks, err := sc.PlanFiles(ctx)
	if err != nil {
		return 0, 0, err
	}
	_, it, err := sc.ReadTasks(ctx, normalizeTasks(tasks))
	if err != nil {
		return 0, 0, err
	}
	for rec, err := range it {
		if err != nil {
			return 0, 0, err
		}
		keyIdx, seqIdx := -1, -1
		for i, f := range rec.Schema().Fields() {
			switch f.Name {
			case core.ColKey:
				keyIdx = i
			case core.ColSeq:
				seqIdx = i
			}
		}
		if keyIdx < 0 || seqIdx < 0 {
			rec.Release()
			return 0, 0, fmt.Errorf("scan did not return %s and %s", core.ColKey, core.ColSeq)
		}
		keys := rec.Column(keyIdx).(*array.String)
		seqs := rec.Column(seqIdx).(*array.Int64)
		for i := 0; i < int(rec.NumRows()); i++ {
			h := fnv.New64a()
			h.Write([]byte(keys.Value(i)))
			h.Write([]byte{'|'})
			h.Write([]byte(strconv.FormatInt(seqs.Value(i), 10)))
			sum += h.Sum64()
			rows++
		}
		rec.Release()
	}
	return rows, sum, nil
}
