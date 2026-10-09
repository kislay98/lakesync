# Architecture

lakesync is one Go binary that serves a React UI and a JSON API, and runs a worker that
copies MongoDB collections into Apache Iceberg tables and keeps them in sync through change
streams. DuckDB, embedded in the same binary, merges the raw change log into clean tables and
powers the data viewer and the SQL console.

```
Browser (React, TanStack Query/Table/Virtual, SSE)
   │ /api (JSON, cookie session + CSRF), /api/events (SSE)
   ▼
lakesync serve ─── internal/api ──── Postgres (users, connections, pipelines, runs, logs)
   │                                   ▲ LISTEN/NOTIFY event bus (also used when API and worker are split)
   ├── internal/core/engine  (worker)  │
   │     reconcile loop: desired state in Postgres  ⇄  one runner per (pipeline, collection), guarded by a
   │     Postgres advisory lock so two workers never run the same collection
   │     runner lifecycle: bookmark → full copy (resumable) → stream → flush every interval → merge → maintenance
   │
   ├── internal/source/mongodb   change streams, _id-range full copy, test checklist     (Source interface)
   ├── internal/dest/iceberg     iceberg-go: tables, Arrow appends + checkpoint, maintenance (Destination interface)
   └── internal/query            DuckDB: ATTACH REST catalog, raw→clean merge, viewer, SQL console
                                   │
                 Iceberg REST catalog (Nessie locally) + S3 API storage (RustFS locally)
```

## Packages

| Package | Responsibility | May import cloud SDKs? |
|---|---|---|
| `internal/core` | Neutral model: entities, change events, documents and values, checkpoints, schema inference, flattening, column-name rules, redaction | no |
| `internal/core/engine` | Worker, reconcile loop, per-collection runner, batching, merge orchestration, maintenance scheduling, metrics | no |
| `internal/source` | `Source` interface, `EventIterator`, type registry with JSON-schema forms | no |
| `internal/source/mongodb` | MongoDB implementation | no |
| `internal/source/faulty` | Test-only wrapper that injects failures (registered when `LAKESYNC_ENABLE_FAULT_INJECTION=true`) | no |
| `internal/dest` | `Destination`, `Inspector`, `Maintainer` interfaces, type registry | no |
| `internal/dest/iceberg` | Iceberg REST + S3 implementation on iceberg-go | no (iceberg-go's own S3 layer is a transitive dependency) |
| `internal/query` | Embedded DuckDB: attach, merge, rebuild, viewer queries, read-only console | no |
| `internal/api` | HTTP handlers, auth, CSRF, SSE hub, OpenAPI, embedded UI | – |
| `internal/store` | Postgres repositories, migrations (goose), advisory locks, notify bus | – |
| `internal/secrets` | `SecretStore` interface + AES-256-GCM implementation | – |
| `internal/conn` | Opens sources/destinations from saved rows (unseals secrets), derives the DuckDB attach config | – |
| `internal/seed`, `internal/verify` | Test data generator and the MongoDB-vs-Iceberg comparison tool | – |
| `cmd/lakesync` | Subcommands: `serve`, `serve --no-worker`, `worker`, `migrate`, `seed`, `verify`, `gen-key`, `duckdb-install` | – |

The "no" column is enforced by `depguard` in `.golangci.yml`.

## Adding a second source

A source implements `source.Source` (test checklist, entity listing, bookmark, sample,
resumable snapshot, stream) and registers itself with a JSON schema for its form:

```go
source.Register(source.Descriptor{Type: "postgres", Title: "PostgreSQL", Schema: schemaJSON, New: newPostgres, Redact: redact, Parse: parseURI})
```

Everything downstream (engine, destination, API, UI forms) only sees `core.ChangeEvent`s and the
schema, so nothing else changes. The UI renders the form from the schema (`x-secret`, `x-group`,
`x-order`, `x-depends-on` hints) and shows the test checklist the source returns.

## Data model in Iceberg

Each collection becomes two tables in a namespace (default: the MongoDB database name):

- `<collection>__raw`: every change event, append only, partitioned by `day(_cdc_ingested_at)`.
  Columns: `_cdc_key`, `_cdc_op`, `_cdc_seq`, `_cdc_generation`, `_cdc_source_ts`,
  `_cdc_ingested_at`, `_cdc_cast_errors`, `_cdc_doc`, then one column per top-level field.
- `<collection>`: latest version of each document; same columns minus `_cdc_op` and `_cdc_doc`.

Type mapping and conflict rules are in brief section 6; the implementation is `core.InferSchema`,
`core.Fits` and `core.Flatten`.

## Exactly-once

Every flush is **one** Iceberg commit on the raw table. The commit carries the checkpoint
(bookmark, last `_cdc_seq`, generation, phase, snapshot cursor, run id) twice in the same
transaction: as snapshot summary properties and as table properties. On start the runner reads
the checkpoint back from the table (`LastCheckpoint`), never from Postgres. A crash before the
commit means the batch is re-read from the bookmark; a crash after means the checkpoint is
already there. Commits that lose a race get `CommitConflictError` and are retried on fresh
metadata; a commit whose response was lost is reconciled by reading the checkpoint back and
comparing run id and sequence.

The merge into the clean table is a DuckDB transaction of `DELETE` (keys with a newer event) and
`INSERT` (latest non-delete version), both guarded by `_cdc_seq`, so re-running a merge is a
no-op and the watermark stored in Postgres can lag safely.

## Resync and generations

When MongoDB reports that the bookmark can no longer be resumed (`ChangeStreamHistoryLost` and
related errors), or the user asks for it, the runner increments `_cdc_generation`, takes a new
bookmark, copies everything again, then rebuilds the clean table from the new generation's rows
only, so documents deleted during the gap disappear. Incremental merges resume afterwards.

## Maintenance

Runs inside the runner (so it never races with appends or merges) every N minutes, or on demand
from the Table health screen: compact raw partitions with too many small files, rewrite the clean
table to apply positional delete files, expire snapshots daily and remove orphan files weekly
where the catalog allows garbage collection (`gc.enabled`). Rewrites verify row count and a
content checksum before and after.

## Processes

`lakesync serve` runs API + UI + worker. `lakesync serve --no-worker` plus one or more
`lakesync worker` processes split them; advisory locks keep collections exclusive and the
Postgres NOTIFY bus carries live updates to whichever process serves the SSE stream.

## Security

Argon2id password hashes, HttpOnly SameSite=Lax session cookies (Secure outside `DEV_MODE`),
CSRF token header on state-changing requests, login rate limiting, AES-256-GCM sealed secrets
that the API never returns, redaction of connection strings in logs, a read-only SQL console
(DuckDB parses every statement; only SELECT/EXPLAIN run; local filesystem disabled and
configuration locked), and an outbound policy that refuses link-local/metadata addresses and,
unless allowed, private networks.
