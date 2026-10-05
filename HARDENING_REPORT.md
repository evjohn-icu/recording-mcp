# Hardening report — correctness / invariant sprint

Base: `c48dfc4` (Add transcript revision supersede, stale propagation and experimental build_context). This sprint turns the revision/stale semantics into code-level guarantees instead of test-shaped coincidences. No new features, no model/UI/infra integrations.

Success criterion: **any superseded transcript content cannot re-enter default current context via direct search, navigation, confirmed relation, evidence expansion, or statistics projection.** Verified by 24 new regression tests; the synthetic eval's hard guards (`stale_leakage == 0`, `invalid_evidence_refs == 0`) hold before and after.

## 1. Fixed invariants

| # | Invariant | Enforcement |
| --- | --- | --- |
| P0-1 | Explicit request scopes (`conversation_id`, `topic_ids`, `started_after/before`) are hard AND-filters on every assembly branch: lexical recall, topic/conversation navigation, confirmed-link expansion, evidence attribution | deterministic code (`internal/contextbuild/scope.go` `requestScope`, single `addItem` gate) + tests |
| P0-2 | Stale (superseded) turns never appear in current evidence windows; audit vs current lookup are separate code paths | deterministic code (`GetCurrentEvidenceContext`) + tests |
| P0-3 | A `confirmed` memory link navigates current retrieval only while its evidence is current; verdicts are never rewritten | deterministic code (read-time `MemoryLink.Stale`) + tests |
| P0-4 | Current-state statistics count only active-revision turns / non-stale derived memory | deterministic code (unified `CurrentTurnPredicate` / `NoteStalePredicate`) + tests |
| P0-5 | At most one active transcript revision per (recording, segment) | **database constraint** (`source_attachments_active_head` partial unique index) + code (supersede transaction order) + tests |
| P1 | Speaker identity never crosses a transcript revision boundary; terminal verdicts are immutable history and never block fresh claims | deterministic code (revision-scoped rows + `SpeakerProjection(recording, revision)`) + DB `UNIQUE(recording_id, speaker_scoped_id, version)` + tests |
| P1 | Stale topic membership cannot route current retrieval back to historical content | deterministic code (topic membership evaluated over current turns only) + test; topic taxonomy itself remains historical (documented TODO) |

## 2. Bugs found

1. **Scope leak (P0-1)**: `build_context({query, conversation_id})` recalled the query globally and *appended* the requested conversation — query matched the whole database; same for topic scopes. Navigation and confirmed-link expansion could pull items from outside the requested conversation/topic/time scope.
2. **Stale evidence window leak (P0-2)**: `GetEvidenceContext` windowed over *all* turns of the recording, so a current target's window mixed in superseded revision neighbours (`A, A', B` around `A'`).
3. **Confirmed links not revision-aware (P0-3)**: after a supersede, a `confirmed` link whose evidence turns were superseded still navigated `build_context`; `ValidateTopicLinks` happily accepted superseded evidence for *new* proposals.
4. **Statistics over historical turns (P0-4)**: `GetProcessingStatus`, `unanalysed_turns` gap counting, and analysis-status derivation counted superseded turns (false unanalysed gaps from dead revisions; stale notes kept analysis status at `partial`).
5. **No DB-level active-head invariant (P0-5)**: single-active-revision was an application convention only.
6. **Speaker binding lifecycle (P1)**: identity keyed on `(recording_id, speaker_scoped_id)` alone — a reused diarization label across revisions inherited the old identity; re-proposal after `revoked` was blocked by the sticky single-row model; `version` was rewritten by review.

## 3. Schema / migration changes

- **v10 `active-head-invariant`**: `CREATE UNIQUE INDEX source_attachments_active_head ON source_attachments(recording_id, segment_id) WHERE active = 1`. No existing migration semantics changed; fresh DB, legacy DB, partial re-apply and the sanctioned supersede transition (deactivate old head, then insert new head in one transaction) are all tested.
- No other schema change. Deliberate decision: **link staleness is computed at read time** from the stored evidence (`json_each(evidence_turn_ids_json) -> attachment_turns -> source_attachments.active`), instead of materialized `derived_artifacts` rows: it is always correct for pre-existing links (no backfill needed), needs no invalidation wiring, and links are few. `derived_artifacts` kinds remain `note` / `candidate`.

## 4. Retrieval semantic changes

- `internal/memory/turnscope.go` (new): `CurrentTurnPredicate(alias)` / `NoteStalePredicate(alias)` — one definition of "current turn" / "stale note" used by every surface (store.Query, GetItem, searchNotes/searchTurns, ListConversations, GetProcessingStatus, GetEvidenceContext, contextbuild gap SQL). The previously hand-copied exclusion SQL is gone.
- **Audit vs current evidence**: `GetEvidenceContext` stays the explicit audit lookup (any revision; `TurnEvidence.Stale` marker per turn added; `EvidenceContext.Stale` marks the target). New `GetCurrentEvidenceContext` is current-only: target must be current (stale target → `turn not found in current revision`), window contains only current turns, ordering unchanged and deterministic after filtering. `build_context` uses only the current path.
- `GetProcessingStatus` counts (`original_turns`, `topic_linked_turns`, `unclassified_turns`, `identity_unconfirmed_turns`, analysis status) are current-revision only.
- `build_context` scope semantics: `query + conversation_id` returns only that conversation's matches; `query + topic_ids` narrows the lexical recall itself (no global recall); time bounds exclude unknown-start conversations (same semantics as retrieval); navigation/link expansion cannot escape any explicit scope; `coverage.conversations`/`coverage.topic_ids` are scope-filtered; raw evidence windows are current-only and attributed to in-scope citing items.
- `pipeline.MemoryLink.Stale` + MCP `list_memory_links` view field `stale`: `confirmed` remains a review verdict, usability is independent; `build_context` skips stale links (audit only).
- `ValidateTopicLinks` rejects proposals citing superseded evidence (`evidence turn %q is not from the current transcript revision`).

## 5. Speaker lifecycle changes

- Bindings are **revision-scoped rows**: id `sb:sha256(recording|speaker|version)`, `version` = stable row ordinal (review never rewrites it; `UNIQUE(recording_id, speaker_scoped_id, version)` keeps rows distinct).
- `SpeakerProjection(recordingID, transcriptRevision)` resolves confirmed bindings for exactly one transcript revision — `speaker_0` in v2 never inherits v1's identity.
- Lifecycle policy (documented in code + tests): a `proposed` row is a refreshable draft; a `confirmed` row for the same revision replays idempotently; every other verdict row (`confirmed` for another revision, `rejected`, `revoked`) is immutable history and a new proposal creates the next versioned row. `proposal v1 → confirm Alice → revoke → proposal v2 → confirm Bob` works; a rejection never blocks future claims. Full event history preserved (`speaker_binding_events`, one row per transition).
- `turns.speaker_entity_id` untouched; identity remains projection-only.

## 6. Regression tests (24 new; no existing test deleted)

No test function was deleted and no assertion was dropped. A few pre-existing assertions were **re-pointed** to the semantics this sprint explicitly redefines, and that is called out here so a diff review does not read the claim as "existing tests untouched":

- `TestSpeakerBindingLifecycle` / `TestSpeakerBindingRevokeClearsProjection`: `Version != 2` after confirm/revoke → `!= 1` — `version` is now the stable row ordinal, review no longer rewrites it (P1 lifecycle fix).
- Three speaker tests: `SpeakerProjection(rec)` → `SpeakerProjection(rec, rev)` — projection is now revision-scoped (P1 fix).
- `cmd/context-memory` tests: `newServer(store, ...)` signature updates from the earlier feature work, unchanged in this sprint.

- **Scope (13, `internal/contextbuild/build_test.go`)**: `TestBuildContextScopeQueryAndConversation`, `...ScopeQueryAndTopic`, `...ScopeQueryAndStartedAfter`, `...ScopeQueryAndStartedBefore`, `...ScopeQueryTopicAndTime`, `...ScopeQueryConversationAndTime`, `...LinkExpansionCannotEscapeConversationScope`, `...LinkExpansionCannotEscapeTimeScope`, `...NavigationCannotEscapeTopicScope`, `...EvidenceContainsCurrentTurnsOnly`, `...StaleHistoricalEvidenceNeverAppears`, `...EvidenceOrderingDeterministic`, `...ConfirmedLinkStopsAfterEvidenceSuperseded`.
- **Evidence / statistics / links (7, `internal/memory/evidence_test.go`, `staleness_test.go`, `internal/pipeline/associations_test.go`)**: `TestEvidenceWindowExcludesSupersededNeighbours`, `TestAuditEvidenceStillReadable`, `TestCurrentEvidenceWindowOrdering`, `TestProcessingStatusCountsCurrentRevisionsOnly`, `TestValidateTopicLinksRejectsSupersededEvidence`, `TestMemoryLinkStaleAfterEvidenceSuperseded`, `TestLinkReproposalAfterSupersedeKeepsHistory`.
- **DB invariant + topics + speaker (4)**: `TestActiveHeadEnforcedByDatabase` (migrations), `TestStaleTopicMembershipDoesNotSurfaceCurrentContent` (memory), `TestSpeakerBindingRevisionScopedIdentity`, `TestSpeakerBindingReproposeAfterRevokeAndReject` (memory).

## 7. Eval before / after

`go run ./cmd/context-eval --db ... --out ...` (deterministic, no model calls):

| metric | before | after | guard |
| --- | --- | --- | --- |
| evidence_recall_at_5 | 0.9286 | 0.9286 | ≥ 0.6 ✓ |
| stale_leakage | 0 | 0 | == 0 ✓ |
| invalid_evidence_refs | 0 | 0 | == 0 ✓ |
| relation_assisted | 6 | 6 | — |
| context_coverage | 0.75 | 0.75 | — |
| model_calls | 0 | 0 | — |

Unchanged, as expected: the eval questions carry no explicit scope constraints (the P0-1 fix changes only scoped requests) and the corpus's stale handling was already correct. No metric was relaxed to keep numbers.

## 8. Remaining risks

- **Topic taxonomy is historical** (`topics` / `topic_conversations` / merged annotation view): a superseded revision's topic links survive as audit rows; `list_topics` still lists topics whose members are all stale. Current *content* isolation holds (membership is evaluated over current turns only, tested), but a full revision-aware topic projection (rebuild membership per active revision at publish) is documented as TODO in `docs/processing.md` and `internal/pipeline/publish.go`.
- **`get_processing_status` semantics tightened**: analysis status returns to `pending` after a supersede until the replacement revision is re-annotated. Clients that treated `partial` as sticky will see the honest current state.
- **Link staleness for legacy evidence**: links whose evidence turns have no attachment rows (legacy imports) are treated as current, matching the legacy convention used everywhere else.
- **Scope time filtering depends on the conversation's single `recording_id` start** (same join the retrieval layer uses); multi-recording conversations are not represented differently.
- Eval remains synthetic (literal substring retrieval, synthetic generator); numbers do not predict real-model quality. `build_context` still runs degraded by default (no decision engine configured) and the reranker interface is trusted to permute only.

## 9. Next recommended task

1. Revision-aware topic projection: rebuild `topic_conversations` / `turn_topics` membership per active revision at publish time and mark stale topics in `list_topics`, removing the documented TODO.
2. Real-adapter eval run (`generate openai` + `--decision-url` rerank) against the same corpus with the same hard guards.
3. Surface the speaker-binding contract over MCP read tools (`SpeakerProjection` per active revision) now that the lifecycle is revision-safe.

## Gates

```
$ go test -race -count=1 ./...
ok  	recordingmcp/cmd/adapter-doubao	1.069s
ok  	recordingmcp/cmd/context-associate	2.656s
?   	recordingmcp/cmd/context-eval	[no test files]
ok  	recordingmcp/cmd/context-memory	4.359s
?   	recordingmcp/cmd/context-worker	[no test files]
?   	recordingmcp/cmd/import-conversation	[no test files]
?   	recordingmcp/cmd/import-transcript	[no test files]
?   	recordingmcp/cmd/server	[no test files]
?   	recordingmcp/fixtures	[no test files]
ok  	recordingmcp/internal/adapters/doubao	1.036s
ok  	recordingmcp/internal/asrinput	1.019s
ok  	recordingmcp/internal/contextbuild	13.408s
ok  	recordingmcp/internal/decision	1.057s
?   	recordingmcp/internal/eval	[no test files]
ok  	recordingmcp/internal/generate	1.058s
ok  	recordingmcp/internal/mcpserver	1.741s
ok  	recordingmcp/internal/memory	14.962s
ok  	recordingmcp/internal/migrations	4.643s
ok  	recordingmcp/internal/pipeline	24.957s
ok  	recordingmcp/internal/plane	1.544s
ok  	recordingmcp/internal/process	5.480s
ok  	recordingmcp/internal/worker	2.815s

$ go vet ./...
(no output; exit 0)

$ make build
go build -o bin/recording-mcp ./cmd/server
go build -o bin/context-memory ./cmd/context-memory
go build -o bin/import-conversation ./cmd/import-conversation
go build -o bin/adapter-doubao ./cmd/adapter-doubao
go build -o bin/import-transcript ./cmd/import-transcript
go build -o bin/context-worker ./cmd/context-worker
go build -o bin/context-associate ./cmd/context-associate
```
