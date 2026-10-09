# MongoDB Atlas

lakesync works against any MongoDB replica set, including Atlas free clusters. The steps:

1. **Create a cluster.** A free (M0) cluster is enough to try lakesync. Note Atlas Free limits:
   500 connections, 100 operations per second, 10 GB transferred per rolling 7 days. lakesync uses
   one client per source connection shared by all its collections, and `SNAPSHOT_MAX_DOCS_PER_SEC`
   caps the full-copy read rate.
2. **Create a database user** (Database Access → Add New Database User) with the built-in
   **`read`** role on the databases you want to sync (or `readAnyDatabase`). The `read` role
   includes the `changeStream` privilege that streaming needs. Note the username and password.
3. **Allow the server's IP** (Network Access → Add IP Address). The lakesync Settings page and the
   connection test show the server's public egress IP. If you cannot see it, set `PUBLIC_IP` in
   the environment.
4. **Copy the connection string** (Connect → Drivers): `mongodb+srv://<user>:<password>@cluster0.xxxxx.mongodb.net/?retryWrites=true&w=majority`.
5. **Paste it** into the "Paste a connection string" box of the source form. lakesync fills the
   fields, moves the password into the masked secret field and strips credentials from the stored
   URI. Press **Test connection**.

The test checklist for MongoDB: parse the string → DNS / SRV lookup → TCP and TLS → authenticate →
replica set → list databases → open a change stream → read one document. Common failures and the
hints shown:

| Symptom | Hint |
|---|---|
| Connection timeout to an Atlas host | Atlas may be blocking this server's IP. Add it in Atlas → Network Access. |
| Authentication failed | Check the username and password, and that the user's auth database is right (usually `admin`). |
| Not a replica set | Change streams need a replica set. Atlas clusters always are; for self-hosted MongoDB, start with `--replSet`. |
| Not authorised to watch | Give this user the `read` role on the database (it includes `changeStream`). |

Secrets are sealed with AES-256-GCM before they reach the database and are never returned by the API.
