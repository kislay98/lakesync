# Destinations

lakesync writes through two standard interfaces only: an **Iceberg REST catalog** and the
**S3 API**. A destination is one catalog plus one storage endpoint. The form fields are:

| Field | Meaning |
|---|---|
| Catalog URI | Base URL of the REST catalog (the one that serves `/v1/config`) |
| Warehouse | Warehouse name or location, sent as the `warehouse` parameter to `/v1/config` |
| Prefix | Optional path prefix some catalogs require |
| Auth type | `none`, `bearer` (static token) or `oauth2` (client credentials: id, secret, token endpoint, scope) |
| S3 endpoint | Storage endpoint. Leave empty to use what the catalog vends (see below) |
| S3 region, access key id, secret access key, path-style | Storage credentials |
| Vended credentials | Use the storage settings the catalog returns instead of the ones above |

When an S3 endpoint is given, lakesync makes it win over anything the catalog vends (Phase 0
showed catalogs vend their own, sometimes internal, endpoint). When it is empty and "vended
credentials" is on, the catalog's storage configuration is used as is.

Every destination test runs the same checklist: reach `/v1/config` → authenticate → namespace →
write and delete a storage object → create, append to, read and drop a temporary table → DuckDB
attaches the catalog and lists it.

## Tested: Project Nessie + RustFS (the local stack)

`deploy/local/docker-compose.yml`. Values (also offered by the "Use the local stack values"
button when `LOCAL_STACK_HINTS=true`):

| Field | Inside Docker (`make up`) | From the host (`make dev`) |
|---|---|---|
| Catalog URI | `http://nessie:19120/iceberg` | `http://localhost:19120/iceberg` |
| Warehouse | `warehouse` | `warehouse` |
| Auth | none | none |
| S3 endpoint | `http://s3:9000` | `http://localhost:9000` |
| Region | `us-east-1` | `us-east-1` |
| Access key / secret | `lakesync` / `lakesync-secret` | same |
| Path style | on | on |

Notes:
- Nessie sets `gc.enabled=false` on every table and serves only the current snapshot through
  Iceberg REST. Snapshot expiry and orphan deletion are therefore the job of Nessie's GC tool;
  lakesync reports the metrics and skips those two jobs. Compaction and clean-table rewrites run.
- RustFS replaces MinIO, whose images were withdrawn from public registries in 2026. Any
  S3-compatible server works; for MinIO specifically, Chainguard's build (`cgr.dev/chainguard/minio`)
  still pulls anonymously: set `MINIO_ROOT_USER`/`MINIO_ROOT_PASSWORD` and keep the same lakesync values.

## Documented, not tested

The following were written from each product's documentation. They were **not** exercised in
this build; the first run against them may need adjustments, which the connection test will show.

### Cloudflare R2 Data Catalog

| Field | Value |
|---|---|
| Catalog URI | `https://catalog.cloudflarestorage.com/<account-id>/<bucket>` |
| Warehouse | `<account-id>_<bucket>` (shown in the R2 bucket's settings) |
| Auth | `bearer`, token: an R2 API token with Data Catalog read/write |
| S3 endpoint | `https://<account-id>.r2.cloudflarestorage.com` |
| Region | `auto` |
| Access key / secret | the R2 S3 API token pair |
| Path style | off |

### AWS Glue (Iceberg REST endpoint) and S3 Tables

Glue's REST endpoint and S3 Tables authenticate with SigV4, which lakesync's catalog client does
not sign today (see KNOWN_LIMITATIONS.md). Use them through a REST catalog that fronts them with
bearer or OAuth2 auth, or add SigV4 signing to the destination (iceberg-go supports it through
`rest.WithSigV4`, which would need an AWS SDK config and therefore lives outside the core).

### Apache Polaris

| Field | Value |
|---|---|
| Catalog URI | `https://<polaris-host>/api/catalog` |
| Warehouse | the Polaris catalog name |
| Auth | `oauth2`, client id/secret from a Polaris principal, token endpoint `https://<polaris-host>/api/catalog/v1/oauth/tokens`, scope `PRINCIPAL_ROLE:ALL` |
| Storage | either explicit S3 settings, or "vended credentials" on (Polaris vends them) |

### Lakekeeper

| Field | Value |
|---|---|
| Catalog URI | `https://<lakekeeper-host>/catalog` |
| Warehouse | the Lakekeeper warehouse name |
| Auth | `oauth2` with the client credentials of a Lakekeeper/OpenID client, or `none` for an unsecured instance |
| Storage | explicit S3 settings or vended credentials |

Lakekeeper keeps `gc.enabled` on, so snapshot expiry and orphan cleanup run in lakesync.

### Any other Iceberg REST catalog

If it serves `/v1/config`, `/v1/namespaces` and the table endpoints, it should work. Run the
connection test; each failing step names the cause.
