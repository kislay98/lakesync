# Known limitations

Stated plainly. Each entry says what is missing, why, and what would unblock it.

## Snapshot expiry and orphan-file deletion do not run on Nessie
- **What:** On tables served by Project Nessie the "Expire snapshots" and "Orphan cleanup" jobs only report metrics; they never delete files.
- **Why:** Nessie sets `gc.enabled=false` on every table because files may still be referenced by its commit history, and iceberg-go refuses GC operations on such tables.
- **Unblock:** Run Nessie's own GC tool on a schedule, or use a catalog that leaves `gc.enabled` on (Lakekeeper, Polaris, R2 Data Catalog), where the app runs both jobs.

## Snapshot history is not visible through Nessie's Iceberg REST endpoint
- **What:** Table health and the snapshots list show one snapshot on Nessie-backed tables.
- **Why:** Nessie keeps history in its own commit log and serves only the current snapshot in Iceberg metadata.
- **Unblock:** Nessie's native API (out of scope, see stretch goal S5) or another catalog.

## MongoDB Atlas was not tested live
- **What:** No Atlas cluster was available during the build. SRV parsing, TLS, authentication and the hint mapping are covered by unit tests (`internal/source/mongodb`), and the connection checklist resolves SRV records and dials with TLS, but no real `mongodb+srv://` cluster was exercised end to end.
- **Unblock:** Point a source at an Atlas free cluster and run the connection test; failures show up as checklist steps with hints.

## SigV4-authenticated catalogs (AWS Glue REST endpoint, S3 Tables) are documented, not supported
- **What:** The destination supports `none`, bearer and OAuth2 client-credentials authentication. SigV4 request signing needs an AWS SDK configuration, which the cloud-neutral rule keeps out of the core.
- **Unblock:** An adapter outside `internal/dest` that provides an `aws.Config` to iceberg-go's `rest.WithSigV4`, or a REST catalog proxy that accepts bearer tokens.

## Destinations other than Nessie + RustFS were not tested
- **What:** Cloudflare R2 Data Catalog, Apache Polaris and Lakekeeper settings in `docs/destinations.md` come from their documentation.

## A failing object store fails maintenance jobs, not the pipeline
- **What:** When the object store answers reads with errors (seen on the local RustFS before its file-descriptor limit was raised, see `DECISIONS.md`), the maintenance job fails with that error in the run history and the pipeline keeps streaming; the next maintenance cycle (or the Table health button) runs it again. Maintenance concurrency is capped at two jobs per process.
- **Unblock:** Give the object store enough file descriptors (the compose files set 65,536) and raise the maintenance interval if the store is slow.

## The single-VM deployment was validated, not deployed
- **What:** `deploy/vm/docker-compose.yml` and the `Caddyfile` pass `docker compose config` and the image they run is the one the quickstart check used, but no public VM with a DNS name was available, so Caddy's certificate issuance and the HTTPS path were not exercised live.
- **Unblock:** Follow `docs/deploy-vm.md` on a VM with ports 80/443 open; Caddy obtains the certificate on first start.

## The containerised test runner is untested
- **What:** `Dockerfile.e2e` and `make e2e-container` (the suites run from inside the compose network) were written but never exercised; the image build was too slow on the build machine. The suites were run from the host with `make e2e`.

## Snapshot diff, time travel and preview-before-publish (stretch goals S1, S2, S5) are not implemented
- **What:** They depend on snapshot history (S1, S2) or Nessie branches (S5); Nessie serves only the current snapshot through Iceberg REST.

## DuckDB positional deletes accumulate until maintenance rewrites the clean table
- **What:** Every merge that updates or deletes rows writes a positional delete file; reads slow down as they pile up. The `rewrite_clean` maintenance job compacts them (automatically above the configured threshold, or from the Table health screen).

## Multi-column sorting in the data viewer
- **What:** The grid sorts by one column (plus the key as tie-breaker) with keyset pagination; TanStack's multi-sort is not exposed.
- **Unblock:** Extend the keyset predicate to several columns.

## 10-million-row viewer benchmark
- See `docs/BENCHMARKS.md` for the table size actually measured and the method.
