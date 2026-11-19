package contextbuild

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"recordingmcp/internal/decision"
	"recordingmcp/internal/memory"
)

// TestAdmissionRequestSizeMatchesWholeMarshal pins the incremental byte
// accounting to the single json.Marshal it replaced. If they ever disagree the
// 64 KiB cap would accept or reject different candidates than before.
func TestAdmissionRequestSizeMatchesWholeMarshal(t *testing.T) {
	query := "预算 <十五> 分钟 & 谁负责"
	candidates := []AdmissionCandidate{
		{Item: memory.Item{ID: "a", Kind: "observation"}, SourceStatus: SourceStatusRawCurrent,
			Evidence: []EvidenceRef{{TurnID: "t1", Text: "含 <标签> 与 \u2028 分隔"}},
			Sources:  []string{"literal"}, Scores: map[string]float64{"lexical": 0.5}},
		{Item: memory.Item{ID: "b", Kind: "decision"}, SourceStatus: SourceStatusDerivedProposed,
			Evidence: []EvidenceRef{{TurnID: "t2", Text: "预算十五分钟 & 明天提交"}}},
		{Item: memory.Item{ID: "c", Kind: "note"}, SourceStatus: SourceStatusNavigationOnly},
	}
	request, err := newAdmissionRequest(query, candidates)
	if err != nil {
		t.Fatalf("newAdmissionRequest: %v", err)
	}
	for _, batch := range [][]int{{0}, {1}, {0, 1}, {0, 2}, {0, 1, 2}, {2, 0}} {
		envelope, err := json.Marshal(struct {
			State     string              `json:"state"`
			Questions []decision.Question `json:"questions"`
		}{State: request.state(batch), Questions: request.questionsFor(batch)})
		if err != nil {
			t.Fatalf("marshal batch %v: %v", batch, err)
		}
		if got, want := request.size(batch), len(envelope); got != want {
			t.Errorf("batch %v: incremental size %d, whole marshal %d", batch, got, want)
		}
	}
}

// recordingEngine answers every question with relation unknown and records the
// state it received, so request construction can be compared structurally.
type recordingEngine struct {
	batches []int
	states  []string
}

func (e *recordingEngine) Evaluate(_ context.Context, state string, questions []decision.Question) (decision.Result, error) {
	e.batches = append(e.batches, len(questions))
	e.states = append(e.states, state)
	answers := map[string]decision.Answer{}
	for _, q := range questions {
		answers[q.ID] = decision.Answer{Type: q.Type, Choice: RelationUnknown, Confidence: 1,
			Distribution: map[string]float64{RelationSupports: 0, RelationRefutes: 0,
				RelationRelated: 0, RelationUnknown: 1}}
	}
	return decision.Result{Answers: answers}, nil
}

func paddingCandidates(count int) []AdmissionCandidate {
	candidates := make([]AdmissionCandidate, count)
	for i := range candidates {
		candidates[i] = AdmissionCandidate{
			Item: memory.Item{ID: fmt.Sprintf("item-%02d", i), Kind: "observation"},
			Evidence: []EvidenceRef{{TurnID: fmt.Sprintf("turn-%02d", i),
				Text: "候选正文"}},
			SourceStatus: SourceStatusRawCurrent,
		}
	}
	return candidates
}

// TestAdmissionCapBoundaryIsExact accepts a request of exactly 64 KiB and
// rejects the same request one byte larger.
func TestAdmissionCapBoundaryIsExact(t *testing.T) {
	for _, delta := range []int{0, 1} {
		candidates := paddingCandidates(2)
		request, err := newAdmissionRequest("query", candidates)
		if err != nil {
			t.Fatalf("newAdmissionRequest: %v", err)
		}
		// Measure with an empty body, then pad with ASCII so the escaped block
		// length grows exactly one byte per character.
		candidates[1].Evidence[0].Text = ""
		if request, err = newAdmissionRequest("query", candidates); err != nil {
			t.Fatalf("newAdmissionRequest: %v", err)
		}
		padding := maxRerankInputBytes + delta - request.size([]int{0, 1})
		if padding < 0 {
			t.Fatalf("baseline batch already exceeds the cap by %d bytes", -padding)
		}
		candidates[1].Evidence[0].Text = strings.Repeat("q", padding)
		request, err = newAdmissionRequest("query", candidates)
		if err != nil {
			t.Fatalf("newAdmissionRequest: %v", err)
		}
		if got := request.size([]int{0, 1}); got != maxRerankInputBytes+delta {
			t.Fatalf("delta %d: size = %d, want %d", delta, got, maxRerankInputBytes+delta)
		}
		engine := &recordingEngine{}
		assessor := DecisionCandidateAssessor{Engine: engine, MaxQuestionsPerBatch: 32}
		if _, err := assessor.AssessCandidates(t.Context(), "query", candidates); err != nil {
			t.Fatalf("AssessCandidates: %v", err)
		}
		if delta == 0 && (len(engine.batches) != 1 || engine.batches[0] != 2) {
			t.Errorf("exact-cap batch: batches = %v, want one batch of 2", engine.batches)
		}
		if delta == 1 && (len(engine.batches) != 2 || engine.batches[0] != 1 || engine.batches[1] != 1) {
			t.Errorf("over-cap batch: batches = %v, want two batches of 1", engine.batches)
		}
	}
}

// TestAdmissionRejectsOversizedCandidateAndKeepsSmallOnes keeps the documented
// behaviour: a candidate whose own evidence cannot fit is dropped, and the next
// smaller candidate is still submitted.
func TestAdmissionRejectsOversizedCandidateAndKeepsSmallOnes(t *testing.T) {
	candidates := paddingCandidates(3)
	candidates[0].Evidence[0].Text = strings.Repeat("q", maxRerankInputBytes)
	engine := &recordingEngine{}
	assessor := DecisionCandidateAssessor{Engine: engine, MaxQuestionsPerBatch: 32}
	decisions, err := assessor.AssessCandidates(t.Context(), "query", candidates)
	if err != nil {
		t.Fatalf("AssessCandidates: %v", err)
	}
	if decisions[0].Relation != RelationUnknown || decisions[0].Probability != 0 {
		t.Errorf("oversized candidate = %+v, want unknown with probability 0", decisions[0])
	}
	if len(engine.batches) != 1 || engine.batches[0] != 2 {
		t.Errorf("batches = %v, want one batch of the two small candidates", engine.batches)
	}
	for _, state := range engine.states {
		if strings.Contains(state, strings.Repeat("q", 64)) {
			t.Error("oversized candidate body was submitted")
		}
	}
}

// TestAdmissionStatsAccountForRequests pins the offline profile counters.
func TestAdmissionStatsAccountForRequests(t *testing.T) {
	candidates := paddingCandidates(5)
	stats := &AdmissionRequestStats{}
	engine := &recordingEngine{}
	assessor := DecisionCandidateAssessor{Engine: engine, MaxQuestionsPerBatch: 2, Stats: stats}
	if _, err := assessor.AssessCandidates(t.Context(), "query", candidates); err != nil {
		t.Fatalf("AssessCandidates: %v", err)
	}
	if stats.Requests != 3 || stats.Candidates != 5 {
		t.Fatalf("stats = %+v, want 3 requests over 5 candidates", stats)
	}
	request, err := newAdmissionRequest("query", candidates)
	if err != nil {
		t.Fatalf("newAdmissionRequest: %v", err)
	}
	want := request.size([]int{0, 1}) + request.size([]int{2, 3}) + request.size([]int{4})
	if stats.Bytes != want {
		t.Errorf("stats bytes = %d, want %d", stats.Bytes, want)
	}
	if got, wantBatches := engine.batches, []int{2, 2, 1}; len(got) != len(wantBatches) {
		t.Fatalf("batches = %v, want %v", got, wantBatches)
	}
}

// TestAdmissionInputStructureIsStable compares the state the engine receives
// with the precomputation's own rendering: the optimisation must not reorder,
// rename or truncate candidates.
func TestAdmissionInputStructureIsStable(t *testing.T) {
	candidates := paddingCandidates(4)
	engine := &recordingEngine{}
	assessor := DecisionCandidateAssessor{Engine: engine, MaxQuestionsPerBatch: 4}
	if _, err := assessor.AssessCandidates(t.Context(), "query", candidates); err != nil {
		t.Fatalf("AssessCandidates: %v", err)
	}
	request, err := newAdmissionRequest("query", candidates)
	if err != nil {
		t.Fatalf("newAdmissionRequest: %v", err)
	}
	if len(engine.states) != 1 {
		t.Fatalf("states = %d, want 1", len(engine.states))
	}
	if engine.states[0] != request.state([]int{0, 1, 2, 3}) {
		t.Error("engine state differs from the precomputed rendering")
	}
	questions := request.questionsFor([]int{0, 1, 2, 3})
	for index, question := range questions {
		if question.ID != fmt.Sprintf("admission_%d", index) {
			t.Errorf("question %d id = %q", index, question.ID)
		}
		if question.Type != decision.TypeChoice || len(question.Criteria) != len(relationPriority) {
			t.Errorf("question %d is not a four-way choice", index)
		}
	}
}
