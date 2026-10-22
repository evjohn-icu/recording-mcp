package memory

import (
	"context"
	"errors"
	"testing"
)

func TestCorrectTranscriptSegmentCreatesRevisionAndRejectsStaleEditor(t *testing.T) {
	ctx := context.Background()
	s := openTempStore(t)
	original := sampleResult("seg-a", "rev-1", 0)
	first, err := s.IngestTranscript(ctx, original)
	if err != nil {
		t.Fatal(err)
	}
	corrected, err := s.CorrectTranscriptSegment(ctx, first.ConversationID, first.RecordingID, "seg-a", "rev-1", "rev-2", map[string]string{"utt-1": "corrected transcript"})
	if err != nil {
		t.Fatal(err)
	}
	if !corrected.Inserted || corrected.TranscriptRevision != "rev-2" {
		t.Fatalf("correction report = %+v", corrected)
	}
	var oldActive, newActive int
	if err := s.DB().QueryRow(`SELECT active FROM source_attachments WHERE recording_id=? AND segment_id='seg-a' AND transcript_revision='rev-1'`, first.RecordingID).Scan(&oldActive); err != nil {
		t.Fatal(err)
	}
	if err := s.DB().QueryRow(`SELECT active FROM source_attachments WHERE recording_id=? AND segment_id='seg-a' AND transcript_revision='rev-2'`, first.RecordingID).Scan(&newActive); err != nil {
		t.Fatal(err)
	}
	if oldActive != 0 || newActive != 1 {
		t.Fatalf("active flags old=%d new=%d", oldActive, newActive)
	}
	current, err := s.TranscriptSegment(ctx, first.ConversationID, first.RecordingID, "seg-a")
	if err != nil {
		t.Fatal(err)
	}
	if current.Turns[0].Text != "corrected transcript" || current.Turns[0].SpeakerScopedID == nil || *current.Turns[0].SpeakerScopedID != "spk:device-1:1" {
		t.Fatalf("correction did not preserve current source fields: %+v", current.Turns[0])
	}
	newTurnID := stableKey("turn", first.RecordingID, "seg-a/rev-2/utt-1")
	source, err := s.TranscriptSourceForTurn(ctx, newTurnID)
	if err != nil || source.SegmentID != "seg-a" || source.Result.Source.TranscriptRevision != "rev-2" {
		t.Fatalf("turn source = %+v, %v", source, err)
	}
	oldTurnID := stableKey("turn", first.RecordingID, "seg-a/rev-1/utt-1")
	if _, err := s.TranscriptSourceForTurn(ctx, oldTurnID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("superseded turn source err = %v, want not found", err)
	}
	if _, err := s.CorrectTranscriptSegment(ctx, first.ConversationID, first.RecordingID, "seg-a", "rev-1", "rev-3", map[string]string{"utt-1": "stale edit"}); err == nil {
		t.Fatal("stale editor was accepted")
	} else {
		var conflict *TranscriptRevisionConflictError
		if !errors.As(err, &conflict) || conflict.Current != "rev-2" {
			t.Fatalf("stale edit error = %v", err)
		}
	}
}

func TestCorrectTranscriptSegmentRequiresCompleteTurnSet(t *testing.T) {
	ctx := context.Background()
	s := openTempStore(t)
	r := sampleResult("seg-a", "rev-1", 0)
	r.Turns = append(r.Turns, r.Turns[0])
	r.Turns[1].ID = "utt-2"
	r.Turns[1].OriginalIndex = 2
	first, err := s.IngestTranscript(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CorrectTranscriptSegment(ctx, first.ConversationID, first.RecordingID, "seg-a", "rev-1", "rev-2", map[string]string{"utt-1": "only one"}); err == nil {
		t.Fatal("partial segment correction was accepted")
	}
	var n int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM source_attachments WHERE recording_id=? AND segment_id='seg-a'`, first.RecordingID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("failed correction inserted %d source revisions", n)
	}
}
