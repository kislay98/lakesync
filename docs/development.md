# Developing lakesync

## Day to day

```bash
make infra        # MongoDB replica set, RustFS (S3), Nessie, Postgres
make dev          # Go server with live reload (air) + Vite dev server on :5173 proxying /api
make test         # go vet, unit tests, integration tests (need `make infra`)
make lint         # golangci-lint (incl. the cloud-neutral depguard rule) + oxlint + tsc
make e2e          # correctness/resync/removed/split/security suites + Playwright on a fresh instance
make bench        # measurements for docs/BENCHMARKS.md
make ui && make build   # production UI embedded into bin/lakesync
```

`deploy/local/dev.env` holds the environment `make dev` and `make verify-host` use on the host
(`localhost` ports, `DEV_MODE=true`). Host-side processes talk to MongoDB with
`?directConnection=true` because the replica set member is named `mongodb:27017` inside Docker.

Integration tests read `LAKESYNC_TEST_*` variables, the end-to-end harness `LAKESYNC_E2E_*`
(see `test/e2e/harness_test.go`); both default to the compose stack on `localhost`.

## macOS with Colima

The stack was developed with Colima (`colima start --vm-type vz --mount-type virtiofs`). Two
things to know:

- **Port forwarding dropped under load.** Lima's SSH port forwarder (and the gRPC one) lost
  every forwarded port whenever a container with published ports restarted or when the test
  harness killed the app with SIGKILL repeatedly. Starting Colima with
  `--network-address --network-preferred-route` gives the VM a routable address (`colima list`)
  and uses Apple's NAT, which held up through the chaos tests and is also much faster for
  downloads inside the VM. Point the suites at it:
  ```bash
  VM=$(colima list -j | jq -r .address)
  export LAKESYNC_E2E_PG_ADMIN="postgres://lakesync:lakesync@$VM:5432/lakesync?sslmode=disable" \
         LAKESYNC_E2E_MONGODB_URI="mongodb://$VM:27017/?directConnection=true" \
         LAKESYNC_E2E_CATALOG_URI="http://$VM:19120/iceberg" LAKESYNC_E2E_S3_ENDPOINT="http://$VM:9000"
  make e2e
  ```
- **The MongoDB restart in the correctness test** is a server-process restart inside the
  container (mongod runs under a supervisor loop in `deploy/local/docker-compose.yml`), not a
  container restart, for the same reason. `make e2e-container` runs the suites from a container
  on the compose network instead, which avoids host port forwarding entirely.

## Layout

See `docs/ARCHITECTURE.md`. Decisions go to `DECISIONS.md`, things that do not work yet to
`KNOWN_LIMITATIONS.md`.
