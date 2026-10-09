# Setting up lakesync on a new machine

These instructions are for a person or an assistant who has just received this repository
(as a zip or a clone) on a machine that has never run it. Follow them in order. Nothing here
needs an account anywhere; everything runs locally in Docker.

## What you are setting up

lakesync copies data from MongoDB into Apache Iceberg tables and keeps them in sync as the
data changes. It is one program with a web interface. For local use it comes with everything
it needs, defined in `deploy/local/docker-compose.yml`: a MongoDB, an Iceberg catalog (Nessie),
an S3-compatible object store (RustFS), a Postgres database for the app's own bookkeeping, and
the app itself. You do not install any of those by hand; Docker starts them.

If you are an assistant: the file `CLAUDE.md` at the repository root is loaded automatically
and tells you how the project is organised and what the rules are. Read `DECISIONS.md` before
changing behaviour and `KNOWN_LIMITATIONS.md` before reporting something as broken.

## 1. Install the two things it needs

1. **Docker, with Compose v2.** Docker Desktop on macOS or Windows, or Docker Engine on Linux.
   Give Docker at least 6 GB of memory (Docker Desktop: Settings → Resources). Check with:
   ```bash
   docker --version
   docker compose version
   ```
2. **`make`.** Present on macOS (install Xcode command line tools if `make` is missing) and on
   most Linux systems. On Windows, either install it (for example through Chocolatey or use
   WSL), or run the compose commands written out in section 7 instead of the `make` targets.

You need internet access for the first start only; it downloads base images and build
dependencies. After that the stack runs offline.

Nothing else is required to *run* the app. Go, Node and the browser for tests are only needed
to develop it or run its test suites (section 8).

## 2. Unpack and go to the folder

```bash
unzip lakesync.zip
cd lakesync
```

(If you cloned it with git instead, just `cd` into the clone.)

## 3. Start the stack

```bash
make up
```

The first run builds the app's container image from source. Expect 10 to 30 minutes depending
on the machine; later starts take seconds. The command returns when every container reports
healthy and prints the address of the web interface. Check for yourself:

```bash
docker compose -f deploy/local/docker-compose.yml --profile app ps
curl -s http://localhost:8080/readyz
```

Every container should say "healthy" (the `s3-init` one runs once and exits; that is normal)
and the second command should print `"ready":true`.

## 4. Put some data into MongoDB

```bash
make seed
```

This creates a database called `shop` with four collections and then keeps changing the data
for five minutes, adding a brand-new field after one minute and type conflicts after two, so
you can watch the app react. Leave it running in that terminal, or stop it with Ctrl-C after
the initial load; the initial load alone is enough to try the app.

## 5. Open the web interface and connect it

Open http://localhost:8080 and log in with `admin@example.com` / `changeme-now`.

1. Press **Start setup**.
2. In the MongoDB form, press **Use the local stack values** (it fills in the address of the
   MongoDB that Docker started), then **Test connection**. Every check should pass.
3. In the destination form, again press **Use the local stack values**, then **Test
   connection**.
4. Pick the `shop` database (all four collections), keep the defaults, and press
   **Create and start**.

Within about ten seconds every collection should show **Streaming**. From there you can open
**Tables** to browse the copied data, the **SQL console** to query it, and **Table health**
for file statistics and maintenance.

If you would rather have all of section 3 to 5 done and checked for you in one go, run
`scripts/quickstart-check.sh`; it starts the stack, seeds data, performs the setup above through
the API, waits for streaming, and verifies the result. It prints "quickstart check passed" at
the end.

## 6. Check that the copy is correct

```bash
make verify
```

This compares every document in MongoDB with the Iceberg tables and prints a line per
collection. It should end with "OK: 4 collection(s) match exactly, zero duplicate keys."

## 7. Stopping, restarting, wiping

- `make down` stops everything but keeps the data; `make up` brings it back as it was.
- `make reset` stops everything and deletes all the data; the next `make up` starts empty.
- `make logs` tails the app's log.

Without `make`, the equivalents are:

```bash
docker compose -f deploy/local/docker-compose.yml --profile app up -d --build --wait   # make up
docker compose -f deploy/local/docker-compose.yml --profile app down                   # make down
docker compose -f deploy/local/docker-compose.yml --profile app down -v                # make reset
docker compose -f deploy/local/docker-compose.yml --profile app run --rm --no-deps lakesync seed --uri "mongodb://mongodb:27017/?replicaSet=rs0" --duration 5m --new-field-after 1m --type-conflict-after 2m   # make seed
docker compose -f deploy/local/docker-compose.yml --profile app run --rm --no-deps lakesync verify   # make verify
```

## 8. Only if you want to develop or run the test suites

Install Go 1.27, Node 22 and golangci-lint, then from the repository root:

```bash
make ui && make build   # builds the web UI, then the binary with it embedded, into bin/lakesync
make lint
make test         # unit and integration tests; the integration ones need the stack running (make up)
make e2e          # end-to-end suites against the stack; the Go suite alone takes about 15 minutes
make bench        # the benchmark, about 10 minutes
```

The test suites reach the stack on `localhost`, which is what Docker Desktop and Docker Engine
provide. The `LAKESYNC_E2E_*` environment variables mentioned in `CLAUDE.md` and
`docs/development.md` are only for the original development Mac, whose Docker ran in a Colima
VM; ignore them elsewhere. Run the suites one at a time, never two against the same stack.
Playwright needs its browser once: `cd web && npx playwright install chromium`.

## 9. When something does not work

| What you see | What it usually means | What to do |
|---|---|---|
| `make up` fails with "port is already allocated" | Something on the machine already uses port 8080, 27017, 5432, 9000, 9001 or 19120 | Stop that program, or change the left-hand side of the port mapping in `deploy/local/docker-compose.yml` |
| The image build fails while compiling Go code | Usually a network hiccup while downloading modules | Run `make up` again; the build resumes from its cache |
| Containers restart or the `nessie` container never becomes healthy | Docker has too little memory | Give Docker at least 6 GB and run `make up` again |
| `/readyz` says the database is not ok | Postgres is not up yet | Wait a minute; `make logs` shows the app retrying |
| The object store (`s3`) answers with HTTP 500 | Its file-descriptor limit is too low | The compose file already raises it to 65,536; if you changed the file, keep that `ulimits` block |
| A connection test fails in the UI | The hint buttons were not used, or the stack is not running | Press **Use the local stack values** again and retest; check `docker compose ... ps` |
| The build is on an Intel (amd64) machine and fails | That half of the multi-arch build has not been exercised yet | Read the error; it is almost certainly a base-image or module download problem, since the code has no architecture-specific parts. Record what you find in `KNOWN_LIMITATIONS.md` |

The web interface shows the app's own log on every pipeline page, and the container log is
one `make logs` away. Most problems show up in one of those two places with a plain-English
message.
