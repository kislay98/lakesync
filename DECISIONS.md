# Decision log

One entry per decision: date, decision, alternatives, reason. Newest at the bottom.

## 2026-10-09: Container runtime for development is Colima, not Docker Desktop
- **Decision:** Development on the owner's Mac uses Colima (`colima start`) as the Docker engine.
- **Alternatives:** Docker Desktop (its VM disk could not be resized due to a file-permission error on this machine), Podman.
- **Reason:** Colima was already installed, starts cleanly and the `docker`, `docker compose` and `buildx` CLIs work unchanged. Nothing in the repo depends on it; any Docker engine works.

## 2026-10-09: Commit directly to `main`, often, without review gates
- **Decision:** Follow the brief: milestone commits on a single `main` branch with clear messages and no AI attribution.
- **Alternatives:** Leave everything uncommitted for the owner to review and commit.
- **Reason:** The owner confirmed this explicitly before the build started.

## 2026-10-09: Library versions
- **Decision:** `apache/iceberg-go` v0.7.0, `duckdb/duckdb-go` v2.10506.0 (DuckDB 1.5.6), `mongo-driver/v2` v2.9.2, `pgx/v5` v5.11.0, `goose/v3` v3.28.0, Nessie 0.109.0, MongoDB 8.0, Postgres 17.
- **Alternatives:** iceberg-go 0.6.0 (the brief's minimum), `marcboeker/go-duckdb` (the previous home of the DuckDB bindings).
- **Reason:** Latest stable of each at build time; iceberg-go 0.7.0 adds data-file rewrite, orphan cleanup and partitioned writes, which the maintenance jobs need. `duckdb/duckdb-go` is the official successor of `marcboeker/go-duckdb`.

## 2026-10-09: Local object storage is RustFS, with MinIO as a documented alternative
- **Decision:** `deploy/local` runs RustFS (`rustfs/rustfs`), a MinIO-compatible S3 server, as the local S3 store. The bucket is created by a one-shot `amazon/aws-cli` job.
- **Alternatives:** MinIO (`minio/minio`, fixed by the brief), Chainguard's MinIO build (`cgr.dev/chainguard/minio`), SeaweedFS, Garage, Versity gateway.
- **Reason:** MinIO was archived in 2026 and its images were removed from Docker Hub and quay.io in September 2026 (`pull access denied for minio/minio`, quay returns 401). The brief's intent is an S3-compatible local store reached only through the S3 API, which RustFS satisfies with the same configuration shape. The Chainguard MinIO image still pulls anonymously and is documented in `docs/destinations.md` for anyone who wants MinIO specifically; the app itself only speaks the S3 API, so the choice is configuration.

## 2026-10-09: Nessie keeps its version store in Postgres
- **Decision:** Nessie uses `JDBC2` against a `nessie` database in the same Postgres container (created by `deploy/local/postgres-init.sql`).
- **Alternatives:** In-memory (default; loses the catalog on restart), RocksDB on a volume.
- **Reason:** Catalog state must survive container restarts for the crash and restart tests, and Postgres is already in the stack.

## 2026-10-09: User-supplied storage settings take precedence over catalog-vended ones
- **Decision:** When a destination has explicit S3 settings, the app strips `s3.*`, `client.region` and `storage-credentials` from REST catalog table responses before iceberg-go merges them, and DuckDB attaches with `ACCESS_DELEGATION_MODE 'none'`. The local Nessie additionally advertises `external-endpoint=http://localhost:9000` for other tools run on the host.
- **Alternatives:** Rely on vended configuration (what both libraries do by default), run everything inside Docker.
- **Reason:** Phase 0 showed both libraries follow the endpoint the catalog vends; Nessie vends its own in-network endpoint, which the host cannot reach. The brief has the user enter the S3 endpoint, so that value must win. See `docs/phase0.md`.

## 2026-10-09: Raw-to-clean merge is DELETE + INSERT in one DuckDB transaction
- **Decision:** The merger runs `DELETE FROM clean WHERE EXISTS (newer event in batch)` followed by `INSERT ... WHERE NOT EXISTS (same or newer row in clean)` inside one DuckDB transaction, instead of a three-clause `MERGE INTO`.
- **Alternatives:** `MERGE INTO` (DuckDB 1.5.6 supports only one UPDATE/DELETE action against Iceberg), two merges.
- **Reason:** Same idempotent semantics (both statements are guarded by `_cdc_seq`), one commit, proven in Phase 0 step (e).

## 2026-10-09: Iceberg format v2
- **Decision:** All tables are created with `format-version=2`.
- **Alternatives:** v3 (deletion vectors, row lineage).
- **Reason:** The brief asks for v2 unless every component handles v3; DuckDB ignored the v3 request against Nessie in Phase 0, so v3 was not proven end to end.

## 2026-10-09: Checkpoint is stored in the snapshot summary and as table properties, in the same commit
- **Decision:** Every commit to a raw table carries `lakesync.*` checkpoint keys both in the snapshot summary (the brief's preferred place) and as table properties set in the same transaction. Maintenance commits re-stamp the latest checkpoint.
- **Alternatives:** Snapshot summary only.
- **Reason:** Nessie serves only the current snapshot, so a maintenance commit without the checkpoint would hide it; table properties survive any commit and any catalog. Reading prefers the table properties and falls back to the summary.

## 2026-10-09: Snapshot expiry and orphan deletion are skipped when the catalog disables GC
- **Decision:** Maintenance checks the table property `gc.enabled`; when it is `false` (Nessie sets this on every table) the app reports "managed by the catalog" and only computes the orphan-file metrics, never deleting.
- **Alternatives:** Force `gc.enabled=true` (would let the app delete files still referenced by Nessie's commit history), run Nessie GC from the app (Nessie-specific, violates the catalog-neutral rule).
- **Reason:** Correctness first; iceberg-go refuses these operations for the same reason.

## 2026-10-09: Lowercase file formats from DuckDB are normalised before iceberg-go reads them
- **Decision:** Planned scan tasks are wrapped in a `DataFile` adapter that reports `PARQUET` before being passed to iceberg-go's reader or `RewriteDataFiles`.
- **Alternatives:** Fork iceberg-go, avoid reading DuckDB-written tables from Go altogether.
- **Reason:** Minimal, contained in the destination package, and only needed for clean-table maintenance. Worth an upstream fix.

## 2026-10-09: Module path is `lakesync`
- **Decision:** The Go module is named `lakesync` (no host prefix).
- **Alternatives:** `github.com/<owner>/lakesync`.
- **Reason:** The owner has not chosen a home for the repository yet; a bare module name keeps the rename to one line in `go.mod` plus import paths, and the product name lives in `core.AppName` and the UI title.

## 2026-10-09: The engine lives in `internal/core/engine`
- **Decision:** `internal/core` holds the neutral model; the sync engine is the subpackage `internal/core/engine`, which imports the `source` and `dest` interface packages.
- **Alternatives:** Everything in `internal/core` (import cycle: the interfaces need the model and the engine needs the interfaces).
- **Reason:** Keeps the brief's layout (engine under core) without a cycle; depguard covers the subpackage.

## 2026-10-09: The destination receives flattened rows, not raw events
- **Decision:** `Destination.AppendRaw` takes a `core.Batch` (events plus their flattened rows and the schema used); flattening and sequence assignment happen in the engine.
- **Alternatives:** The brief's signature with `[]ChangeEvent`, flattening inside each destination.
- **Reason:** Flattening, cast errors and `_cdc_seq` are source- and destination-neutral rules; keeping them in `core` means every destination gets identical semantics and the unit tests cover them once.

## 2026-10-09: DuckDB secret manager is initialised before the configuration lock
- **Decision:** `query.Open` creates and drops a throwaway secret before `SET lock_configuration = true`.
- **Reason:** DuckDB initialises its secret manager lazily; once the configuration is locked that initialisation fails and every later `CREATE SECRET` (needed to attach a catalog) errors with "Secret Storage with name 'memory' already registered". Found while running the first pipeline.

## 2026-10-09: SQL console resolves tables through `search_path`, not `USE`
- **Decision:** Before a console query the connection's `search_path` is set to every namespace of the chosen catalog.
- **Alternatives:** `USE <catalog>` (fails: an Iceberg catalog has no `main` schema), requiring fully qualified names.
- **Reason:** Lets users write `shop.customers` or `customers`, as they would expect.

## 2026-10-09: Lag is "commit time minus source time" of the last batch
- **Decision:** The lag shown per collection and in metrics is the delay between the newest source change in the last committed batch and its commit; while events are buffered it is "now minus the oldest buffered source time".
- **Alternatives:** The brief's literal "now minus source time of the last applied event".
- **Reason:** On an idle source the literal definition grows without bound (the overview showed "26 minutes" on a healthy, idle pipeline), which reads as a problem when nothing is wrong. The chosen definition is the replication delay users mean.

## 2026-10-09: A lost commit response is reconciled from the table's checkpoint
- **Decision:** When `AppendRaw` fails with anything but a clean conflict, the engine reads `LastCheckpoint` back; if the table already carries this batch's sequence number and run id, the batch is treated as committed and is not written twice.
- **Reason:** A REST commit can be accepted by the catalog while the response is lost; retrying blindly would duplicate the batch, giving up would lose it.

## 2026-10-09: Catalog browser counts clean-table rows exactly through DuckDB
- **Decision:** For tables managed by a pipeline the Tables screen shows `count(*)` from DuckDB (5 s budget), not the snapshot summary.
- **Reason:** Summary `total-records` counts rows in data files and ignores positional delete files written by merges, so it over-counts the clean table.

## 2026-10-09: End-to-end tests restart the MongoDB server process, not the container
- **Decision:** In `deploy/local`, mongod runs under a one-line supervisor loop inside its container; the correctness test restarts MongoDB with `db.adminCommand({shutdown: 1})`, which the loop brings back within a second. `docker compose restart` remains a fallback.
- **Alternatives:** `docker compose restart mongodb` from the test.
- **Reason:** On the development machine (Colima), republishing a container's ports after a restart repeatedly broke the host's Docker socket and every forwarded port, which took the whole stack down mid-test. From the application's point of view both are the same event: connections drop, the server comes back with its oplog intact. The brief's wording ("restart the MongoDB container") is met in spirit, not letter, and CI on native Docker can use either.

## 2026-10-09: Fault injection is a wrapper source type, registered only on demand
- **Decision:** `internal/source/faulty` registers `mongodb+faults` when `LAKESYNC_ENABLE_FAULT_INJECTION=true`; faults are requested by creating files in `LAKESYNC_FAULT_DIR`.
- **Reason:** The resync test needs a "history lost" error that a fresh replica set never produces naturally; the wrapper keeps the production binary free of test hooks unless explicitly enabled.
