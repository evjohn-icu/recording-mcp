package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"recordingmcp/internal/asrinput"
)

// TranscriptRevisionConflictError means the editor's base revision is no
// longer the active head for this segment.
type TranscriptRevisionConflictError struct {
	Expected string
	Current  string
}

// TranscriptSource identifies the active segment snapshot containing a turn.
type TranscriptSource struct {
	SourceAttachmentID string          `json:"source_attachment_id"`
	ConversationID     string          `json:"conversation_id"`
	RecordingID        string          `json:"recording_id"`
	SegmentID          string          `json:"segment_id"`
	Result             asrinput.Result `json:"result"`
}

func (e *TranscriptRevisionConflictError) Error() string {
	return fmt.Sprintf("transcript revision conflict: expected %q, current %q", e.Expected, e.Current)
}

// TranscriptSegment returns the immutable source snapshot for the active
// revision of a segment. The provider result is returned for audit/display;
// callers should submit corrections through CorrectTranscriptSegment.
func (s *Store) TranscriptSegment(ctx context.Context, conversationID, recordingID, segmentID string) (asrinput.Result, error) {
	if strings.TrimSpace(conversationID) == "" || strings.TrimSpace(recordingID) == "" || strings.TrimSpace(segmentID) == "" {
		return asrinput.Result{}, errors.New("conversation_id, recording_id and segment_id are required")
	}
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT result_json FROM source_attachments
		WHERE conversation_id=? AND recording_id=? AND segment_id=? AND active=1`, conversationID, recordingID, segmentID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return asrinput.Result{}, ErrNotFound
	}
	if err != nil {
		return asrinput.Result{}, err
	}
	var result asrinput.Result
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return asrinput.Result{}, fmt.Errorf("decode active transcript segment: %w", err)
	}
	return result, nil
}

// TranscriptSourceForTurn resolves a current, non-ambient turn to its active
// source segment and complete ASR snapshot for correction workflows.
func (s *Store) TranscriptSourceForTurn(ctx context.Context, turnID string) (TranscriptSource, error) {
	if strings.TrimSpace(turnID) == "" {
		return TranscriptSource{}, errors.New("turn_id is required")
	}
	var out TranscriptSource
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT t.conversation_id,t.recording_id,a.segment_id,a.id,a.result_json
		FROM turns t JOIN attachment_turns at ON at.turn_id=t.id
		JOIN source_attachments a ON a.id=at.attachment_id
		WHERE t.id=? AND a.active=1
		AND NOT EXISTS(SELECT 1 FROM turn_classes tc WHERE tc.turn_id=t.id AND tc.class='ambient')`, turnID).
		Scan(&out.ConversationID, &out.RecordingID, &out.SegmentID, &out.SourceAttachmentID, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return TranscriptSource{}, ErrNotFound
	}
	if err != nil {
		return TranscriptSource{}, err
	}
	if err := json.Unmarshal([]byte(raw), &out.Result); err != nil {
		return TranscriptSource{}, fmt.Errorf("decode active transcript segment: %w", err)
	}
	return out, nil
}

// CorrectTranscriptSegment creates a new immutable revision of a segment by
// changing the text of every turn in one submitted full-segment correction.
// IDs, timing, speaker labels, source envelope, and provider metadata are
// copied from the active snapshot. IngestTranscript performs the final active
// head check and atomically supersedes it, so a concurrent correction cannot
// replace a newer revision.
func (s *Store) CorrectTranscriptSegment(ctx context.Context, conversationID, recordingID, segmentID, expectedRevision, newRevision string, correctedTexts map[string]string) (IngestReport, error) {
	if strings.TrimSpace(expectedRevision) == "" || strings.TrimSpace(newRevision) == "" || expectedRevision == newRevision {
		return IngestReport{}, errors.New("expected_revision and a different new_revision are required")
	}
	result, err := s.TranscriptSegment(ctx, conversationID, recordingID, segmentID)
	if err != nil {
		return IngestReport{}, err
	}
	if result.Source.TranscriptRevision != expectedRevision {
		return IngestReport{}, &TranscriptRevisionConflictError{Expected: expectedRevision, Current: result.Source.TranscriptRevision}
	}
	if len(correctedTexts) != len(result.Turns) {
		return IngestReport{}, errors.New("correction must include text for every turn in the active segment")
	}
	seen := make(map[string]bool, len(result.Turns))
	for i := range result.Turns {
		id := result.Turns[i].ID
		text, ok := correctedTexts[id]
		if !ok || strings.TrimSpace(text) == "" {
			return IngestReport{}, fmt.Errorf("correction is missing non-empty text for turn %q", id)
		}
		result.Turns[i].Text = text
		seen[id] = true
	}
	for id := range correctedTexts {
		if !seen[id] {
			return IngestReport{}, fmt.Errorf("correction contains unknown turn %q", id)
		}
	}
	result.Source.TranscriptRevision = newRevision
	result.Source.SupersedesRevision = expectedRevision
	report, err := s.IngestTranscript(ctx, result)
	if err != nil {
		var conflict *TranscriptRevisionConflictError
		if errors.As(err, &conflict) {
			return IngestReport{}, err
		}
		// IngestTranscript's active-head error includes the revision observed
		// inside its transaction; normalize it into a typed 409 for the API.
		var active string
		readErr := s.db.QueryRowContext(ctx, `SELECT transcript_revision FROM source_attachments
			WHERE recording_id=? AND segment_id=? AND active=1`, recordingID, segmentID).Scan(&active)
		if readErr == nil && active != expectedRevision {
			return IngestReport{}, &TranscriptRevisionConflictError{Expected: expectedRevision, Current: active}
		}
	}
	return report, err
}
