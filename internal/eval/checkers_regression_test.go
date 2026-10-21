package eval

import (
	"context"
	"testing"

	"recordingmcp/internal/asrinput"
	"recordingmcp/internal/contextbuild"
	"recordingmcp/internal/memory"
)

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
