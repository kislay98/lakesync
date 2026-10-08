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
