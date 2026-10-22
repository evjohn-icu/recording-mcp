# Local semantic retrieval measurements

Raw synthetic benchmark records are in [local-v2-retrieval.json](local-v2-retrieval.json).
They use deterministic precomputed float32 vectors and make no provider calls.
These are in-memory algorithm measurements; they do not measure SQLite SQL
filters, authoritative artifact rechecks, sidecar load time, profile lifecycle,
or end-to-end MCP latency.

On the 12 CPU / 24 GB host, exact scan at one million vectors measured p95
223.8 ms at 256 dimensions and 565.2 ms at 768 dimensions, with process RSS
1.08 GB and 3.04 GB respectively. Five timed queries were used to keep the
exact oracle bounded.

The first HNSW settings (M=16, construction ef=64) missed the recall gates at
100k items: recall@10 was 0.735 and recall@40 was 0.633. Raising construction
to M=32 / ef=200 passed the 10k check (recall@10 and @40 both 1.0) at a 38 s
build cost. The million-item HNSW build was started with a 600 s cap; no
completed million-item ANN result is claimed. The sidecar therefore remains
opt-in, with exact SQLite retrieval as the default and fallback.

The M=32 / ef=200 million-item build also hit its 600 s cap. At the last
observation near eight minutes its RSS was approximately 439 MiB; it produced
no completed graph, query latency, or recall measurement. See the explicit
incomplete record in the raw JSON.

The before/after context-eval comparison is recorded separately in
[local-v2-evaluation.json](local-v2-evaluation.json). Across three fresh-DB
runs on each revision, quality metrics were identical: evidence recall@5
0.9348, lexical recall@5 0.6154, hybrid recall@5 0.6923, and seven hybrid
false-positive items. Process wall time varied (baseline 8.40–9.75 s, current
5.59–10.03 s), so it does not support a speedup claim. Evaluation latency
means were also similar and are preserved per-run in the raw report. No
external embedding endpoint was configured.
