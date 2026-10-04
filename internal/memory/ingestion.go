package memory

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"recordingmcp/internal/asrinput"
	"recordingmcp/internal/process"
)

// extractProcessorVersion versions the extract-stage job identity so that a
// change to the extract pipeline starts a fresh, disjoint idempotency key.
const extractProcessorVersion = "extract-v1"

// IngestReport describes one immutable, final transcript segment snapshot.
type IngestReport struct {
	ConversationID     string `json:"conversation_id"`
	RecordingID        string `json:"recording_id"`
	SegmentID          string `json:"segment_id"`
	TranscriptRevision string `json:"transcript_revision"`
	Inserted           bool   `json:"inserted"`
	Turns              int    `json:"turns"`
	SegmentStartMS     int64  `json:"segment_start_ms"`
	SegmentEndMS       int64  `json:"segment_end_ms"`
	SegmentIndex       int    `json:"segment_index"`
	AnalysisStatus     string `json:"analysis_status"`
}

// ProcessingStatus is calculated from persisted source turns and topic links.
type ProcessingStatus struct {
	ConversationID           string `json:"conversation_id"`
	ReceivedSegments         int    `json:"received_segments"`
	OriginalTurns            int    `json:"original_turns"`
	TopicLinkedTurns         int    `json:"topic_linked_turns"`
	UnclassifiedTurns        int    `json:"unclassified_turns"`
	IdentityUnconfirmedTurns int    `json:"identity_unconfirmed_turns"`
	AnalysisStatus           string `json:"analysis_status"`
}

func stableKey(kind string, parts ...string) string {
	return kind + ":" + hashID(strings.Join(append([]string{"recording-mcp/asr-input/v1", kind}, parts...), "\x00"))
}

// semanticJSONHash canonicalizes object key order while preserving the exact
// decimal representation of numbers. This is used only for ingestion replay
// identity; legacy hashJSON behavior remains unchanged for existing imports.
func semanticJSONHash(v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return semanticRawJSONHash(raw)
}

func semanticRawJSONHash(raw []byte) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return "", err
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return "", errors.New("multiple JSON values in stored transcript result")
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return hashID(string(canonical)), nil
}

// IngestTranscript stores only final, complete segment results. Replays are idempotent;
// a different payload for the same revision, or an unannounced revision, is rejected.
func (s *Store) IngestTranscript(ctx context.Context, result asrinput.Result) (IngestReport, error) {
	src := result.Source
	if result.ContractVersion != asrinput.ContractVersion {
		return IngestReport{}, fmt.Errorf("unsupported ASR input contract %q", result.ContractVersion)
	}
	if err := asrinput.Validate(result); err != nil {
		return IngestReport{}, err
	}
	if strings.TrimSpace(src.ConversationID) == "" {
		return IngestReport{}, errors.New("conversation_id is required to assign an ASR segment to a conversation")
	}
	if src.CompletionStatus != "final" {
		return IngestReport{}, errors.New("only final transcript results can be ingested")
	}
	for label, value := range map[string]string{"provider": src.Provider, "provider_request_id": src.ProviderRequestID, "device_id": src.DeviceID, "recording_id": src.RecordingID, "segment_id": src.SegmentID, "transcript_revision": src.TranscriptRevision} {
		if !validID(value) {
			return IngestReport{}, fmt.Errorf("%s is required", label)
		}
	}
	if src.SegmentOffsetMS < 0 || src.SegmentDurationMS <= 0 {
		return IngestReport{}, errors.New("segment offset must be nonnegative and duration positive")
	}
	resultJSONBytes, err := json.Marshal(result)
	if err != nil {
		return IngestReport{}, err
	}
	resultJSON := string(resultJSONBytes)
	digest, err := semanticJSONHash(result)
	if err != nil {
		return IngestReport{}, err
	}
	sourceJSON, err := json.Marshal(src)
	if err != nil {
		return IngestReport{}, err
	}
	conversationID := stableKey("conversation", src.DeviceID, src.ConversationID)
	recordingID := stableKey("recording", src.DeviceID, src.RecordingID)
	transcriptVersion := "asr-" + hashID(src.TranscriptRevision)[:16]
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return IngestReport{}, err
	}
	defer tx.Rollback()
	var oldHash, oldResult string
	err = tx.QueryRowContext(ctx, "SELECT content_sha256,result_json FROM source_attachments WHERE recording_id=? AND segment_id=? AND transcript_revision=?", recordingID, src.SegmentID, src.TranscriptRevision).Scan(&oldHash, &oldResult)
	if err == nil {
		storedDigest, hashErr := semanticRawJSONHash([]byte(oldResult))
		if hashErr != nil {
			return IngestReport{}, fmt.Errorf("cannot verify stored transcript snapshot: %w", hashErr)
		}
		if oldHash != digest && storedDigest != digest {
			return IngestReport{}, errors.New("conflicting content for existing transcript revision")
		}
		var segmentIndex, retained int
		if err = tx.QueryRowContext(ctx, `SELECT a.segment_index,COUNT(at.turn_id) FROM source_attachments a LEFT JOIN attachment_turns at ON at.attachment_id=a.id WHERE a.recording_id=? AND a.segment_id=? AND a.transcript_revision=? GROUP BY a.id`, recordingID, src.SegmentID, src.TranscriptRevision).Scan(&segmentIndex, &retained); err != nil {
			return IngestReport{}, err
		}
		if err = tx.Commit(); err != nil {
			return IngestReport{}, err
		}
		status, err := s.GetProcessingStatus(ctx, conversationID)
		if err != nil {
			return IngestReport{}, err
		}
		return IngestReport{ConversationID: conversationID, RecordingID: recordingID, SegmentID: src.SegmentID, TranscriptRevision: src.TranscriptRevision, Inserted: false, Turns: retained, SegmentStartMS: src.SegmentOffsetMS, SegmentEndMS: src.SegmentOffsetMS + src.SegmentDurationMS, SegmentIndex: segmentIndex, AnalysisStatus: status.AnalysisStatus}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return IngestReport{}, err
	}
	var activeRevision string
	err = tx.QueryRowContext(ctx, "SELECT transcript_revision FROM source_attachments WHERE recording_id=? AND segment_id=? AND active=1", recordingID, src.SegmentID).Scan(&activeRevision)
	if err == nil {
		return IngestReport{}, fmt.Errorf("segment already has active revision %q; revision replacement is not supported", activeRevision)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return IngestReport{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO conversations(id,title,summary,annotation_version,annotation_basis,recording_id,source_sha256,transcript_version,duration_ms,transcript_json,annotations_json) VALUES(?, ?, '', '', '', ?, ?, ?, ?, '{}', '{}')`, conversationID, "Recording "+src.RecordingID, recordingID, deref(src.SourceAudioSHA256), transcriptVersion, src.SegmentOffsetMS+src.SegmentDurationMS); err != nil {
		return IngestReport{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO recordings(id,conversation_id,source_file,source_sha256,duration_ms,recording_started_at,absolute_time_known) VALUES(?,?,?,?,?,?,?)`, recordingID, conversationID, deref(src.SourceRef), deref(src.SourceAudioSHA256), src.SegmentOffsetMS+src.SegmentDurationMS, nullable(src.RecordingStartedAt), boolInt(src.RecordingStartedAt != nil)); err != nil {
		return IngestReport{}, err
	}
	var boundConversation string
	if err = tx.QueryRowContext(ctx, "SELECT conversation_id FROM recordings WHERE id=?", recordingID).Scan(&boundConversation); err != nil {
		return IngestReport{}, err
	}
	if boundConversation != conversationID {
		return IngestReport{}, errors.New("recording is already bound to a different conversation")
	}
	if _, err = tx.ExecContext(ctx, `UPDATE recordings SET duration_ms=MAX(duration_ms,?), recording_started_at=COALESCE(recording_started_at,?), absolute_time_known=MAX(absolute_time_known,?) WHERE id=?`, src.SegmentOffsetMS+src.SegmentDurationMS, nullable(src.RecordingStartedAt), boolInt(src.RecordingStartedAt != nil), recordingID); err != nil {
		return IngestReport{}, err
	}
	var segmentIndex int
	if err = tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(segment_index),0)+1 FROM source_attachments WHERE recording_id=?", recordingID).Scan(&segmentIndex); err != nil {
		return IngestReport{}, err
	}
	attachmentID := stableKey("attachment", recordingID, src.SegmentID, src.TranscriptRevision)
	if _, err = tx.ExecContext(ctx, `INSERT INTO source_attachments(id,conversation_id,recording_id,segment_index,segment_id,transcript_revision,source_json,result_json,content_sha256,active,supersedes_revision) VALUES(?,?,?,?,?,?,?,?,?,1,NULL)`, attachmentID, conversationID, recordingID, segmentIndex, src.SegmentID, src.TranscriptRevision, string(sourceJSON), resultJSON, digest); err != nil {
		return IngestReport{}, err
	}
	for i, turn := range result.Turns {
		if !validID(turn.ID) {
			return IngestReport{}, fmt.Errorf("invalid turn id at index %d", i)
		}
		if strings.TrimSpace(turn.Text) == "" {
			continue
		}
		turnJSON, _, e := hashJSON(turn)
		if e != nil {
			return IngestReport{}, e
		}
		origID := src.SegmentID + "/" + src.TranscriptRevision + "/" + turn.ID
		turnID := stableKey("turn", recordingID, origID)
		globalStart, globalEnd := turn.GlobalStartMS, turn.GlobalEndMS
		if globalStart == nil && turn.LocalStartMS != nil {
			v := src.SegmentOffsetMS + *turn.LocalStartMS
			globalStart = &v
		}
		if globalEnd == nil && turn.LocalEndMS != nil {
			v := src.SegmentOffsetMS + *turn.LocalEndMS
			globalEnd = &v
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO turns(id,conversation_id,recording_id,segment_index,global_start_ms,global_end_ms,local_start_ms,local_end_ms,speaker_scoped_id,speaker_entity_id,content,definite,source_sha256,transcript_version,original_turn_id,turn_json) VALUES(?,?,?,?,?,?,?, ?, ?,NULL,?,?, ?,?,?,?)`, turnID, conversationID, recordingID, segmentIndex, ptrInt(globalStart), ptrInt(globalEnd), ptrInt(turn.LocalStartMS), ptrInt(turn.LocalEndMS), ptrText(turn.SpeakerScopedID), turn.Text, nil, deref(src.SourceAudioSHA256), transcriptVersion, origID, turnJSON); err != nil {
			return IngestReport{}, err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO attachment_turns(attachment_id,turn_id) VALUES(?,?)", attachmentID, turnID); err != nil {
			return IngestReport{}, err
		}
	}
	payload, err := json.Marshal(map[string]string{
		"conversation_id": conversationID,
		"recording_id":    recordingID,
		"segment_id":      src.SegmentID,
		"attachment_id":   attachmentID,
	})
	if err != nil {
		return IngestReport{}, err
	}
	if _, _, err := process.EnqueueTx(ctx, tx, process.Spec{
		Stage:           "extract",
		InputSnapshotID: attachmentID,
		IdempotencyKey:  stableKey("job", "extract", attachmentID, extractProcessorVersion),
		PayloadJSON:     string(payload),
		MaxAttempts:     3,
	}, time.Now()); err != nil {
		return IngestReport{}, err
	}
	if err = tx.Commit(); err != nil {
		return IngestReport{}, err
	}
	retained := 0
	for _, turn := range result.Turns {
		if strings.TrimSpace(turn.Text) != "" {
			retained++
		}
	}
	status, err := s.GetProcessingStatus(ctx, conversationID)
	if err != nil {
		return IngestReport{}, err
	}
	return IngestReport{ConversationID: conversationID, RecordingID: recordingID, SegmentID: src.SegmentID, TranscriptRevision: src.TranscriptRevision, Inserted: true, Turns: retained, SegmentStartMS: src.SegmentOffsetMS, SegmentEndMS: src.SegmentOffsetMS + src.SegmentDurationMS, SegmentIndex: segmentIndex, AnalysisStatus: status.AnalysisStatus}, nil
}

func nullable(p *string) any {
	if p == nil || strings.TrimSpace(*p) == "" {
		return nil
	}
	return *p
}
func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func (s *Store) GetProcessingStatus(ctx context.Context, conversationID string) (ProcessingStatus, error) {
	var out ProcessingStatus
	out.ConversationID = conversationID
	if _, err := s.Conversation(ctx, conversationID); err != nil {
		return out, err
	}
	err := s.db.QueryRowContext(ctx, `SELECT
 (SELECT COUNT(*) FROM source_attachments a WHERE a.conversation_id=? AND a.active=1) +
 (SELECT COUNT(*) FROM recordings r WHERE r.conversation_id=? AND NOT EXISTS(SELECT 1 FROM source_attachments a WHERE a.recording_id=r.id)),
 (SELECT COUNT(*) FROM turns t WHERE t.conversation_id=?),
 (SELECT COUNT(*) FROM turns t WHERE t.conversation_id=? AND EXISTS(SELECT 1 FROM turn_topics tt WHERE tt.turn_id=t.id)),
 (SELECT COUNT(*) FROM turns t WHERE t.conversation_id=? AND NOT EXISTS(SELECT 1 FROM turn_topics tt WHERE tt.turn_id=t.id)),
 (SELECT COUNT(*) FROM turns t WHERE t.conversation_id=? AND t.speaker_entity_id IS NULL)`, conversationID, conversationID, conversationID, conversationID, conversationID, conversationID).Scan(&out.ReceivedSegments, &out.OriginalTurns, &out.TopicLinkedTurns, &out.UnclassifiedTurns, &out.IdentityUnconfirmedTurns)
	if err != nil {
		return ProcessingStatus{}, err
	}
	var analyzed int
	if err = s.db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM notes WHERE conversation_id=?) + (SELECT COUNT(*) FROM turn_topics tt JOIN turns t ON t.id=tt.turn_id WHERE t.conversation_id=?)`, conversationID, conversationID).Scan(&analyzed); err != nil {
		return ProcessingStatus{}, err
	}
	out.AnalysisStatus = "pending"
	if analyzed > 0 {
		out.AnalysisStatus = "partial"
	}
	return out, nil
}
