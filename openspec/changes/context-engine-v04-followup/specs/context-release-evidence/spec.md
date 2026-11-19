# Spec delta：context-release-evidence

## ADDED Requirements

### Requirement: Every evidence row states what it actually proves

The v0.4 release results document SHALL use `context-engine-v04-release-results-v2` and mark every row as
`current` or `historical`. A `PASS` row MUST name the command that was run, MUST carry exit code 0, and
MUST carry an artifact path with the matching SHA-256 when it names an artifact. `NOT_RUN`, `BLOCKED` and
`EXPERIMENTAL` rows MUST carry a reason.

#### Scenario: CE04F-RELEASE-001
- **WHEN** a `PASS` row has no command or a non-zero exit code
- **THEN** validation fails

#### Scenario: CE04F-RELEASE-002
- **WHEN** a `PASS` row names an artifact that is missing or whose hash changed
- **THEN** validation fails

#### Scenario: CE04F-RELEASE-006
- **WHEN** a `required_for_merge` row is marked `historical`
- **THEN** validation refuses to treat it as current evidence for a READY merge decision

### Requirement: Readiness claims must match the recorded evidence

`decision` SHALL carry `merge_readiness`, `release_readiness`, `scope`, `blockers`,
`evidence_snapshot`, `required_for_merge`, `required_for_release` and
`production_ready_recommendation`. READY MUST require every required row to be `PASS` and `current`, and
MUST contradict neither a blocker marked `blocks_merge` nor one marked `blocks_release`. A `FAIL` row MUST
never support READY. `EXPERIMENTAL` or incomplete evidence MUST never support a production recommendation,
which additionally requires a pre-approved production threshold. `release_readiness` BLOCKED MUST name at
least one blocker with `blocks_release=true`.

#### Scenario: CE04F-RELEASE-003
- **WHEN** v3 calibration is recorded `FAIL` but every v1 engineering row is `PASS` and `current`
- **THEN** the decision may be merge READY while release stays BLOCKED, and the recommendation stays false

#### Scenario: CE04F-RELEASE-004
- **WHEN** a production recommendation cites `EXPERIMENTAL` evidence, or no pre-approved threshold exists
- **THEN** validation fails

#### Scenario: CE04F-RELEASE-005
- **WHEN** a file hashed in `evidence_snapshot` changed, or the snapshot does not pin a commit and a diff hash
- **THEN** validation fails

### Requirement: The snapshot pins the state the evidence was collected against

`evidence_snapshot` SHALL record the candidate HEAD, the SHA-256 of the relevant sources and configuration,
and the hash of the uncommitted diff. Validation MUST recompute those hashes from the working tree and
MUST fail when they differ.

#### Scenario: CE04F-RELEASE-007
- **WHEN** `evidence_snapshot` omits the candidate HEAD or the uncommitted diff hash
- **THEN** validation fails before any readiness claim is accepted

### Requirement: The release decision consumes the quality gate

`release_readiness` READY MUST carry a non-empty `required_for_release` whose rows are all `PASS` and
`current` and MUST contradict no blocker marked `blocks_release`. It MUST also contain the audio quality gate
row (`decision.audio_quality_evidence_id`), the v3 calibration and the v3 holdout. `production_threshold`
MUST pin `artifact` and `artifact_sha256`; the gate row's artifact MUST be a score containing a
`production_gate` whose `thresholds_sha256` matches the approved artifact. Validation MUST recompute every
ratio from the stored counts, MUST reject negative or non-integer counts, a mismatched adjudication
denominator or a hand-written PASS string, and MUST fail when a recomputed value misses its threshold.

#### Scenario: CE04F-RELEASE-008
- **WHEN** release READY has an empty or incomplete required list, or a required row is not `PASS` and `current`
- **THEN** validation fails

#### Scenario: CE04F-RELEASE-009
- **WHEN** release READY contradicts a blocker marked `blocks_release`, or omits the audio gate, calibration or holdout rows
- **THEN** validation fails

#### Scenario: CE04F-RELEASE-010
- **WHEN** a gate artifact claims PASS while its stored counts breach a threshold
- **THEN** validation recomputes the ratio and fails with the category and metric

#### Scenario: CE04F-RELEASE-011
- **WHEN** the gate `thresholds_sha256` differs from the approved threshold artifact, or the artifact hash changed
- **THEN** validation fails

#### Scenario: CE04F-RELEASE-012
- **WHEN** the gate stores a negative or non-integer count, or an adjudication denominator that does not match its total
- **THEN** validation fails

#### Scenario: CE04F-RELEASE-013
- **WHEN** `decision.audio_quality_evidence_id` is missing or names a row outside `required_for_release`
- **THEN** validation fails
