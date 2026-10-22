package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"recordingmcp/internal/asrinput"
	"recordingmcp/internal/memory"
)

// TranscriptSegment reads the active source snapshot for GUI correction.
func (s *Service) TranscriptSegment(ctx context.Context, conversationID, recordingID, segmentID string) (asrinput.Result, error) {
	return s.Store.TranscriptSegment(ctx, conversationID, recordingID, segmentID)
}

func (s *Service) TranscriptSourceForTurn(ctx context.Context, turnID string) (memory.TranscriptSource, error) {
	return s.Store.TranscriptSourceForTurn(ctx, turnID)
}

// CorrectTranscriptSegment appends a full-segment transcript revision based
// on expectedRevision. The store rejects stale editors and enqueues the usual
// extraction/index jobs through IngestTranscript.
func (s *Service) CorrectTranscriptSegment(ctx context.Context, conversationID, recordingID, segmentID, expectedRevision, newRevision string, texts map[string]string) (memory.IngestReport, error) {
	report, err := s.Store.CorrectTranscriptSegment(ctx, conversationID, recordingID, segmentID, expectedRevision, newRevision, texts)
	var conflict *memory.TranscriptRevisionConflictError
	if errors.As(err, &conflict) {
		return report, &Error{Code: "transcript_revision_conflict", Status: 409, Message: err.Error(), CurrentVersion: conflict.Current}
	}
	return report, err
}

// ListSpeakerBindings returns the recording's versioned speaker review log.
func (s *Service) ListSpeakerBindings(ctx context.Context, recordingID, speakerScopedID string) ([]memory.SpeakerBinding, error) {
	return s.Store.ListSpeakerBindings(ctx, recordingID, speakerScopedID)
}

// ProposeSpeakerBinding only accepts evidence turns from the active transcript
// revision. Speaker identities remain recording- and revision-scoped.
func (s *Service) ProposeSpeakerBinding(ctx context.Context, recordingID, speakerScopedID, transcriptRevision string, evidenceTurnIDs []string, model string) (memory.SpeakerBinding, error) {
	if err := s.requireActiveSpeakerEvidence(ctx, recordingID, transcriptRevision, speakerScopedID, evidenceTurnIDs); err != nil {
		return memory.SpeakerBinding{}, err
	}
	return s.Store.ProposeSpeakerBinding(ctx, recordingID, speakerScopedID, transcriptRevision, evidenceTurnIDs, model)
}

func (s *Service) ConfirmSpeakerBinding(ctx context.Context, bindingID, personID, reviewer string) (memory.SpeakerBinding, error) {
	if err := s.requireActiveBinding(ctx, bindingID); err != nil {
		return memory.SpeakerBinding{}, err
	}
	return s.Store.ConfirmSpeakerBinding(ctx, bindingID, personID, reviewer)
}

func (s *Service) RejectSpeakerBinding(ctx context.Context, bindingID, reviewer, note string) (memory.SpeakerBinding, error) {
	if err := s.requireActiveBinding(ctx, bindingID); err != nil {
		return memory.SpeakerBinding{}, err
	}
	return s.Store.RejectSpeakerBinding(ctx, bindingID, reviewer, note)
}

func (s *Service) RevokeSpeakerBinding(ctx context.Context, bindingID, reviewer, note string) (memory.SpeakerBinding, error) {
	if err := s.requireActiveBinding(ctx, bindingID); err != nil {
		return memory.SpeakerBinding{}, err
	}
	return s.Store.RevokeSpeakerBinding(ctx, bindingID, reviewer, note)
}

func (s *Service) requireActiveSpeakerEvidence(ctx context.Context, recordingID, revision, label string, turnIDs []string) error {
	if strings.TrimSpace(recordingID) == "" || strings.TrimSpace(revision) == "" || strings.TrimSpace(label) == "" || len(turnIDs) == 0 {
		return validationError("recording_id, transcript_revision, speaker_scoped_id and evidence_turn_ids are required")
	}
	for _, id := range turnIDs {
		var count int
		err := s.Store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM turns t
			JOIN attachment_turns at ON at.turn_id=t.id
			JOIN source_attachments a ON a.id=at.attachment_id
			WHERE t.id=? AND t.recording_id=? AND t.speaker_scoped_id=? AND a.active=1 AND a.transcript_revision=?`,
			id, recordingID, label, revision).Scan(&count)
		if err != nil {
			return err
		}
		if count != 1 {
			return &Error{Code: "transcript_revision_conflict", Status: 409, Message: "speaker evidence is not in the requested active transcript revision"}
		}
	}
	return nil
}

func (s *Service) requireActiveBinding(ctx context.Context, bindingID string) error {
	if strings.TrimSpace(bindingID) == "" {
		return validationError("binding_id is required")
	}
	var recordingID, label string
	err := s.Store.DB().QueryRowContext(ctx, `SELECT recording_id,speaker_scoped_id FROM speaker_bindings WHERE id=?`, bindingID).Scan(&recordingID, &label)
	if errors.Is(err, sql.ErrNoRows) {
		return notFoundError("speaker binding not found")
	}
	if err != nil {
		return err
	}
	bindings, err := s.Store.ListSpeakerBindings(ctx, recordingID, label)
	if err != nil {
		return err
	}
	for _, b := range bindings {
		if b.ID != bindingID {
			continue
		}
		if b.TranscriptRevision == "" || len(b.EvidenceTurnIDs) == 0 {
			return fmt.Errorf("speaker binding lacks revision-scoped evidence")
		}
		for _, id := range b.EvidenceTurnIDs {
			var count int
			if err := s.Store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM turns t
				JOIN attachment_turns at ON at.turn_id=t.id
				JOIN source_attachments a ON a.id=at.attachment_id
				WHERE t.id=? AND t.recording_id=? AND t.speaker_scoped_id=? AND a.active=1 AND a.transcript_revision=?`,
				id, recordingID, label, b.TranscriptRevision).Scan(&count); err != nil {
				return err
			}
			if count != 1 {
				return &Error{Code: "transcript_revision_conflict", Status: 409, Message: "speaker binding evidence belongs to a superseded transcript revision"}
			}
		}
		return nil
	}
	return notFoundError("speaker binding not found")
}
