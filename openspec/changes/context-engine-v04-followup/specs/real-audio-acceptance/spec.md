# Spec delta：real-audio-acceptance（follow-up）

## ADDED Requirements

### Requirement: Human gold authoring is a three-step, non-delegable flow

The system SHALL provide `prepare`, `finalize` and `score` steps that each write only into a new private
directory. `prepare` MUST emit a draft with `provenance:"draft"` and empty annotator/reviewed_at/category,
MUST NOT write a human gold document, and MUST refuse an output path that already exists.
`finalize` MUST refuse a draft the operator did not sign (any `provenance` other than `human`), an empty
annotator, a `reviewed_at` without timezone, an empty category, and an empty `scored_regions`.
`score` MUST NOT start a service, load a model or run a Go test.

#### Scenario: CE04F-GOLD-001
- **WHEN** `prepare` runs on an explicitly authorized recording
- **THEN** the private root is 0700, every file is 0600, the page references no external resource, and the draft has `provenance:"draft"` with empty annotator, reviewed_at and category

#### Scenario: CE04F-GOLD-002
- **WHEN** `finalize` is given the unedited draft
- **THEN** it fails with `draft_not_reviewed` and creates no output directory

#### Scenario: CE04F-GOLD-003
- **WHEN** a reviewed document fails any gold rule (empty annotator, timezone-free `reviewed_at`, empty category, empty or overlapping or out-of-range regions, duplicate identifiers)
- **THEN** `finalize` rejects it before creating any output directory

#### Scenario: CE04F-GOLD-004
- **WHEN** a human-signed reviewed document is finalized
- **THEN** a `context-engine-acceptance-v1` manifest pins the audio and gold hashes, the gold keeps `provenance:"human"`, and the prepare directory keeps its own copy of the audio

### Requirement: Gold schema distinguishes unannotated from confirmed-empty

`context-engine-gold-v1` MUST require `sample_id`, `provenance:"human"`, `annotator`, a timezone-bearing
`reviewed_at`, `audio_duration_ms` and non-overlapping integer `scored_regions` within the recording.
An omitted array means "not annotated"; an explicit empty array means "confirmed empty". Speaker trials
MUST carry id, split, `same_speaker` and left/right `{sample_id, speaker, start_ms, end_ms}` that reference
samples authorized by the same manifest. A `queries` entry with `evidence_completeness:"none"` MUST NOT
carry `required_spans`; `"complete"` MUST.

#### Scenario: CE04F-GOLD-014
- **WHEN** gold speaker trials reference a sample outside the manifest or cannot form two speakers with cross-recording positives and negatives
- **THEN** validation rejects the former and EER reports NOT_EVALUABLE with the missing coverage instead of a number

### Requirement: Prediction bundles and adjudications stay separate from gold

A `context-engine-predictions-v1` bundle MUST carry the audio/gold/config/model/binary hashes, the actual
provider and revision, per-sample current turns, per-query Web and MCP outputs, claims, stage status and
resource samples, and MUST NOT carry voiceprint vectors. `context-engine-adjudications-v1` MUST carry the
`prediction_digest` of that bundle, an annotator, a timezone-bearing `reviewed_at` and per-claim / per-ref
boolean verdicts. Unknown or duplicate claim/ref identifiers and a mismatched digest MUST fail.

#### Scenario: CE04F-GOLD-008
- **WHEN** adjudications carry a digest that does not match the prediction bundle
- **THEN** scoring fails with `digest_mismatch` and writes no score file

#### Scenario: CE04F-GOLD-012
- **WHEN** a real acceptance run is requested without a trusted local ASR configuration, or with a test-stub model
- **THEN** the run is BLOCKED with the missing requirement and no stack starts

#### Scenario: CE04F-GOLD-013
- **WHEN** a sample's manifest entry and human gold both validate
- **THEN** the sample is executed for real and is never reported as a NOT_RUN placeholder

### Requirement: Scoring denominators match the acceptance definition

`event_pr` MUST use a same-label maximum-cardinality matching, not a greedy one. `evidence_accuracy` MUST
use every returned ref as the denominator, count invalid, non-current, wrong-recording, wrong-revision,
out-of-scope, ambient and forgotten refs as errors, return null only for zero returned refs and 0 for a
non-empty all-illegal set, and report the structural legal rate separately from a semantic accuracy that
needs a human verdict per ref. `decision_precision` MUST report a formal precision only for a kind whose
predictions are all adjudicated, MUST keep null plus review counts otherwise, and MUST report
NOT_EVALUABLE for zero predictions. CER/WER MUST be pooled by numerator and denominator. Scoring MUST be
reproducible without any model or service call.

#### Scenario: CE04F-GOLD-005
- **WHEN** two same-label reference events each overlap one hypothesis event such that greedy IoU matching takes one and blocks the other
- **THEN** `event_pr` reports `tp=2`

#### Scenario: CE04F-GOLD-006
- **WHEN** one supported ref and one out-of-scope ref are returned for a scoped query
- **THEN** `evidence_accuracy` reports accuracy 0.5 with `out_of_scope: 1` in the error counts

#### Scenario: CE04F-GOLD-007
- **WHEN** only one of two predicted claims has an adjudication
- **THEN** the formal precision is null with status NOT_RUN while the review counts stay visible

#### Scenario: CE04F-GOLD-009
- **WHEN** the reference text is non-empty and the prediction returned no turns
- **THEN** CER and WER report the full deletion error rather than NOT_EVALUABLE

#### Scenario: CE04F-GOLD-010
- **WHEN** one manifest sample fails validation during scoring
- **THEN** the other samples are still scored and the failed sample is listed in `excluded_samples`

#### Scenario: CE04F-GOLD-011
- **WHEN** scoring runs with a valid frozen manifest and bundle
- **THEN** no subprocess, model or service is invoked

### Requirement: Real collection reads the shipped state, not its first page

The real harness SHALL page a recording's turns until `next_offset` is null, and MUST fail on a repeated
turn id or a page whose offset does not advance instead of treating the first page as the transcript. The
MCP probe MUST open the stack's own profile root and inherit its process environment. Acoustic and diarization
predictions MUST come from current, active, succeeded audio-analysis runs with the event clock shifted by
`run_start_ms` (falling back to the run's `start_ms`); ASR speaker labels MUST NOT stand in for diarization.
Decision predictions MUST come from the read-only decision trail, MUST be deduplicated by candidate, MUST
only include evidence attributed to the same sample, and a truncated or failed read MUST block a quality pass.

#### Scenario: CE04F-GOLD-015
- **WHEN** a recording has more than one page of turns
- **THEN** every page is collected, and a duplicate turn id or a stalled `next_offset` fails the collection

#### Scenario: CE04F-GOLD-016
- **WHEN** the MCP probe runs for a sample
- **THEN** it opens the stack's profile root with the stack's environment and requests raw evidence

#### Scenario: CE04F-GOLD-017
- **WHEN** audio-analysis runs exist for the recording
- **THEN** only current, active, succeeded runs contribute, event spans use the `run_start_ms` clock, and a failed capability stays distinguishable from a successful empty result

#### Scenario: CE04F-GOLD-018
- **WHEN** a decision record is a candidate or confirmed state
- **THEN** it becomes one prediction per candidate, and a record whose evidence belongs to another sample or to no sample is skipped

#### Scenario: CE04F-GOLD-019
- **WHEN** the decision trail read is truncated or the tool fails
- **THEN** the collection is marked incomplete and the run cannot support a quality pass

### Requirement: A reviewed set freezes only when every cross-sample reference resolves

`finalize-set` SHALL accept a `context-engine-reviewed-set-v1` document naming each reviewed draft and its
authorization reference, and MUST refuse a non-human draft, a path-like or duplicate sample id, a symlinked
source, an existing target, a bad hash, a duplicate global query/claim/trial id, a reference outside the set,
a missing acceptance category and speaker evaluation trials that cannot form an EER. All checks MUST run
before the new root is created, and an I/O failure MUST remove only the root this call created.

#### Scenario: CE04F-GOLD-020
- **WHEN** every draft is human-signed and all four categories have samples
- **THEN** one manifest is written under a new 0700 root with `samples/<id>/audio.wav` and `samples/<id>/gold.json`

#### Scenario: CE04F-GOLD-021
- **WHEN** two samples reuse a query, claim or trial id, or reference a sample outside the set
- **THEN** finalization fails and creates no root

#### Scenario: CE04F-GOLD-022
- **WHEN** a configured category has no sample, or speaker evaluation trials cannot cover an EER
- **THEN** finalization fails with the missing coverage instead of freezing a set

#### Scenario: CE04F-GOLD-023
- **WHEN** a sample id is path-like, a source audio file is a symlink, or the target root exists
- **THEN** finalization fails before writing

### Requirement: Adjudication drafting never pre-fills a verdict

`adjudicate-prepare` SHALL write an empty `context-engine-adjudications-v1` draft plus a readable review
sheet into a new private directory. Every claim and every returned Web/MCP ref MUST appear with
`accepted:null` and empty annotator/reviewed_at, and the existing validator MUST refuse that draft until a
reviewer signs every row.

#### Scenario: CE04F-GOLD-024
- **WHEN** a prediction bundle is prepared for adjudication
- **THEN** the draft carries the bundle digest, every row is null, and the sheet shows the prediction text and evidence

#### Scenario: CE04F-GOLD-025
- **WHEN** the unfilled draft is passed to the validator
- **THEN** it is refused; an unknown, duplicate or digest-mismatched row is also refused

### Requirement: The production gate is computed from stored counts

Scoring with `--production-thresholds` SHALL attach a `production_gate` computed from stored numerators and
denominators and MUST refuse to pass on a missing, non-finite or zero-denominator metric, an experimental
EER, an unadjudicated decision or ref, an excluded sample, a missing category or incomplete stages. A
measured value over its threshold is `FAIL`, missing material is `BLOCKED`, and `FAIL` wins. The gate MUST
write the counts it used, the SHA-256 of the thresholds, manifest, predictions and adjudications, and the
command MUST exit non-zero unless the gate is `PASS`.

#### Scenario: CE04F-GOLD-026
- **WHEN** every category meets the approved thresholds with complete adjudications
- **THEN** the gate is `PASS` and the CLI exits zero

#### Scenario: CE04F-GOLD-027
- **WHEN** one category misses one metric while the pooled average would pass
- **THEN** the gate is `FAIL` and names the category and metric

#### Scenario: CE04F-GOLD-028
- **WHEN** a category, denominator, adjudication or the EER evaluation coverage is missing
- **THEN** the gate is `BLOCKED` rather than scored from a partial set

#### Scenario: CE04F-GOLD-029
- **WHEN** the EER is experimental, or a decision or returned ref is unadjudicated
- **THEN** the gate cannot pass

#### Scenario: CE04F-GOLD-030
- **WHEN** DER is scored from a prediction bundle
- **THEN** it uses the `diarization` field; a bundle without it reports NOT_RUN and the ASR labels are never substituted

#### Scenario: CE04F-GOLD-031
- **WHEN** `score_eer` sees evaluation trials with overlapping scores, a calibration trial, a missing score or no model
- **THEN** the EER reflects the evaluation split only, calibration never changes it, and a missing score or model is NOT_EVALUABLE or BLOCKED
