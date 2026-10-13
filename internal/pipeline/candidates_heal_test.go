package pipeline

import (
	"context"
	"testing"
	"time"
)

// TestValidatedInsertHealsRejectedCandidate pins the heal: identical content
// first rejected (its evidence fell outside the producing window) is replaced
// when a later window validates it, and a rejected re-insert never demotes a
// validated row. candidateID is content-derived, so without the status-aware
// upsert the rejected row would permanently block the validated one. Real
// conversation/job parents are required: memory_candidates carries FK
// constraints (observed as SQLite error 787 with fabricated ids).
func TestValidatedInsertHealsRejectedCandidate(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now()
	start := int64(1000)
	seedConversation(t, db, "conv-heal", []struct {
		id      string
		seg     int
		start   *int64
		orig    int
		speaker *string
	}{{id: "t:u3", seg: 0, start: &start, orig: 1, speaker: nil}})

	draft := CandidateDraft{
		Kind:            "observation",
		Summary:         "会议室投影仪损坏需要报修",
		TopicTitle:      "设备报修",
		EvidenceTurnIDs: []string{"t:u3"},
	}
	rejected := ValidatedDraft{
		CandidateDraft:  draft,
		Status:          "rejected",
		RejectionReason: `evidence turn id "t:u3" not in window`,
	}
	validatedDraft := ValidatedDraft{CandidateDraft: draft, Status: "validated"}

	// Window 1 rejects the draft.
	job1 := enqueueExtractWithPayload(t, db, `{"conversation_id":"conv-heal"}`, "extract:heal:1")
	if _, err := InsertCandidates(ctx, db, "conv-heal", "run:1", job1.ID, "stub",
		[]ValidatedDraft{rejected}, now); err != nil {
		t.Fatalf("insert rejected: %v", err)
	}
	validated, err := ListCandidates(ctx, db, "conv-heal", "validated")
	if err != nil {
		t.Fatal(err)
	}
	if len(validated) != 0 {
		t.Fatalf("validated candidates before heal = %d, want 0", len(validated))
	}

	// Window 2 validates the identical content: the rejected row must heal.
	job2 := enqueueExtractWithPayload(t, db, `{"conversation_id":"conv-heal"}`, "extract:heal:2")
	if _, err := InsertCandidates(ctx, db, "conv-heal", "run:2", job2.ID, "stub",
		[]ValidatedDraft{validatedDraft}, now.Add(time.Second)); err != nil {
		t.Fatalf("insert validated: %v", err)
	}
	validated, err = ListCandidates(ctx, db, "conv-heal", "validated")
	if err != nil {
		t.Fatal(err)
	}
	if len(validated) != 1 {
		t.Fatalf("validated candidates after heal = %d, want 1", len(validated))
	}
	if validated[0].Status != "validated" || validated[0].RejectionReason != "" {
		t.Fatalf("healed candidate = %+v, want validated with empty rejection_reason", validated[0])
	}
	if validated[0].RunID != "run:2" || validated[0].JobID != job2.ID {
		t.Fatalf("healed provenance = %s/%s, want the validating window run:2/%s",
			validated[0].RunID, validated[0].JobID, job2.ID)
	}

	// A later rejected re-insert of the same content must not demote it.
	job3 := enqueueExtractWithPayload(t, db, `{"conversation_id":"conv-heal"}`, "extract:heal:3")
	if _, err := InsertCandidates(ctx, db, "conv-heal", "run:3", job3.ID, "stub",
		[]ValidatedDraft{rejected}, now.Add(2*time.Second)); err != nil {
		t.Fatalf("re-insert rejected: %v", err)
	}
	validated, err = ListCandidates(ctx, db, "conv-heal", "validated")
	if err != nil {
		t.Fatal(err)
	}
	rejectedRows, err := ListCandidates(ctx, db, "conv-heal", "rejected")
	if err != nil {
		t.Fatal(err)
	}
	if len(validated) != 1 || len(rejectedRows) != 0 {
		t.Fatalf("after rejected re-insert: validated=%d rejected=%d, want 1/0 (validated row intact)",
			len(validated), len(rejectedRows))
	}
}
