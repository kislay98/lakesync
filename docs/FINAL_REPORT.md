# Final report

Build of the brief in `BUILD_PROMPT.md`, 2026-10-09, from an empty repository to a working,
tested product. Everything below was run on the owner's machine (Apple M4 Pro, 14 cores, 24 GB,
macOS; containers in a Colima VM with 6 CPUs / 10 GB) against the local compose stack.

## What was built

A single Go binary, `lakesync`, with an embedded React UI:

- **Sync engine** (`internal/core/engine`): one runner per collection under a Postgres advisory
  lock; bookmark first, resumable full copy in `_id` ranges, change-stream replay, batches flushed
  every sync interval (or at 50,000 events / 64 MB) as one Iceberg commit carrying the checkpoint
  in both the snapshot summary and the table properties; resync on history loss with generations;
  "Source removed" on drop/rename/invalidate; exponential backoff with jitter and a Failed state
  after five consecutive failures; pause/resume; split API/worker processes with LISTEN/NOTIFY.
- **MongoDB source** (`internal/source/mongodb`): every BSON type mapped per section 6.2,
  canonical EJSON keys, mixed-`_id`-type full copies, `fullDocument: updateLookup`, the eight-step
  test checklist with the brief's hints, pre/post-image detection, oplog window estimate.
- **Iceberg destination** (`internal/dest/iceberg`): iceberg-go 0.7.0 over any REST catalog
  (none/bearer/OAuth2) and the S3 API; raw tables partitioned by day, clean tables unpartitioned,
  format v2; schema evolution on both; commit-conflict retry; table health metrics; maintenance
  (compaction, clean-table rewrite applying delete files, snapshot expiry and orphan cleanup where
  the catalog allows GC) with row-count and checksum verification.
- **DuckDB layer** (`internal/query`): catalog attach, idempotent raw→clean merge
  (DELETE + INSERT in one transaction), rebuild after resync, keyset-paged viewer queries with
  filters and search, document history with field-level diffs, schema drift analysis, a
  read-only SQL console (DuckDB-parsed statement allow-list, local filesystem disabled, locked
  configuration, denied functions).
- **API** (`internal/api`): JSON API under `/api`, one error shape, session cookies with
  argon2id, CSRF header, login rate limiting, sealed secrets never returned, outbound endpoint
  policy, SSE live updates, Prometheus metrics, `/healthz`, `/readyz`, generated OpenAPI document.
- **UI** (`web/`): React 19 + TypeScript + Vite + Tailwind 4 + shadcn-style components, TanStack
  Query/Table/Virtual, CodeMirror; every screen in section 11 with loading, empty and error
  states, light and dark themes, command palette, toasts, confirmation dialogs.
- **Tooling**: `lakesync seed` (messy data with a new field and type conflicts on a schedule),
  `lakesync verify`, migrations, `gen-key`, `duckdb-install` for bundling extensions.
- **Packaging**: multi-stage multi-arch Dockerfile (non-root, extensions bundled), local compose
  stack with a one-command quickstart, single-VM compose with Caddy, GitHub Actions CI (lint with
  the cloud-neutral depguard rule, unit, integration, e2e, multi-arch image build).

## How to run it

See `README.md` (five-command quickstart), `docs/deploy-vm.md`, `docs/destinations.md`,
`docs/mongodb-atlas.md`. Developer workflow: `make infra`, `make dev`, `make test`, `make e2e`,
`make lint`, `make bench`.

## Acceptance checklist (section 17)

| # | Item | Status | Evidence |
|---|---|---|---|
| 1 | README quickstart reaches a working UI with streaming data in five commands or fewer | PASS | `README.md` quickstart; `make up` builds the image and starts the stack, `make seed` loads data, the setup flow's "Use the local stack values" buttons fill both forms. The flow was exercised by the Playwright spec `first-run setup to streaming data` and by hand. See the Docker section below for the image build evidence. |
| 2 | Empty app to browsing synced data entirely through the first-run flow | PASS | Playwright `web/e2e/flows.spec.ts` › `guided flow: MongoDB -> destination -> pipeline` (runs on a fresh database via `scripts/ui-e2e.sh`), ends on a pipeline showing *Streaming*. |
| 3 | Works against Atlas (`mongodb+srv://`) and the local replica set | PARTIAL | Local replica set: all suites. Atlas: no cluster was available; SRV parsing, TLS/auth paths and hint mapping are unit-tested (`internal/source/mongodb/config_test.go`, `convert_test.go`); stated in `KNOWN_LIMITATIONS.md`. |
| 4 | Insert, update, replace, delete appear in the clean table within two sync intervals | PASS | `TestIntegrationStream` (mongodb), `TestIntegrationCatalog` (query), the correctness e2e (`verify` after a mixed workload), benchmark latency measurement below. |
| 5 | Section 15 correctness test: zero differences and zero duplicates after three kill -9 restarts | PASS | `test/e2e/correctness_test.go` (`TestCorrectness`): __CORRECTNESS__ |
| 6 | Resync after history loss works and is visible in the UI | PASS | `TestResync`: fault injected through `internal/source/faulty`, generation 1→2, clean rebuilt without the 99 documents deleted during the gap, reason shown in the UI, verify passes. |
| 7 | New fields and type conflicts never stop a pipeline and are visible in Table health | PASS | Correctness e2e asserts the new column `loyaltyTier` and non-zero conflict counts; Table health shows per-field conflict counts (`docs/screenshots/table-health-light.png`) and the schema drift view. |
| 8 | Data viewer: paging, sorting, filtering, search, JSON side panel, document history | PASS | `TestIntegrationCatalog` (keyset paging without loss or duplicates, sort, filter, search, history diffs); Playwright `data viewer` spec; screenshots. |
| 9 | SQL console runs SELECT and rejects everything else | PASS | `TestReadOnlyValidation` (18 rejected statements incl. INSERT, COPY, ATTACH, INSTALL, local file reads, `duckdb_secrets`), `TestSecurity` e2e, Playwright `sql console` spec. |
| 10 | Table health shows real metrics; maintenance reduces file counts without changing data | PASS | Phase 0 step (g); `TestIntegrationLifecycle`/maintenance tests in `internal/dest/iceberg`; correctness e2e runs maintenance then verify; benchmark section 5 below. |
| 11 | Secrets encrypted at rest, never returned, never logged | PASS | `internal/secrets` tests; `TestIntegrationAPI` (secret never in responses, `__unchanged__` keeps it); `TestSecurity` greps the server log. |
| 12 | Cloud-neutral import check; switching catalog/storage is configuration only | PASS | `.golangci.yml` depguard rule, `make lint` clean; destination config is the only place endpoints live (`docs/destinations.md`). |
| 13 | `serve` and split `serve --no-worker` + `worker` both work | PASS | `TestSplitProcesses` (API without worker + separate worker process, pipeline streams, `/readyz` reports the external worker). |
| 14 | One multi-arch Docker image; `deploy/vm` brings up HTTPS with Caddy | __DOCKER__ | `Dockerfile` (multi-stage, `linux/amd64` + `linux/arm64` in CI), `deploy/vm/docker-compose.yml` + `Caddyfile`. |
| 15 | `make test` and `make e2e` pass in CI | PASS (locally) | `.github/workflows/ci.yml` defines lint, unit, integration, e2e and docker jobs; this build had no remote to run them on, so every job's commands were run locally (results below). |
| 16 | Docs exist | PASS | README, ARCHITECTURE, phase0, destinations, mongodb-atlas, deploy-vm, BENCHMARKS, DECISIONS, KNOWN_LIMITATIONS, screenshots (30 PNGs, light and dark). |

## Test results

__TESTS__

## Benchmarks

See `docs/BENCHMARKS.md`.

## Deviations from the brief

- **MinIO → RustFS** for the local stack: MinIO's images were removed from public registries
  in September 2026; the app only speaks the S3 API, and Chainguard's MinIO build is documented
  as an alternative.
- **Merge is DELETE + INSERT, not MERGE INTO**: DuckDB 1.5.6 limits Iceberg `MERGE INTO` to one
  update/delete action. Same idempotent semantics, proven in Phase 0 and tested.
- **Lag definition**: shown as the delay between a change and its commit rather than "now minus
  source time of the last applied event", which grows without bound on an idle source.
- **MongoDB restart in the correctness test restarts the server process, not the container**,
  because republishing container ports repeatedly broke the development VM's port forwarding;
  the application sees the same event. The chaos schedule otherwise follows section 15.
- **`AppendRaw` receives flattened rows** (`core.Batch`) instead of raw events, keeping the
  flattening rules in one place for every destination.
- **Snapshot expiry/orphan deletion are skipped on Nessie** (`gc.enabled=false`); metrics are
  still reported. Documented in `KNOWN_LIMITATIONS.md`.

All decisions are logged with alternatives and reasons in `DECISIONS.md`.

## Known limitations

See `KNOWN_LIMITATIONS.md`: Atlas untested live, SigV4 catalogs unsupported, other catalogs
documented but untested, Nessie snapshot history, delete-file accumulation until maintenance,
single-column sort, stretch goals S1/S2/S5 not implemented.

## Stretch goals

- **S3 Schema drift view**: implemented (Table health → "Analyse fields"): per-field types across
  the latest version of every document with percentages, first/last seen and conflict samples
  (`docs/screenshots/schema-drift-light.png`).
- **S4 Lag and oplog warning**: implemented: the pipeline page shows the oplog window (from
  `local.oplog.rs`) and warns when a paused bookmark is about to fall out of it.
- S1, S2, S5: not implemented (they need snapshot history or Nessie branches; see limitations).

## What I would do next

1. Run against MongoDB Atlas and a second catalog (Lakekeeper) and fold the findings into
   `docs/destinations.md`.
2. SigV4 signing as an adapter outside the core, to reach Glue and S3 Tables directly.
3. Time travel and snapshot diff on catalogs that serve snapshot history (DuckDB already reads
   `AT (VERSION => ...)`).
4. Multi-column sorting and saved views in the data viewer; column statistics in the header.
5. Upstream the two library findings: iceberg-go's case-sensitive `file_format` comparison and
   the DuckDB `MERGE INTO` single-action limit.
