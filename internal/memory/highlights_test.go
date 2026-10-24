package memory

import (
	"context"
	"errors"
	"testing"
)

func TestHighlightCreateReplayVersionAndDelete(t *testing.T) {
	ctx := context.Background()
	s := openTempStore(t)
	_, err := s.IngestTranscript(ctx, sampleResult("seg-highlight", "rev-1", 0))
	if err != nil { t.Fatal(err) }
	attachmentID := stableKey("attachment", "rec-1", "seg-highlight", "rev-1")
	start, end := int64(20), int64(80)
	input := HighlightCreate{RequestID:"req-highlight-1", RecordingID:"rec-1", AnchorKind:"range", StartMS:&start, EndMS:&end, SourceAttachmentID:attachmentID, TranscriptRevision:"rev-1", Tag:"important", NoteText:"keep this"}
	created, err := s.CreateHighlight(ctx, input)
	if err != nil { t.Fatal(err) }
	if created.State != "current" || created.Version != 1 || created.ConversationID != "meeting-1" { t.Fatalf("unexpected highlight: %+v", created) }
	replay, err := s.CreateHighlight(ctx, input)
	if err != nil || replay.ID != created.ID { t.Fatalf("same request did not replay: %+v %v", replay, err) }
	input.NoteText = "different payload"
	if _, err := s.CreateHighlight(ctx, input); !errors.Is(err, ErrHighlightRequestConflict) { t.Fatalf("changed request payload: %v", err) }
	if _, err := s.UpdateHighlight(ctx, created.ID, HighlightUpdate{ExpectedVersion:1, NoteText: stringPtr("updated")}); err != nil { t.Fatal(err) }
	if _, err := s.UpdateHighlight(ctx, created.ID, HighlightUpdate{ExpectedVersion:1, NoteText: stringPtr("lost update")}); !errors.Is(err, ErrHighlightVersionConflict) { t.Fatalf("stale version accepted: %v", err) }
	if err := s.DeleteHighlight(ctx, created.ID, 2); err != nil { t.Fatal(err) }
	if _, err := s.CreateHighlight(ctx, HighlightCreate{RequestID:"req-highlight-1", RecordingID:"rec-1", AnchorKind:"range", StartMS:&start, EndMS:&end, SourceAttachmentID:attachmentID, TranscriptRevision:"rev-1", Tag:"important", NoteText:"keep this"}); !errors.Is(err, ErrHighlightGone) { t.Fatalf("deleted request revived: %v", err) }
}

func TestHighlightRequiresCurrentMatchingEvidence(t *testing.T) {
	ctx := context.Background()
	s := openTempStore(t)
	_, err := s.IngestTranscript(ctx, sampleResult("seg-highlight", "rev-1", 0))
	if err != nil { t.Fatal(err) }
	attachmentID := stableKey("attachment", "rec-1", "seg-highlight", "rev-1")
	turnID := "seg-highlight/rev-1/utt-1"
	start, end := int64(20), int64(80)
	input := HighlightCreate{RequestID:"req-turn-highlight", RecordingID:"rec-1", AnchorKind:"turn", StartMS:&start, EndMS:&end, TurnID:turnID, SourceAttachmentID:attachmentID, TranscriptRevision:"rev-1", Tag:"question"}
	if _, err := s.CreateHighlight(ctx, input); err != nil { t.Fatal(err) }
	input.RequestID = "req-wrong-turn"
	input.TurnID = "other-recording/turn"
	if _, err := s.CreateHighlight(ctx, input); !errors.Is(err, ErrHighlightSourceMismatch) { t.Fatalf("foreign turn accepted: %v", err) }
	_, err = s.IngestTranscript(ctx, sampleResult("seg-highlight", "rev-2", 0))
	if err != nil { t.Fatal(err) }
	items, err := s.ListHighlights(ctx, "rec-1")
	if err != nil { t.Fatal(err) }
	if len(items) != 1 || items[0].State != "needs_relink" || items[0].QuoteText != nil { t.Fatalf("stale evidence was quoted: %+v", items) }
}

func stringPtr(v string) *string { return &v }
