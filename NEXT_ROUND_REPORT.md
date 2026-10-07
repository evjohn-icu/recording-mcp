# NEXT_ROUND_REPORT — Hybrid Retrieval v0 + Context Projection v0

## 1. What changed

- **Hybrid retrieval v0**: `contextbuild.Build` gained a semantic recall branch (vector similarity over an offline-built embedding index) fused with the existing lexical, navigation and confirmed-link branches. Every returned item now carries per-item provenance: `Package.Ranking[]` with `retrieval_sources` (`lexical` / `semantic` / `confirmed_link` / `navigation`), the fused `retrieval_score`, per-branch raw scores and, for link-assisted items, `retrieval_link_ids`.
- **Context projection v0**: new read-time derivation layer (`internal/projection`) served by the new MCP tool `get_current_context`: person / topic / commitment / preference / event objects with evidence, provenance, status (`current` / `superseded` / `unresolved`) and conflicts.
- **Semantic index**: migration v12 (`memory_embeddings`) plus `internal/semantic` (vector math, providers, indexer, cosine retriever) and the `cmd/context-embed` CLI.
- **Eval**: harness runs every retrieval question twice (lexical vs hybrid), adds ambient/scope/semantic-assisted metrics, adds a projection pass with honesty metrics, and gained hermetic tests.
- **Docs**: new `docs/hybrid-retrieval.md`, `docs/context-projection.md`; restructured `README.md`; synced `docs/context-layer-runtime.md`, `docs/processing.md`, `docs/associations.md`, `docs/context-memory.md`.
- **CI**: `.github/workflows/ci.yml` (`go vet`, `go test -race -count=1 ./...`, `make build`).

## 2. Hybrid retrieval architecture

- Boundary: `semantic.SemanticRetriever` (`Search` / `Ready` / `Describe`) is the only thing `contextbuild` depends on. `Deps{Store, DB, Reranker, Semantic}` replaces the positional `Build` signature; `Deps.Semantic == nil` degrades to lexical-only and records why (`gaps: semantic_unavailable`; empty index → `semantic_index_empty`).
- Providers: default `HashProvider` (`hash-ngram-v1`, 256 dims, offline deterministic: lowercased letter/digit tokens + overlapping CJK 2-grams → signed FNV-1a 64 hashing trick → L2-normalize). Opt-in `OpenAICompatProvider` for real embedding endpoints. Field names are `ModelName` / `Dimensions` because the `Provider` interface requires `Model()` / `Dims()` methods (Go forbids field/method name collisions).
- Index: `memory_embeddings` keyed `(kind, artifact_id, provider_id, model_id)`; `content_sha256` makes re-indexing unchanged artifacts a no-op (`IndexReport.Skipped` proves it). Stale artifacts are never indexed; ambient artifacts ARE indexed (query path filters them, so the isolation invariant is testable — a turn indexed while current and superseded later stays in the index but can never be recalled).
- Query path SQL re-applies the exact store predicates: `CurrentTurnPredicate` + ambient exclusion for turns, `NoteStalePredicate` + ordinal-0 ambient exclusion for notes. Resolved hits go through `GetItem` and the same `addItem` guard as every other branch (stale → `trace.semantic.rejected_stale`, scope → `rejected_scope`; ambient rejected before entering).
- Fusion: fixed weights lexical 1.0, semantic 0.8, confirmed_link 0.45, navigation 0.30; per branch `max` of raw scores, `score = Σ w·raw` summed in a fixed branch order for float determinism. Optional recency bonus (`0.10·exp(-ageDays/30)`) only when the caller passes `Now`; runs without a clock are byte-identical.
- `Request.Mode`: `""`/`"hybrid"` (all branches) or `"lexical"` (semantic branch off, nav + confirmed links stay on); anything else errors `build_context: mode must be hybrid or lexical`.

## 3. Context projection architecture

- Read-time, no projection table: `projection.Build(ctx, store, db, req)` rebuilds everything from stored rows, so it can never drift from the database.
- Rule 1 maps current, non-ambient notes: `intention`→commitment 0.6, `request`→commitment 0.5, `constraint`→preference 0.6, `observation`→event 0.5 only when the primary evidence turn is time-anchored; `suggestion` and untimed observations land in `Unresolved{reason: kind_not_projected_in_v0}`.
- Rule 2 (commitment conflicts, deterministic): group by subject, order by primary evidence start, first (i<j) pair where the later summary contains a supersede cue and the earlier does not → earlier `superseded`/`negated_by_later_note`, later `current`, one `Conflict` per group with the matched cue. Cues are multi-character only (`取消 暂时不 不再 放弃 改为 延期 不去了 不搬 不办 no longer cancelled`); a bare "不" never triggers. Conflicts are reported, never deleted.
- Rule 3 topics from current membership (notes via `json_each` or current non-ambient `turn_topics`), Rule 4 persons from `confirmed` bindings only (revision-scoped evidence, merged per person, `evidence_superseded` → `unresolved`).
- Scope (any-of conversation/topic/person/kind + time bounds, unknown start excluded), ordering (kind, subject), limit (default 20, max 200; conflicts and unresolved are never truncated — they are honesty output).
- Determinism: every collection sorted before emission; `trace.rules` is a fixed literal list; no map iteration order and no clock reaches the output.

## 4. Schema / migration changes

- Migration **v12 `memory-embeddings`**: `memory_embeddings(kind, artifact_id, conversation_id, content_sha256, provider_id, model_id, dims, vector, created_at)` with PK `(kind, artifact_id, provider_id, model_id)` and index `memory_embeddings_conv(conversation_id, kind)`. Rows for superseded/ambient artifacts are intentionally kept (query-path filtering keeps the invariants testable). Idempotent re-apply covered by `migrations_test` (version asserts bumped 11→12, table + index added to expectations).
- Shared refactors: `memory.WithinStartedRange` exported (the contextbuild duplicate deleted), `Store.ConversationStartedAt` added (scope memo now calls it), `GetItem` not-found errors now wrap `memory.ErrNotFound`, `Store.ItemAmbient` added so id-resolving branches enforce the same ambient rule as the SQL filters.

## 5. MCP / API changes

- New read tool `get_current_context` (memory family 18 → **19 tools: 14 read + 5 write**; 27 total with the 8 event-prototype tools). Input: `query, person_id, topic_id, conversation_id, kinds, started_after, started_before, audit, limit`. With a `query`, one small `build_context` pass narrows the projection to the reached conversations/topics; empty coverage returns the projection plus a `no_retrieval_hits` unresolved entry. Unknown `kinds` values are rejected.
- `build_context` input gains `mode` (and `now` for the optional recency bonus); output gains `ranking` and `trace.semantic` (`query, backend, indexed, hit_ids, rejected_stale, rejected_scope`), `coverage.branches` gains `semantic`. `Package.Items` element type is unchanged (provenance ships as the `ranking` sidecar).
- Server flags: `--semantic-provider` (`off|hash|openai`, default `hash`), `--semantic-dims` (default 256, also validates the openai endpoint), `--semantic-endpoint`, `--semantic-model`, `--semantic-key-env`. `off` → nil retriever → lexical-only with recorded degrade.

## 6. README / doc changes

- `README.md` rewritten: English one-liner + Chinese summary, real-code Mermaid architecture, 已实现/实验/计划 lists, quick start (ingest → worker → index → server → `get_current_context` example), core concepts, category tool table with counts, security/trust semantics, verbatim AGPL paragraph. Historical Recording-MCP prototype narrative removed (pointer kept to `docs/contract.md`).
- New: `docs/hybrid-retrieval.md` (audit of every retrieval surface with symbols/line anchors, data flow, fusion design, FTS5 rejection rationale, index/run guide, degraded modes), `docs/context-projection.md` (kind/status model, mapping table, exact cue list, "what is not projected").
- Synced: `docs/context-layer-runtime.md` (tool table + flags + degraded modes + counts), `docs/processing.md` (v12 semantic index stage + projection layer), `docs/associations.md` (confirmed links as a retrieval branch), `docs/context-memory.md` (drift fix). Drift re-grep clean; `HARDENING_REPORT.md` / `OVERNIGHT_REPORT.md` left as historical records (no contradictions introduced).

## 7. Tests

- `internal/semantic`: deterministic hash provider, encode/decode round-trip + wrong-length rejection, httptest OpenAI-compat parsing + non-2xx error, hash-keyed idempotent indexing (re-index → `Skipped == first-run total`; one edited note re-embeds alone), index-includes-ambient/search-filters-stale-and-ambient, conversation filter + limit.
- `internal/contextbuild` (`build_semantic_test.go`): semantic-only hit appears with `["semantic"]` ranking; A4-1 stale, A4-2 ambient, A4-3/4/5 conversation/topic/time scope escapes all rejected with trace counts; A4-6 superseded-revision embedding cannot leak (real `SQLiteRetriever` + `HashProvider`); A4-7 confirmed link assists (with `retrieval_link_ids`), A4-8 proposed link does not; A4-9 nil-backend lexical fallback identical to `mode:"lexical"`; lexical mode skips the semantic branch; unknown mode rejected; literal `hybrid` accepted; fusion deterministic (byte-identical JSON); recency bonus only with a clock.
- `internal/projection`: note-kind mapping (incl. suggestion/untimed → unresolved), conflict pair (搬去珠海 / 暂时不搬珠海了) with cue `暂时不`, non-cue negation ignored, confirmed-only person objects, superseded-evidence person unresolved, stale note absent unless audit, ambient-primary note not projected, scope/limit hard filters, byte-identical determinism, and the evidence-backed sweep (every `EvidenceTurnIDs` resolves to a current non-ambient turn).
- `internal/eval`: hermetic `TestRunOnShippedFixtures` regression gate (stale/ambient/scope/invalid all 0, hybrid ≥ lexical, semantic-assisted > 0, projection deterministic + honest) and `TestRunLexicalOnlySkipsHybrid`.
- `cmd/context-memory`: `get_current_context` in-process MCP test (seeded note + topic → commitment/current object, unknown kind rejected); read-only and full tool-surface lists bumped to 14/19 memory tools (27 total).

## 8. Eval before / after

`context-eval` now always builds the semantic index and runs the retrieval + projection passes over the shipped fixtures; `--retrieval <path>` / `--projection <path>` (empty disables; defaults are the fixture paths). Classic questions still run in lexical mode so the historical metrics stay comparable. "Before (new corpus)" was captured by building `cmd/context-eval` from the pre-round commit and running it against the *new* corpus, isolating code change from fixture change.

| metric | before (main, old corpus) | before (main, new corpus) | after (this round) |
|---|---|---|---|
| evidence_recall_at_5 | 0.9286 | 0.9286 | **0.9286** (unchanged) |
| stale_leakage | 0 | 0 | **0** |
| invalid_evidence_refs | 0 | 0 | **0** |
| relation_assisted | 6 | 6 | **6** |
| context_coverage | 0.75 | 0.75 | **0.75** |
| lexical_recall_at_5 | — | — | 0.75 |
| hybrid_recall_at_5 | — | — | **1.00** |
| semantic_assisted_recall | — | — | **0.25** |
| ambient_leakage | — | — | **0** |
| scope_escape_count | — | — | **0** |
| projection.evidence_valid | — | — | **1.0** |
| projection.stale_not_current | — | — | **1.0** |
| projection.superseded_not_current | — | — | **1.0** |
| projection.conflicting_honest | — | — | **1.0** |
| projection.deterministic | — | — | **true** (53 objects, 1 conflict, 6 unprojected) |

End-to-end stdio smoke (outside the harness): `context-eval --db /tmp/smoke.db` → `context-embed` (74 rows already indexed by the eval, `skipped=74` proves idempotence) → real `context-memory` stdio: `get_current_context kinds:[commitment]` returned 5 evidence-backed commitment objects plus the 珠海 conflict pair (cue `暂时不`); `build_context {mode:"hybrid"}` returned `trace.semantic.indexed=12` and ranking entries with `retrieval_sources:["semantic"]`, `["confirmed_link"]`, `["navigation"]`; `tools/list` shows 27 tools.

## 9. Known limitations

- Semantic default is the offline hashing embedder (`hash-ngram-v1`, 256 dims), not a real embedding endpoint. Switching the default to a real endpoint is only `--semantic-provider` + an indexed corpus.
- No FTS5 this round (documented rationale in `docs/hybrid-retrieval.md` §4): the lexical branch keeps the literal scan; FTS5 remains a listed future option.
- Projection is read-time, has no projection table, reserves the `project` kind, and uses no LLM. `StatusConflicting` is reserved for a future truth engine.
- Provenance ships as `Package.Ranking`, not by changing `Package.Items`' element type.
- Indexing is a CLI (`bin/context-embed`), not an MCP write tool; nothing re-indexes automatically after publish — until then, hybrid recall only covers what the last index run saw.
- Recency ranking signal is off unless the caller passes `now`.
- Ranking fairness: notes sort before turns (`kindRank`), so a semantic turn-gold can be crowded out of top-5 by navigation notes. The semantic-only eval question therefore targets the seeded note whose evidence cites the gold turn (`semantic_assisted_recall` 0.25). A score-first mode is a candidate next step.
- The eval corpus grew by two conversations (`conv-relocation`, `conv-ambient-mix`); classic metrics are unchanged (verified against the old binary on the new corpus).
- `--retrieval` / `--projection` are path-valued flags (default: the shipped fixtures; empty disables). The plan's shorthand `--retrieval --projection` needs the path values.
- `OpenAICompatProvider` exposes `ModelName` / `Dimensions` fields (interface method names collide with the plan's field names). It validates the endpoint against `--semantic-dims` and fails loudly on mismatch instead of learning dims from the first response.
- CI uses `go-version-file: go.mod` (Go 1.27.1); if the runner cannot resolve it, switch to `go-version: '1.27.x'` + `check-latest: true`.

## 10. Single most recommended next step

**Auto-index on publish.** Wire a semantic-index refresh into the end of the publish stage (or enqueue a small `embed` job from `PublishHandler`), so `memory_embeddings` tracks the current state without an operator running `bin/context-embed`. That removes the only operational gap between "memory updated" and "semantic recall current", and it unlocks honest end-to-end latency/recall numbers for hybrid retrieval in the next eval round.
