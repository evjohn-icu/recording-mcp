package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"recordingmcp/internal/decision"
	"recordingmcp/internal/plane"
)

func TestWorldlineSemanticQuestionAllowsAbstention(t *testing.T) {
	q := worldlineSemanticQuestion("pair-1")
	if q.ID != "pair-1" {
		t.Fatalf("question ID = %q", q.ID)
	}
	if _, ok := q.Criteria["insufficient_evidence"]; !ok {
		t.Fatal("semantic criteria does not allow abstention")
	}
	if !strings.Contains(q.Instructions, "do not turn uncertainty into complementary_context") {
		t.Fatalf("prompt does not preserve abstention: %q", q.Instructions)
	}
	if err := validateWorldlineSemanticAnswer(decision.Answer{Type: decision.TypeChoice, Choice: "insufficient_evidence", Confidence: .8, Distribution: map[string]float64{
		"text_similarity": .05, "complementary_context": .05, "review_required": .1, "unrelated": .1, "insufficient_evidence": .7,
	}}, q.Criteria); err != nil {
		t.Fatalf("valid abstention rejected: %v", err)
	}
}

func TestSemanticAppendPurgesEventsWhenSourcesChangeBeforePostcheck(t *testing.T) {
	events := []plane.Event{{ID: "suggestion-a"}, {ID: "suggestion-b"}}
	var purged []string
	verifyErr := errors.New("source revision was forgotten")
	err := appendSemanticEventsWithCheck(context.Background(), events,
		func(context.Context, []plane.Event) error { return nil },
		func(context.Context) error { return verifyErr },
		func(_ context.Context, ids []string) error { purged = append([]string{}, ids...); return nil })
	if !errors.Is(err, verifyErr) {
		t.Fatalf("error = %v, want source validation error", err)
	}
	if len(purged) != 2 || purged[0] != "suggestion-a" || purged[1] != "suggestion-b" {
		t.Fatalf("newly appended semantic events were not targeted for purge: %v", purged)
	}
}

func TestSemanticAppendReportsPurgeFailure(t *testing.T) {
	verifyErr := errors.New("source revision changed")
	purgeErr := errors.New("event store unavailable")
	err := appendSemanticEventsWithCheck(context.Background(), []plane.Event{{ID: "suggestion"}},
		func(context.Context, []plane.Event) error { return nil },
		func(context.Context) error { return verifyErr },
		func(context.Context, []string) error { return purgeErr })
	if !errors.Is(err, purgeErr) || !strings.Contains(err.Error(), verifyErr.Error()) {
		t.Fatalf("error should report both source race and failed cleanup: %v", err)
	}
}

func TestWorldlineSemanticAnswerRejectsMalformedDistribution(t *testing.T) {
	q := worldlineSemanticQuestion("pair-2")
	answer := decision.Answer{Type: decision.TypeChoice, Choice: "complementary_context", Confidence: .7, Distribution: map[string]float64{
		"text_similarity": .1, "complementary_context": .7, "review_required": .1, "unrelated": .1,
	}}
	if err := validateWorldlineSemanticAnswer(answer, q.Criteria); err == nil {
		t.Fatal("incomplete distribution was accepted")
	}
	answer.Distribution["insufficient_evidence"] = .2
	if err := validateWorldlineSemanticAnswer(answer, q.Criteria); err == nil {
		t.Fatal("non-normalized distribution was accepted")
	}
}

func TestSemanticPairEntryMarksClippedAndMissingEvidence(t *testing.T) {
	entry := WorldlineThreadEntry{ID: "entry", ClaimID: "claim", Summary: "summary", Evidence: []WorldlineEvidence{{TurnID: "turn", Revision: "rev", Text: strings.Repeat("界", 801)}}}
	pair := semanticPairEntry(entry)
	if !pair.ExcerptTruncated || len(pair.Sources) != 1 || !pair.Sources[0].Truncated {
		t.Fatalf("clipped excerpt not reported: %+v", pair)
	}
	if len(pair.SourceRefs) != 1 || pair.SourceRefs[0].TurnID != "turn" || pair.SourceRefs[0].Revision != "rev" {
		t.Fatalf("clipping lost full source reference: %+v", pair.SourceRefs)
	}
	empty := semanticPairEntry(WorldlineThreadEntry{ID: "empty", ClaimID: "claim-empty"})
	if len(empty.Sources) != 0 || len(empty.SourceRefs) != 0 {
		t.Fatalf("missing evidence unexpectedly synthesized: %+v", empty)
	}
}
