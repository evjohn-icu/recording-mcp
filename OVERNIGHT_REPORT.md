# Overnight report — context-revision-stale

Session goal: versioned invalidation semantics for transcript revisions plus a task-scoped `build_context` assembler. All work is local, hermetic (no network, no `pi`, no real models), and lands in this working tree.

## Completed

- **Phase 0 — repo hygiene + docs**: root build artifacts `context-associate` / `context-memory` / `context-worker` untracked (`git rm --cached`, still buildable via `make build` into `bin/`); `.gitignore` extended with root-anchored entries. `README.md`, `docs/context-layer-runtime.md`, `docs/associations.md`, `docs/curator-prompt.md`, `docs/processing.md` (plus `docs/context-memory.md`, `docs/asr-memory-input.md`) re-synced to code: auto-enqueued curate after committed publish, existing `list_memory_links` / `review_memory_link`, real tool count, sticky rejections, honest curator trust boundary (local process with DB-file access; enforcement is `--read-only` registration + prompt + in-process `ValidateTopicLinks`).
- **Phase 1 — transcript revision supersede + stale propagation**:
  - `IngestTranscript` now supports replacement via `supersedes_revision`: the old attachment becomes `active=0` (row, `result_json`, turns and `attachment_turns` kept as immutable audit), the new revision becomes the only active one, one `extract` job is enqueued for it, and every derived artifact built from the old attachment is marked stale in the same transaction.
  - Explicit rejections with exact literals: missing `supersedes_revision` on a replace attempt, self-target, unknown target, non-head target, cross-segment target.
  - Migration v8 adds `derived_artifacts` (kind `note` / `candidate`, `stale`, `stale_reason`, `stale_at`) and `derived_artifact_sources` (attachment links), with a backfill that derives the same state for pre-existing rows.
  - Write-time provenance: `InsertCandidates` and `ApplyAnnotations` record/rebuild staleness state transactionally; late writes over a superseded attachment land stale immediately; republishing is idempotent.
  - `Store.InvalidateDerivedArtifacts` is the idempotent explicit invalidation entry point (`WHERE stale=0`; repeats return 0 and cannot spawn duplicate jobs).
  - Retrieval isolation: stale notes and turns from inactive attachments are excluded from `query_context`, `get_context_item`, `search_memory`, `list_conversations` counts (opt-in `include_stale`); `search_memory` / `get_context_item` expose `include_stale`, items/hits carry `stale` and `derived_from`, `get_evidence_context` keeps returning any turn by id and reports `stale`, `get_annotation_history` keeps old annotation versions.
  - Publish excludes stale candidates and derives the new annotation version hash only from the remaining set (all-stale → `publish:noop`).
- **Phase 2 — `build_context`**: new package `internal/contextbuild` (deterministic assembly: lexical recall, bounded topic/conversation navigation, expansion through `confirmed` topic links only, optional raw-evidence windows, evidence-subset dedupe, byte/item budget, coverage, gaps, trace; stale is never current). Note evidence is hydrated from `note_evidence` for items found through navigation and link expansion (the query view carries none), so link-discovered items are citable by `include_raw_evidence` and take part in the source-evidence dedupe. MCP tool `build_context` registered as read-only (`ReadOnlyHint: true`), plus server flags `--decision-url` (empty = deterministic, `degraded: true` with honest reason) and `--decision-model`. `DecisionReranker` reorders only, never fabricates.
- **Step 3 — curator trust boundary**: `context-memory --read-only` registers exactly the 11 read tools and none of the 3 write tools (test-enforced); `scripts/run-curator.sh` registers the MCP server with `--read-only`.
- **Step 4 — speaker/person binding contract**: migration v9 `speaker_bindings` / `speaker_binding_events`, `internal/memory/speakers.go` (propose / confirm / reject / revoke + `SpeakerProjection` returning confirmed bindings only, scoped per recording), lifecycle tests.
- **Step 5 — synthetic eval harness**: `fixtures/eval/{corpus,questions,links}.json` (18 synthetic conversations / 22 segments, 42 questions, 2 confirmed + 1 proposed links) and `cmd/context-eval` + `internal/eval`. Deterministic default run (no network, `model_calls: 0`).

## Architecture decisions

- **Stale is excluded at retrieval, never deleted.** Live tables keep the derived rows; current-state queries filter them; audit paths are `include_stale`, `get_evidence_context().stale`, and `get_annotation_history`.
- **`source_attachments.active` is the revision-level source of truth**; derived state is a materialized projection of it (`derived_artifacts.stale`), refreshed at write time and at supersede time. Legacy rows without attachment links stay current (`stale=0`, zero source rows).
- **One revision per segment at a time.** Replacement is explicit and targeted (`supersedes_revision` must name the current active revision); replays of the same revision remain idempotent and conflicting content is still rejected.
- **Job dedup is inherited, not invented**: the only new job is the replacement's `extract` job; invalidation itself enqueues nothing, so repeated invalidation cannot duplicate work.
- **`build_context` is deterministic by default.** The reranker is optional and shadow-only; `degraded: true` never means empty. Assembly always excludes stale content. `Item.ID` (stored turn row id) is what evidence lookups accept; `Item.TurnID` carries the original transcript reference — the builder hydrates evidence ids from the stored item / evidence window for every branch so items from navigation and link expansion are self-describing.
- **Read-only is a tool-surface boundary, not a sandbox.** The curator remains a local process able to open the SQLite file; docs state this and list the three actual enforcement layers.
- **Eval gold is authored once** as human turn/topic refs and resolved deterministically to stable ids; negative controls (superseded v1 evidence, proposed-but-unconfirmed link) are kept and expected to score zero.

## Migrations

| version | name | contents |
| --- | --- | --- |
| v7 | `annotation-history` | adopts the lazily created `annotation_history` table into the migration ledger (`CREATE TABLE IF NOT EXISTS`, safe on DBs created by older builds) |
| v8 | `derived-artifact-staleness` | `derived_artifacts` + `derived_artifact_sources` + indexes + backfill for existing notes/candidates (stale iff any evidence attachment is inactive) |
| v9 | `speaker-binding` | `speaker_bindings` + `speaker_binding_events` (scoped `(recording_id, speaker_scoped_id)`, status proposed/confirmed/rejected/revoked, version bump on confirm) |

`internal/memory/revisions.go` no longer creates any table at runtime. Migration tests cover fresh apply, repeated apply, legacy upgrade, partial re-apply, adoption of a lazily created `annotation_history`, and a seeded backfill over pre-existing notes/candidates.

## MCP/API changes

`context-memory` now exposes **14 tools: 11 read + 3 write** (`ingest_transcript`, `apply_annotations`, `review_memory_link` are the writes).

- New tool `build_context` (read-only, experimental): input `Request{query, conversation_id, topic_ids, started_after, started_before, max_bytes, max_items, include_raw_evidence}`; output `Package{items, evidence, coverage, gaps, truncated, degraded, degrade_reason, trace}` with exact JSON names as specified. At least one of `query` / `conversation_id` / `topic_ids` required; limits default 16000 bytes / 12 items and clamp to [1, 262144] / [1, 64].
- New server flags: `--read-only`, `--decision-url` (empty disables reranking), `--decision-model` (default `jev-style-2b-v2`).
- Additive fields: `include_stale` on `query_context` / `get_context_item` / `search_memory`; `stale` + `derived_from` on items and search hits; `stale` on `get_evidence_context`; `supersedes_revision` on `ingest_transcript` is now enforced (previously ignored/rejected); `list_conversations` counts are now active-revision scoped.

## Tests

35 new/extended tests; the full suite is green (`-count=1`, race detector). Headline additions: the 12 named Phase 1 tests (including `TestRetrievalExcludesStaleDerivedMemory` for the retrieval-isolation surface) plus `TestEndToEndSupersedeReprocess` (ingest → extract → publish → search → supersede → stale marked → re-extract → re-publish → old note only via `include_stale`, `get_evidence_context` stale, old annotation archived), 12 `contextbuild` cases, the read-only surface test, the `build_context` MCP smoke test, the migration backfill test, and 6 speaker-binding lifecycle tests.

```
$ go test -race -count=1 ./...
ok  	recordingmcp/cmd/adapter-doubao	1.033s
ok  	recordingmcp/cmd/context-associate	3.147s
?   	recordingmcp/cmd/context-eval	[no test files]
ok  	recordingmcp/cmd/context-memory	4.894s
?   	recordingmcp/cmd/context-worker	[no test files]
?   	recordingmcp/cmd/import-conversation	[no test files]
?   	recordingmcp/cmd/import-transcript	[no test files]
?   	recordingmcp/cmd/server	[no test files]
?   	recordingmcp/fixtures	[no test files]
ok  	recordingmcp/internal/adapters/doubao	1.040s
ok  	recordingmcp/internal/asrinput	1.031s
ok  	recordingmcp/internal/contextbuild	7.977s
ok  	recordingmcp/internal/decision	1.053s
?   	recordingmcp/internal/eval	[no test files]
ok  	recordingmcp/internal/generate	1.048s
ok  	recordingmcp/internal/mcpserver	1.919s
ok  	recordingmcp/internal/memory	14.748s
ok  	recordingmcp/internal/migrations	4.796s
ok  	recordingmcp/internal/pipeline	30.140s
ok  	recordingmcp/internal/plane	1.580s
ok  	recordingmcp/internal/process	6.240s
ok  	recordingmcp/internal/worker	3.136s

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

Eval harness (`go run ./cmd/context-eval --db /tmp/eval-check.db --out /tmp/eval-report-check.json`, deterministic, no model calls):

| metric | value |
| --- | --- |
| corpus segments / conversations / questions | 22 / 18 / 42 |
| evidence_recall_at_5 | 0.9286 |
| stale_leakage | 0 |
| invalid_evidence_refs | 0 |
| relation_assisted | 6 (hits whose gold is reachable only through a confirmed link) |
| context_coverage | 0.75 |
| model_calls | 0 |

Metrics are from the final code state: after fixing the evidence attribution for navigation/link-discovered items, `relation_assisted` rose (2 → 6; those items can now be matched as gold-evidence hits) and `context_coverage` fell (1.0 → 0.75; evidence-subset dedupe now also drops redundant query-discovered items, whose gold topics therefore no longer count as covered). Both directions are honest consequences of the fixed evidence semantics; the hard acceptance guards (`stale_leakage == 0`, `invalid_evidence_refs == 0`, `evidence_recall_at_5 >= 0.6`) hold.

## Remaining risks

- **`get_processing_status` still counts retained turns**, including turns of superseded revisions, so its `original_turns` / `unclassified_turns` and `analysis_status` are not revision-scoped. `list_conversations` counts are honest; the status tool was intentionally left unchanged in this session.
- **`build_context` runs degraded by default** (no decision engine configured): deterministic ordering only. The reranker interface is trusted to reorder only; `DecisionReranker` validates answer completeness, but a custom `Reranker` could still return a different set — `Build` does not re-verify identity.
- **Eval numbers are synthetic**: hand-authored corpus, literal-substring retrieval, synthetic generator; they do not predict real-model extraction quality.
- **Speaker binding has no MCP surface yet** (store contract + migration + tests only); nothing consumes `SpeakerProjection` in retrieval.
- **Read-only is tool-level**: the curator process retains filesystem access to the DB file; only the registered tool surface, the prompt and in-process re-validation constrain it.
- Decision remains shadow-only inside extract (`decision_runs` recorded, routing not enabled), per the pre-existing plan.
- `derived_artifacts` has no rows for turn-level artifacts; turn staleness is computed live from `source_attachments.active` (the revision unit), which is deliberate.

## Next recommended task

1. **Wire the speaker-binding contract through MCP + retrieval**: `propose/confirm/reject/revoke` tools and an opt-in projection that annotates retrieved turns with confirmed `person_id` (never rewriting `turns.speaker_entity_id`), plus docs.
2. **Run the eval harness against real adapters** (`generate openai` + `--decision-url` rerank) to produce the first model-backed baselines and compare against the deterministic baseline recorded above; keep `stale_leakage`/`invalid_evidence_refs` as hard guards.
3. **Incremental re-analysis beyond the replaced segment**: use `analysis_coverage` + `UncoveredTurnIDs` and stale-candidate repair so a supersede only recomputes affected windows instead of re-extracting the whole replacement segment.
