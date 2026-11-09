package memory

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"recordingmcp/internal/asrinput"
	"recordingmcp/internal/process"
	"recordingmcp/internal/turnclass"
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
// Ambient-sound turns are processed work: they are never topic-linked, so
// they are excluded from the unclassified gap count and reported in their
// own AmbientTurns bucket instead.
type ProcessingStatus struct {
	ConversationID           string `json:"conversation_id"`
	ReceivedSegments         int    `json:"received_segments"`
	OriginalTurns            int    `json:"original_turns"`
	TopicLinkedTurns         int    `json:"topic_linked_turns"`
	UnclassifiedTurns        int    `json:"unclassified_turns"`
	AmbientTurns             int    `json:"ambient_turns"`
	IdentityUnconfirmedTurns int    `json:"identity_unconfirmed_turns"`
	AnalysisStatus           string `json:"analysis_status"`
	// IndexingStatus reports the latest embed-stage job for the conversation:
	// "pending" (no job, or queued/running/retry_wait), "ready" (succeeded and
	// indexed), "failed" (attempts exhausted; rerun bin/context-embed or wait
	// for the next successful embed job), or "disabled" (worker runs without
	// an embedding provider).
	IndexingStatus string `json:"indexing_status"`
	// ASR progress is the window-level projection of the active attachments.
	// completed counts committed windows, coverage is the union of committed
	// intervals (never MAX(global_end_ms)), and state mirrors
	// sync_recordings.transcription_state.
	ASRState           string `json:"asr_state"`
	ASRExpectedChunks  int    `json:"asr_expected_chunks"`
	ASRCompletedChunks int    `json:"asr_completed_chunks"`
	ASRFailedChunks    int    `json:"asr_failed_chunks"`
	ASRCoverageMS      int64  `json:"asr_coverage_ms"`
	ASRDurationMS      int64  `json:"asr_duration_ms"`
	ASRLastError       string `json:"asr_last_error,omitempty"`
}

func stableKey(kind string, parts ...string) string {
	return kind + ":" + hashID(strings.Join(append([]string{"recording-mcp/asr-input/v1", kind}, parts...), "\x00"))
}

// RecordingID returns the stored recordings.id for one provider recording
// identity. It is the inverse lookup the audio resolver needs because
// sync_recordings keeps the provider UUID, not the hashed memory id.
func RecordingID(deviceID, recordingID string) string {
	return stableKey("recording", deviceID, recordingID)
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
	var activeID, activeRevision string
	err = tx.QueryRowContext(ctx, "SELECT id, transcript_revision FROM source_attachments WHERE recording_id=? AND segment_id=? AND active=1", recordingID, src.SegmentID).Scan(&activeID, &activeRevision)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return IngestReport{}, err
	}
	if err == nil && src.SupersedesRevision == "" {
		return IngestReport{}, fmt.Errorf("segment already has active revision %q; supersedes_revision is required to replace it", activeRevision)
	}
	// supersededID is the attachment the new revision replaces; only set for a
	// valid supersede request. The old row keeps its result_json and turns: it
	// stays as immutable audit history with active=0.
	supersededID := ""
	if src.SupersedesRevision != "" {
		if src.SupersedesRevision == src.TranscriptRevision {
			return IngestReport{}, errors.New("supersedes_revision must differ from transcript_revision")
		}
		if src.SupersedesRevision == activeRevision {
			supersededID = activeID
		} else {
			var targetID string
			var targetSegmentIndex int
			e := tx.QueryRowContext(ctx, "SELECT id, segment_index FROM source_attachments WHERE recording_id=? AND transcript_revision=? AND segment_id=?", recordingID, src.SupersedesRevision, src.SegmentID).Scan(&targetID, &targetSegmentIndex)
			if e == nil {
				return IngestReport{}, fmt.Errorf("supersedes target %q is not the active revision for segment %q (active %q)", src.SupersedesRevision, src.SegmentID, activeRevision)
			}
			if !errors.Is(e, sql.ErrNoRows) {
				return IngestReport{}, e
			}
			var otherSegmentID string
			e = tx.QueryRowContext(ctx, "SELECT segment_id FROM source_attachments WHERE recording_id=? AND transcript_revision=? LIMIT 1", recordingID, src.SupersedesRevision).Scan(&otherSegmentID)
			if e == nil {
				return IngestReport{}, fmt.Errorf("supersedes target %q belongs to segment %q; cross-segment supersede is not allowed", src.SupersedesRevision, otherSegmentID)
			}
			if !errors.Is(e, sql.ErrNoRows) {
				return IngestReport{}, e
			}
			return IngestReport{}, fmt.Errorf("supersedes target %q not found for segment %q", src.SupersedesRevision, src.SegmentID)
		}
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
	if supersededID != "" {
		if _, err = tx.ExecContext(ctx, `UPDATE source_attachments SET active=0 WHERE id=? AND active=1`, supersededID); err != nil {
			return IngestReport{}, err
		}
	}
	var supersedes any
	if src.SupersedesRevision != "" {
		supersedes = src.SupersedesRevision
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO source_attachments(id,conversation_id,recording_id,segment_index,segment_id,transcript_revision,source_json,result_json,content_sha256,active,supersedes_revision) VALUES(?,?,?,?,?,?,?,?,?,1,?)`, attachmentID, conversationID, recordingID, segmentIndex, src.SegmentID, src.TranscriptRevision, string(sourceJSON), resultJSON, digest, supersedes); err != nil {
		return IngestReport{}, err
	}
	// Retained turns only: blank text is skipped below and never enters turns.
	retainedTurnIDs := []string{}
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
		retainedTurnIDs = append(retainedTurnIDs, turnID)
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
		// Ambient classification is content-derived and deterministic, so it
		// is recorded at ingest: a re-ingested identical turn is the same row,
		// and a corrected turn gets a new id and is classified again. Only
		// rule matches are recorded here; unmatched turns stay unclassified
		// until the model pass rules on them.
		if class, subtype, matched := turnclass.Classify(turn.Text); matched {
			if _, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO turn_classes(turn_id,class,subtype,method,created_at) VALUES(?,?,?,?,?)", turnID, class, subtype, turnclass.MethodRule, time.Now().UnixMilli()); err != nil {
				return IngestReport{}, err
			}
		}
	}
	if supersededID != "" {
		if _, err = markStaleForAttachment(ctx, tx, supersededID, staleReasonSuperseded, time.Now()); err != nil {
			return IngestReport{}, err
		}
	}
	payload, err := json.Marshal(struct {
		ConversationID string   `json:"conversation_id"`
		RecordingID    string   `json:"recording_id"`
		SegmentID      string   `json:"segment_id"`
		TurnIDs        []string `json:"turn_ids"`
	}{
		ConversationID: conversationID,
		RecordingID:    recordingID,
		SegmentID:      src.SegmentID,
		TurnIDs:        retainedTurnIDs,
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
	// Raw turns are retrievable before any distillation: the embed job runs in
	// the same transaction so even a generator-off worker indexes the transcript.
	embedPayload, err := json.Marshal(struct {
		ConversationID string `json:"conversation_id"`
	}{ConversationID: conversationID})
	if err != nil {
		return IngestReport{}, err
	}
	if _, _, err := process.EnqueueTx(ctx, tx, process.Spec{
		Stage:           "embed",
		InputSnapshotID: conversationID,
		IdempotencyKey:  "embed|" + attachmentID,
		PayloadJSON:     string(embedPayload),
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
 (SELECT COUNT(*) FROM turns t WHERE t.conversation_id=? AND `+CurrentTurnPredicate("t")+`),
 (SELECT COUNT(*) FROM turns t WHERE t.conversation_id=? AND `+CurrentTurnPredicate("t")+` AND EXISTS(SELECT 1 FROM turn_topics tt WHERE tt.turn_id=t.id)),
 (SELECT COUNT(*) FROM turns t WHERE t.conversation_id=? AND `+CurrentTurnPredicate("t")+` AND NOT EXISTS(SELECT 1 FROM turn_topics tt WHERE tt.turn_id=t.id) AND NOT EXISTS(SELECT 1 FROM turn_classes tc WHERE tc.turn_id=t.id AND tc.class='ambient')),
 (SELECT COUNT(*) FROM turns t JOIN turn_classes tc ON tc.turn_id=t.id WHERE t.conversation_id=? AND `+CurrentTurnPredicate("t")+` AND tc.class='ambient'),
 (SELECT COUNT(*) FROM turns t WHERE t.conversation_id=? AND `+CurrentTurnPredicate("t")+` AND t.speaker_entity_id IS NULL)`, conversationID, conversationID, conversationID, conversationID, conversationID, conversationID, conversationID).Scan(&out.ReceivedSegments, &out.OriginalTurns, &out.TopicLinkedTurns, &out.UnclassifiedTurns, &out.AmbientTurns, &out.IdentityUnconfirmedTurns)
	if err != nil {
		return ProcessingStatus{}, err
	}
	var analyzed int
	if err = s.db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM notes n WHERE n.conversation_id=? AND NOT `+NoteStalePredicate("n")+`) + (SELECT COUNT(*) FROM turn_topics tt JOIN turns t ON t.id=tt.turn_id WHERE t.conversation_id=? AND `+CurrentTurnPredicate("t")+`)`, conversationID, conversationID).Scan(&analyzed); err != nil {
		return ProcessingStatus{}, err
	}
	out.AnalysisStatus = "pending"
	if analyzed > 0 {
		out.AnalysisStatus = "partial"
	}
	out.IndexingStatus, err = deriveIndexingStatus(ctx, s.db, conversationID)
	if err != nil {
		return ProcessingStatus{}, err
	}
	if err := deriveASRProgress(ctx, s.db, conversationID, &out); err != nil {
		return ProcessingStatus{}, err
	}
	return out, nil
}

// deriveASRProgress reads the ASR window projection. The sync row is written
// by the ingest stage; when it is absent (legacy or CLI imports) the count and
// coverage are derived from the active attachments instead of guessing.
func deriveASRProgress(ctx context.Context, db *sql.DB, conversationID string, out *ProcessingStatus) error {
	out.ASRState = "none"
	var payloadJSON string
	err := db.QueryRowContext(ctx, `SELECT payload_json FROM processing_jobs WHERE stage='asr' AND input_snapshot_id=? ORDER BY created_at DESC, rowid DESC LIMIT 1`, conversationID).Scan(&payloadJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var payload struct {
		ProviderRecordingID string `json:"provider_recording_id"`
		DeviceID            string `json:"device_id"`
		DurationMS          int64  `json:"duration_ms"`
	}
	if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
		return nil
	}
	out.ASRDurationMS = payload.DurationMS
	if payload.ProviderRecordingID == "" {
		return nil
	}
	var state, lastError sql.NullString
	var total, completed, failed, coverage sql.NullInt64
	err = db.QueryRowContext(ctx, `SELECT transcription_state, total_chunks, completed_chunks, failed_chunks, coverage_ms, last_error FROM sync_recordings WHERE recording_id=?`, payload.ProviderRecordingID).
		Scan(&state, &total, &completed, &failed, &coverage, &lastError)
	if err == nil {
		out.ASRState = state.String
		out.ASRExpectedChunks = int(total.Int64)
		out.ASRCompletedChunks = int(completed.Int64)
		out.ASRFailedChunks = int(failed.Int64)
		out.ASRCoverageMS = coverage.Int64
		if lastError.Valid {
			out.ASRLastError = lastError.String
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	recordingID := stableKey("recording", payload.DeviceID, payload.ProviderRecordingID)
	rows, err := db.QueryContext(ctx, `SELECT result_json FROM source_attachments WHERE recording_id=? AND active=1`, recordingID)
	if err != nil {
		return err
	}
	defer rows.Close()
	var intervals [][2]int64
	count := 0
	for rows.Next() {
		var resultJSON string
		if err := rows.Scan(&resultJSON); err != nil {
			return err
		}
		count++
		var result asrinput.Result
		if err := json.Unmarshal([]byte(resultJSON), &result); err != nil {
			continue
		}
		start, end := result.Source.SegmentOffsetMS, result.Source.SegmentOffsetMS+result.Source.SegmentDurationMS
		if end > start {
			intervals = append(intervals, [2]int64{start, end})
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	out.ASRExpectedChunks = count
	out.ASRCompletedChunks = count
	out.ASRCoverageMS = unionIntervals(intervals)
	if count == 0 {
		out.ASRState = "pending"
	} else {
		out.ASRState = "done"
	}
	return nil
}

// unionIntervals returns the length covered by at least one interval.
func unionIntervals(intervals [][2]int64) int64 {
	if len(intervals) == 0 {
		return 0
	}
	ordered := append([][2]int64(nil), intervals...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i][0] == ordered[j][0] {
			return ordered[i][1] < ordered[j][1]
		}
		return ordered[i][0] < ordered[j][0]
	})
	var total int64
	current := ordered[0]
	for _, interval := range ordered[1:] {
		if interval[0] > current[1] {
			total += current[1] - current[0]
			current = interval
			continue
		}
		if interval[1] > current[1] {
			current[1] = interval[1]
		}
	}
	return total + current[1] - current[0]
}

// deriveIndexingStatus maps the latest embed-stage job of a conversation onto
// the index surface. With no embed job at all the index is still pending; a
// succeeded job whose newest attempt skipped indexing (no embedding provider
// configured) reports disabled; an exhausted job reports failed; everything in
// flight stays pending until it lands.
func deriveIndexingStatus(ctx context.Context, db *sql.DB, conversationID string) (string, error) {
	var jobID, jobStatus string
	err := db.QueryRowContext(ctx, `SELECT id, status FROM processing_jobs WHERE stage='embed' AND input_snapshot_id=? ORDER BY created_at DESC, rowid DESC LIMIT 1`, conversationID).Scan(&jobID, &jobStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return "pending", nil
	}
	if err != nil {
		return "", err
	}
	switch jobStatus {
	case "failed":
		return "failed", nil
	case "succeeded":
		var resultRef *string
		if e := db.QueryRowContext(ctx, `SELECT result_ref FROM processing_attempts WHERE job_id=? ORDER BY attempt DESC LIMIT 1`, jobID).Scan(&resultRef); e == nil && resultRef != nil && strings.HasPrefix(*resultRef, "embed:skipped") {
			return "disabled", nil
		}
		return "ready", nil
	default:
		// queued, running, retry_wait and cancelled jobs stay pending until a
		// later embed job lands.
		return "pending", nil
	}
}
