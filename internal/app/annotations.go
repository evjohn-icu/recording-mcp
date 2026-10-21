package app

import (
	"context"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"recordingmcp/internal/memory"
)

// maxSummaryRunes bounds one note summary edit. The value is a UI contract
// (the edit box is limited to it) and keeps a single field from dwarfing the
// annotation payload.
const maxSummaryRunes = 4000

// EditResult reports one applied summary correction.
type EditResult struct {
	AnnotationVersion string `json:"annotation_version"`
	NoteID            string `json:"note_id"`
	RowID             string `json:"row_id"`
	IndexQueued       bool   `json:"index_queued"`
}

// EditNote corrects the summary of one note inside the conversation's current
// logical annotation view and applies the whole view as one new revision.
// Kind, topics and evidence are preserved: the claim_mode/proposed boundary is
// derived from them, so an edit may only change the wording.
//
// newVersion is the caller's idempotency key. Replaying the same
// (expectedVersion, newVersion, summary) triple after the edit committed
// succeeds without adding history; any other version mismatch is a conflict.
func (s *Service) EditNote(ctx context.Context, conversationID, rowID, expectedVersion, newVersion, summary string) (EditResult, error) {
	out := EditResult{RowID: rowID}
	if strings.TrimSpace(conversationID) == "" {
		return out, validationError("conversation_id is required")
	}
	if strings.TrimSpace(rowID) == "" {
		return out, validationError("note id is required")
	}
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return out, validationError("summary is required")
	}
	if utf8.RuneCountInString(summary) > maxSummaryRunes {
		return out, validationError("summary must be at most 4000 characters")
	}
	if !validVersion(newVersion) {
		return out, validationError("new_version must be a stable token of at most 256 bytes without control characters")
	}
	if newVersion == expectedVersion {
		return out, validationError("new_version must differ from expected_version")
	}
	if err := s.requireConversation(ctx, conversationID); err != nil {
		return out, err
	}
	current, version, err := s.Store.CurrentAnnotations(ctx, conversationID)
	if err != nil {
		return out, err
	}
	index, logicalID := findNote(current.Notes, conversationID, rowID)
	if expectedVersion != version {
		if version == newVersion && index >= 0 && strings.TrimSpace(current.Notes[index].Summary) == summary {
			// This is the retry of an edit that already committed: the same
			// key produced the current version and the wording already landed.
			out.NoteID = logicalID
			out.AnnotationVersion = newVersion
			out.IndexQueued = true
			return out, nil
		}
		// The conflict wins over a missing note so the caller's input can be
		// shown next to what the store actually holds.
		note, err := s.noteView(ctx, conversationID, current.Notes, index, version)
		if err != nil {
			return out, err
		}
		return out, &Error{
			Code:           CodeAnnotationVersionConflict,
			Status:         409,
			Message:        (&memory.AnnotationConflictError{Expected: expectedVersion, Current: version}).Error(),
			CurrentVersion: version,
			Current:        current,
			CurrentNote:    note,
		}
	}
	if index < 0 {
		return out, &Error{Code: CodeNoteNotFound, Status: 404, Message: "note not found: " + rowID}
	}

	merged := current
	merged.Notes = append([]memory.NoteAnnotation{}, current.Notes...)
	merged.Notes[index].Summary = summary
	merged.Notes[index].Edited = true
	merged.ConversationID = conversationID
	merged.AnnotationVersion = newVersion
	if strings.TrimSpace(merged.Title) == "" {
		title, err := s.Store.Conversation(ctx, conversationID)
		if err != nil {
			return out, err
		}
		merged.Title = title
	}
	if err := s.Store.ApplyAnnotations(ctx, conversationID, expectedVersion, merged); err != nil {
		var conflict *memory.AnnotationConflictError
		var reused *memory.AnnotationVersionReusedError
		var invalid *memory.ValidationError
		switch {
		case errors.As(err, &conflict) || errors.As(err, &reused):
			// The read above raced another writer: re-read and report the same
			// conflict shape instead of failing as an internal error.
			latest, latestVersion, readErr := s.Store.CurrentAnnotations(ctx, conversationID)
			if readErr != nil {
				return out, readErr
			}
			latestIndex, _ := findNote(latest.Notes, conversationID, rowID)
			note, noteErr := s.noteView(ctx, conversationID, latest.Notes, latestIndex, latestVersion)
			if noteErr != nil {
				return out, noteErr
			}
			return out, &Error{
				Code:           CodeAnnotationVersionConflict,
				Status:         409,
				Message:        err.Error(),
				CurrentVersion: latestVersion,
				Current:        latest,
				CurrentNote:    note,
			}
		case errors.As(err, &invalid):
			return out, validationError(invalid.Msg)
		default:
			return out, internalError(err)
		}
	}
	out.NoteID = logicalID
	out.AnnotationVersion = newVersion
	out.IndexQueued = true
	return out, nil
}

// findNote resolves a stored note row id to its position in the logical
// annotation view and its logical id. It returns -1 when the view no longer
// has that note.
func findNote(notes []memory.NoteAnnotation, conversationID, rowID string) (int, string) {
	for i := range notes {
		if memory.NoteRowID(conversationID, notes[i].ID) == rowID {
			return i, notes[i].ID
		}
	}
	return -1, ""
}

// noteView renders one logical note at index as a NoteView, or nil when the
// index is negative.
func (s *Service) noteView(ctx context.Context, conversationID string, notes []memory.NoteAnnotation, index int, version string) (*NoteView, error) {
	if index < 0 || index >= len(notes) {
		return nil, nil
	}
	n := notes[index]
	rowID := memory.NoteRowID(conversationID, n.ID)
	evidence, err := s.noteEvidence(ctx, conversationID)
	if err != nil {
		return nil, err
	}
	return &NoteView{
		NoteID:            n.ID,
		RowID:             rowID,
		Kind:              n.Kind,
		Summary:           n.Summary,
		TopicIDs:          nonNilStrings(n.TopicIDs),
		EvidenceTurnIDs:   nonNilStrings(evidence[rowID]),
		AnnotationVersion: version,
		Edited:            n.Edited,
	}, nil
}

// validVersion mirrors the store's id token rule: printable, non-empty and at
// most 256 bytes, so a caller-generated version cannot smuggle control
// characters into the annotation history.
func validVersion(v string) bool {
	if strings.TrimSpace(v) == "" || len(v) > 256 {
		return false
	}
	for _, r := range v {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
