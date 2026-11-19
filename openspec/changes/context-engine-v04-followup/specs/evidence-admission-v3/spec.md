# Spec delta：evidence-admission-v3（follow-up）

## ADDED Requirements

### Requirement: A bounded profile separates model cost from construction cost

The v3 protocol SHALL provide a `profile` phase that uses the train split only, fixes the admission
threshold at 0, writes no selection lock and never submits a test question. It MUST report, per query,
the accumulated wall time spent inside `decision.Engine.Evaluate` (admission plus final assessment), the
admission/assessor/rerank stage distributions, the end-to-end distribution and the assessor request
count, candidate count and request bytes. A warmup on the fixed first train question MUST precede each
measured run and the embedding cache MUST be flushed afterwards; the warmup MUST NOT enter any metric.

#### Scenario: CE04F-LATENCY-001
- **WHEN** `--phase profile` runs
- **THEN** every executed split is `train`, no selection lock exists afterwards, and the manifest records
  `test_touched=false`

#### Scenario: CE04F-LATENCY-002
- **WHEN** the per-query engine p95 already exceeds 1.5× the same-run v1 p95
- **THEN** the profile reports `model-bound` and states that client-side construction cannot bring this
  configuration under budget, scoped to that train configuration, provider and binary only

#### Scenario: CE04F-LATENCY-004
- **WHEN** the incremental request-size accounting is compared with `json.Marshal` of the whole
  `{state, questions}` document
- **THEN** the two byte counts are equal for every batch of the fixture

### Requirement: Batch construction is computed once and stays byte-identical

`DecisionCandidateAssessor` SHALL render each candidate block and each fixed question once per assessment
pass, SHALL reuse those encodings for both the 64 KiB cap check and the submitted request, and SHALL keep
candidate IDs, order and evidence text unchanged. A candidate that cannot fit the cap MUST be rejected
with relation unknown and probability 0 without truncation, and the following candidates MUST still be
submitted. Request accounting MUST be collected only when an offline profile asks for it.

#### Scenario: CE04F-LATENCY-003
- **WHEN** a batch measures exactly 64 KiB
- **THEN** it is submitted as one request, and the same batch one byte larger is split; a candidate larger
  than the cap is dropped while the next small candidate is still submitted

### Requirement: The selection lock restores everything holdout needs

`retrieval-v3-selection-lock-v2` SHALL store the complete configuration (including `embedding_input_type`,
the admission batch size and the fixed `max_items=10`/`max_bytes=65536` budget), the configuration hash,
the actual embedding and assessor identity, the fixture/binary/policy-template hashes and the warmup rule.
A lock whose configuration does not match its own hash, whose budget differs, whose binary or identity
differs, or which was written by the older schema MUST be rejected. The holdout MUST restore provider,
endpoint, model, revision, batch size, reranker and budget from the lock, MUST fail before sending any
test query when an explicitly provided flag conflicts with the lock, and MUST reject the calibration
probability cache and a flush URL that does not target the embedding service's own host and port.

#### Scenario: CE04F-HOLDOUT-004
- **WHEN** holdout runs with only `--selection-lock` and no model flags
- **THEN** the provider, endpoint, model, batch size and budget are taken from the lock, and an explicitly
  conflicting flag fails before any test query

#### Scenario: CE04F-HOLDOUT-010
- **WHEN** holdout is given an explicitly different endpoint, model, batch size or input-type flag
- **THEN** it fails with a lock-conflict error, sends no test query and leaves `holdout_started` false

#### Scenario: CE04F-HOLDOUT-005
- **WHEN** holdout is given the calibration probability cache flag
- **THEN** the run is refused before any binary invocation

#### Scenario: CE04F-HOLDOUT-007
- **WHEN** a lock names another binary, identity, fixture, policy template, budget or the old schema
- **THEN** validation fails and no test question is sent

#### Scenario: CE04F-HOLDOUT-008
- **WHEN** the assessor model is a floating `latest` alias without a recorded actual identity
- **THEN** no lock is written and the reason names the unverifiable identity

#### Scenario: CE04F-HOLDOUT-009
- **WHEN** `--flush-embed-cache-url` targets a different host or port than `--embed-endpoint`
- **THEN** the run is refused instead of reporting an uncached measurement

### Requirement: Calibration covers the whole grid and the holdout is consumed once

Every v3 phase SHALL require a new or empty output directory. Calibration MUST execute every threshold in
0..100 for each screened backend rather than inferring the grid from the threshold-0 latency, and the final
gate MUST come from one uncached rerun of the selected candidate on the same hardware. No lock MUST be
written when that rerun or the gate fails. The holdout claim MUST use `O_CREAT|O_EXCL`, MUST be fsynced and
MUST be written before any test request; a crash or timeout MUST NOT allow removing the marker to rerun.

#### Scenario: CE04F-HOLDOUT-001
- **WHEN** a v3 phase is pointed at a non-empty output directory
- **THEN** it is refused instead of overwriting existing calibration or holdout evidence

#### Scenario: CE04F-HOLDOUT-002
- **WHEN** calibration completes with an assessor configured
- **THEN** all 101 thresholds are executed and exactly one uncached final rerun decides the gate and lock

#### Scenario: CE04F-HOLDOUT-003
- **WHEN** the selected candidate fails its uncached final rerun
- **THEN** no selection lock is written and the manifest records the failure

#### Scenario: CE04F-HOLDOUT-006
- **WHEN** a holdout was claimed and the process died before finishing
- **THEN** a second holdout attempt is refused and the consumed marker is never removed
