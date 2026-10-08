# Phase 0: spike results

Run on 2026-10-09 against the local compose stack (`deploy/local/docker-compose.yml`) from an
Apple M4 Pro (macOS, arm64) with Go 1.27.1. The throwaway program lives in `hack/phase0/`
(`go run . <step>`); each step is a separate process so that step (b) genuinely reads a
checkpoint written by an earlier process.

| Component | Version | Notes |
|---|---|---|
| iceberg-go | v0.7.0 (released 2026-10-01) | REST catalog, Arrow appends, snapshot properties, schema evolution, partitioned fan-out writes, data-file rewrite (compaction), snapshot expiry, orphan-file planning |
| DuckDB (Go bindings) | duckdb-go v2.10506.0 = DuckDB v1.5.6 | `iceberg` and `httpfs` extensions; Iceberg writes need an attached REST catalog |
| Project Nessie | 0.109.0 | Iceberg REST at `/iceberg`; version store in Postgres (JDBC2) |
| Object storage | RustFS (MinIO-compatible S3 API) | MinIO's own images were withdrawn from public registries in 2026, see DECISIONS.md |
| MongoDB | 8.0, single-node replica set | |

## Results

| Step | Goal | Result |
|---|---|---|
| a | iceberg-go creates a table in Nessie over REST and appends Arrow data | **Pass.** Format v2 table partitioned by `day(_cdc_ingested_at)`; a 3-row Arrow batch was appended (Parquet written through the S3 API to RustFS, commit through the REST catalog). |
| b | Checkpoint stored atomically with an append and read back after a process restart | **Pass.** Snapshot summary properties (`lakesync.seq`, `lakesync.phase`, `lakesync.snapshot_cursor`, `lakesync.bookmark`, ...) are set on the append and read back by a fresh process from `CurrentSnapshot().Summary.Properties`. An append that is staged but never committed leaves no snapshot (its data file becomes an orphan). A commit from stale base metadata is rejected by the catalog (`Requirement failed: snapshot id changed`), so retries must reload the table first. |
| c | Schema evolution adds a column | **Pass.** `Transaction.UpdateSchema().AddColumn()` adds a nullable column without rewriting data; a following append uses the new column. |
| d | DuckDB attaches the same REST catalog with S3 credentials and reads the table | **Pass.** `ATTACH 'warehouse' AS cat (TYPE iceberg, ENDPOINT ..., AUTHORIZATION_TYPE 'none', ACCESS_DELEGATION_MODE 'none')` with a separate `TYPE s3` secret (`URL_STYLE 'path'`). DuckDB sees new columns and partitioned files. |
| e | DuckDB runs an upsert with update and delete on an Iceberg table | **Pass, with a change.** DuckDB 1.5.6's Iceberg `MERGE INTO` supports only a single UPDATE/DELETE action, so the merge is expressed as `DELETE` (keys with a newer event) + `INSERT` (latest non-delete version) inside one DuckDB transaction. Both statements are guarded by `_cdc_seq`, so re-running is a no-op (verified). DuckDB writes positional delete files (merge-on-read). |
| f | iceberg-go and DuckDB each see the other's commits | **Pass.** iceberg-go appends on top of DuckDB's commits and vice versa; a fresh DuckDB connection sees iceberg-go's latest snapshot immediately. iceberg-go reads DuckDB-written data and positional delete files once the manifest's lowercase `parquet` format string is normalised (see below). |
| g | Maintenance support | See table below. |

### Maintenance operations

| Operation | iceberg-go 0.7.0 | DuckDB 1.5.6 | Decision |
|---|---|---|---|
| Compact data files (raw) | `Table.RewriteDataFiles` with task groups; verified 4 files -> 1, rows unchanged | No | iceberg-go |
| Rewrite clean table applying positional deletes | `RewriteDataFiles` removes the referenced delete files; verified 2 files + 1 delete file -> 1 file, rows unchanged, DuckDB reads the result | No | iceberg-go |
| Expire snapshots | `Transaction.ExpireSnapshots(WithRetainLast, WithOlderThan)` | No | iceberg-go, **only when the table property `gc.enabled` is not `false`** |
| Orphan file cleanup | `Table.PlanOrphanFiles` (dry run) and `DeleteOrphanFiles` | No | iceberg-go plan always (for metrics); deletion only when `gc.enabled` is not `false` |

## Findings that changed the design

1. **Catalog-vended storage settings override client settings.** Both iceberg-go (REST response `config` is merged over client properties) and DuckDB follow the S3 endpoint the catalog vends. Nessie vends its own internal endpoint (`http://s3:9000`), unreachable from the host. The product therefore makes the user's S3 settings win: the iceberg-go HTTP transport strips `s3.*`/`client.region`/`storage-credentials` from table responses when the user configured storage explicitly, DuckDB attaches with `ACCESS_DELEGATION_MODE 'none'`, and the local Nessie is configured with `external-endpoint=http://localhost:9000` for third-party tools run on the host.
2. **Nessie sets `gc.enabled=false` on every table** because its commit history may still reference files; iceberg-go refuses to expire snapshots or delete orphans on such tables. On Nessie, snapshot expiry and orphan deletion are the job of the Nessie GC tool. The app detects the property and reports maintenance as "managed by the catalog" instead of failing. Catalogs that leave GC enabled (Lakekeeper, Polaris, ...) get the full in-app maintenance.
3. **Nessie exposes only the current snapshot** in the Iceberg metadata it serves (history lives in Nessie's commit log). Snapshot counts on Nessie are therefore always 1, and the checkpoint must survive maintenance commits: every commit the app makes (append, compaction, rewrite) re-stamps the latest checkpoint into its snapshot summary, and the same values are also written as table properties in the same transaction.
4. **DuckDB writes `file_format` as lowercase `parquet`** in manifest entries; iceberg-go compares against `PARQUET` case-sensitively (`table/internal/interfaces.go`) and refuses to open such files. The app wraps planned files in a `DataFile` adapter that reports `PARQUET` before handing them to iceberg-go's reader or compaction (`normalizeTasks` in the spike). Worth an upstream fix in iceberg-go.
5. **Format version:** DuckDB ignored `'format-version' = '3'` on CREATE TABLE against Nessie (the table came back as v2). iceberg-go and DuckDB both read and write v2 reliably, so **tables use format v2**.
6. **DuckDB `MERGE INTO` limitation** (see step e). The DELETE+INSERT pair in one transaction gives the same idempotent semantics.
7. **Stale-base commits are rejected** rather than merged (step b). The writer retries by reloading table metadata, which is also what serialises the merger and maintenance per table together with the Postgres advisory lock.

## Operational notes

- The DuckDB extensions were installed from the official repository at first use (`~/.duckdb/extensions/v1.5.6/osx_arm64/`). The container image bundles them (`iceberg`, `httpfs`, `avro`, `parquet` is built in) so nothing is downloaded at runtime.
- Nessie needs its S3 secret passed as the literal dotted property names (`nessie-catalog-secrets.s3-access-key.name`) rather than the upper-cased environment form.
- Nessie's health endpoints are on its management port (9000 inside the container), not on 19120.
