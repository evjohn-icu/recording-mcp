# Worldline relation quality fixture and runner

This is a frozen, synthetic-only evaluation of the five-way relation decision used by `internal/app/worldline_semantic.go` (`worldlineSemanticCriteria` and `worldlineSemanticQuestion`): `text_similarity`, `complementary_context`, `review_required`, `unrelated`, and `insufficient_evidence`. It tests Chinese paraphrases, aliases, negation/contradiction, time and value updates, different subjects, clipped/quoted partial evidence, and unknown identity. A relation is not proof of identity or fact. No output authorizes merging or asserting reality.

## Fixture and split

`fixtures/worldline-relations-quality.json` contains 120 distinct meaningful synthetic pairs: 60 calibration and 60 test, 12 examples per class in each split. Every example has its own conversation group; no conversation group crosses the split boundary. Both items deliberately share a normalized title, matching the production question's stated assumption. Gold labels live outside the model state. Do not tune or relabel the frozen test set based on inference outcomes. Fixture SHA-256 is calculated by the runner and included in each output.

Inspection found no exact duplicate normalized item text, and no cross-split item pair at or above 0.90 `difflib.SequenceMatcher` similarity after NFKC normalization and removal of non-word characters; each case's synthetic source IDs agree with its conversation group. These are narrow heuristic checks only: they do not establish semantic independence, and the per-case unique group construction cannot verify real-world source provenance. The fixture is synthetic and does not model naturally occurring group/source reuse.

## Run

The runner uses only Python standard library and permits only plain HTTP literal loopback endpoints. It follows the SystemOne request/response contract used by `scripts/worldline-decision-experiment.py` and `internal/decision.SystemOneClient`: `state` is JSON-encoded text, questions are keyed by question ID, and choice answers contain a `choice` plus a full `probabilities` distribution. It does not interpret a probability distribution as an answer choice. Requests disable proxy use and redirects. Every response must map exactly to the requested question ID and provide nonempty, stable `model` and `weights_version` identities across the run.

```sh
python3 scripts/worldline-relation-quality.py --validate-only
python3 scripts/worldline-relation-quality.py --endpoint http://127.0.0.1:8799/v1/systemone --split calibration --limit 1 --output /tmp/worldline-relation-smoke.json
python3 scripts/worldline-relation-quality.py --endpoint http://127.0.0.1:8799/v1/systemone --split calibration --output docs/evals/worldline-relations-calibration.json
python3 scripts/worldline-relation-quality.py --endpoint http://127.0.0.1:8799/v1/systemone --split test --output docs/evals/worldline-relations-test.json
python3 scripts/test_worldline_relation_quality.py
```

The runner validates exact probability keys, finite [0,1] values and sum-to-one, selected choice, question-ID mapping, and nonempty engine/weights identities; it records distributions, per-class precision/recall and supports, confusion matrix, abstentions, denominators, hashes, latency and request/validation errors. Errors are not converted to `insufficient_evidence`. The denominator counts valid model judgments only; abstain counts actual valid `insufficient_evidence` choices. Any attempted inference/schema error, identity drift, or incorrect answer yields `status: FAIL` and a nonzero exit. The CLI default is the existing local model endpoint on port 8799. A test run is not `NOT_RUN` merely because failures occurred; `NOT_RUN` is reserved for no inference configuration/attempt. `--limit 1` supports smoke testing before a full batch.

Calibration may inform decision thresholds only; preserve the test split for independent reporting. Test-set outcomes must not be used to alter fixture labels. The fixture and scores are small synthetic evidence, not a production quality guarantee.

## Local run status

Both complete 60-case splits were run against the already-running local endpoint `http://127.0.0.1:8799/v1/systemone`; port 8790 was not used because it is the web SPA. Every inference response passed the runner's question-ID, choice, probability-distribution, and identity checks; request/schema errors were zero. All responses identified `Jev-Style-0.8B-Decision-v3`, weights `b023d1f9c7858fbf01504577a3bfc349ea5c7385`. No model was started/stopped, no weights installed, and no external provider or private data was used.

**FAIL — calibration quality:** 37/60 correct (23 incorrect), with zero request/schema errors. Mean/p50/p95 latency was 100.8/102.3/136.1 ms. Per-class precision/recall: text_similarity 0.545/1.000; complementary_context 0.429/0.250; review_required 0.857/0.500; unrelated 0.579/0.917; insufficient_evidence 1.000/0.417. Confusion matrix and per-case outputs are in [calibration](evals/worldline-relations-calibration.json).

**FAIL — held-out test quality:** 37/60 correct (23 incorrect), with zero request/schema errors. Mean/p50/p95 latency was 88.9/87.4/108.6 ms. Per-class precision/recall: text_similarity 0.500/0.917; complementary_context 0.571/0.333; review_required 0.857/0.500; unrelated 0.579/0.917; insufficient_evidence 1.000/0.417. Confusion matrix and per-case outputs are in [test](evals/worldline-relations-test.json). These are observed synthetic-fixture results, not a claim of production support or quality acceptance.

The one-case calibration smoke check passed with the expected model and weights identity. Full-run latency means were 108.4 ms calibration and 92.7 ms test. The reports use the server's returned choice and validate its actual full distribution; mistakes remain visible as quality failures rather than being counted as supported behavior.

Commands actually executed: one-case smoke run above; both full split commands above; `python3 scripts/test_worldline_relation_quality.py` (results recorded after implementation). Protocol/schema validation passed for all 120 responses; the overall quality result failed because 23 judgments in each split were incorrect. External providers remain `NOT_RUN`; these were real inference calls to an already-running loopback JEV model. No private-data inspection was performed.
