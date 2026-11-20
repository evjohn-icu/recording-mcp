package eval

import (
	"os"
	"path/filepath"
	"testing"

	"recordingmcp/internal/memory"
)

// TestRunOnShippedFixtures is the regression gate for the round: hybrid
// retrieval must not regress the lexical baseline, the semantic branch must
// add recall, ambient/stale/scope invariants must hold, and the read-time
// projection must be deterministic and honest about conflicts.
func TestRunOnShippedFixtures(t *testing.T) {
	report, err := Run(t.Context(), Options{
		DBPath:         filepath.Join(t.TempDir(), "eval.db"),
		CorpusPath:     filepath.Join("..", "..", "fixtures", "eval", "corpus.json"),
		QuestionsPath:  filepath.Join("..", "..", "fixtures", "eval", "questions.json"),
		LinksPath:      filepath.Join("..", "..", "fixtures", "eval", "links.json"),
		RetrievalPath:  filepath.Join("..", "..", "fixtures", "eval", "retrieval.json"),
		ProjectionPath: filepath.Join("..", "..", "fixtures", "eval", "projection.json"),
		MaxItems:       8,
		MaxBytes:       16000,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	m := report.Metrics
	if m.StaleLeakage != 0 {
		t.Errorf("stale_leakage = %f, want 0", m.StaleLeakage)
	}
	if m.AmbientLeakage != 0 {
		t.Errorf("ambient_leakage = %f, want 0", m.AmbientLeakage)
	}
	if m.ScopeEscapeCount != 0 {
		t.Errorf("scope_escape_count = %d, want 0", m.ScopeEscapeCount)
	}
	if m.InvalidEvidenceRefs != 0 {
		t.Errorf("invalid_evidence_refs = %d, want 0", m.InvalidEvidenceRefs)
	}
	if m.HybridRecallAt5 < m.LexicalRecallAt5 {
		t.Errorf("hybrid_recall_at_5 = %f regressed below lexical %f", m.HybridRecallAt5, m.LexicalRecallAt5)
	}
	if m.SemanticAssistedRecall <= 0 {
		t.Errorf("semantic_assisted_recall = %f, want > 0", m.SemanticAssistedRecall)
	}
	if m.UnanswerableQuestions <= 0 {
		t.Errorf("unanswerable_questions = %d, want > 0", m.UnanswerableQuestions)
	}
	if m.UnanswerableLexicalFalsePositiveItems != 0 {
		t.Errorf("unanswerable_lexical_false_positive_items = %d, want 0", m.UnanswerableLexicalFalsePositiveItems)
	}
	if m.TurnTop1Accuracy <= 0 {
		t.Errorf("turn_top1_accuracy = %f, want > 0", m.TurnTop1Accuracy)
	}
	if m.EvidenceRecallAt5 <= 0 {
		t.Errorf("evidence_recall_at_5 = %f, want > 0", m.EvidenceRecallAt5)
	}

	p := m.Projection
	if !p.Deterministic {
		t.Error("projection.deterministic = false, want true")
	}
	if p.StaleNotCurrent != 1 {
		t.Errorf("projection.stale_not_current = %f, want 1", p.StaleNotCurrent)
	}
	if p.ConflictingHonest != 1 {
		t.Errorf("projection.conflicting_honest = %f, want 1", p.ConflictingHonest)
	}
	if p.EvidenceValid != 1 {
		t.Errorf("projection.evidence_valid = %f, want 1", p.EvidenceValid)
	}
	if p.Objects == 0 {
		t.Errorf("projection produced objects=%d, want > 0", p.Objects)
	}
	// Conflicts are legitimately empty since commitments project as proposed
	// (unconfirmed): the supersede-cue rule only applies to current commitments,
	// and no fixture or rule emits one. The pair invariant is still enforced as
	// a hard error inside the harness whenever a conflict does appear.
	if p.Conflicts != 0 {
		t.Errorf("projection produced conflicts=%d, want 0: no source yields a current commitment", p.Conflicts)
	}
	if report.RetrievalQuestions == 0 {
		t.Error("retrieval pass did not run")
	}
}

// TestRunLexicalOnlySkipsHybrid proves the LexicalOnly option really disables
// the semantic pass: no index is built and no hybrid metrics are produced.
func TestRunLexicalOnlySkipsHybrid(t *testing.T) {
	report, err := Run(t.Context(), Options{
		DBPath:         filepath.Join(t.TempDir(), "eval-lex.db"),
		CorpusPath:     filepath.Join("..", "..", "fixtures", "eval", "corpus.json"),
		QuestionsPath:  filepath.Join("..", "..", "fixtures", "eval", "questions.json"),
		LinksPath:      filepath.Join("..", "..", "fixtures", "eval", "links.json"),
		RetrievalPath:  filepath.Join("..", "..", "fixtures", "eval", "retrieval.json"),
		ProjectionPath: "",
		LexicalOnly:    true,
		MaxItems:       8,
		MaxBytes:       16000,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Metrics.HybridRecallAt5 != 0 || report.Metrics.SemanticAssistedRecall != 0 {
		t.Errorf("lexical-only run produced hybrid metrics: %+v", report.Metrics)
	}
	if report.RetrievalQuestions == 0 {
		t.Error("retrieval questions not loaded")
	}
}

func TestRunAuthoredQualityFixtureReportsAccountingBucketsAndMRR(t *testing.T) {
	base := filepath.Join("..", "..", "fixtures", "eval", "quality")
	report, err := Run(t.Context(), Options{
		DBPath:         filepath.Join(t.TempDir(), "quality.db"),
		CorpusPath:     filepath.Join(base, "corpus.json"),
		QuestionsPath:  filepath.Join(base, "questions.json"),
		LinksPath:      filepath.Join(base, "links.json"),
		RetrievalPath:  filepath.Join(base, "retrieval.json"),
		ProjectionPath: "",
	})
	if err != nil {
		t.Fatalf("Run quality fixture: %v", err)
	}
	if report.RetrievalQuestions != 160 {
		t.Fatalf("retrieval questions=%d, want 160", report.RetrievalQuestions)
	}
	m := report.Metrics
	if m.RetrievalRequests != 320 || m.SemanticQueryEmbedRequests != 160 {
		t.Errorf("request accounting = retrieval:%d semantic-query:%d, want 320/160", m.RetrievalRequests, m.SemanticQueryEmbedRequests)
	}
	if m.SemanticProviderEmbedRequests != m.SemanticIndexEmbedRequests+m.SemanticQueryEmbedRequests || m.SemanticProviderEmbedErrors != 0 {
		t.Errorf("provider calls do not reconcile with index/query calls: %+v", m)
	}
	if m.SemanticIndexEmbedRequests == 0 || m.SemanticIndexEmbeddedItems == 0 || m.SemanticIndexDurationMS <= 0 {
		t.Errorf("index accounting missing: requests=%d items=%d ms=%f", m.SemanticIndexEmbedRequests, m.SemanticIndexEmbeddedItems, m.SemanticIndexDurationMS)
	}
	if m.RetrievalHybridLatencyMSP50 <= 0 || m.RetrievalHybridLatencyMSP95 < m.RetrievalHybridLatencyMSP50 {
		t.Errorf("hybrid latency percentiles invalid: p50=%f p95=%f", m.RetrievalHybridLatencyMSP50, m.RetrievalHybridLatencyMSP95)
	}
	if m.RetrievalHybridMRRAt10 <= 0 {
		t.Error("hybrid MRR@10 is zero")
	}
	if m.RetrievalHybridPrecisionAt5 <= 0 {
		t.Error("hybrid precision@5 is zero")
	}
	if _, ok := m.RetrievalBuckets["test/hybrid/education"]; !ok {
		t.Errorf("test education bucket missing from %d buckets", len(m.RetrievalBuckets))
	}
	if _, ok := m.RetrievalBuckets["/lexical/"]; ok {
		t.Error("classic evaluation rows leaked into retrieval bucket aggregation")
	}
	noEvidence := m.RetrievalBuckets["test/hybrid/no_evidence"]
	if noEvidence.NoEvidenceQuestions != 10 || noEvidence.LatencyMSP95 <= 0 {
		t.Errorf("no-evidence bucket lost its denominator/latency: %+v", noEvidence)
	}
}

// TestRunLifecycleForgetsPreviewedTurn covers the v3 lifecycle path: the
// harness must derive the forget state hash from a fresh preview inside the
// temporary evaluation database instead of requiring it in the frozen fixture.
func TestRunLifecycleForgetsPreviewedTurn(t *testing.T) {
	dir := t.TempDir()
	lifecycle := filepath.Join(dir, "lifecycle.json")
	const fixture = `{"schema":"retrieval-lifecycle-v1","sources":[{"segment_id":"seg-relocation","transcript_revision":"rev-1","turn_id":"utt-1","reason":"test forget"}]}`
	if err := os.WriteFile(lifecycle, []byte(fixture), 0o600); err != nil {
		t.Fatalf("write lifecycle: %v", err)
	}
	report, err := Run(t.Context(), Options{
		DBPath:        filepath.Join(dir, "eval.db"),
		CorpusPath:    filepath.Join("..", "..", "fixtures", "eval", "corpus.json"),
		QuestionsPath: filepath.Join("..", "..", "fixtures", "eval", "questions.json"),
		LinksPath:     filepath.Join("..", "..", "fixtures", "eval", "links.json"),
		LifecyclePath: lifecycle,
		MaxItems:      8,
		MaxBytes:      16000,
	})
	if err != nil {
		t.Fatalf("Run with lifecycle: %v", err)
	}
	if report.Metrics.ForgottenLeakage != 0 {
		t.Errorf("forgotten_leakage = %d, want 0", report.Metrics.ForgottenLeakage)
	}
}

// TestLoadRetrievalQuestionsResolvesForgottenRefs covers v3 revocation
// questions: their authored gold refs point at turns removed by the lifecycle,
// so the loader must reuse the forbidden ID captured before the forget.
func TestLoadRetrievalQuestionsResolvesForgottenRefs(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	corpus, err := LoadCorpus(filepath.Join("..", "..", "fixtures", "eval", "corpus.json"))
	if err != nil {
		t.Fatalf("LoadCorpus: %v", err)
	}
	dbPath := filepath.Join(dir, "eval.db")
	if err := resetDatabase(dbPath); err != nil {
		t.Fatalf("resetDatabase: %v", err)
	}
	store, err := memory.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	if err := ingestCorpus(ctx, store, corpus); err != nil {
		t.Fatalf("ingestCorpus: %v", err)
	}
	if err := drainPipeline(ctx, store.DB(), store); err != nil {
		t.Fatalf("drainPipeline: %v", err)
	}
	ref := TurnRef{SegmentID: "seg-budget", TranscriptRevision: "rev-1", TurnID: "utt-1"}
	turnID, err := resolveTurnRef(ctx, store.DB(), ref)
	if err != nil {
		t.Fatalf("resolveTurnRef: %v", err)
	}
	preview, err := store.PreviewForget(ctx, memory.ForgetRequest{Scope: "turn", TurnID: turnID})
	if err != nil {
		t.Fatalf("PreviewForget: %v", err)
	}
	req := preview.Request
	req.ExpectedState = preview.ExpectedState
	if _, err := store.CommitForget(ctx, req); err != nil {
		t.Fatalf("CommitForget: %v", err)
	}
	fixture := `{"questions":[{"id":"forgotten-q","query":"旧通知写了什么？","split":"legacy","bucket":"revocation","unanswerable":false,"gold_turn_refs":[{"segment_id":"seg-budget","transcript_revision":"rev-1","turn_id":"utt-1"}],"expected_sufficiency":"insufficient","evidence_completeness":"partial","expect_forgotten_excluded":true}]}`
	qpath := filepath.Join(dir, "retrieval.json")
	if err := os.WriteFile(qpath, []byte(fixture), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if _, err := LoadRetrievalQuestions(ctx, store.DB(), qpath, nil); err == nil {
		t.Fatal("LoadRetrievalQuestions resolved a forgotten ref without the forbidden ID map")
	}
	questions, err := LoadRetrievalQuestions(ctx, store.DB(), qpath, map[string]string{turnRefKey(ref): turnID})
	if err != nil {
		t.Fatalf("LoadRetrievalQuestions with forgotten refs: %v", err)
	}
	if len(questions) != 1 || len(questions[0].GoldEvidenceTurnIDs) != 1 || questions[0].GoldEvidenceTurnIDs[0] != turnID {
		t.Fatalf("forgotten gold ref was not preserved: %+v", questions)
	}
}

// TestRunExcludesForgottenQuestionFromRecallDenominator keeps leakage-only
// questions out of recall/MRR while still reporting their rows.
func TestRunExcludesForgottenQuestionFromRecallDenominator(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	lifecycle := filepath.Join(dir, "lifecycle.json")
	const lc = `{"schema":"retrieval-lifecycle-v1","sources":[{"segment_id":"seg-relocation","transcript_revision":"rev-1","turn_id":"utt-1","reason":"test forget"}]}`
	if err := os.WriteFile(lifecycle, []byte(lc), 0o600); err != nil {
		t.Fatalf("write lifecycle: %v", err)
	}
	retrieval := filepath.Join(dir, "retrieval.json")
	const retrievalFixture = `{"questions":[
{"id":"q-normal","query":"预算批准了吗？","purpose":"synonym","split":"legacy","bucket":"synonym","unanswerable":false,"gold_turn_refs":[{"segment_id":"seg-budget","transcript_revision":"rev-2","turn_id":"utt-1"}],"expected_sufficiency":"sufficient","evidence_completeness":"complete"},
{"id":"q-forgotten","query":"我们打算搬去哪里定居？","purpose":"revocation","split":"legacy","bucket":"revocation","unanswerable":false,"gold_turn_refs":[{"segment_id":"seg-relocation","transcript_revision":"rev-1","turn_id":"utt-1"}],"expected_sufficiency":"insufficient","evidence_completeness":"partial","expect_forgotten_excluded":true},
{"id":"q-flag-live","query":"预算改动了吗？","purpose":"revocation","split":"legacy","bucket":"revocation","unanswerable":false,"gold_turn_refs":[{"segment_id":"seg-budget","transcript_revision":"rev-2","turn_id":"utt-1"}],"expected_sufficiency":"sufficient","evidence_completeness":"complete","expect_forgotten_excluded":true}]}`
	if err := os.WriteFile(retrieval, []byte(retrievalFixture), 0o600); err != nil {
		t.Fatalf("write retrieval: %v", err)
	}
	report, err := Run(ctx, Options{
		DBPath:        filepath.Join(dir, "eval.db"),
		CorpusPath:    filepath.Join("..", "..", "fixtures", "eval", "corpus.json"),
		QuestionsPath: filepath.Join("..", "..", "fixtures", "eval", "questions.json"),
		LinksPath:     filepath.Join("..", "..", "fixtures", "eval", "links.json"),
		RetrievalPath: retrieval,
		LifecyclePath: lifecycle,
		MaxItems:      8,
		MaxBytes:      16000,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	normal, forgotten, flaggedLive := QuestionResult{}, QuestionResult{}, QuestionResult{}
	for _, row := range report.PerQuestion {
		switch {
		case row.ID == "q-normal" && row.Mode == "hybrid":
			normal = row
		case row.ID == "q-forgotten" && row.Mode == "hybrid":
			forgotten = row
		case row.ID == "q-flag-live" && row.Mode == "hybrid":
			flaggedLive = row
		}
	}
	if !forgotten.ExpectForgottenExcluded {
		t.Fatalf("forgotten question row missing flag: %+v", forgotten)
	}
	if report.Metrics.ForgottenLeakage != 0 {
		t.Errorf("forgotten_leakage = %d, want 0", report.Metrics.ForgottenLeakage)
	}
	if !flaggedLive.RecallEligible {
		t.Errorf("live-gold revocation question was marked ineligible: %+v", flaggedLive)
	}
	if forgotten.RecallEligible {
		t.Errorf("forgotten-gold question was marked eligible: %+v", forgotten)
	}
	want := (normal.RecallAt5 + flaggedLive.RecallAt5) / 2
	if report.Metrics.HybridRecallAt5 != want {
		t.Errorf("hybrid recall = %f, want average of eligible positives %f", report.Metrics.HybridRecallAt5, want)
	}
}
