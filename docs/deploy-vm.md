# Deploy on a single VM

Any Linux VM on any provider (AWS, Oracle, Hetzner, GCP, a homelab box) with Docker installed.
MongoDB, object storage and the Iceberg catalog are external (Atlas, S3 or R2, a managed REST
catalog) and are configured in the UI after the first login.

1. **Create a VM** with 2 vCPU / 4 GB RAM or more (DuckDB and compaction like memory) and open
   ports 80 and 443. Install Docker Engine with the Compose plugin.
2. **Point DNS** at the VM: an `A` record for `lakesync.example.com`.
3. **Copy `deploy/vm`** to the VM and fill in the environment:
   ```bash
   cd deploy/vm
   cp .env.example .env
   docker run --rm ghcr.io/lakesync/lakesync gen-key   # paste into APP_SECRET_KEY
   nano .env                                           # DOMAIN, ACME_EMAIL, ADMIN_EMAIL, ADMIN_PASSWORD, POSTGRES_PASSWORD
   ```
   `LAKESYNC_IMAGE` defaults to a published image name; build your own with `make docker` and
   push it to any registry if you prefer.
4. **Start it:**
   ```bash
   docker compose up -d
   ```
   Caddy obtains a Let's Encrypt certificate for `DOMAIN` and proxies to lakesync; Postgres
   stores the app data in a named volume.
5. **Allow the VM's IP in Atlas** (Network Access). The Settings page shows the egress IP.
6. Open `https://lakesync.example.com`, log in with `ADMIN_EMAIL` / `ADMIN_PASSWORD`, and follow
   the setup flow.

Operational notes:

- Private networks are refused by default on a VM (`ALLOW_PRIVATE_ENDPOINTS=false`); set it to
  `true` if your catalog or storage lives on a private network.
- Logs are JSON on stdout: `docker compose logs -f lakesync`.
- `/healthz`, `/readyz` and `/metrics` are served by lakesync (behind Caddy, same host).
- To scale out, run extra `lakesync worker` containers against the same `DATABASE_URL`; advisory
  locks keep each collection on exactly one worker.
- Upgrades: pull the new image and `docker compose up -d`; migrations run on start.
