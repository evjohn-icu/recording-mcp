package app

import (
	"context"
	"testing"
)

func TestSpeakerConfirmedTracksBindingRevocation(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	turnID := f.turnRowID(t, "seg-a", "t-1")
	label := "spk:device-1:1"
	if _, err := f.store.DB().Exec(`UPDATE turns SET speaker_scoped_id=? WHERE id=?`, label, turnID); err != nil {
		t.Fatal(err)
	}
	binding, err := f.store.ProposeSpeakerBinding(ctx, f.recordingID, label, "rev-1", []string{turnID}, "test-model")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.ConfirmSpeakerBinding(ctx, binding.ID, "person:alice", "reviewer"); err != nil {
		t.Fatal(err)
	}

	assertConfirmed := func(want bool) {
		t.Helper()
		page, err := f.svc.TurnPage(ctx, f.conversationID, f.recordingID, false, 0, 50)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, turn := range page.Items {
			if turn.ID == turnID {
				found = true
				if turn.SpeakerConfirmed != want {
					t.Errorf("TurnView SpeakerConfirmed = %v, want %v", turn.SpeakerConfirmed, want)
				}
			}
		}
		if !found {
			t.Fatalf("turn %s missing from TurnPage", turnID)
		}
		item, err := f.svc.GetWebItem(ctx, turnID)
		if err != nil {
			t.Fatal(err)
		}
		if item.Item.SpeakerConfirmed != want {
			t.Errorf("ItemView SpeakerConfirmed = %v, want %v", item.Item.SpeakerConfirmed, want)
		}
	}

	assertConfirmed(true)
	if _, err := f.store.RevokeSpeakerBinding(ctx, binding.ID, "reviewer", "identity withdrawn"); err != nil {
		t.Fatal(err)
	}
	assertConfirmed(false)
}
