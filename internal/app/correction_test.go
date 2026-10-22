package app

import (
	"context"
	"testing"
)

func TestSpeakerReviewRequiresActiveRevisionEvidence(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	turnID := f.turnRowID(t, "seg-a", "t-1")
	label := "spk:device-1:1"
	if _, err := f.store.DB().Exec(`UPDATE turns SET speaker_scoped_id=? WHERE id=?`, label, turnID); err != nil {
		t.Fatal(err)
	}
	proposal, err := f.svc.ProposeSpeakerBinding(ctx, f.recordingID, label, "rev-1", []string{turnID}, "")
	if err != nil {
		t.Fatal(err)
	}
	confirmed, err := f.svc.ConfirmSpeakerBinding(ctx, proposal.ID, "person:alice", "local-web")
	if err != nil || confirmed.Status != "confirmed" {
		t.Fatalf("confirm = %+v, %v", confirmed, err)
	}
	if _, err := f.svc.CorrectTranscriptSegment(ctx, f.conversationID, f.recordingID, "seg-a", "rev-1", "rev-2", map[string]string{"t-1": "改正后的逐字稿", "t-2": "第二条原始转写"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.ProposeSpeakerBinding(ctx, f.recordingID, label, "rev-1", []string{turnID}, ""); err == nil {
		t.Fatal("proposal against a superseded revision was accepted")
	} else if appErr, ok := AsError(err); !ok || appErr.Status != 409 {
		t.Fatalf("stale proposal error = %v", err)
	}
	if _, err := f.svc.RevokeSpeakerBinding(ctx, confirmed.ID, "local-web", ""); err == nil {
		t.Fatal("review of superseded binding was accepted")
	} else if appErr, ok := AsError(err); !ok || appErr.Status != 409 {
		t.Fatalf("stale review error = %v", err)
	}
}
