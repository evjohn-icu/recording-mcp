package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestDecisionTrailScenarioA covers a single conversation that moves from
// observation to a confirmed decision and then to a superseding decision.
func TestDecisionTrailScenarioA(t *testing.T) {
	t.Run("observations_and_proposals_are_not_confirmed", func(t *testing.T) {
		env := newDecisionEnv(t)
		env.setNote(t, "note:one", "observation", "客户要求 6 月前上线")
		env.setNote(t, "note:two", "suggestion", "建议先做原型")
		// Proposal-only topics are not part of the default decision list, so
		// select the topic explicitly.
		topics, _, err := env.wl.Snapshot(context.Background(), env.scope)
		if err != nil || len(topics) != 1 {
			t.Fatalf("snapshot: %v %d", err, len(topics))
		}
		trail, err := env.ds.Build(context.Background(), env.scope, DecisionQuery{TopicID: topics[0].ID, IncludeHistory: true})
		if err != nil {
			t.Fatal(err)
		}
		topic := onlyTopic(t, trail)
		if topic.Counts.Observations != 1 || topic.Counts.Proposals != 1 {
			t.Fatalf("counts=%+v", topic.Counts)
		}
		if topic.Counts.Confirmed != 0 || topic.CurrentState != DecisionProposal || topic.Current != nil {
			t.Fatalf("topic=%+v", topic)
		}
		if env.eventCount(t, "decision.review") != 0 {
			t.Fatal("projection wrote a review event")
		}
	})

	t.Run("candidate_from_decision_cue", func(t *testing.T) {
		env := newDecisionEnv(t)
		env.setNote(t, "note:one", "suggestion", "最终确定采用方案 A")
		env.removeNote(t, "note:two")
		topic := env.topic(t)
		if topic.Counts.Candidates != 1 || topic.CurrentState != DecisionCandidate {
			t.Fatalf("topic=%+v", topic)
		}
		var candidate DecisionRecord
		for _, record := range topic.History {
			if record.State == DecisionCandidate {
				candidate = record
			}
		}
		if candidate.CandidateSource != "rule-v1" || candidate.ValidFrom != nil {
			t.Fatalf("candidate=%+v", candidate)
		}
	})

	t.Run("confirmed_after_review", func(t *testing.T) {
		env := newDecisionEnv(t)
		env.setNote(t, "note:one", "suggestion", "最终确定采用方案 A")
		env.removeNote(t, "note:two")
		topic := env.topic(t)
		id := candidateID(t, topic, "方案 A")
		trail := env.review(t, DecisionReview{
			ExpectedVersion: trailVersion(t, env), CandidateID: id, Action: "confirm",
			RequestID: "req-confirm-a", Reviewer: "alice", Note: "会上确认",
		})
		updated := onlyTopic(t, trail)
		if updated.CurrentState != DecisionConfirmed || updated.Current == nil {
			t.Fatalf("topic=%+v", updated)
		}
		if updated.Current.Reviewer != "alice" || updated.Current.ReviewNote != "会上确认" || updated.Current.ReviewStatus != "accepted" {
			t.Fatalf("current=%+v", updated.Current)
		}
	})

	t.Run("history_retains_superseded_decision", func(t *testing.T) {
		env := newDecisionEnv(t)
		env.setNote(t, "note:one", "suggestion", "最终确定采用方案 A")
		env.removeNote(t, "note:two")
		idA := candidateID(t, env.topic(t), "方案 A")
		env.review(t, DecisionReview{ExpectedVersion: trailVersion(t, env), CandidateID: idA, Action: "confirm", RequestID: "req-a"})

		env.setNote(t, "note:one", "suggestion", "最终确定采用方案 B")
		idB := candidateID(t, env.topic(t), "方案 B")
		env.review(t, DecisionReview{ExpectedVersion: trailVersion(t, env), CandidateID: idB, Action: "confirm", RequestID: "req-b"})

		topic := env.topic(t)
		counts := topic.Counts
		if counts.Confirmed != 1 || counts.Superseded != 1 {
			t.Fatalf("counts=%+v", counts)
		}
		foundSuperseded := false
		for _, record := range topic.History {
			if record.State == DecisionSuperseded {
				foundSuperseded = true
				if record.SupersededBy == "" {
					t.Fatalf("superseded record has no successor: %+v", record)
				}
			}
		}
		if !foundSuperseded {
			t.Fatalf("no superseded record: %+v", topic.History)
		}
	})

	t.Run("current_returns_latest_confirmed", func(t *testing.T) {
		env := newDecisionEnv(t)
		env.setNote(t, "note:one", "suggestion", "最终确定采用方案 A")
		env.removeNote(t, "note:two")
		env.review(t, DecisionReview{ExpectedVersion: trailVersion(t, env), CandidateID: candidateID(t, env.topic(t), "方案 A"), Action: "confirm", RequestID: "req-a"})
		env.setNote(t, "note:one", "suggestion", "最终确定采用方案 B")
		env.review(t, DecisionReview{ExpectedVersion: trailVersion(t, env), CandidateID: candidateID(t, env.topic(t), "方案 B"), Action: "confirm", RequestID: "req-b"})
		topic := env.topic(t)
		if topic.Current == nil || !strings.Contains(topic.Current.Text, "方案 B") || topic.CurrentState != DecisionConfirmed {
			t.Fatalf("current=%+v state=%s", topic.Current, topic.CurrentState)
		}
	})

	t.Run("evidence_resolves_to_source_revision", func(t *testing.T) {
		env := newDecisionEnv(t)
		env.setNote(t, "note:one", "suggestion", "最终确定采用方案 A")
		env.removeNote(t, "note:two")
		env.review(t, DecisionReview{ExpectedVersion: trailVersion(t, env), CandidateID: candidateID(t, env.topic(t), "方案 A"), Action: "confirm", RequestID: "req-a"})
		current := env.topic(t).Current
		if current == nil || len(current.Evidence) == 0 {
			t.Fatalf("current=%+v", current)
		}
		ev := current.Evidence[0]
		if ev.SourceStatus != "current" || ev.Revision != "rev-1" || ev.TurnID == "" || ev.ConversationID != env.conversationID {
			t.Fatalf("evidence=%+v", ev)
		}
	})

	t.Run("time_order_uses_valid_from_when_known", func(t *testing.T) {
		env := newDecisionEnv(t)
		env.setNote(t, "note:one", "suggestion", "最终确定采用方案 A")
		env.removeNote(t, "note:two")
		id := candidateID(t, env.topic(t), "方案 A")
		early := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
		late := time.Date(2026, 2, 5, 0, 0, 0, 0, time.UTC)
		env.review(t, DecisionReview{ExpectedVersion: trailVersion(t, env), CandidateID: id, Action: "confirm", RequestID: "req-late", ValidFrom: &late})
		// Re-open and confirm again with an earlier declared validity to prove
		// the projection orders by valid_from rather than by insertion time.
		env.review(t, DecisionReview{ExpectedVersion: trailVersion(t, env), CandidateID: id, Action: "reopen", RequestID: "req-reopen"})
		env.review(t, DecisionReview{ExpectedVersion: trailVersion(t, env), CandidateID: id, Action: "confirm", RequestID: "req-early", ValidFrom: &early})

		topic := env.topic(t)
		var confirmed ValidFroms
		for _, record := range topic.History {
			if record.ReviewStatus == "accepted" && record.ValidFrom != nil {
				confirmed = append(confirmed, *record.ValidFrom)
			}
		}
		if len(confirmed) != 2 || !confirmed[0].Equal(early) || !confirmed[1].Equal(late) {
			t.Fatalf("valid_from order=%v want [%v %v]", confirmed, early, late)
		}
	})

	t.Run("review_replay_is_idempotent", func(t *testing.T) {
		env := newDecisionEnv(t)
		env.setNote(t, "note:one", "suggestion", "最终确定采用方案 A")
		env.removeNote(t, "note:two")
		id := candidateID(t, env.topic(t), "方案 A")
		req := DecisionReview{ExpectedVersion: trailVersion(t, env), CandidateID: id, Action: "confirm", RequestID: "req-replay"}
		first := env.review(t, req)
		second := env.review(t, req)
		if env.eventCount(t, "decision.review") != 1 {
			t.Fatalf("reviews=%d want 1", env.eventCount(t, "decision.review"))
		}
		a, _ := json.Marshal(first)
		b, _ := json.Marshal(second)
		if string(a) != string(b) {
			t.Fatal("replayed review returned a different trail")
		}
	})
}

// ValidFroms is a tiny sortable slice used by the ordering assertion.
type ValidFroms []time.Time

func trailVersion(t *testing.T, env *decisionEnv) string {
	t.Helper()
	trail := env.build(t)
	if trail.Version == "" {
		t.Fatal("empty projection version")
	}
	return trail.Version
}

func onlyTopic(t *testing.T, trail DecisionTrail) DecisionTopic {
	t.Helper()
	if len(trail.Topics) != 1 {
		t.Fatalf("topics=%d want 1", len(trail.Topics))
	}
	return trail.Topics[0]
}

// TestDecisionTrailScenarioB covers conflicting candidates and the
// reject/reopen loop.
func TestDecisionTrailScenarioB(t *testing.T) {
	setup := func(t *testing.T) *decisionEnv {
		env := newDecisionEnv(t)
		env.setNote(t, "note:one", "suggestion", "采用方案 A 快速上线")
		env.setNote(t, "note:two", "suggestion", "采用方案 B 更稳")
		return env
	}

	t.Run("conflicting_candidates_are_unresolved", func(t *testing.T) {
		env := setup(t)
		topic := env.topic(t)
		if topic.CurrentState != DecisionUnresolved || topic.Current != nil {
			t.Fatalf("topic=%+v", topic)
		}
		if !decisionContains(topic.UnresolvedReasons, "multiple_candidates") {
			t.Fatalf("reasons=%v", topic.UnresolvedReasons)
		}
		if topic.Counts.Candidates != 2 {
			t.Fatalf("counts=%+v", topic.Counts)
		}
	})

	t.Run("discussion_does_not_auto_confirm", func(t *testing.T) {
		env := setup(t)
		topic := env.topic(t)
		if topic.Counts.Confirmed != 0 || env.eventCount(t, "decision.review") != 0 {
			t.Fatalf("discussion created a confirmation: %+v", topic.Counts)
		}
	})

	t.Run("reject_records_reviewer_action", func(t *testing.T) {
		env := setup(t)
		idA := candidateID(t, env.topic(t), "方案 A")
		trail := env.review(t, DecisionReview{ExpectedVersion: trailVersion(t, env), CandidateID: idA, Action: "reject", RequestID: "req-reject", Reviewer: "bob", Note: "成本过高"})
		topic := onlyTopic(t, trail)
		if topic.Current != nil {
			t.Fatalf("reject produced a current decision: %+v", topic.Current)
		}
		found := false
		for _, record := range topic.History {
			if record.ReviewStatus == "rejected" && record.CandidateID == idA {
				found = true
				if record.Reviewer != "bob" || record.ReviewNote != "成本过高" {
					t.Fatalf("review=%+v", record)
				}
			}
		}
		if !found {
			t.Fatalf("rejected review missing: %+v", topic.History)
		}
	})

	t.Run("reopen_returns_to_unresolved", func(t *testing.T) {
		env := setup(t)
		idA := candidateID(t, env.topic(t), "方案 A")
		env.review(t, DecisionReview{ExpectedVersion: trailVersion(t, env), CandidateID: idA, Action: "reject", RequestID: "req-reject"})
		trail := env.review(t, DecisionReview{ExpectedVersion: trailVersion(t, env), CandidateID: idA, Action: "reopen", RequestID: "req-reopen"})
		topic := onlyTopic(t, trail)
		if topic.CurrentState != DecisionUnresolved || topic.Current != nil {
			t.Fatalf("topic=%+v", topic)
		}
		reopened := false
		for _, record := range topic.History {
			if record.ReviewStatus == "reopened" {
				reopened = true
			}
		}
		if !reopened {
			t.Fatalf("no reopened record: %+v", topic.History)
		}
	})

	t.Run("reopen_requires_new_confirmation", func(t *testing.T) {
		env := setup(t)
		idA := candidateID(t, env.topic(t), "方案 A")
		env.review(t, DecisionReview{ExpectedVersion: trailVersion(t, env), CandidateID: idA, Action: "confirm", RequestID: "req-confirm"})
		env.review(t, DecisionReview{ExpectedVersion: trailVersion(t, env), CandidateID: idA, Action: "reopen", RequestID: "req-reopen"})
		topic := env.topic(t)
		if topic.Current != nil || topic.CurrentState != DecisionUnresolved {
			t.Fatalf("reopen kept the decision confirmed: %+v", topic)
		}
	})

	t.Run("history_preserves_both_candidates", func(t *testing.T) {
		env := setup(t)
		topic := env.topic(t)
		texts := []string{}
		for _, record := range topic.History {
			if record.State == DecisionCandidate {
				texts = append(texts, record.Text)
			}
		}
		if len(texts) != 2 || !strings.Contains(strings.Join(texts, "|"), "方案 A") || !strings.Contains(strings.Join(texts, "|"), "方案 B") {
			t.Fatalf("candidates=%v", texts)
		}
	})

	t.Run("current_is_nil_without_final_adjudication", func(t *testing.T) {
		env := setup(t)
		idA := candidateID(t, env.topic(t), "方案 A")
		env.review(t, DecisionReview{ExpectedVersion: trailVersion(t, env), CandidateID: idA, Action: "reject", RequestID: "req-reject"})
		if got := env.topic(t).Current; got != nil {
			t.Fatalf("current=%+v", got)
		}
	})

	t.Run("repeated_reject_does_not_duplicate_state", func(t *testing.T) {
		env := setup(t)
		idA := candidateID(t, env.topic(t), "方案 A")
		env.review(t, DecisionReview{ExpectedVersion: trailVersion(t, env), CandidateID: idA, Action: "reject", RequestID: "req-1"})
		env.review(t, DecisionReview{ExpectedVersion: trailVersion(t, env), CandidateID: idA, Action: "reject", RequestID: "req-2"})
		if got := env.eventCount(t, "decision.review"); got != 1 {
			t.Fatalf("reviews=%d want 1", got)
		}
		if got := env.topic(t).Counts.Unresolved; got != 1 {
			t.Fatalf("unresolved=%d", got)
		}
	})
}

// TestDecisionTrailScenarioC covers stale revisions, corrections and purged
// content.
func TestDecisionTrailScenarioC(t *testing.T) {
	setupConfirmed := func(t *testing.T) (*decisionEnv, string) {
		env := newDecisionEnv(t)
		env.setNote(t, "note:one", "suggestion", "最终确定采用方案 A")
		env.removeNote(t, "note:two")
		id := candidateID(t, env.topic(t), "方案 A")
		env.review(t, DecisionReview{ExpectedVersion: trailVersion(t, env), CandidateID: id, Action: "confirm", RequestID: "req-confirm"})
		return env, id
	}

	t.Run("stale_revision_invalidates_confirmed", func(t *testing.T) {
		env, _ := setupConfirmed(t)
		env.supersedeSegment(t)
		topic := env.topic(t)
		if topic.Current != nil || topic.CurrentState != DecisionUnresolved {
			t.Fatalf("stale confirmation still current: %+v", topic)
		}
		if !decisionContains(topic.UnresolvedReasons, "latest_confirmation_source_invalidated") {
			t.Fatalf("reasons=%v", topic.UnresolvedReasons)
		}
	})

	t.Run("stale_decision_not_in_current", func(t *testing.T) {
		env, _ := setupConfirmed(t)
		env.supersedeSegment(t)
		if current := env.topic(t).Current; current != nil {
			t.Fatalf("current=%+v", current)
		}
	})

	t.Run("history_marks_source_invalidated", func(t *testing.T) {
		env, _ := setupConfirmed(t)
		env.supersedeSegment(t)
		topic := env.topic(t)
		found := false
		for _, record := range topic.History {
			if record.State != DecisionUnresolved {
				continue
			}
			found = true
			if !decisionContains(record.UnresolvedReasons, "source_invalidated") {
				t.Fatalf("reasons=%v", record.UnresolvedReasons)
			}
			for _, ev := range record.Evidence {
				if ev.SourceStatus != "stale" {
					t.Fatalf("evidence status=%s", ev.SourceStatus)
				}
			}
		}
		if !found {
			t.Fatalf("no invalidated record: %+v", topic.History)
		}
	})

	t.Run("corrected_revision_creates_new_candidate", func(t *testing.T) {
		env, _ := setupConfirmed(t)
		env.supersedeSegment(t)
		env.applyCorrectedNote(t, "rev-2", "note:corrected-a", "最终确定采用方案 A（修订）")
		topic := env.topic(t)
		if topic.Counts.Candidates == 0 && topic.Counts.Confirmed == 0 {
			t.Fatalf("no candidate after correction: %+v", topic)
		}
	})

	t.Run("forgotten_source_removes_event_content", func(t *testing.T) {
		env, _ := setupConfirmed(t)
		if err := env.ds.Time.PurgeContent(context.Background(), []string{
			"decision-review-" + shortHash(env.scope+"\x00req-confirm"),
		}); err != nil {
			t.Fatal(err)
		}
		topic := env.topic(t)
		if topic.Current != nil {
			t.Fatalf("purged confirmation still current: %+v", topic.Current)
		}
		for _, record := range topic.History {
			if record.SourceKind == "review" && strings.Contains(record.Text, "方案") {
				t.Fatalf("purged review content resurrected: %+v", record)
			}
		}
	})

	t.Run("forgotten_decision_not_resurrected", func(t *testing.T) {
		env, _ := setupConfirmed(t)
		if err := env.ds.Time.PurgeContent(context.Background(), []string{
			"decision-review-" + shortHash(env.scope+"\x00req-confirm"),
		}); err != nil {
			t.Fatal(err)
		}
		again := env.build(t)
		for _, topic := range again.Topics {
			if topic.CurrentState == DecisionConfirmed || topic.Current != nil {
				t.Fatalf("forgotten decision resurrected: %+v", topic)
			}
		}
	})

	t.Run("superseded_by_corrected_confirmation", func(t *testing.T) {
		env, _ := setupConfirmed(t)
		env.supersedeSegment(t)
		env.applyCorrectedNote(t, "rev-2", "note:corrected-b", "最终确定采用方案 B（修订）")
		id := candidateID(t, env.topic(t), "方案 B")
		env.review(t, DecisionReview{ExpectedVersion: trailVersion(t, env), CandidateID: id, Action: "confirm", RequestID: "req-corrected"})
		topic := env.topic(t)
		if topic.Current == nil || !strings.Contains(topic.Current.Text, "方案 B") || topic.CurrentState != DecisionConfirmed {
			t.Fatalf("topic=%+v", topic)
		}
	})

	t.Run("evidence_refs_never_point_to_inactive_attachment", func(t *testing.T) {
		env, _ := setupConfirmed(t)
		env.supersedeSegment(t)
		topic := env.topic(t)
		for _, record := range topic.History {
			for _, ev := range record.Evidence {
				if ev.Revision == "rev-1" && ev.SourceStatus == "current" && record.State == DecisionUnresolved {
					t.Fatalf("inactive revision reported current: %+v", ev)
				}
			}
		}
	})
}

func decisionContains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
