package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// ErrImportConflict reports that stable ids derived from the uploaded bytes
// disagree with rows already stored for that identity.
var ErrImportConflict = errors.New("memory: import identity conflict")

// PendingRecording is the pre-transcription record an upload creates before
// any audio has been decoded. It carries only facts the server knows: the
// names, the byte hash, the size and the probed duration. Recording start time
// stays unknown (NULL) because an upload has no clock evidence.
type PendingRecording struct {
	DeviceID               string
	ProviderConversationID string
	ProviderRecordingID    string
	Title                  string
	SourceFile             string
	SourceSHA256           string
	SourceBytes            int64
	DurationMS             int64
}

// PendingRecordingResult reports the stable ids and whether this call inserted
// a new pending conversation.
type PendingRecordingResult struct {
	ConversationID string
	RecordingID    string
	Inserted       bool
}

// ConversationID returns the stored conversation id for one provider
// conversation identity. It matches the id IngestTranscript derives.
func ConversationID(deviceID, providerConversationID string) string {
	return stableKey("conversation", deviceID, providerConversationID)
}

// EnsurePendingRecordingTx inserts the conversation/recording pair a queued
// ingest job refers to, or verifies the existing pair matches. Callers run it
// in the same transaction as the enqueue so a crash cannot leave a job whose
// source identity is missing.
func (s *Store) EnsurePendingRecordingTx(ctx context.Context, tx *sql.Tx, p PendingRecording) (PendingRecordingResult, error) {
	if !validID(p.DeviceID) || !validID(p.ProviderConversationID) || !validID(p.ProviderRecordingID) {
		return PendingRecordingResult{}, errors.New("memory: import identity is required")
	}
	if strings.TrimSpace(p.SourceSHA256) == "" {
		return PendingRecordingResult{}, errors.New("memory: source hash is required")
	}
	result := PendingRecordingResult{
		ConversationID: ConversationID(p.DeviceID, p.ProviderConversationID),
		RecordingID:    RecordingID(p.DeviceID, p.ProviderRecordingID),
	}
	title := cleanImportTitle(p.Title)
	if title == "" {
		title = "Imported recording"
	}
	sourceFile := filepath.Base(strings.TrimSpace(p.SourceFile))
	if sourceFile == "." || sourceFile == string(filepath.Separator) || sourceFile == "" {
		sourceFile = title
	}

	var boundRecording string
	err := tx.QueryRowContext(ctx, `SELECT recording_id FROM conversations WHERE id=?`, result.ConversationID).Scan(&boundRecording)
	switch {
	case err == nil:
		if boundRecording != result.RecordingID {
			return PendingRecordingResult{}, fmt.Errorf("%w: conversation is bound to recording %q", ErrImportConflict, boundRecording)
		}
		var conversationID, sha string
		if err := tx.QueryRowContext(ctx, `SELECT conversation_id, source_sha256 FROM recordings WHERE id=?`, result.RecordingID).Scan(&conversationID, &sha); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return PendingRecordingResult{}, fmt.Errorf("%w: recording row missing for conversation", ErrImportConflict)
			}
			return PendingRecordingResult{}, err
		}
		if conversationID != result.ConversationID || sha != p.SourceSHA256 {
			return PendingRecordingResult{}, fmt.Errorf("%w: recording identity differs", ErrImportConflict)
		}
		return result, nil
	case errors.Is(err, sql.ErrNoRows):
	default:
		return PendingRecordingResult{}, err
	}

	if _, err := tx.ExecContext(ctx, `INSERT INTO conversations(id,title,summary,annotation_version,annotation_basis,recording_id,source_sha256,transcript_version,duration_ms,transcript_json,annotations_json) VALUES(?,?,'','','',?,?, 'pending',?,'{}','{}')`,
		result.ConversationID, title, result.RecordingID, p.SourceSHA256, p.DurationMS); err != nil {
		return PendingRecordingResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO recordings(id,conversation_id,source_file,source_sha256,source_bytes,duration_ms,recording_started_at,absolute_time_known) VALUES(?,?,?,?,?,?,NULL,0)`,
		result.RecordingID, result.ConversationID, sourceFile, p.SourceSHA256, p.SourceBytes, p.DurationMS); err != nil {
		return PendingRecordingResult{}, err
	}
	result.Inserted = true
	return result, nil
}

// cleanImportTitle strips directory components and control characters so the
// title shown by the UI is the file the user picked, not a path.
func cleanImportTitle(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
	return strings.TrimSpace(name)
}
