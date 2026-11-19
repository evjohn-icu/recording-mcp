package contextbuild

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"recordingmcp/internal/decision"
	"recordingmcp/internal/memory"
)

// v3Policy returns a valid selected policy matching fakeSemantic's identity.
func v3Policy() *AdmissionPolicy {
	return &AdmissionPolicy{
		SchemaVersion:    AdmissionPolicySchema,
		RetrievalVersion: "v3",
		ProviderID:       "fake",
		ModelID:          "test",
		ModelRevision:    "rev-1",
		AssessorModel:    "stub-assessor",
		FixtureSHA256:    strings.Repeat("a", 64),
		Threshold:        0.5,
		Phase:            "selected",
	}
}

// stubCandidateAssessor returns scripted decisions per item id.
type stubCandidateAssessor struct {
	decisions map[string]AdmissionDecision
	err       error
	block     bool
	seen      []AdmissionCandidate
	force     []AdmissionDecision
}

func (s *stubCandidateAssessor) AssessCandidates(ctx context.Context, _ string, candidates []AdmissionCandidate) ([]AdmissionDecision, error) {
	s.seen = append([]AdmissionCandidate(nil), candidates...)
	if s.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if s.err != nil {
		return nil, s.err
	}
	if s.force != nil {
		return s.force, nil
	}
	out := make([]AdmissionDecision, len(candidates))
	for i, candidate := range candidates {
		decision, ok := s.decisions[candidate.Item.ID]
		if !ok {
			decision = AdmissionDecision{Relation: RelationUnknown}
		}
		decision.ItemID = candidate.Item.ID
		out[i] = decision
	}
	return out, nil
}

func support(probability float64) AdmissionDecision {
	return AdmissionDecision{Relation: RelationSupports, Probability: probability}
}

func v3Request(query string) Request {
	return Request{Query: query, RetrievalVersion: "v3", IncludeRawEvidence: true, MaxBytes: 65536}
}

func TestBuildV3AdmissionAndSufficiency(t *testing.T) {
	seed := func(t *testing.T) *fixture {
		t.Helper()
		f := newFixture(t)
		start := "2026-03-01T10:00:00Z"
		f.conversation("conv:a", "Alpha", "rec:a")
		f.recording("rec:a", "conv:a", &start)
		f.turn("t:fact", "conv:a", "rec:a", 0, 1000, nil, "预算周三批准")
		f.turn("t:neg", "conv:a", "rec:a", 1, 2000, nil, "预算没有批准")
		f.note("n:related", "conv:a", "observation", "预算话题相关但无事实", nil, []string{"t:fact"})
		return f
	}

	t.Run("related_candidate_is_rejected", func(t *testing.T) {
		f := seed(t)
		assessor := &stubCandidateAssessor{decisions: map[string]AdmissionDecision{
			"n:related": {Relation: RelationRelated, Probability: 0.9},
			"t:fact":    support(0.9),
		}}
		pkg, err := Build(context.Background(), Deps{
			Store: f.store, DB: f.db, Semantic: &fakeSemantic{ready: true},
			CandidateAssessor: assessor, Assessor: &fakeAssessor{result: EvidenceAssessment{Status: "insufficient"}},
			AdmissionPolicy: v3Policy(),
		}, v3Request("预算"))
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if containsString(itemIDs(pkg.Items), "n:related") {
			t.Fatalf("related candidate entered items: %v", itemIDs(pkg.Items))
		}
		if pkg.Trace.Admission == nil {
			t.Fatal("no admission trace")
		}
		found := false
		for _, rejection := range pkg.Trace.Admission.Rejected {
			if rejection.ItemID == "n:related" && rejection.Reason == "relation_related" {
				found = true
			}
		}
		if !found {
			t.Fatalf("rejections = %+v", pkg.Trace.Admission.Rejected)
		}
		if pkg.Sufficiency == nil || pkg.Sufficiency.Status == "sufficient" {
			t.Fatalf("sufficiency = %+v", pkg.Sufficiency)
		}
	})

	t.Run("refuting_utterance_is_admitted", func(t *testing.T) {
		f := seed(t)
		assessor := &stubCandidateAssessor{decisions: map[string]AdmissionDecision{
			"t:neg":     {Relation: RelationRefutes, Probability: 0.95},
			"t:fact":    support(0.9),
			"n:related": {Relation: RelationUnknown, Probability: 0},
		}}
		pkg, err := Build(context.Background(), Deps{
			Store: f.store, DB: f.db, Semantic: &fakeSemantic{ready: true},
			CandidateAssessor: assessor, Assessor: termAssessor{required: []string{"没有批准"}},
			AdmissionPolicy: v3Policy(),
		}, v3Request("预算"))
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if !containsString(itemIDs(pkg.Items), "t:neg") {
			t.Fatalf("refuting evidence missing: %v", itemIDs(pkg.Items))
		}
		if pkg.Sufficiency == nil || pkg.Sufficiency.Status != "sufficient" {
			t.Fatalf("sufficiency = %+v", pkg.Sufficiency)
		}
	})

	t.Run("cross_session_missing_ref_is_insufficient", func(t *testing.T) {
		f := seed(t)
		assessor := &stubCandidateAssessor{decisions: map[string]AdmissionDecision{"t:fact": support(0.9)}}
		pkg, err := Build(context.Background(), Deps{
			Store: f.store, DB: f.db, Semantic: &fakeSemantic{ready: true},
			CandidateAssessor: assessor, Assessor: termAssessor{required: []string{"预算周三批准", "付款期限"}},
			AdmissionPolicy: v3Policy(),
		}, v3Request("预算"))
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if pkg.Sufficiency == nil || pkg.Sufficiency.Status != "insufficient" {
			t.Fatalf("sufficiency = %+v", pkg.Sufficiency)
		}
	})

	t.Run("proposal_is_not_a_confirmed_fact", func(t *testing.T) {
		f := seed(t)
		assessor := &stubCandidateAssessor{decisions: map[string]AdmissionDecision{"n:related": support(0.9)}}
		_, err := Build(context.Background(), Deps{
			Store: f.store, DB: f.db, Semantic: &fakeSemantic{ready: true},
			CandidateAssessor: assessor, Assessor: &fakeAssessor{result: EvidenceAssessment{Status: "insufficient"}},
			AdmissionPolicy: v3Policy(),
		}, v3Request("预算"))
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if len(assessor.seen) == 0 {
			t.Fatal("assessor saw no candidates")
		}
		for _, candidate := range assessor.seen {
			if candidate.Item.ID != "n:related" {
				continue
			}
			if candidate.SourceStatus != SourceStatusDerivedProposed {
				t.Fatalf("source status = %q", candidate.SourceStatus)
			}
			for _, ref := range candidate.Evidence {
				if ref.SpeakerConfirmed || ref.SpeakerPersonID != nil {
					t.Fatalf("proposal evidence claims a confirmed speaker: %+v", ref)
				}
			}
		}
	})

	t.Run("assessor_failure_is_unknown_and_empty", func(t *testing.T) {
		f := seed(t)
		pkg, err := Build(context.Background(), Deps{
			Store: f.store, DB: f.db, Semantic: &fakeSemantic{ready: true},
			CandidateAssessor: &stubCandidateAssessor{err: errors.New("endpoint down")},
			Assessor:          &fakeAssessor{result: EvidenceAssessment{Status: "sufficient"}},
			AdmissionPolicy:   v3Policy(),
		}, v3Request("预算"))
		if err != nil {
			t.Fatalf("Build returned an error instead of a degraded package: %v", err)
		}
		if len(pkg.Items) != 0 || len(pkg.Evidence) != 0 {
			t.Fatalf("failure leaked candidates: items=%v evidence=%+v", itemIDs(pkg.Items), pkg.Evidence)
		}
		if !hasGap(pkg, "admission_failed") || !pkg.Degraded {
			t.Fatalf("gaps=%+v degraded=%v", pkg.Gaps, pkg.Degraded)
		}
		if pkg.Sufficiency == nil || pkg.Sufficiency.Status != "unknown" {
			t.Fatalf("sufficiency = %+v", pkg.Sufficiency)
		}
	})

	t.Run("invalid_assessment_refs_are_not_sufficient", func(t *testing.T) {
		f := seed(t)
		pkg, err := Build(context.Background(), Deps{
			Store: f.store, DB: f.db, Semantic: &fakeSemantic{ready: true},
			CandidateAssessor: &stubCandidateAssessor{decisions: map[string]AdmissionDecision{"t:fact": support(0.9)}},
			Assessor:          scriptedAssessor{mode: "invalid_refs"},
			AdmissionPolicy:   v3Policy(),
		}, v3Request("预算"))
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if pkg.Sufficiency == nil || pkg.Sufficiency.Status == "sufficient" {
			t.Fatalf("invalid refs accepted: %+v", pkg.Sufficiency)
		}
		if !hasGap(pkg, "evidence_assessment_failed") {
			t.Fatalf("gaps = %+v", pkg.Gaps)
		}
	})

	t.Run("cancellation_returns_context_error", func(t *testing.T) {
		f := seed(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := Build(ctx, Deps{
			Store: f.store, DB: f.db, Semantic: &fakeSemantic{ready: true},
			CandidateAssessor: &stubCandidateAssessor{block: true},
			AdmissionPolicy:   v3Policy(),
		}, v3Request("预算"))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	})

	t.Run("cancellation_during_assessment", func(t *testing.T) {
		f := seed(t)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		_, err := Build(ctx, Deps{
			Store: f.store, DB: f.db, Semantic: &fakeSemantic{ready: true},
			CandidateAssessor: &stubCandidateAssessor{block: true},
			AdmissionPolicy:   v3Policy(),
		}, v3Request("预算"))
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want context.DeadlineExceeded", err)
		}
	})

	t.Run("missing_policy_is_unavailable", func(t *testing.T) {
		f := seed(t)
		pkg, err := Build(context.Background(), Deps{
			Store: f.store, DB: f.db, Semantic: &fakeSemantic{ready: true},
			CandidateAssessor: &stubCandidateAssessor{},
		}, v3Request("预算"))
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if len(pkg.Items) != 0 || !hasGap(pkg, "admission_unavailable") {
			t.Fatalf("items=%v gaps=%+v", itemIDs(pkg.Items), pkg.Gaps)
		}
	})

	t.Run("policy_identity_mismatch_is_unavailable", func(t *testing.T) {
		f := seed(t)
		policy := v3Policy()
		policy.ProviderID = "other"
		pkg, err := Build(context.Background(), Deps{
			Store: f.store, DB: f.db, Semantic: &fakeSemantic{ready: true},
			CandidateAssessor: &stubCandidateAssessor{}, AdmissionPolicy: policy,
		}, v3Request("预算"))
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if !hasGap(pkg, "admission_unavailable") {
			t.Fatalf("gaps = %+v", pkg.Gaps)
		}
	})

	t.Run("calibration_policy_is_rejected_when_served", func(t *testing.T) {
		f := seed(t)
		policy := v3Policy()
		policy.Phase = "calibration"
		pkg, err := Build(context.Background(), Deps{
			Store: f.store, DB: f.db, Semantic: &fakeSemantic{ready: true},
			CandidateAssessor: &stubCandidateAssessor{}, AdmissionPolicy: policy,
		}, v3Request("预算"))
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if !hasGap(pkg, "admission_unavailable") {
			t.Fatalf("gaps = %+v", pkg.Gaps)
		}
	})

	t.Run("below_threshold_is_rejected", func(t *testing.T) {
		f := seed(t)
		pkg, err := Build(context.Background(), Deps{
			Store: f.store, DB: f.db, Semantic: &fakeSemantic{ready: true},
			CandidateAssessor: &stubCandidateAssessor{decisions: map[string]AdmissionDecision{"t:fact": support(0.4)}},
			AdmissionPolicy:   v3Policy(),
		}, v3Request("预算"))
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if len(pkg.Items) != 0 {
			t.Fatalf("below-threshold candidate entered items: %v", itemIDs(pkg.Items))
		}
		found := false
		for _, rejection := range pkg.Trace.Admission.Rejected {
			if rejection.ItemID == "t:fact" && rejection.Reason == "below_threshold" {
				found = true
			}
		}
		if !found {
			t.Fatalf("rejections = %+v", pkg.Trace.Admission.Rejected)
		}
	})

	t.Run("pool_limit_is_reported", func(t *testing.T) {
		f := seed(t)
		f.note("n:second", "conv:a", "observation", "预算第二个相关话题", nil, []string{"t:neg"})
		req := v3Request("预算")
		req.RerankCandidateLimit = 1
		pkg, err := Build(context.Background(), Deps{
			Store: f.store, DB: f.db, Semantic: &fakeSemantic{ready: true},
			CandidateAssessor: &stubCandidateAssessor{decisions: map[string]AdmissionDecision{}},
			AdmissionPolicy:   v3Policy(),
		}, req)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if !hasGap(pkg, "admission_pool_limited") {
			t.Fatalf("gaps = %+v", pkg.Gaps)
		}
		if pkg.Trace.Admission == nil || len(pkg.Trace.Admission.PoolLimitedIDs) == 0 {
			t.Fatalf("trace = %+v", pkg.Trace.Admission)
		}
	})

	t.Run("decision_count_mismatch_fails", func(t *testing.T) {
		f := seed(t)
		pkg, err := Build(context.Background(), Deps{
			Store: f.store, DB: f.db, Semantic: &fakeSemantic{ready: true},
			CandidateAssessor: &stubCandidateAssessor{force: []AdmissionDecision{}},
			AdmissionPolicy:   v3Policy(),
		}, v3Request("预算"))
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if !hasGap(pkg, "admission_failed") || len(pkg.Items) != 0 {
			t.Fatalf("gaps=%+v items=%v", pkg.Gaps, itemIDs(pkg.Items))
		}
	})

	t.Run("empty_query_navigation_is_unknown", func(t *testing.T) {
		f := seed(t)
		pkg, err := Build(context.Background(), Deps{
			Store: f.store, DB: f.db, Semantic: &fakeSemantic{ready: true},
			CandidateAssessor: &stubCandidateAssessor{},
			AdmissionPolicy:   v3Policy(),
		}, Request{ConversationID: "conv:a", RetrievalVersion: "v3", MaxBytes: 65536})
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if len(pkg.Items) == 0 {
			t.Fatal("empty-query navigation returned no context")
		}
		if !hasGap(pkg, "assessment_query_missing") {
			t.Fatalf("gaps = %+v", pkg.Gaps)
		}
		if pkg.Sufficiency == nil || pkg.Sufficiency.Status != "unknown" {
			t.Fatalf("sufficiency = %+v", pkg.Sufficiency)
		}
	})
}

type batchRecordingEngine struct{ batches []int }

func (e *batchRecordingEngine) Evaluate(_ context.Context, _ string, questions []decision.Question) (decision.Result, error) {
	e.batches = append(e.batches, len(questions))
	answers := map[string]decision.Answer{}
	for _, q := range questions {
		answers[q.ID] = decision.Answer{Type: q.Type, Choice: RelationUnknown, Confidence: 1,
			Distribution: map[string]float64{RelationSupports: 0, RelationRefutes: 0, RelationRelated: 0, RelationUnknown: 1}}
	}
	return decision.Result{Answers: answers}, nil
}

// TestCandidateAssessorRespectsBatchQuestionLimit keeps the admission client
// within the shipped local assessor's 32-question request contract.
func TestCandidateAssessorRespectsBatchQuestionLimit(t *testing.T) {
	engine := &batchRecordingEngine{}
	assessor := DecisionCandidateAssessor{Engine: engine, MaxQuestionsPerBatch: 32}
	candidates := make([]AdmissionCandidate, 40)
	for i := range candidates {
		candidates[i] = AdmissionCandidate{Item: memory.Item{ID: fmt.Sprintf("item-%02d", i)}, SourceStatus: "raw_current"}
	}
	decisions, err := assessor.AssessCandidates(t.Context(), "query", candidates)
	if err != nil {
		t.Fatalf("AssessCandidates: %v", err)
	}
	if len(decisions) != len(candidates) {
		t.Fatalf("decisions = %d, want %d", len(decisions), len(candidates))
	}
	for _, n := range engine.batches {
		if n > 32 {
			t.Errorf("batch had %d questions, want <= 32", n)
		}
	}
	if len(engine.batches) != 2 {
		t.Errorf("batches = %v, want 2", engine.batches)
	}
}

// TestCandidateAssessorDefaultsToOneQuestionPerRequest documents the shipped
// local assessor's sequential scoring: multi-question batches multiply cost.
func TestCandidateAssessorDefaultsToOneQuestionPerRequest(t *testing.T) {
	engine := &batchRecordingEngine{}
	assessor := DecisionCandidateAssessor{Engine: engine}
	candidates := make([]AdmissionCandidate, 5)
	for i := range candidates {
		candidates[i] = AdmissionCandidate{Item: memory.Item{ID: fmt.Sprintf("item-%d", i)}, SourceStatus: "raw_current"}
	}
	if _, err := assessor.AssessCandidates(t.Context(), "query", candidates); err != nil {
		t.Fatalf("AssessCandidates: %v", err)
	}
	if len(engine.batches) != 5 {
		t.Fatalf("batches = %v, want 5 single-question requests", engine.batches)
	}
}
