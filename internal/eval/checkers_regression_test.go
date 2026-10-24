package eval

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"

	"recordingmcp/internal/asrinput"
	"recordingmcp/internal/contextbuild"
	"recordingmcp/internal/memory"
)

type evalTestReranker struct{ calls int }

func (r *evalTestReranker) Rerank(_ context.Context, _ string, items []memory.Item) ([]memory.Item, error) {
	r.calls++
	return items, nil
}

type evalTestAssessor struct {
	calls int
	err   error
}

func (a *evalTestAssessor) Assess(_ context.Context, _ string, evidence []contextbuild.EvidenceRef) (contextbuild.EvidenceAssessment, error) {
	a.calls++
	if a.err != nil {
		return contextbuild.EvidenceAssessment{}, a.err
	}
	return contextbuild.EvidenceAssessment{Status: "sufficient", EvidenceRefs: []string{"turn:visible"}}, nil
}

func TestCountingAssessorCountsFailedInvocation(t *testing.T) {
	inner := &evalTestAssessor{err: errors.New("decision unavailable")}
	counted := &countingAssessor{inner: inner}
	_, err := counted.Assess(context.Background(), "q", []contextbuild.EvidenceRef{{TurnID: "turn:visible", Text: "visible"}})
	if err == nil {
		t.Fatal("expected assessor failure")
	}
	if counted.calls != 1 || inner.calls != 1 {
		t.Fatalf("failed call counts wrapper=%d inner=%d, want one actual invocation", counted.calls, inner.calls)
	}
}

func TestEvaluationCountingAdaptersCountOnlyActualCalls(t *testing.T) {
	r := &evalTestReranker{}
	a := &evalTestAssessor{}
	countedReranker := &countingReranker{inner: r}
	countedAssessor := &countingAssessor{inner: a}
	if _, err := countedReranker.Rerank(context.Background(), "q", []memory.Item{{ID: "i"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := countedAssessor.Assess(context.Background(), "q", []contextbuild.EvidenceRef{{TurnID: "turn:visible", Text: "visible"}}); err != nil {
		t.Fatal(err)
	}
	if countedReranker.calls != 1 || countedAssessor.calls != 1 || r.calls != 1 || a.calls != 1 {
		t.Fatalf("call counts wrapper=(%d,%d) inner=(%d,%d), want one actual call each", countedReranker.calls, countedAssessor.calls, r.calls, a.calls)
	}
}

func TestV2DecisionRerankerIsUpgradedBeforeCountingWrapper(t *testing.T) {
	selected := decisionRerankerForVersion(contextbuild.DecisionReranker{}, "v2")
	if _, ok := selected.(contextbuild.DecisionRerankerV2); !ok {
		t.Fatalf("v2 adapter = %T, want DecisionRerankerV2", selected)
	}
	v1 := decisionRerankerForVersion(contextbuild.DecisionReranker{}, "v1")
	if _, ok := v1.(contextbuild.DecisionReranker); !ok {
		t.Fatalf("v1 adapter = %T, want DecisionReranker", v1)
	}
}

func TestEvaluateQuestionFlagsMixedStaleEvidence(t *testing.T) {
	pkg := contextbuild.Package{Items: []memory.Item{{
		ID:              "note:mixed",
		Kind:            "observation",
		EvidenceTurnIDs: []string{"turn:current", "turn:stale"},
	}}}
	got := evaluateQuestion(Question{ID: "mixed-stale"}, pkg,
		map[string]bool{"turn:stale": true},
		map[string]bool{"turn:current": true, "turn:stale": true}, nil)
	if got.staleLeakage != 1 {
		t.Fatalf("mixed stale evidence leakage = %f, want 1", got.staleLeakage)
	}
}

func TestEvaluateQuestionChecksInvalidRefsOnStaleItems(t *testing.T) {
	pkg := contextbuild.Package{
		Items: []memory.Item{{
			ID:              "note:stale-invalid",
			Kind:            "observation",
			Stale:           true,
			EvidenceTurnIDs: []string{"turn:item-invalid"},
		}},
		Evidence: []contextbuild.EvidenceRef{{TurnID: "turn:evidence-invalid"}},
	}
	got := evaluateQuestion(Question{ID: "stale-invalid"}, pkg, nil, nil, nil)
	if got.invalidRefs != 2 {
		t.Fatalf("invalid refs = %d, want 2 across item and package evidence", got.invalidRefs)
	}
}

func TestAllGoldEvidencePresentUsesOnlyVisibleFinalEvidence(t *testing.T) {
	q := Question{ID: "long-note", GoldEvidenceTurnIDs: []string{"turn:gold"}}
	pkg := contextbuild.Package{Items: []memory.Item{{
		ID: "note:long", Kind: "observation", EvidenceTurnIDs: []string{"turn:gold"},
		Summary: "a note citation that is not present as visible evidence",
	}}}
	if allGoldEvidencePresent(q, pkg) {
		t.Fatal("note metadata counted as visible evidence without a final evidence ref or raw turn")
	}
}

func TestAllGoldEvidencePresentRequiresEveryFinalVisibleRef(t *testing.T) {
	q := Question{ID: "cross-session", GoldEvidenceTurnIDs: []string{"turn:first", "turn:second"}}
	pkg := contextbuild.Package{Evidence: []contextbuild.EvidenceRef{
		{TurnID: "turn:first", Text: "first session fact"},
		{TurnID: "turn:second", Text: "second session qualifier"},
	}}
	if !allGoldEvidencePresent(q, pkg) {
		t.Fatal("both required evidence refs are visible in the final package")
	}
	pkg.Evidence = pkg.Evidence[:1]
	if allGoldEvidencePresent(q, pkg) {
		t.Fatal("one visible ref must not satisfy a two-ref required-evidence group")
	}
}

func TestAllGoldEvidencePresentAcceptsVisibleRawTurnOnly(t *testing.T) {
	q := Question{ID: "raw-turn", GoldEvidenceTurnIDs: []string{"turn:raw"}}
	pkg := contextbuild.Package{Items: []memory.Item{{Kind: "turn", TurnID: "turn:raw", Content: "the visible transcript text"}}}
	if !allGoldEvidencePresent(q, pkg) {
		t.Fatal("visible raw turn content should satisfy its required evidence ref")
	}
	pkg.Items[0].Content = ""
	if allGoldEvidencePresent(q, pkg) {
		t.Fatal("turn id without visible content must not satisfy required evidence")
	}
}

func TestReciprocalRankAt10Boundary(t *testing.T) {
	for _, tc := range []struct {
		rank int
		want float64
	}{{rank: 9, want: 1.0 / 9}, {rank: 10, want: .1}, {rank: 11, want: 0}} {
		items := make([]memory.Item, 11)
		for i := range items {
			items[i] = memory.Item{Kind: "turn", ID: fmt.Sprintf("turn:%d", i+1)}
		}
		goldIndex := tc.rank - 1
		items[goldIndex].ID = "turn:gold"
		pkg := contextbuild.Package{Items: items}
		got := reciprocalRankAt10([]string{"turn:gold"}, pkg, nil)
		if math.Abs(got.Value-tc.want) > 1e-12 {
			t.Errorf("rank %d MRR = %f, want %f", tc.rank, got.Value, tc.want)
		}
	}
}

func TestLoadQuestionsEmptyPathDisablesClassicEvaluation(t *testing.T) {
	questions, err := LoadQuestions(context.Background(), nil, "")
	if err != nil {
		t.Fatalf("empty classic question path: %v", err)
	}
	if len(questions) != 0 {
		t.Fatalf("empty classic question path loaded %d questions", len(questions))
	}
}
func TestCountScopeEscapesChecksEvidenceRefs(t *testing.T) {
	ctx := context.Background()
	store, err := memory.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	inConversationID, _ := seedScopeTurn(t, ctx, store, "conv-in", "seg-in", "turn-in")
	_, outTurnID := seedScopeTurn(t, ctx, store, "conv-out", "seg-out", "turn-out")
	q := RetrievalQuestion{ID: "scoped-evidence", ScopeConversationID: inConversationID}
	pkg := contextbuild.Package{
		Items:    []memory.Item{{ID: "note:in", Kind: "observation", ConversationID: inConversationID}},
		Evidence: []contextbuild.EvidenceRef{{TurnID: outTurnID}},
	}

	got, err := countScopeEscapes(ctx, store, q, pkg, nil)
	if err != nil {
		t.Fatalf("countScopeEscapes: %v", err)
	}
	if got != 1 {
		t.Fatalf("scope escapes = %d, want 1 for the out-of-scope evidence ref", got)
	}
}

func seedScopeTurn(t *testing.T, ctx context.Context, store *memory.Store, conversationID, segmentID, turnID string) (string, string) {
	t.Helper()
	start := "2026-03-01T09:00:00+08:00"
	report, err := store.IngestTranscript(ctx, asrinput.Result{
		ContractVersion: asrinput.ContractVersion,
		Source: asrinput.Source{
			Provider:           "eval-test",
			ProviderRequestID:  segmentID,
			DeviceID:           "device-eval-test",
			RecordingID:        "rec-" + conversationID,
			ConversationID:     conversationID,
			SegmentID:          segmentID,
			SegmentDurationMS:  1000,
			TranscriptRevision: "rev-1",
			CompletionStatus:   "final",
			RecordingStartedAt: &start,
		},
		Turns: []asrinput.Turn{{
			ID: turnID, OriginalIndex: 1, Text: "scope test",
			LocalStartMS: int64ptr(0), LocalEndMS: int64ptr(500),
			GlobalStartMS: int64ptr(0), GlobalEndMS: int64ptr(500),
		}},
	})
	if err != nil {
		t.Fatalf("ingest %s: %v", conversationID, err)
	}
	var storedTurnID string
	if err := store.DB().QueryRowContext(ctx, `SELECT id FROM turns WHERE original_turn_id=?`, segmentID+"/rev-1/"+turnID).Scan(&storedTurnID); err != nil {
		t.Fatalf("resolve ingested turn %s: %v", turnID, err)
	}
	return report.ConversationID, storedTurnID
}

func int64ptr(v int64) *int64 { return &v }
