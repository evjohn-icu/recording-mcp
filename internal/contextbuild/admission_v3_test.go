package contextbuild

import (
	"context"
	"errors"
	"fmt"
	"reflect"
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

	t.Run("queryless_build_never_calls_candidate_assessor", func(t *testing.T) {
		f := seed(t)
		assessor := &stubCandidateAssessor{}
		pkg, err := Build(context.Background(), Deps{
			Store: f.store, DB: f.db, Semantic: &fakeSemantic{ready: true},
			CandidateAssessor: assessor,
			AdmissionPolicy:   v3Policy(),
		}, Request{ConversationID: "conv:a", RetrievalVersion: "v3", IncludeRawEvidence: true, MaxBytes: 65536})
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if len(assessor.seen) != 0 {
			t.Fatalf("candidate assessor was called for a queryless navigation build: seen=%d", len(assessor.seen))
		}
		if pkg.Trace.Admission != nil {
			t.Fatalf("queryless build recorded an admission trace: %+v", pkg.Trace.Admission)
		}
		if !hasGap(pkg, "assessment_query_missing") {
			t.Fatalf("gaps = %+v", pkg.Gaps)
		}
		if pkg.Sufficiency == nil || pkg.Sufficiency.Status != "unknown" {
			t.Fatalf("sufficiency = %+v", pkg.Sufficiency)
		}
		// The navigation context keeps its legacy evidence windows.
		if !containsString(evidenceTurnIDs(pkg), "t:fact") {
			t.Fatalf("navigation evidence windows missing: %+v", pkg.Evidence)
		}
	})

	t.Run("adjacent_rejected_turn_is_not_evidence", func(t *testing.T) {
		f := newFixture(t)
		start := "2026-03-01T10:00:00Z"
		f.conversation("conv:adj", "Adjacent", "rec:adj")
		f.recording("rec:adj", "conv:adj", &start)
		f.turn("t:core", "conv:adj", "rec:adj", 0, 1000, nil, "准入相邻核心原话")
		f.turn("t:adjacent", "conv:adj", "rec:adj", 1, 2000, nil, "准入相邻相关原话")
		assessor := &stubCandidateAssessor{decisions: map[string]AdmissionDecision{
			"t:core":     support(0.9),
			"t:adjacent": {Relation: RelationRelated, Probability: 0.9},
		}}
		pkg, err := Build(context.Background(), Deps{
			Store: f.store, DB: f.db, Semantic: &fakeSemantic{ready: true},
			CandidateAssessor: assessor, Assessor: termAssessor{required: []string{"准入相邻核心原话"}},
			AdmissionPolicy: v3Policy(),
		}, v3Request("准入相邻"))
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if got := itemIDs(pkg.Items); !reflect.DeepEqual(got, []string{"t:core"}) {
			t.Fatalf("items = %v, want only the admitted candidate", got)
		}
		refs := evidenceTurnIDs(pkg)
		if !reflect.DeepEqual(refs, []string{"t:core"}) {
			t.Fatalf("evidence = %v, want only the admitted candidate's own required ref", refs)
		}
		for _, ref := range pkg.Evidence {
			if strings.Contains(ref.Text, "准入相邻相关原话") {
				t.Fatalf("rejected neighbour text entered the evidence: %+v", pkg.Evidence)
			}
		}
		found := false
		for _, rejection := range pkg.Trace.Admission.Rejected {
			if rejection.ItemID == "t:adjacent" && rejection.Reason == "relation_related" {
				found = true
			}
		}
		if !found {
			t.Fatalf("rejections = %+v", pkg.Trace.Admission.Rejected)
		}
		for _, candidate := range assessor.seen {
			if candidate.Item.ID != "t:core" {
				continue
			}
			seenRefs := make([]string, 0, len(candidate.Evidence))
			for _, ref := range candidate.Evidence {
				seenRefs = append(seenRefs, ref.TurnID)
			}
			// The admitted candidate is assessed on its own canonical turn id;
			// no neighbour stands in for a citation.
			if !reflect.DeepEqual(seenRefs, []string{"t:core"}) {
				t.Fatalf("admitted candidate evidence = %v, want its own canonical turn id", seenRefs)
			}
		}
		if pkg.Sufficiency == nil || !reflect.DeepEqual(pkg.Sufficiency.EvidenceRefs, []string{"t:core"}) {
			t.Fatalf("sufficiency = %+v", pkg.Sufficiency)
		}
	})

	t.Run("partially_out_of_scope_required_ref_rejects_item", func(t *testing.T) {
		f := newFixture(t)
		f.conversation("conv:scope", "Scope", "rec:scope")
		f.recording("rec:scope", "conv:scope", nil)
		f.topic("topic:main", "Main topic")
		f.topic("topic:side", "Side topic")
		f.turn("t:in", "conv:scope", "rec:scope", 0, 1000, nil, "范围内事实原话")
		f.turn("t:out", "conv:scope", "rec:scope", 1, 2000, nil, "范围外事实原话")
		f.turnTopic("t:in", "topic:main")
		f.turnTopic("t:out", "topic:side")
		f.note("n:partial", "conv:scope", "observation", "部分范围标记 note", []string{"topic:main"}, []string{"t:in", "t:out"})
		assessor := &stubCandidateAssessor{decisions: map[string]AdmissionDecision{
			"n:partial": support(0.9),
		}}
		req := v3Request("部分范围标记")
		req.TopicIDs = []string{"topic:main"}
		pkg, err := Build(context.Background(), Deps{
			Store: f.store, DB: f.db, Semantic: &fakeSemantic{ready: true},
			CandidateAssessor: assessor, Assessor: termAssessor{},
			AdmissionPolicy: v3Policy(),
		}, req)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if len(pkg.Items) != 0 {
			t.Fatalf("partially out-of-scope item entered the package: %v", itemIDs(pkg.Items))
		}
		if len(assessor.seen) != 0 {
			t.Fatalf("assessor saw %d candidates for an item that must be rejected before submission", len(assessor.seen))
		}
		rejected := false
		for _, rejection := range pkg.Trace.Admission.Rejected {
			if rejection.ItemID == "n:partial" {
				rejected = true
			}
		}
		if !rejected {
			t.Fatalf("rejections = %+v", pkg.Trace.Admission.Rejected)
		}
		for _, ref := range pkg.Evidence {
			if strings.Contains(ref.Text, "范围外事实原话") {
				t.Fatalf("out-of-scope body text leaked into the evidence: %+v", pkg.Evidence)
			}
		}
		if pkg.Sufficiency == nil || pkg.Sufficiency.Status == "sufficient" {
			t.Fatalf("sufficiency = %+v", pkg.Sufficiency)
		}
	})

	t.Run("multi_ref_item_over_budget_provides_no_evidence", func(t *testing.T) {
		f := newFixture(t)
		start := "2026-03-01T10:00:00Z"
		f.conversation("conv:multi", "Multi", "rec:multi")
		f.recording("rec:multi", "conv:multi", &start)
		f.turn("t:a1", "conv:multi", "rec:multi", 0, 1000, nil, strings.Repeat("甲", 100))
		f.turn("t:a2", "conv:multi", "rec:multi", 1, 2000, nil, strings.Repeat("乙", 100))
		f.turn("t:b1", "conv:multi", "rec:multi", 2, 3000, nil, strings.Repeat("丙", 300))
		f.turn("t:b2", "conv:multi", "rec:multi", 3, 4000, nil, strings.Repeat("丁", 300))
		f.turn("t:c1", "conv:multi", "rec:multi", 4, 5000, nil, strings.Repeat("戊", 100))
		f.turn("t:cnear", "conv:multi", "rec:multi", 5, 6000, nil, strings.Repeat("近", 30))
		f.note("n:alpha", "conv:multi", "observation", "多引用预算 甲记录", nil, []string{"t:a1", "t:a2"})
		f.note("n:beta", "conv:multi", "observation", "多引用预算 乙记录", nil, []string{"t:b1", "t:b2"})
		f.note("n:gamma", "conv:multi", "observation", "多引用预算 丙记录", nil, []string{"t:c1"})
		decisions := map[string]AdmissionDecision{
			"n:alpha": support(0.9), "n:beta": support(0.9), "n:gamma": support(0.9),
		}
		newAssessor := func() *stubCandidateAssessor { return &stubCandidateAssessor{decisions: decisions} }
		probe, err := Build(context.Background(), Deps{
			Store: f.store, DB: f.db, Semantic: &fakeSemantic{ready: true},
			CandidateAssessor: newAssessor(), Assessor: termAssessor{},
			AdmissionPolicy: v3Policy(),
		}, v3Request("多引用预算"))
		if err != nil {
			t.Fatalf("probe Build: %v", err)
		}
		budget := 0
		for _, id := range []string{"n:alpha", "n:beta", "n:gamma"} {
			item, ok := itemByID(probe, id)
			if !ok {
				t.Fatalf("probe item %s missing: %v", id, itemIDs(probe.Items))
			}
			budget += itemTextBytes(item)
		}
		for _, id := range []string{"t:a1", "t:a2", "t:c1"} {
			text, ok := evidenceText(probe, id)
			if !ok {
				t.Fatalf("probe evidence %s missing: %+v", id, probe.Evidence)
			}
			budget += len(text)
		}
		req := v3Request("多引用预算")
		req.MaxBytes = budget
		pkg, err := Build(context.Background(), Deps{
			Store: f.store, DB: f.db, Semantic: &fakeSemantic{ready: true},
			CandidateAssessor: newAssessor(), Assessor: termAssessor{},
			AdmissionPolicy: v3Policy(),
		}, req)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		for _, id := range []string{"n:alpha", "n:beta", "n:gamma"} {
			if !containsString(itemIDs(pkg.Items), id) {
				t.Fatalf("admitted item %s missing from items: %v", id, itemIDs(pkg.Items))
			}
		}
		if got := evidenceTurnIDs(pkg); !reflect.DeepEqual(got, []string{"t:a1", "t:a2", "t:c1"}) {
			t.Fatalf("evidence = %v, want exactly the refitted items' required refs", got)
		}
		if got := pkg.Trace.Budget.SkippedEvidenceTurnIDs; !reflect.DeepEqual(got, []string{"t:b1", "t:b2"}) {
			t.Fatalf("skipped evidence = %v, want the over-budget item's whole ref set", got)
		}
		if !hasGap(pkg, "evidence_budget_exceeded") || !pkg.Truncated {
			t.Fatalf("gaps=%+v truncated=%v", pkg.Gaps, pkg.Truncated)
		}
		if containsString(pkg.Trace.Budget.SkippedEvidenceTurnIDs, "t:cnear") {
			t.Fatalf("an uncited neighbour turn was charged to the v3 evidence budget: %+v", pkg.Trace.Budget)
		}
	})

	t.Run("single_rerank_applies_pool_permutation_once", func(t *testing.T) {
		f := newFixture(t)
		start := "2026-03-01T10:00:00Z"
		f.conversation("conv:rr", "Rerank", "rec:rr")
		f.recording("rec:rr", "conv:rr", &start)
		f.turn("t:aa", "conv:rr", "rec:rr", 0, 1000, nil, "重排准入事实甲")
		f.turn("t:bb", "conv:rr", "rec:rr", 1, 2000, nil, "重排准入事实乙")
		f.turn("t:rel", "conv:rr", "rec:rr", 2, 3000, nil, "重排准入相关无事实")
		reranker := &v3PoolReranker{}
		assessor := &stubCandidateAssessor{decisions: map[string]AdmissionDecision{
			"t:aa": support(0.9), "t:bb": support(0.9),
			"t:rel": {Relation: RelationRelated, Probability: 0.9},
		}}
		pkg, err := Build(context.Background(), Deps{
			Store: f.store, DB: f.db, Semantic: &fakeSemantic{ready: true}, Reranker: reranker,
			CandidateAssessor: assessor, Assessor: termAssessor{required: []string{"重排准入事实甲", "重排准入事实乙"}},
			AdmissionPolicy: v3Policy(),
		}, v3Request("重排准入"))
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if reranker.calls != 1 {
			t.Fatalf("rerank calls = %d, want exactly the one pre-budget pool rerank", reranker.calls)
		}
		want := []string{"t:bb", "t:aa"}
		if got := itemIDs(pkg.Items); !reflect.DeepEqual(got, want) {
			t.Fatalf("items = %v, want the pool permutation applied to admitted candidates %v", got, want)
		}
		if !pkg.Trace.DecisionRerank {
			t.Fatalf("pool rerank not recorded: %+v", pkg.Trace)
		}
	})
}

// v3PoolReranker counts its invocations and reverses the submitted order.
type v3PoolReranker struct{ calls int }

func (r *v3PoolReranker) Rerank(_ context.Context, _ string, items []memory.Item) ([]memory.Item, error) {
	r.calls++
	out := make([]memory.Item, len(items))
	for i := range items {
		out[i] = items[len(items)-1-i]
	}
	return out, nil
}

func evidenceTurnIDs(pkg Package) []string {
	out := make([]string, 0, len(pkg.Evidence))
	for _, ref := range pkg.Evidence {
		out = append(out, ref.TurnID)
	}
	return out
}

// TestBuildV3EvidenceSharedRefChargedOnceAcrossItems pins the v3 evidence
// budget: a ref cited by several admitted items is charged once and merges
// UsedByItemIDs, and an item whose refs are already deduped into the budget
// still gets its evidence.
func TestBuildV3EvidenceSharedRefChargedOnceAcrossItems(t *testing.T) {
	f := newFixture(t)
	start := "2026-03-01T10:00:00Z"
	f.conversation("conv:share", "Share", "rec:share")
	f.recording("rec:share", "conv:share", &start)
	f.turn("t:shared", "conv:share", "rec:share", 0, 1000, nil, strings.Repeat("共", 200))
	f.turn("t:own1", "conv:share", "rec:share", 1, 2000, nil, strings.Repeat("甲", 100))
	f.turn("t:own2", "conv:share", "rec:share", 2, 3000, nil, strings.Repeat("乙", 100))
	f.note("n:one", "conv:share", "observation", "共享引用标记 one", nil, []string{"t:shared", "t:own1"})
	f.note("n:two", "conv:share", "observation", "共享引用标记 two", nil, []string{"t:shared", "t:own2"})
	decisions := map[string]AdmissionDecision{"n:one": support(0.9), "n:two": support(0.9)}
	newAssessor := func() *stubCandidateAssessor { return &stubCandidateAssessor{decisions: decisions} }
	probe, err := Build(context.Background(), Deps{
		Store: f.store, DB: f.db, Semantic: &fakeSemantic{ready: true},
		CandidateAssessor: newAssessor(), Assessor: termAssessor{},
		AdmissionPolicy: v3Policy(),
	}, v3Request("共享引用标记"))
	if err != nil {
		t.Fatalf("probe Build: %v", err)
	}
	budget := 0
	for _, id := range []string{"n:one", "n:two"} {
		item, ok := itemByID(probe, id)
		if !ok {
			t.Fatalf("probe item %s missing: %v", id, itemIDs(probe.Items))
		}
		budget += itemTextBytes(item)
	}
	for _, id := range []string{"t:shared", "t:own1", "t:own2"} {
		// Turn sizes come from the store, not from a probe build: the buggy
		// window path can omit a required ref before the fix lands.
		var text string
		if err := f.db.QueryRow(`SELECT content FROM turns WHERE id=?`, id).Scan(&text); err != nil {
			t.Fatalf("turn %s: %v", id, err)
		}
		budget += len(text)
	}
	req := v3Request("共享引用标记")
	req.MaxBytes = budget
	pkg, err := Build(context.Background(), Deps{
		Store: f.store, DB: f.db, Semantic: &fakeSemantic{ready: true},
		CandidateAssessor: newAssessor(), Assessor: termAssessor{},
		AdmissionPolicy: v3Policy(),
	}, req)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := evidenceTurnIDs(pkg); !reflect.DeepEqual(got, []string{"t:shared", "t:own1", "t:own2"}) {
		t.Fatalf("evidence = %v, want both items' refs with the shared turn deduped", got)
	}
	for _, ref := range pkg.Evidence {
		if ref.TurnID == "t:shared" && !reflect.DeepEqual(ref.UsedByItemIDs, []string{"n:one", "n:two"}) {
			t.Fatalf("shared ref used-by = %v, want both items", ref.UsedByItemIDs)
		}
	}
	if pkg.Truncated || hasGap(pkg, "evidence_budget_exceeded") || len(pkg.Trace.Budget.SkippedEvidenceTurnIDs) != 0 {
		t.Fatalf("budget charged a deduped ref twice: gaps=%+v trace=%+v", pkg.Gaps, pkg.Trace.Budget)
	}
}

// TestBuildV3EvidenceItemWindowCapsAtThree keeps the documented cap: at most
// three selected items provide evidence, and the capped item's refs are
// omitted without a budget-skip record.
func TestBuildV3EvidenceItemWindowCapsAtThree(t *testing.T) {
	f := newFixture(t)
	start := "2026-03-01T10:00:00Z"
	for i := 1; i <= 4; i++ {
		conv := fmt.Sprintf("conv:cap%d", i)
		rec := fmt.Sprintf("rec:cap%d", i)
		f.conversation(conv, fmt.Sprintf("Cap %d", i), rec)
		f.recording(rec, conv, &start)
		f.turn(fmt.Sprintf("t:%d", i), conv, rec, 0, 1000, nil, fmt.Sprintf("三窗口上限 原话 %d", i))
		f.note(fmt.Sprintf("n:%d", i), conv, "observation", fmt.Sprintf("三窗口上限 note %d", i), nil, []string{fmt.Sprintf("t:%d", i)})
	}
	assessor := &stubCandidateAssessor{decisions: map[string]AdmissionDecision{
		"n:1": support(0.9), "n:2": support(0.9), "n:3": support(0.9), "n:4": support(0.9),
	}}
	pkg, err := Build(context.Background(), Deps{
		Store: f.store, DB: f.db, Semantic: &fakeSemantic{ready: true},
		CandidateAssessor: assessor, Assessor: termAssessor{},
		AdmissionPolicy: v3Policy(),
	}, v3Request("三窗口上限"))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := evidenceTurnIDs(pkg); !reflect.DeepEqual(got, []string{"t:1", "t:2", "t:3"}) {
		t.Fatalf("evidence = %v, want the first three items' refs", got)
	}
	if containsString(pkg.Trace.Budget.SkippedEvidenceTurnIDs, "t:4") {
		t.Fatalf("the capped item's ref was recorded as a budget skip: %+v", pkg.Trace.Budget)
	}
	if hasGap(pkg, "evidence_budget_exceeded") {
		t.Fatalf("the item cap was reported as a budget gap: %+v", pkg.Gaps)
	}
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
