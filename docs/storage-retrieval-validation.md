# SQLite retrieval storage benchmark protocol and validation

## Purpose and scope

`cmd/context-storage-bench` exercises the production `semantic.SQLiteRetriever` and
`semantic.HNSWRetriever` APIs against a temporary SQLite database populated with
seeded synthetic float32 vectors. The provider is an in-process lookup only; no
network, credentials, model downloads, or real semantic-quality claims are
involved. Both retrievers receive the same synthetic query vectors. The HNSW
path still calls the production authoritative SQLite candidate recheck; results
are compared with the exact retriever oracle.

Run via `python3 scripts/run-storage-bench.py --output docs/evals/storage-bench.json`.
Defaults are 10k and 100k rows, three repeats, four scenarios and 20 queries per
scenario/repeat. The runner isolates each size and imposes a 600-second timeout.
If the budget is exceeded it exits with `BLOCKED` and the exact path remains the
default. For a quick smoke test use `--sizes 10000` (still three repeats).
The JSON records exact and HNSW query p50/p95, recall@10/@40, Linux RSS,
scenario, safety leak count, and fallback diagnostic. Fixed seed is 730241.

Scenarios cover unscoped current rows, conversation scope, superseded source
revision filtering, and ambient-turn filtering. The fixture synthesizes active
state using the same attachment/current predicate data and ambient classes;
it is not a production-data benchmark. `safety_leaks` counts any returned hit
that violates its scenario invariant. A zero is necessary but insufficient for
safety approval.

## Fallback measurement

`HNSWRetriever.SearchWithDiagnostics` returns per-invocation path, fallback
status, and fallback reason; `Search` delegates to the same implementation.
There is no mutable shared “last result” state. The benchmark counts exact
fallback queries independently and excludes their latency and results from ANN
performance and recall. Candidate rechecks are processed in bounded batches of
400 tuple-membership pairs, preserving the existing current/stale/ambient/scope
predicates and content-hash validation. Candidate ranking and the result cap
remain global across batches.

## Decision gate

Do not recommend enabling HNSW by default unless each evaluated target size
shows recall@10 >= 0.95, recall@40 >= 0.98, at least 20% p95 improvement against
exact SQLite, and zero safety leaks, with no hidden exact fallback represented
as ANN speed. Exact SQLite remains the default regardless of this benchmark;
this task does not alter provider/index configuration behavior.

## sqlite-vec compatibility review (not adopted)

This checkout builds `modernc.org/sqlite v1.60.1` (pure-Go SQLite driver; see
`go.mod`) and its runtime hook integration in
`internal/semantic/sidecar_retriever.go`. The local reference checkout
`../recording-mcp-references/sqlite-vec` resolves to tag
`v0.1.10-alpha.4`, commit
`04d28bd21773981e2d266bbf6aa4efbd011eb4f6`. Its `README.md` describes sqlite-vec
as a C extension, and `Makefile` targets compile/link `sqlite-vec.c` with
SQLite C sources (including the standalone CLI recipe around lines 209-222).
That API/build shape is not directly compatible with modernc's pure-Go SQLite
engine and the existing module has no C extension loading path. No extension
was installed, no CGO/driver change was made, and no compatibility is claimed.
An adopt decision needs an independently validated pure-Go integration or a
separate explicit driver/build decision; this benchmark is not evidence for
switching.

## Recorded execution

Recorded metrics are in [`docs/evals/storage-bench.json`](evals/storage-bench.json).
The 2,050-candidate
SQLite expression-depth regression is fixed and covered. The 10k run completed
all 240 synthetic queries with recall@10/@40 of 1.0, no exact fallbacks, and no
safety leaks. HNSW was slower than exact retrieval in all four scenarios, so the
performance gate failed and exact SQLite remains the default. The 100k run
exceeded the 600-second size timeout and is **BLOCKED**; it has no reportable
performance or quality result. These synthetic mechanics results do not claim
real-world semantic quality. No million-row or no-CGO experiment was run.
