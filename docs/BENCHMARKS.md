# Benchmarks

Measured with `make bench` (`test/e2e/bench_test.go`, `LAKESYNC_BENCH=1`), which starts a
dedicated lakesync instance against the local compose stack, seeds MongoDB, creates a pipeline
through the API and times each stage. Numbers are from one run; they are meant to show the
order of magnitude on a developer laptop, not a tuned deployment.

**Machine:** Apple M4 Pro, 14 cores, 24 GB, macOS. MongoDB 8.0, Nessie 0.109, RustFS and
Postgres 17 run in a Colima VM (6 vCPU, 10 GB, Apple Virtualization framework); lakesync and the
benchmark driver run on the host and reach the VM over its NAT address. DuckDB memory limit for
the instance under test: 6 GB. Sync interval 10 s unless stated.

## Method

1. **Full-copy throughput:** seed N small documents (the `events` collection: six fields) plus
   2,000 customers, create the pipeline, measure the time from pipeline creation to every
   collection reaching *Streaming*. Includes table creation, Parquet writes to S3, catalog commits
   and the first raw→clean merge.
2. **Streaming throughput:** run the generator at a fixed rate with 70% inserts, 20% updates,
   5% replaces, 5% deletes for a fixed duration; divide the number of events written to the raw
   tables by the duration. The generator, not the sink, is the bottleneck at these rates; the
   per-batch commit cost is reported separately.
3. **End-to-end latency:** every 15 s insert a marker document and poll the data viewer's rows
   endpoint (a filtered query on the clean table) until it appears; report median, p95 and max.
   The theoretical floor at a 10 s interval is 0–10 s plus merge time.
4. **Viewer page time:** HTTP time of the rows endpoint on the large table: first page with the
   total count, without the count, a sorted page, a filtered page, a full-text search page and a
   keyset second page (best of three).
5. **Files and snapshots:** after a run of commits at a 5 s interval, the Table health metrics
   of the raw and clean tables before and after "Run maintenance now", and the verify result
   afterwards. 24 simulated hours at the default interval is 1,440 commits; the extrapolation is
   stated as such.

## Results

Run of 2026-10-09 (`LAKESYNC_BENCH=1`, defaults: 1,000,000 `events` documents, generator at
500 ops/s for 2 minutes, 60 commits at a 5 s interval). Two earlier runs of the same day gave
the same numbers within a few percent for stages 1 to 4.

| Stage | Result |
|---|---|
| Seed | 1,000,000 events + 2,000 customers inserted into MongoDB in 7 s |
| 1. Full copy | 1,002,000 documents in 14 s = 70,867 docs/s, from pipeline creation to every collection *Streaming* (table creation, Parquet writes to S3, catalog commits and the first raw→clean merge included) |
| 2. Streaming | generator at 500 ops/s for 2 min (70% inserts, 20% updates, 5% replaces, 5% deletes): 37,996 events written to the raw tables = 317 events/s sustained; the generator (one round trip per operation to the VM) was the bottleneck, not the sink |
| 3. End-to-end latency | MongoDB insert → visible in the data viewer at a 10 s interval, 6 samples: median 7 s, p95 10.7 s, max 10.7 s (theoretical floor 0–10 s plus merge time) |
| 4. Viewer, 1,000,000-row table | first page with total count 67 ms · first page without count 46 ms · sorted page 45 ms · filtered page 47 ms · full-text search page 126 ms · keyset second page 45 ms (HTTP time, best of 3) |
| 5. Files after 60 commits at 5 s | raw: 83 data files (all below 16 MB), 0 delete files, 19.6 MB · clean: 83 data files, 707 delete files, 13.0 MB |
| 5. After "Run maintenance now" | raw: 1 data file, 20.8 MB · clean: 1 data file, 0 delete files, 12.6 MB · `verify`: zero differences, zero duplicates |

Extrapolation: 24 hours at the default 60 s interval is 1,440 commits. Without maintenance each
commit adds one data file per partition day to the raw table and one delete file plus one data
file per merge that changed rows to the clean table; with the default thresholds compaction
runs long before that (every 15 minutes the checks run, compacting partitions with more than
50 small files and rewriting the clean table above 20 delete files).

Snapshot counts read "1" because Nessie serves only the current snapshot through its Iceberg
REST endpoint (`KNOWN_LIMITATIONS.md`).
