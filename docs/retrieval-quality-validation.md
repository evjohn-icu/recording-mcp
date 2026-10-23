# Retrieval quality validation

`internal/semantic.SQLiteRetriever` keeps its historical minimum similarity of
`0.05` when no explicit threshold is supplied. `context-eval --min-similarity N`
is an opt-in calibration override. The default remains unchanged unless a
calibration threshold matches or exceeds baseline answerable recall, has a
no-evidence false-positive query rate at most `0.05`, and has zero stale,
ambient, scope, and invalid-evidence leaks on a separate, untouched holdout.

## Authored synthetic benchmark

`fixtures/eval/quality/` contains a frozen, fully synthetic Chinese benchmark:
60 independently written source facts and 160 independently written queries
(120 answerable, 40 no-evidence). Each positive concept group has two distinct
queries against the same gold source turn; each negative concept group has one
query and no gold reference. The group split is exactly 50/25/25 (train /
calibration / test), which yields 80/40/40 queries. Groups cannot cross splits;
positive gold references are checked against the dedicated corpus. The domains
cover finance, schedules, travel, health, home, education, work, food, devices,
and community topics. No user recordings or private data are included.

`questions.json` supplies one direct query per positive source turn for the
harness's classic regression pass. `links.json` is intentionally empty so no
curated association can alter the benchmark outcomes. The retrieval fixture
carries `group_id`, `split`, `bucket`, answerability, gold source references,
and author notes.

The runner verifies exact counts, Chinese non-empty queries, duplicate IDs and
queries, source-reference existence, group sizes, one split per concept group,
and split question/group totals. Its current threshold protocol is:

1. Run baseline on train as a diagnostic.
2. Compare baseline and candidate thresholds on calibration only.
3. Select a candidate only if all calibration gates pass. If none does, the
   best false-positive-safe threshold is labeled diagnostic only.
4. Run baseline and (when available) diagnostic/selected candidate on test.
   A diagnostic-only candidate cannot enable a default.

Run it with a locally built CLI:

```sh
go build -o /tmp/context-eval ./cmd/context-eval
python3 scripts/run-retrieval-quality.py \
  --binary /tmp/context-eval \
  --repo-root . \
  --out /tmp/retrieval-quality \
  --timeout 300
```

The JSON report contains input SHA-256 hashes, per-run command and exit status,
retrieval request and semantic embedding invocation counts, embedding index
item/request/time counts, per-query latency, lexical and hybrid p50/p95, MRR@10,
precision@5, recall@5, false-positive rates, and split/mode/bucket metrics.
The archived first-freeze reports predate the precision@5 field; current harness
and CLI output includes it. These are local
hash-embedding measurements; `embedding_provider` explicitly reports that no
external embedding provider was configured or called. Index latency is
reported separately from per-query build latency.

## First frozen run

The first run is preserved in
[`docs/evals/retrieval-quality-offline-v1/manifest.json`](evals/retrieval-quality-offline-v1/manifest.json)
with its split files and baseline/calibration reports. Its original fixture
hashes are:

- retrieval questions: `c00819b67715bd4970adf534f7b75e93c0f6a509b77b32c7052dbbc312dc8a9d`
- classic questions: `4337eb5215a92f0eb5ea2d4084aaf697d4dd538d8e4093cf082ed60949a003bf`
- source corpus: `6887982ebe26f4d56780350081691ea21fa75c002c09bbb5369f7a0861de19b0`

On the first run, baseline hybrid recall@5 was `0.40` on calibration and `0.667`
on test; baseline hybrid MRR@10 was `0.158` and `0.365`, respectively. Baseline
hybrid no-evidence false-positive rate was `1.0` on both splits. The observed
calibration candidate table was:

| Threshold | Hybrid recall@5 | No-evidence FP rate | MRR@10 | Calibration gate |
|---:|---:|---:|---:|---|
| 0.10 | 0.567 | 1.00 | 0.302 | fail: false positives |
| 0.20 | 0.367 | 0.30 | 0.350 | fail: recall and false positives |
| 0.30 | 0.100 | 0.10 | 0.100 | fail: recall and false positives |
| 0.50 | 0.000 | 0.00 | 0.000 | fail: recall |
| 0.80 | 0.000 | 0.00 | 0.000 | fail: recall |
| 1.00 | 0.000 | 0.00 | 0.000 | fail: recall |

No candidate passed all calibration gates. Thus there is no selected threshold
and the default stays `0.05`. The only held-out run was baseline; no threshold
candidate was evaluated on test because calibration selected none.

A query-wording revision was attempted after looking at the first scores, then
discarded. The repository fixture was restored byte-for-byte to the first-run
retrieval and classic question hashes above. The first-run test results are retained for audit, but the manifest marks the
split as exposed after a subsequent query-wording edit attempt. Do not treat this
benchmark version as an independent release holdout or use it to claim quality
improvement. The next release-quality claim needs a newly authored and frozen
holdout that has not been inspected during calibration.

For audit, the archived raw run was executed before the final bucket aggregation
filter landed. Its `retrieval_buckets` contains an extra `/lexical/` row for the
classic pass whose split/bucket labels are empty; ignore that row. The other
split/mode/bucket rows and aggregate query metrics are retrieval-only. Current
harness runs exclude classic checks from retrieval bucket aggregation.

## Metric interpretation and limits

The harness report's MRR@10 ranks returned package items by their order and
counts the first item whose evidence contains a gold turn. p50/p95 cover each
retrieval build request and do not include corpus ingest or index time. The
`semantic_*_embed_requests` values are provider method invocations; the shipped
benchmark uses a deterministic local hash embedder, so they are not network API
calls. A `PASS` run means the evaluation command completed; it does not mean the
quality gate passed. The first benchmark is intentionally exploratory, and its
results do not justify a default threshold change.
