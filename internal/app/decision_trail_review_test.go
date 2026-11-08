package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func confirmedSetup(t *testing.T) (*decisionEnv, string) {
	t.Helper()
	env := newDecisionEnv(t)
	env.setNote(t, "note:one", "suggestion", "最终确定采用方案 A")
	env.removeNote(t, "note:two")
	id := candidateID(t, env.topic(t), "方案 A")
	return env, id
}

func TestDecisionReviewIsIdempotent(t *testing.T) {
	env, id := confirmedSetup(t)
	req := DecisionReview{ExpectedVersion: trailVersion(t, env), CandidateID: id, Action: "confirm", RequestID: "req-idem"}
	env.review(t, req)
	env.review(t, req)
	if got := env.eventCount(t, "decision.review"); got != 1 {
		t.Fatalf("review events=%d want 1", got)
	}
	// A different payload under the same request id is a conflict, not a
	// silent overwrite.
	req.Action = "reject"
	if err := env.reviewErr(t, req); !errors.Is(err, ErrDecisionRequestConflict) {
		t.Fatalf("conflicting replay err=%v", err)
	}
}

func TestDecisionReviewRecordsActorAndNote(t *testing.T) {
	env, id := confirmedSetup(t)
	trail := env.review(t, DecisionReview{ExpectedVersion: trailVersion(t, env), CandidateID: id, Action: "confirm", RequestID: "req-actor", Reviewer: "carol", Note: "客户签回"})
	for _, record := range onlyTopic(t, trail).History {
		if record.ReviewStatus == "accepted" {
			if record.Reviewer != "carol" || record.ReviewNote != "客户签回" || record.ConfirmedAt == nil {
				t.Fatalf("review=%+v", record)
			}
			return
		}
	}
	t.Fatal("accepted review missing")
}

func TestDecisionReviewRejectsStaleVersion(t *testing.T) {
	env, id := confirmedSetup(t)
	err := env.reviewErr(t, DecisionReview{ExpectedVersion: "wl-stale", CandidateID: id, Action: "confirm", RequestID: "req-stale"})
	if !errors.Is(err, ErrDecisionVersionConflict) {
		t.Fatalf("err=%v want version conflict", err)
	}
	if got := env.eventCount(t, "decision.review"); got != 0 {
		t.Fatalf("stale review wrote %d events", got)
	}
}

func TestDecisionTrailDoesNotInferValidFrom(t *testing.T) {
	env, id := confirmedSetup(t)
	trail := env.review(t, DecisionReview{ExpectedVersion: trailVersion(t, env), CandidateID: id, Action: "confirm", RequestID: "req-no-time"})
	current := onlyTopic(t, trail).Current
	if current == nil || current.ValidFrom != nil {
		t.Fatalf("valid_from was inferred: %+v", current)
	}
	if current.RecordedAt == nil || current.ConfirmedAt == nil {
		t.Fatalf("recorded/confirmed timestamps missing: %+v", current)
	}
}

func TestDecisionTrailOrdersByDeclaredTimeBasis(t *testing.T) {
	env, id := confirmedSetup(t)
	early := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	late := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	env.review(t, DecisionReview{ExpectedVersion: trailVersion(t, env), CandidateID: id, Action: "confirm", RequestID: "req-late", ValidFrom: &late})
	env.review(t, DecisionReview{ExpectedVersion: trailVersion(t, env), CandidateID: id, Action: "reopen", RequestID: "req-open"})
	trail := env.review(t, DecisionReview{ExpectedVersion: trailVersion(t, env), CandidateID: id, Action: "confirm", RequestID: "req-early", ValidFrom: &early})
	var got []time.Time
	for _, record := range onlyTopic(t, trail).History {
		if record.ValidFrom != nil {
			got = append(got, *record.ValidFrom)
		}
	}
	if len(got) != 2 || !got[0].Equal(early) || !got[1].Equal(late) {
		t.Fatalf("order=%v", got)
	}
}

func TestDecisionTrailInvalidatesStaleSource(t *testing.T) {
	env, id := confirmedSetup(t)
	env.review(t, DecisionReview{ExpectedVersion: trailVersion(t, env), CandidateID: id, Action: "confirm", RequestID: "req-stale-source"})
	env.supersedeSegment(t)
	topic := env.topic(t)
	if topic.Current != nil || topic.CurrentState != DecisionUnresolved {
		t.Fatalf("topic=%+v", topic)
	}
	found := false
	for _, record := range topic.History {
		if record.State == DecisionUnresolved && decisionContains(record.UnresolvedReasons, "source_invalidated") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no invalidated record: %+v", topic.History)
	}
}

func TestDecisionTrailSkipsPurgedEvents(t *testing.T) {
	env, id := confirmedSetup(t)
	env.review(t, DecisionReview{ExpectedVersion: trailVersion(t, env), CandidateID: id, Action: "confirm", RequestID: "req-purge"})
	if err := env.ds.Time.PurgeContent(context.Background(), []string{
		"decision-review-" + shortHash(env.scope+"\x00req-purge"),
	}); err != nil {
		t.Fatal(err)
	}
	trail := env.build(t)
	for _, topic := range trail.Topics {
		for _, record := range topic.History {
			if record.SourceKind == "review" && strings.Contains(record.Text, "方案 A") {
				t.Fatalf("purged review resurfaced: %+v", record)
			}
		}
		if topic.CurrentState == DecisionConfirmed {
			t.Fatalf("purged confirmation is current: %+v", topic)
		}
	}
}
