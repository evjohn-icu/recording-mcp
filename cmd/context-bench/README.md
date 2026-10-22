# Semantic index benchmark

`context-bench` compares the pure-Go HNSW index with an exact cosine oracle
over seeded synthetic float32 vectors. It calls no embedding provider and uses
the same precomputed vectors for both paths. Results are JSON records with
build time, warm query p50/p95, recall@10/@40, Go heap, and Linux process RSS.

Run the full target matrix on the 12 CPU, 24 GB reference host with:

```sh
GOMAXPROCS=12 go run ./cmd/context-bench \
  --sizes=10000,100000,1000000 \
  --dims=256,768 \
  --queries=100 \
  --warmup=10
```

The exact oracle scans every vector per query and is intentionally expensive
at one million items. Use smaller `--queries` values for an initial run, then
increase them when checking final p95 and recall gates. The command does not
write a database or sidecar file.

The implementation is in-memory HNSW over synthetic vectors. These numbers
do not represent SQLite filtering, authoritative candidate rechecks, sidecar
load time, profile lifecycle, or end-to-end MCP latency. The current raw
measurements and explicit failed ANN gates are recorded in
[`docs/local-v2-retrieval.json`](../../docs/local-v2-retrieval.json) and
summarized in [`docs/local-v2-retrieval.md`](../../docs/local-v2-retrieval.md).

To measure only exact-scan memory and throughput without paying HNSW build
time, add `--exact-only`. A bounded target run is:

```sh
GOMAXPROCS=12 go run ./cmd/context-bench \
  --sizes=1000000 --dims=256,768 \
  --queries=5 --warmup=1 --exact-only \
  --output=docs/local-v2-retrieval.json
```
