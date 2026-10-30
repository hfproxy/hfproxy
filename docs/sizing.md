# Sizing guide

hfproxy's own footprint is small: files are streamed, never held in memory, and the catalog lives in PostgreSQL. What you size is the **staging volume**, the **object store**, the **database connections** and the **network**. Gather four numbers first:

| Input | How to find it |
| --- | --- |
| **Hot set**: the models and datasets people use in a typical month | Add up the repositories you register; after a few weeks `hfproxy_cache_size_bytes` tells you |
| **Largest file** | The biggest shard of the biggest model you serve (often 5–50 GiB; the hard ceiling is `HFPROXY_CACHE_MAX_FILE_SIZE`, default 100 GiB) |
| **Simultaneous cold downloads**: how many different files may be fetched from the Hub at once | Usually a handful; it is capped by `HFPROXY_CACHE_FILL_CONCURRENCY` (default 4 per instance) |
| **Replicas** | One is a valid choice; more give availability, not capacity (the bucket is shared) |

## Staging volume

Every cold fill is written to a local staging file before it is verified and published. Per instance:

```
staging ≥ HFPROXY_CACHE_FILL_CONCURRENCY × largest file, plus 10–20% headroom
```

At the default concurrency of 4 and a 30 GiB largest file, that is 120 GiB, so about 140 GiB. With the chart, set `staging.size` (PVC) or `staging.sizeLimit` (`emptyDir`). Fills that do not fit are refused with 507, and `HFPROXY_STAGING_MIN_FREE_BYTES` takes an instance out of rotation (`/readyz` reports `staging_space`) before the disk is full. Watch `hfproxy_staging_free_bytes` and `hfproxy_staging_reserved_bytes`. Use fast local disk: staging throughput bounds how quickly cold files arrive.

## Object storage

```
bucket ≥ hot set × 1.2, and set HFPROXY_CACHE_MAX_TOTAL_SIZE to that figure
```

With a quota, unpinned files are evicted when it is exceeded, least recently used first unless another [strategy](cache-strategies.md) is chosen; without one the cache only grows. Pinned files (`hfproxy cache pins`) are never evicted: keep them well under the quota (the alert fires at 80%). The bucket must stay private and allow the service identity to read, write and delete.

## PostgreSQL

- **Connections.** Each instance opens up to `max(20, 2 × fill concurrency + 16)` connections unless `HFPROXY_DATABASE_MAX_CONNS` is set (24 by default; it must be at least fill concurrency + 8, because each running fill holds one). Total demand is replicas × pool: three replicas need 72 connections, so keep PostgreSQL's `max_connections` above that plus your own tools (the default of 100 is enough for three).
- **Size.** The catalog is small. The tables that grow are `downloads` (one row per download, purged after `HFPROXY_RETENTION_DOWNLOADS`, 90 days by default) and `audit_events` (`HFPROXY_RETENTION_AUDIT`, 365 days). Measure rather than guess: `SELECT pg_size_pretty(pg_total_relation_size('downloads'))`, and shorten retention if it matters.
- Back it up together with the bucket ([operations.md](operations.md#backup-and-restore)).

## CPU and memory

- A cache hit is a streamed copy from the bucket: negligible CPU. A cold fill hashes the file once (SHA-256), about one core per concurrent fill.
- The chart asks for 250m CPU and 256 MiB and limits memory to 1 GiB. Memory does not grow with file size. It grows with the number of open connections and `HFPROXY_API_CACHE_MAX_BYTES` (32 MiB by default). Measured with the real binary (`scripts/large-file-test.py`: one cold fill, a cached read and a range read), a 256 MiB file and a 2 GiB file both peaked at 162 MiB of resident memory, and staging reached exactly the size of the file. That machine and object store are not yours, so check with your own load before lowering the limit (see below).

## Network

A cold fill moves a file twice through the instance: Hub → instance, then instance → bucket. A hit moves it once, bucket → instance → client.

```
time to warm one file ≈ size ÷ min(Hub bandwidth, staging write speed, bucket bandwidth)
```

A 30 GiB file is about 258 Gbit: roughly 4.5 minutes at 1 Gbit/s, 26 seconds at 10 Gbit/s, when nothing else is slower. Size the link between the instance and the bucket for the largest burst of simultaneous downloads, and the egress to the Hub for the number of cold fills you allow at once. If many users ask for the same new model at once, a large (LFS) file is fetched from the Hub once and the others wait for the cache entry, so egress is not multiplied; only small files without a digest can be fetched several times in a burst. The first user still waits for the whole download, so preload known releases beforehand (administrators can start a preload from the console).

## A worked example

50 engineers, a 2 TB hot set, 30 GiB largest file, two replicas, default fill concurrency:

| Resource | Figure |
| --- | --- |
| Staging, per replica | 4 × 30 GiB = 120 GiB → 150 GiB volume |
| Bucket and quota | 2 TB × 1.2 = 2.4 TB |
| Database connections | 2 × 24 = 48 → `max_connections` of at least 100 is comfortable |
| CPU, memory per replica | 1 vCPU, 512 MiB to 1 GiB to start |
| Network | 1 Gbit/s to the bucket and to the Hub: about 4.5 minutes to warm a 30 GiB shard |

Everything here is arithmetic from the defaults, not a benchmark of your environment.

## Checking your sizing

1. **Load test.** The repository has an opt-in test that starts the real server against PostgreSQL and S3 and downloads files in parallel, cold then cached, failing on any error, wrong byte, leftover staging file or leaked connection and reporting latencies, heap and connections:
   ```bash
   HFPROXY_LOAD_TEST=1 HFPROXY_LOAD_CLIENTS=16 HFPROXY_LOAD_FILE_MB=512 \
     go test ./internal/server -run TestLoadParallelDownloads -v -count=1
   ```
   It needs the same `HFPROXY_TEST_*` variables as the other integration tests, and an S3 service that can hold the data (the `moto` test server keeps everything in RAM; use Garage or MinIO for gigabytes).
2. **A multi-gigabyte file through the real binary.** `scripts/large-file-test.py` runs the proxy as a separate process and reports its peak memory and staging use ([validation.md](validation.md#4-a-multi-gigabyte-file-on-your-own-machine)); run it against your own object store for realistic throughput.
3. **Watch the real thing** for two weeks: `hfproxy_cache_size_bytes`, `hfproxy_staging_free_bytes`, the eviction rate, the miss rate and the time-to-first-byte of cache misses ([slo.md](slo.md)). A steady eviction rate with a high miss rate means the quota is below the hot set.
4. **Re-run the numbers** when you add a model family with larger files.
