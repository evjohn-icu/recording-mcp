package app

import (
	"context"
	"testing"

	"recordingmcp/internal/memory"
)

// TestForgottenTurnIsNotSearchableOrPlayable proves a committed turn-scope
// forget removes the turn from retrieval and from the audio resolver.
func TestForgottenTurnIsNotSearchableOrPlayable(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	turnID := f.turnRowID(t, "seg-a", "t-1")

	before, err := f.svc.Query(ctx, memory.Query{Query: "第一条原始转写", IncludeTurns: true, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Items) == 0 {
		t.Fatal("fixture turn is not searchable before forget")
	}

	req := memory.ForgetRequest{Scope: "turn", ConversationID: f.conversationID, TurnID: turnID}
	preview, err := f.svc.PreviewForget(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.CommitForget(ctx, preview.Request); err != nil {
		t.Fatalf("forget: %v", err)
	}

	after, err := f.svc.Query(ctx, memory.Query{Query: "第一条原始转写", IncludeTurns: true, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range after.Items {
		if item.ID == turnID {
			t.Fatalf("forgotten turn is still searchable: %+v", item)
		}
	}
	if _, err := f.svc.AudioClip(ctx, turnID); err == nil {
		t.Fatal("forgotten turn still resolves to audio")
	}
}
