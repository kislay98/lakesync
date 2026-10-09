# CLAUDE.md — lakesync

Self-hostable MongoDB → Apache Iceberg CDC app: one Go binary (`cmd/lakesync`) with an embedded
React UI (`web/`), Postgres app database, any Iceberg REST catalog + S3-compatible storage,
DuckDB for the viewer, merges and SQL console. The original brief is
`/Users/kislaykumar/Downloads/BUILD_PROMPT.md`; the build status against it is in
`docs/FINAL_REPORT.md`.

## Read these before changing anything
- `docs/ARCHITECTURE.md` (design), `docs/phase0.md` (library findings the code must respect),
  `DECISIONS.md` (every non-obvious choice, with alternatives), `KNOWN_LIMITATIONS.md`,
  `docs/development.md` (how to run everything on this Mac).

## Commands
- `make up` / `make down` / `make reset` (wipes volumes): local stack in `deploy/local/`, app at
  http://localhost:8080, login admin@example.com / changeme-now.
- `make dev` (Go live reload + Vite), `make build` (embeds the UI, needs Go 1.27 and Node),
  `make test` (unit + integration), `make e2e` (Go suites, then Playwright), `make bench`,
  `make lint` (golangci-lint with the cloud-neutral depguard rule, oxlint + tsc), `make seed`,
  `make verify`, `scripts/quickstart-check.sh`, `scripts/ui-e2e.sh`.
- Go is Homebrew-only on this machine: start shell commands with `export PATH=/opt/homebrew/bin:$PATH`.

## Running the suites on this machine (Colima, not Docker Desktop)
- Docker runs in Colima started with `colima start --network-address --network-preferred-route`;
  the VM is `192.168.64.2`. Host port forwarding breaks under the chaos tests, so point suites at the VM:
  ```bash
  export LAKESYNC_E2E_PG_HOST=192.168.64.2:5432 \
    LAKESYNC_E2E_MONGODB_URI="mongodb://192.168.64.2:27017/?directConnection=true" \
    LAKESYNC_E2E_CATALOG_URI="http://192.168.64.2:19120/iceberg" \
    LAKESYNC_E2E_S3_ENDPOINT="http://192.168.64.2:9000" \
    SEED_MONGODB_URI="mongodb://192.168.64.2:27017/?directConnection=true"
  go test -tags e2e -count=1 -timeout 40m ./test/e2e/ -v        # Go e2e (TestCorrectness runs 10 min)
  ./scripts/ui-e2e.sh                                            # Playwright, fresh database
  go test -tags integration -count=1 ./internal/...              # uses LAKESYNC_TEST_* vars, defaults to localhost
  LAKESYNC_BENCH=1 go test -tags e2e -timeout 3h ./test/e2e/ -run 'TestBenchmark$' -v
  ```
- Run suites one at a time against the shared stack; never `pkill lakesync` broadly while one runs.
- RustFS needs `ulimits.nofile` 65536 (set in compose). If `s3` answers 500s, recreate that container.
- The chaos test restarts mongod with `docker exec lakesync-mongodb-1 mongosh`; mongod runs under a
  supervisor loop in compose for that reason. Do not `docker compose restart` during a suite.

## Conventions
- Cloud-neutral core: no cloud SDK imports in `internal/core`, `internal/source`, `internal/dest`,
  `internal/query` (depguard enforces it). Endpoints live only in destination configuration.
- Every behavioural decision gets a `DECISIONS.md` entry (decision, alternatives, reason); anything
  not working or not verified goes in `KNOWN_LIMITATIONS.md`.
- The raw Iceberg table is the source of truth for the checkpoint (snapshot summary + table
  properties); the app database only mirrors state for the UI.
- Tests that touch infrastructure use build tags `integration` or `e2e` and unique namespaces
  (`lakesync_it_`, `e2e_`, `bench_`, `uie2e_`) that `test/e2e/cleanup_test.go` drops.
- Commit messages: plain, no AI attribution. Kislay reviews in difit before pushing.
