package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"recordingmcp/internal/contextbrief"
	"recordingmcp/internal/memory"
)

func TestBriefPolishRejectsForgetDuringModelCall(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	req := contextbrief.Request{Scope: contextbrief.Scope{Kind: "conversation", ConversationIDs: []string{f.conversationID}}}
	base, err := f.svc.BuildContextBrief(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	f.svc.BriefPolisher = testPolisher(func(_ context.Context, _ contextbrief.Result) ([]contextbrief.PolishCandidate, error) {
		forget := memory.ForgetRequest{Scope: "recording", RecordingID: f.recordingID}
		preview, e := f.svc.PreviewForget(ctx, forget)
		if e != nil {
			return nil, e
		}
		forget.ExpectedState = preview.ExpectedState
		if _, e = f.svc.CommitForget(ctx, forget); e != nil {
			return nil, e
		}
		return nil, nil
	})
	out, err := f.svc.PolishContextBrief(ctx, BriefPolishRequest{Request: req, SnapshotID: base.SnapshotID, Consent: true})
	var appErr *Error
	if !errors.As(err, &appErr) || appErr.Status != 409 || appErr.Code != "snapshot_changed" || len(out.Candidates) != 0 {
		t.Fatalf("forget during polish returned candidates or wrong error: %+v, %v", out, err)
	}
}

func TestBriefPolishDoesNotCrossProfilesWithIdenticalSourceIDs(t *testing.T) {
	ctx := context.Background()
	first, second := newFixture(t), newFixture(t)
	if first.conversationID != second.conversationID || first.recordingID != second.recordingID {
		t.Fatal("fixtures must have identical synthetic source IDs")
	}
	turn := first.turnRowID(t, "seg-a", "t-1")
	source, err := first.store.TranscriptSourceForTurn(ctx, turn)
	if err != nil {
		t.Fatal(err)
	}
	private := "SYNTHETIC_PRIVATE_PROFILE_ALPHA_HIGHLIGHT"
	start, end := int64(1000), int64(2000)
	_, err = first.svc.CreateHighlight(ctx, memory.HighlightCreate{RequestID: "polish-profile-test", RecordingID: first.recordingID,
		AnchorKind: "turn", StartMS: &start, EndMS: &end, TurnID: turn, SourceAttachmentID: source.SourceAttachmentID,
		TranscriptRevision: source.Result.Source.TranscriptRevision, Tag: "important", NoteText: private})
	if err != nil {
		t.Fatal(err)
	}
	req := contextbrief.Request{Scope: contextbrief.Scope{Kind: "conversation", ConversationIDs: []string{second.conversationID}}}
	base, err := second.svc.BuildContextBrief(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	second.svc.BriefPolisher = testPolisher(func(_ context.Context, received contextbrief.Result) ([]contextbrief.PolishCandidate, error) {
		called = true
		encoded, _ := json.Marshal(received)
		if strings.Contains(string(encoded), private) {
			t.Fatal("another profile's private highlight reached the polisher")
		}
		return nil, nil
	})
	out, err := second.svc.PolishContextBrief(ctx, BriefPolishRequest{Request: req, SnapshotID: base.SnapshotID, Consent: true})
	if err != nil || !called || out.Degraded || len(out.Candidates) != 0 {
		t.Fatalf("cross-profile polish result=%+v called=%v err=%v", out, called, err)
	}
}

func TestBriefPolishRejectsInvalidAdapterOutputAndKeepsSourceBaseIsolated(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	req := contextbrief.Request{Scope: contextbrief.Scope{Kind: "conversation", ConversationIDs: []string{f.conversationID}}}
	before, err := f.svc.BuildContextBrief(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	f.svc.BriefPolisher = testPolisher(func(_ context.Context, received contextbrief.Result) ([]contextbrief.PolishCandidate, error) {
		// An adapter is arbitrary code. Mutating nested input must not affect the
		// deterministic source snapshot held by the service.
		received.Sections.Background[0].Text = "adapter mutation"
		received.Sections.Background[0].EvidenceIDs[0] = "invented-evidence"
		return []contextbrief.PolishCandidate{{EntryID: "invented-entry", Text: "untrusted output", EvidenceIDs: []string{"invented-evidence"}, AssertionState: "proposed"}}, nil
	})
	out, err := f.svc.PolishContextBrief(ctx, BriefPolishRequest{Request: req, SnapshotID: before.SnapshotID, Consent: true})
	if err != nil || !out.Degraded || len(out.Candidates) != 0 || out.Reason != "polish_unavailable" {
		t.Fatalf("invalid adapter output accepted: %+v, %v", out, err)
	}
	after, err := f.svc.BuildContextBrief(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if after.SnapshotID != before.SnapshotID {
		t.Fatalf("adapter mutation changed snapshot: before=%s after=%s", before.SnapshotID, after.SnapshotID)
	}
	beforeJSON, _ := json.Marshal(before)
	afterJSON, _ := json.Marshal(after)
	if string(beforeJSON) != string(afterJSON) || strings.Contains(string(afterJSON), "adapter mutation") || strings.Contains(string(afterJSON), "invented-evidence") {
		t.Fatal("adapter mutation leaked into deterministic brief")
	}
}
