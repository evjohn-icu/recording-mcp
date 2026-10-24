package contextbuild

import (
	"context"
	"errors"
	"strings"
	"testing"

	"recordingmcp/internal/decision"
	"recordingmcp/internal/memory"
)

type reorderReranker struct {
	fn         func([]memory.Item) []memory.Item
	got        int
	inputBytes int
}

func (r *reorderReranker) Rerank(_ context.Context, query string, items []memory.Item) ([]memory.Item, error) {
	r.got = len(items)
	r.inputBytes = rerankInputBytes(query, items)
	return r.fn(items), nil
}

type fakeAssessor struct {
	got    []EvidenceRef
	result EvidenceAssessment
	err    error
}

type captureDecisionEngine struct {
	questions []decision.Question
	state     string
	badScore  bool
}

func (e *captureDecisionEngine) Evaluate(_ context.Context, state string, questions []decision.Question) (decision.Result, error) {
	e.state = state
	e.questions = append([]decision.Question(nil), questions...)
	answers := map[string]decision.Answer{}
	for _, q := range questions {
		score := 1.0
		if e.badScore {
			score = 100
		}
		answers[q.ID] = decision.Answer{Type: q.Type, Score: score, Confidence: 1, Distribution: map[string]float64{"irrelevant": 0, "weak": 0, "relevant": 1, "critical": 0}}
	}
	return decision.Result{Answers: answers}, nil
}

func (a *fakeAssessor) Assess(_ context.Context, _ string, evidence []EvidenceRef) (EvidenceAssessment, error) {
	a.got = append([]EvidenceRef(nil), evidence...)
	return a.result, a.err
}

func TestBuildV2ReranksBoundedPoolBeforeFinalBudget(t *testing.T) {
	f := newFixture(t)
	start := "2026-03-01T10:00:00Z"
	f.conversation("conv:a", "Alpha", "rec:a")
	f.recording("rec:a", "conv:a", &start)
	f.note("n:a", "conv:a", "observation", "query phrase first", nil, nil)
	f.note("n:b", "conv:a", "observation", "obscure query phrase winner", nil, nil)
	r := &reorderReranker{fn: func(items []memory.Item) []memory.Item {
		out := append([]memory.Item(nil), items...)
		for i := range out {
			if out[i].ID == "n:b" {
				out[0], out[i] = out[i], out[0]
				break
			}
		}
		for i, j := 1, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
		return out
	}}
	pkg, err := Build(context.Background(), Deps{Store: f.store, DB: f.db, Reranker: r}, Request{Query: "query phrase", RetrievalVersion: "v2", MaxItems: 1, MaxBytes: 65536, RerankCandidateLimit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if r.got != 2 {
		t.Fatalf("rerank pool = %d, want configured limit 2", r.got)
	}
	if len(pkg.Items) != 1 || pkg.Items[0].ID != "n:b" {
		t.Fatalf("pre-budget rerank result = %v, want [n:b]", itemIDs(pkg.Items))
	}
}

func TestBuildV2NilRerankerKeepsCandidatesWhenQueryExceedsModelCap(t *testing.T) {
	f := newFixture(t)
	f.conversation("conv:a", "A", "rec:a")
	f.recording("rec:a", "conv:a", nil)
	query := strings.Repeat("q", maxRerankInputBytes+1024)
	pkg, err := Build(context.Background(), Deps{Store: f.store, DB: f.db}, Request{Query: query, ConversationID: "conv:a", RetrievalVersion: "v2", MaxBytes: 100, MaxItems: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(pkg.Items) != 1 || pkg.Items[0].ID != "conv:a" {
		t.Fatalf("nil reranker dropped candidate for model-only byte cap: %v", itemIDs(pkg.Items))
	}
	if pkg.Trace.Budget.BytesUsed > 100 {
		t.Fatalf("output budget exceeded: %+v", pkg.Trace.Budget)
	}
}

func TestBuildV2CustomRerankerInputStaysWithin64KiB(t *testing.T) {
	f := newFixture(t)
	f.conversation("conv:a", "A", "rec:a")
	f.recording("rec:a", "conv:a", nil)
	r := &reorderReranker{fn: func(items []memory.Item) []memory.Item { return items }}
	query := strings.Repeat("q", maxRerankInputBytes-1024)
	pkg, err := Build(context.Background(), Deps{Store: f.store, DB: f.db, Reranker: r}, Request{Query: query, ConversationID: "conv:a", RetrievalVersion: "v2", MaxBytes: 100, MaxItems: 1})
	if err != nil {
		t.Fatal(err)
	}
	if r.got == 0 {
		t.Fatalf("configured reranker was not called: gaps=%+v", pkg.Gaps)
	}
	if r.inputBytes > maxRerankInputBytes {
		t.Fatalf("custom reranker input is %d bytes", r.inputBytes)
	}
}

func TestDecisionRerankerV1KeepsHistoricalQuestionPrompt(t *testing.T) {
	e := &captureDecisionEngine{}
	_, err := (DecisionReranker{Engine: e}).Rerank(context.Background(), "legacy query", []memory.Item{{ID: "i:1", Kind: "note"}})
	if err != nil {
		t.Fatal(err)
	}
	want := "Rate how relevant item_0 is to the query \"legacy query\" against the rubric."
	if len(e.questions) != 1 || e.questions[0].Instructions != want {
		t.Fatalf("v1 prompt = %q, want historical %q", e.questions[0].Instructions, want)
	}
}

func TestBuildV2DecisionRerankerRejectsInvalidScore(t *testing.T) {
	e := &captureDecisionEngine{badScore: true}
	_, err := (DecisionRerankerV2{Engine: e}).Rerank(context.Background(), "query", []memory.Item{{ID: "i:1", Kind: "note"}})
	if err == nil {
		t.Fatal("v2 decision reranker accepted an out-of-rubric score")
	}
}

func TestBuildV2ConversationFilterSurvivesRerank(t *testing.T) {
	f := newFixture(t)
	f.conversation("conv:a", "A", "rec:a")
	f.conversation("conv:b", "B", "rec:b")
	f.recording("rec:a", "conv:a", nil)
	f.recording("rec:b", "conv:b", nil)
	f.turn("t:a", "conv:a", "rec:a", 0, 0, nil, "scoped common phrase")
	f.turn("t:b", "conv:b", "rec:b", 0, 0, nil, "scoped common phrase")
	r := &reorderReranker{fn: func(items []memory.Item) []memory.Item { return items }}
	pkg, err := Build(context.Background(), Deps{Store: f.store, DB: f.db, Reranker: r}, Request{Query: "scoped common", ConversationID: "conv:a", RetrievalVersion: "v2"})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range pkg.Items {
		if item.ConversationID != "conv:a" {
			t.Fatalf("v2 rerank leaked out-of-scope item: %+v", item)
		}
	}
}

func TestBuildV2RejectsInvalidRerankAndFallsBack(t *testing.T) {
	f := newFixture(t)
	f.conversation("conv:a", "A", "rec:a")
	f.recording("rec:a", "conv:a", nil)
	f.note("n:a", "conv:a", "observation", "unique fallback phrase", nil, nil)
	r := &reorderReranker{fn: func(items []memory.Item) []memory.Item { return []memory.Item{items[0], items[0]} }}
	pkg, err := Build(context.Background(), Deps{Store: f.store, DB: f.db, Reranker: r}, Request{Query: "unique fallback", RetrievalVersion: "v2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(pkg.Items) == 0 || pkg.Items[0].ID != "n:a" {
		t.Fatalf("invalid rerank changed canonical leading result: %v", itemIDs(pkg.Items))
	}
	if !pkg.Degraded || !hasGap(pkg, "rerank_failed") {
		t.Fatalf("invalid permutation not reported: degraded=%v gaps=%+v", pkg.Degraded, pkg.Gaps)
	}
}

func TestBuildV2RejectsRerankerMutation(t *testing.T) {
	f := newFixture(t)
	f.conversation("conv:a", "A", "rec:a")
	f.recording("rec:a", "conv:a", nil)
	f.note("n:a", "conv:a", "observation", "mutation guard phrase", nil, nil)
	r := &reorderReranker{fn: func(items []memory.Item) []memory.Item { items[0].Summary = "fabricated"; return items }}
	pkg, err := Build(context.Background(), Deps{Store: f.store, DB: f.db, Reranker: r}, Request{Query: "mutation guard", RetrievalVersion: "v2"})
	if err != nil {
		t.Fatal(err)
	}
	if !hasGap(pkg, "rerank_failed") || len(pkg.Items) == 0 || pkg.Items[0].Summary == "fabricated" {
		t.Fatalf("reranker mutation was not rejected: %+v gaps=%+v", pkg.Items, pkg.Gaps)
	}
}

func TestBuildV2AssessesFinalEvidenceAndRetainsPartialResultsOnError(t *testing.T) {
	f := newFixture(t)
	f.conversation("conv:a", "A", "rec:a")
	f.recording("rec:a", "conv:a", nil)
	f.turn("t:a", "conv:a", "rec:a", 0, 0, nil, "final evidence content")
	a := &fakeAssessor{err: errors.New("assessor offline")}
	pkg, err := Build(context.Background(), Deps{Store: f.store, DB: f.db, Assessor: a}, Request{Query: "final evidence", RetrievalVersion: "v2", IncludeRawEvidence: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(pkg.Items) == 0 {
		t.Fatal("assessment error discarded partial retrieval")
	}
	if len(a.got) == 0 || len(pkg.Evidence) == 0 || len(a.got) != len(pkg.Evidence) {
		t.Fatalf("assessor did not receive final evidence: assessor=%v package=%v", a.got, pkg.Evidence)
	}
	if pkg.Sufficiency.Status != "unknown" || !pkg.Degraded || !hasGap(pkg, "evidence_assessment_failed") {
		t.Fatalf("assessment failure not explicit: %+v gaps=%+v", pkg.Sufficiency, pkg.Gaps)
	}
}

func TestBuildV2RejectsOutOfSetAssessmentReferences(t *testing.T) {
	f := newFixture(t)
	f.conversation("conv:a", "A", "rec:a")
	f.recording("rec:a", "conv:a", nil)
	f.turn("t:a", "conv:a", "rec:a", 0, 0, nil, "evidence claim")
	a := &fakeAssessor{result: EvidenceAssessment{Status: "sufficient", EvidenceRefs: []string{"not-in-final-evidence"}}}
	pkg, err := Build(context.Background(), Deps{Store: f.store, DB: f.db, Assessor: a}, Request{Query: "evidence claim", RetrievalVersion: "v2", IncludeRawEvidence: true})
	if err != nil {
		t.Fatal(err)
	}
	if pkg.Sufficiency.Status != "unknown" || len(pkg.Sufficiency.EvidenceRefs) != 0 {
		t.Fatalf("invalid refs accepted: %+v", pkg.Sufficiency)
	}
}

func TestBuildV2MissingAssessmentDistinguishesNoEvidence(t *testing.T) {
	f := newFixture(t)
	f.conversation("conv:a", "A", "rec:a")
	f.recording("rec:a", "conv:a", nil)
	f.note("n:a", "conv:a", "observation", "no source evidence here", nil, nil)
	pkg, err := Build(context.Background(), Deps{Store: f.store, DB: f.db}, Request{Query: "source evidence", RetrievalVersion: "v2"})
	if err != nil {
		t.Fatal(err)
	}
	if pkg.Sufficiency == nil || pkg.Sufficiency.Status != "insufficient" {
		t.Fatalf("empty final evidence status = %+v, want insufficient", pkg.Sufficiency)
	}
}

func TestBuildV2AssessmentCannotUseSourceEvidenceOutsideFinalBudget(t *testing.T) {
	f := newFixture(t)
	f.conversation("conv:a", "A", "rec:a")
	f.recording("rec:a", "conv:a", nil)
	longSource := "hidden answer " + strings.Repeat("supporting detail ", 3000)
	f.turn("t:hidden", "conv:a", "rec:a", 0, 0, nil, longSource)
	f.note("n:brief", "conv:a", "observation", "budget only query phrase", nil, []string{"t:hidden"})
	a := &fakeAssessor{result: EvidenceAssessment{Status: "sufficient", EvidenceRefs: []string{"t:hidden"}}}
	pkg, err := Build(context.Background(), Deps{Store: f.store, DB: f.db, Assessor: a}, Request{Query: "budget only query", RetrievalVersion: "v2", MaxItems: 1, MaxBytes: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(pkg.Items) != 1 || pkg.Items[0].ID != "n:brief" {
		t.Fatalf("brief package = %v", itemIDs(pkg.Items))
	}
	if len(pkg.Evidence) != 0 || len(a.got) != 0 {
		t.Fatalf("assessor saw evidence outside final budget: package=%+v assessor=%+v", pkg.Evidence, a.got)
	}
	if pkg.Sufficiency == nil || pkg.Sufficiency.Status != "insufficient" {
		t.Fatalf("status = %+v, want insufficient when source does not fit", pkg.Sufficiency)
	}
	if !hasGap(pkg, "budget_exhausted") {
		t.Fatalf("missing budget gap: %+v", pkg.Gaps)
	}
}

func TestBuildV2NoAssessorWithUnreturnedSourceIsUnknown(t *testing.T) {
	f := newFixture(t)
	f.conversation("conv:a", "A", "rec:a")
	f.recording("rec:a", "conv:a", nil)
	f.turn("t:source", "conv:a", "rec:a", 0, 0, nil, "unreturned source")
	f.note("n:ref", "conv:a", "observation", "unreturned source query", nil, []string{"t:source"})
	pkg, err := Build(context.Background(), Deps{Store: f.store, DB: f.db}, Request{Query: "unreturned source query", RetrievalVersion: "v2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(pkg.Evidence) != 0 {
		t.Fatalf("unexpected raw evidence without requested output: %+v", pkg.Evidence)
	}
	if pkg.Sufficiency == nil || pkg.Sufficiency.Status != "unknown" {
		t.Fatalf("status = %+v, want unknown for unassessed cited source", pkg.Sufficiency)
	}
}
