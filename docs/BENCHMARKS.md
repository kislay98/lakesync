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

__RESULTS__
