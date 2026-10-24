package memory

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"recordingmcp/internal/asrinput"
)

// ForgetRequest is the preview-bound request to permanently erase local data.
// ExpectedState must be copied from PreviewForget. Turn audio is only deleted
// as a whole file when the preview showed unknown offsets and the caller
// explicitly opts into the broader audio deletion.
type ForgetRequest struct {
	Scope                  string `json:"scope"`
	ConversationID         string `json:"conversation_id,omitempty"`
	RecordingID            string `json:"recording_id,omitempty"`
	TurnID                 string `json:"turn_id,omitempty"`
	ExpectedState          string `json:"expected_state,omitempty"`
	AllowWholeAudioRemoval bool   `json:"allow_whole_audio_removal,omitempty"`
}

// ForgetAudioPreview describes a local audio file which the request would
// affect. Turn offsets are milliseconds in the transcript clock; byte mapping
// is intentionally left unknown unless a trusted mapper supplies it.
type ForgetAudioPreview struct {
	Path         string `json:"path"`
	RecordingID  string `json:"recording_id"`
	StartMS      *int64 `json:"start_ms,omitempty"`
	EndMS        *int64 `json:"end_ms,omitempty"`
	OffsetsKnown bool   `json:"offsets_known"`
	RemoveFile   bool   `json:"remove_file"`
}

type ForgetPreview struct {
	Request                 ForgetRequest        `json:"request"`
	ExpectedState           string               `json:"expected_state"`
	Locator                 string               `json:"locator"`
	TargetTurnID            string               `json:"target_turn_id,omitempty"`
	SourceSegmentID         string               `json:"source_segment_id,omitempty"`
	RecordingIDs            []string             `json:"recording_ids,omitempty"`
	Turns                   int                  `json:"turns"`
	Notes                   int                  `json:"notes"`
	Highlights              int                  `json:"highlights"`
	Attachments             int                  `json:"attachments"`
	Candidates              int                  `json:"candidates"`
	Vectors                 int                  `json:"vectors"`
	Jobs                    int                  `json:"jobs"`
	AudioFiles              []ForgetAudioPreview `json:"audio_files,omitempty"`
	RequiresBroadenedAudio  bool                 `json:"requires_broadened_audio"`
	RequiresExternalCleanup bool                 `json:"requires_external_cleanup"`
	Status                  string               `json:"status,omitempty"`
}

type ForgetFile struct {
	Path       string `json:"path"`
	StartByte  *int64 `json:"start_byte,omitempty"`
	EndByte    *int64 `json:"end_byte,omitempty"`
	RemoveFile bool   `json:"remove_file"`
	Done       bool   `json:"done"`
}

type ForgetProviderFence struct {
	ProviderRecordingID string `json:"provider_recording_id"`
	DeviceID            string `json:"device_id"`
}

type ForgetStatus struct {
	ID             string                `json:"id"`
	Scope          string                `json:"scope"`
	ConversationID string                `json:"conversation_id,omitempty"`
	RecordingID    string                `json:"recording_id,omitempty"`
	RequestedID    string                `json:"requested_id"`
	Status         string                `json:"status"`
	RedactionDone  bool                  `json:"redaction_done"`
	Files          []ForgetFile          `json:"files"`
	ProviderFences []ForgetProviderFence `json:"provider_fences"`
}

type ForgetResult struct {
	ForgetPreview
	TombstoneID      string `json:"tombstone_id"`
	AlreadyCommitted bool   `json:"already_committed"`
	RedactionDone    bool   `json:"redaction_done"`
}

type forgetTarget struct {
	request        ForgetRequest
	conversationID string
	recordingID    string
	sourceSegment  string
	targetID       string
	originalID     string
	segmentIndex   int
	startMS        sql.NullInt64
	endMS          sql.NullInt64
	locator        string
}

type queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func stableForgetLocator(scope, target string) string {
	sum := sha256.Sum256([]byte(scope + "\x00" + target))
	return hex.EncodeToString(sum[:])
}

func resolveForgetTarget(ctx context.Context, q queryer, req ForgetRequest) (forgetTarget, error) {
	t := forgetTarget{request: req}
	switch req.Scope {
	case "conversation":
		if strings.TrimSpace(req.ConversationID) == "" {
			return t, errors.New("conversation_id required")
		}
		t.conversationID = req.ConversationID
		err := q.QueryRowContext(ctx, `SELECT recording_id FROM conversations WHERE id=?`, req.ConversationID).Scan(&t.recordingID)
		if err != nil {
			return t, err
		}
		t.targetID = req.ConversationID
	case "recording":
		if strings.TrimSpace(req.RecordingID) == "" {
			return t, errors.New("recording_id required")
		}
		t.recordingID = req.RecordingID
		if err := q.QueryRowContext(ctx, `SELECT conversation_id FROM recordings WHERE id=?`, req.RecordingID).Scan(&t.conversationID); err != nil {
			return t, err
		}
		t.targetID = req.RecordingID
	case "turn":
		if strings.TrimSpace(req.TurnID) == "" {
			return t, errors.New("turn_id required")
		}
		t.targetID = req.TurnID
		var orig string
		if err := q.QueryRowContext(ctx, `SELECT conversation_id,recording_id,segment_index,global_start_ms,global_end_ms,original_turn_id FROM turns WHERE id=?`, req.TurnID).Scan(&t.conversationID, &t.recordingID, &t.segmentIndex, &t.startMS, &t.endMS, &orig); err != nil {
			return t, err
		}
		t.originalID = orig
		if !t.startMS.Valid || !t.endMS.Valid || t.endMS.Int64 <= t.startMS.Int64 {
			return t, errors.New("granular forget blocked: turn timing is missing")
		}
		_ = q.QueryRowContext(ctx, `SELECT a.segment_id FROM attachment_turns at JOIN source_attachments a ON a.id=at.attachment_id WHERE at.turn_id=? ORDER BY a.active DESC,a.id LIMIT 1`, req.TurnID).Scan(&t.sourceSegment)
	default:
		return t, errors.New("scope must be turn, recording, or conversation")
	}
	t.request.ConversationID = t.conversationID
	t.request.RecordingID = t.recordingID
	t.locator = stableForgetLocator(req.Scope, t.targetID)
	return t, nil
}

// PreviewForget computes a content-free state digest and cleanup preview. The
// digest covers the conversation's current raw and derived state so a commit
// after any concurrent content edit must be previewed again.
func (s *Store) PreviewForget(ctx context.Context, req ForgetRequest) (ForgetPreview, error) {
	t, err := resolveForgetTarget(ctx, s.db, req)
	if err != nil {
		if status, e := s.statusByLocator(ctx, req.Scope, req.TurnID, req.RecordingID, req.ConversationID); e == nil {
			return ForgetPreview{Request: req, Locator: status.ID, Status: status.Status, ExpectedState: "forgotten", RequiresExternalCleanup: true}, nil
		}
		return ForgetPreview{}, err
	}
	return s.previewTarget(ctx, s.db, t)
}

func (s *Store) previewTarget(ctx context.Context, q queryer, t forgetTarget) (ForgetPreview, error) {
	p := ForgetPreview{Request: t.request, Locator: t.locator, TargetTurnID: t.targetID, SourceSegmentID: t.sourceSegment, RequiresExternalCleanup: true}
	if t.request.Scope != "turn" {
		p.TargetTurnID = ""
	}
	p.RecordingIDs, _ = selectedRecordingIDs(ctx, q, t)
	state, err := forgetStateHash(ctx, q, t.conversationID)
	if err != nil {
		return p, err
	}
	p.ExpectedState = state
	where, args := scopeTurnWhere(t)
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM turns t WHERE `+where, args...).Scan(&p.Turns); err != nil {
		return p, err
	}
	if t.request.Scope == "turn" {
		p.Turns = 1
	}
	if err := q.QueryRowContext(ctx, `SELECT COUNT(DISTINCT n.id) FROM notes n JOIN note_evidence e ON e.note_id=n.id JOIN turns t ON t.id=e.turn_id WHERE `+where, args...).Scan(&p.Notes); err != nil {
		return p, err
	}
	if err := countHighlightsForForget(ctx, q, t, &p.Highlights); err != nil {
		return p, err
	}
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM source_attachments a WHERE `+scopeAttachmentWhere(t), scopeAttachmentArgs(t)...).Scan(&p.Attachments); err != nil {
		return p, fmt.Errorf("count attachments: %w", err)
	}
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_candidates WHERE conversation_id=?`, t.conversationID).Scan(&p.Candidates); err != nil {
		return p, err
	}
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_embeddings WHERE conversation_id=?`, t.conversationID).Scan(&p.Vectors); err != nil {
		return p, err
	}
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM processing_jobs j WHERE `+forgetJobScope("j"), t.conversationID, t.conversationID, t.conversationID).Scan(&p.Jobs); err != nil {
		return p, err
	}
	p.AudioFiles, err = previewAudioFiles(ctx, q, t)
	if err != nil {
		return p, err
	}
	for _, file := range p.AudioFiles {
		if t.request.Scope == "turn" && !file.OffsetsKnown {
			p.RequiresBroadenedAudio = true
		}
	}
	p.Request.ExpectedState = p.ExpectedState
	return p, nil
}

func scopeTurnWhere(t forgetTarget) (string, []any) {
	switch t.request.Scope {
	case "conversation":
		return "t.conversation_id=?", []any{t.conversationID}
	case "recording":
		return "t.recording_id=?", []any{t.recordingID}
	default:
		return "t.id=?", []any{t.targetID}
	}
}

func scopeAttachmentWhere(t forgetTarget) string {
	switch t.request.Scope {
	case "conversation":
		return "a.conversation_id=?"
	case "recording":
		return "a.recording_id=?"
	default:
		if t.sourceSegment != "" {
			return "a.recording_id=? AND a.segment_id=?"
		}
		return "id IN (SELECT attachment_id FROM attachment_turns WHERE turn_id=?)"
	}
}

func scopeAttachmentArgs(t forgetTarget) []any {
	if t.request.Scope == "conversation" {
		return []any{t.conversationID}
	}
	if t.request.Scope == "recording" {
		return []any{t.recordingID}
	}
	if t.sourceSegment != "" {
		return []any{t.recordingID, t.sourceSegment}
	}
	return []any{t.targetID}
}

func selectedRecordingIDs(ctx context.Context, q queryer, t forgetTarget) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT id FROM recordings WHERE `+map[bool]string{true: "conversation_id", false: "id"}[t.request.Scope == "conversation"]+`=? ORDER BY id`, map[bool]any{true: t.conversationID, false: t.recordingID}[t.request.Scope == "conversation"])
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// queryDigest streams ordered SQLite values into a hash without materializing
// content in the API response.
func queryDigest(ctx context.Context, q queryer, h interface{ Write([]byte) (int, error) }, label, query string, args ...any) error {
	_, _ = h.Write([]byte(label + "\x00"))
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	values := make([]any, len(cols))
	dest := make([]any, len(cols))
	for i := range values {
		dest[i] = &values[i]
	}
	for rows.Next() {
		if err := rows.Scan(dest...); err != nil {
			return err
		}
		for _, value := range values {
			switch v := value.(type) {
			case nil:
				_, _ = h.Write([]byte{0})
			case []byte:
				_, _ = h.Write([]byte{1})
				_, _ = h.Write([]byte(fmt.Sprintf("%d:", len(v))))
				_, _ = h.Write(v)
			case string:
				_, _ = h.Write([]byte{2})
				_, _ = h.Write([]byte(fmt.Sprintf("%d:", len(v))))
				_, _ = h.Write([]byte(v))
			default:
				_, _ = h.Write([]byte{3})
				_, _ = h.Write([]byte(fmt.Sprintf("%T:%v\x00", v, v)))
			}
		}
	}
	return rows.Err()
}

func forgetStateHash(ctx context.Context, q queryer, conversationID string) (string, error) {
	h := sha256.New()
	queries := []struct{ label, sql string }{
		{"conversation", `SELECT * FROM conversations WHERE id=?`},
		{"recordings", `SELECT * FROM recordings WHERE conversation_id=? ORDER BY id`},
		{"turns", `SELECT * FROM turns WHERE conversation_id=? ORDER BY id`},
		{"attachments", `SELECT * FROM source_attachments WHERE conversation_id=? ORDER BY id`},
		{"notes", `SELECT * FROM notes WHERE conversation_id=? ORDER BY id`},
		{"highlights", `SELECT * FROM memory_highlights WHERE conversation_id=? ORDER BY id`},
		{"highlight_requests", `SELECT request_id,conversation_id,recording_id,anchor_kind,start_ms,end_ms,turn_id,payload_sha256,highlight_id,status,created_at_ms FROM memory_highlight_requests WHERE conversation_id=? ORDER BY request_id`},
		{"note_evidence", `SELECT ne.* FROM note_evidence ne JOIN notes n ON n.id=ne.note_id WHERE n.conversation_id=? ORDER BY ne.note_id,ne.turn_id`},
		{"history", `SELECT * FROM annotation_history WHERE conversation_id=? ORDER BY annotation_version`},
		{"candidates", `SELECT * FROM memory_candidates WHERE conversation_id=? ORDER BY id`},
		{"publication_runs", `SELECT * FROM publication_runs WHERE conversation_id=? ORDER BY id`},
		{"embeddings", `SELECT * FROM memory_embeddings WHERE conversation_id=? ORDER BY kind,artifact_id,provider_id,model_id`},
		{"analysis", `SELECT * FROM analysis_coverage WHERE conversation_id=? ORDER BY turn_id,processor_version`},
		{"topic_conversations", `SELECT * FROM topic_conversations WHERE conversation_id=? ORDER BY topic_id`},
		{"turn_topics", `SELECT tt.* FROM turn_topics tt JOIN turns t ON t.id=tt.turn_id WHERE t.conversation_id=? ORDER BY tt.turn_id,tt.topic_id`},
		{"speaker_bindings", `SELECT * FROM speaker_bindings WHERE recording_id IN (SELECT id FROM recordings WHERE conversation_id=?) ORDER BY id`},
		{"speaker_events", `SELECT e.* FROM speaker_binding_events e JOIN speaker_bindings b ON b.id=e.binding_id WHERE b.recording_id IN (SELECT id FROM recordings WHERE conversation_id=?) ORDER BY e.id`},
		{"links", `SELECT * FROM memory_links WHERE from_id IN (SELECT id FROM turns WHERE conversation_id=? UNION SELECT id FROM notes WHERE conversation_id=?) OR to_id IN (SELECT id FROM turns WHERE conversation_id=? UNION SELECT id FROM notes WHERE conversation_id=?) ORDER BY id`},
		{"jobs", `SELECT j.id,j.stage,j.input_snapshot_id,j.idempotency_key,j.payload_json FROM processing_jobs j WHERE ` + forgetJobScope("j") + ` ORDER BY j.id`},
		{"attempts", `SELECT a.id,a.job_id,a.attempt,a.error_json,a.result_ref,a.usage_json FROM processing_attempts a JOIN processing_jobs j ON j.id=a.job_id WHERE ` + forgetJobScope("j") + ` ORDER BY a.id`},
		{"decision_runs", `SELECT * FROM decision_runs WHERE input_snapshot_id IN (SELECT id FROM source_attachments WHERE conversation_id=?) OR input_snapshot_id=? OR job_id IN (SELECT j.id FROM processing_jobs j WHERE ` + forgetJobScope("j") + `) ORDER BY id`},
		{"sync_recordings", `SELECT * FROM sync_recordings WHERE recording_id IN (SELECT json_extract(source_json,'$.recording_id') FROM source_attachments WHERE conversation_id=?) ORDER BY recording_id`},
		{"sync_events", `SELECT * FROM sync_events WHERE recording_id IN (SELECT json_extract(source_json,'$.recording_id') FROM source_attachments WHERE conversation_id=?) ORDER BY event_id`},
		{"sync_chunks", `SELECT * FROM sync_chunks WHERE recording_id IN (SELECT json_extract(source_json,'$.recording_id') FROM source_attachments WHERE conversation_id=?) ORDER BY recording_id,seq`},
	}
	for _, item := range queries {
		args := []any{conversationID}
		if strings.Count(item.sql, "?") > 1 {
			for len(args) < strings.Count(item.sql, "?") {
				args = append(args, conversationID)
			}
		}
		if item.label == "jobs" || item.label == "attempts" || item.label == "decision_runs" {
			args = make([]any, strings.Count(item.sql, "?"))
			for i := range args {
				args[i] = conversationID
			}
		}
		if err := queryDigest(ctx, q, h, item.label, item.sql, args...); err != nil {
			return "", fmt.Errorf("forget digest %s: %w", item.label, err)
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func previewAudioFiles(ctx context.Context, q queryer, t forgetTarget) ([]ForgetAudioPreview, error) {
	var out []ForgetAudioPreview
	ids, _, err := providerSources(ctx, q, t)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		var stitched sql.NullString
		var samples sql.NullInt64
		if err := q.QueryRowContext(ctx, `SELECT stitched_path,total_samples FROM sync_recordings WHERE recording_id=?`, id).Scan(&stitched, &samples); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if stitched.Valid && stitched.String != "" {
			f := ForgetAudioPreview{Path: stitched.String, RecordingID: id, RemoveFile: t.request.Scope != "turn"}
			if t.request.Scope == "turn" {
				start, end := t.startMS.Int64, t.endMS.Int64
				f.StartMS = &start
				f.EndMS = &end
				if samples.Valid && samples.Int64*2 >= end*32 {
					f.OffsetsKnown = true
				}
			}
			out = append(out, f)
		}
		rows, e := q.QueryContext(ctx, `SELECT seq,bytes,file_path FROM sync_chunks WHERE recording_id=? ORDER BY seq`, id)
		if e != nil {
			return nil, e
		}
		type chunk struct {
			seq   int
			bytes int64
			path  string
		}
		var chunks []chunk
		for rows.Next() {
			var c chunk
			if e = rows.Scan(&c.seq, &c.bytes, &c.path); e != nil {
				rows.Close()
				return nil, e
			}
			chunks = append(chunks, c)
		}
		if e = rows.Close(); e != nil {
			return nil, e
		}
		contiguous := len(chunks) > 0
		for i, c := range chunks {
			if c.seq != i || c.bytes <= 0 || c.bytes%32 != 0 {
				contiguous = false
			}
		}
		var off int64
		for _, c := range chunks {
			f := ForgetAudioPreview{Path: c.path, RecordingID: id, RemoveFile: t.request.Scope != "turn"}
			if t.request.Scope == "turn" && contiguous {
				a, b := t.startMS.Int64*32, t.endMS.Int64*32
				lo, hi := off, off+c.bytes
				if lo < a {
					lo = a
				}
				if hi > b {
					hi = b
				}
				if lo < hi {
					loMS, hiMS := lo/32, (hi+31)/32
					f.StartMS = &loMS
					f.EndMS = &hiMS
					f.OffsetsKnown = true
				} else {
					off += c.bytes
					continue
				}
			}
			if f.Path != "" {
				out = append(out, f)
			}
			off += c.bytes
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path == out[j].Path {
			return out[i].RecordingID < out[j].RecordingID
		}
		return out[i].Path < out[j].Path
	})
	return out, nil
}

func providerSources(ctx context.Context, q queryer, t forgetTarget) ([]string, map[string]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT source_json FROM source_attachments a WHERE `+scopeAttachmentWhere(t), scopeAttachmentArgs(t)...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	ids, devices := map[string]bool{}, map[string]string{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, nil, err
		}
		var src asrinput.Source
		if err := json.Unmarshal([]byte(raw), &src); err != nil {
			continue
		}
		if src.RecordingID != "" {
			ids[src.RecordingID] = true
			devices[src.RecordingID] = src.DeviceID
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	var out []string
	for id := range ids {
		out = append(out, id)
	}
	sort.Strings(out)
	return out, devices, nil
}

// CommitForget inserts a durable fence and cleanup manifest before removing
// local database content. If redaction fails, ResumeForget continues from that
// fence without accepting more source writes.
func (s *Store) CommitForget(ctx context.Context, req ForgetRequest) (ForgetResult, error) {
	if status, err := s.statusByLocator(ctx, req.Scope, req.TurnID, req.RecordingID, req.ConversationID); err == nil {
		return s.resumeResult(ctx, status.ID, true)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return ForgetResult{}, err
	}
	if strings.TrimSpace(req.ExpectedState) == "" {
		return ForgetResult{}, errors.New("expected_state from a fresh forget preview is required")
	}
	t, err := resolveForgetTarget(ctx, s.db, req)
	if err != nil {
		return ForgetResult{}, err
	}
	p, err := s.previewTarget(ctx, s.db, t)
	if err != nil {
		return ForgetResult{}, err
	}
	if subtleHashMismatch(req.ExpectedState, p.ExpectedState) {
		return ForgetResult{}, errors.New("forget preview is stale; request a fresh preview")
	}
	if p.RequiresBroadenedAudio && !req.AllowWholeAudioRemoval {
		return ForgetResult{}, errors.New("audio offsets cannot be mapped safely; explicitly broaden preview to remove the whole local audio file")
	}
	result := ForgetResult{ForgetPreview: p}
	result.Request.AllowWholeAudioRemoval = req.AllowWholeAudioRemoval
	result.ForgetPreview.Request = result.Request
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	// Re-resolve and rehash under the write transaction: the preview digest is a
	// compare-and-swap guard against edits between preview and commit.
	current, err := resolveForgetTarget(ctx, tx, req)
	if err != nil {
		return result, err
	}
	currentHash, err := forgetStateHash(ctx, tx, current.conversationID)
	if err != nil {
		return result, err
	}
	if subtleHashMismatch(req.ExpectedState, currentHash) {
		return result, errors.New("forget preview is stale; request a fresh preview")
	}
	var existing string
	err = tx.QueryRowContext(ctx, `SELECT id FROM forget_tombstones WHERE locator_key=?`, current.locator).Scan(&existing)
	if err == nil {
		_ = tx.Rollback()
		return s.resumeResult(ctx, existing, true)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return result, err
	}
	tombstoneID := "forget:" + current.locator
	_, err = tx.ExecContext(ctx, `INSERT INTO forget_tombstones(id,scope,conversation_id,recording_id,source_segment_id,segment_index,requested_id,start_ms,end_ms,locator_key,expected_state_hash,redaction_done,status,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,0,'pending_external_cleanup',?)`, tombstoneID, current.request.Scope, current.conversationID, current.recordingID, current.sourceSegment, current.segmentIndex, current.targetID, nullableInt(current.startMS), nullableInt(current.endMS), current.locator, currentHash, time.Now().UnixMilli())
	if err != nil {
		return result, err
	}
	providerIDs, devices, err := providerSources(ctx, tx, current)
	if err != nil {
		return result, err
	}
	for _, providerID := range providerIDs {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO forget_provider_fences(tombstone_id,provider_recording_id,device_id,created_at) VALUES(?,?,?,?)`, tombstoneID, providerID, devices[providerID], time.Now().UnixMilli()); err != nil {
			return result, err
		}
	}
	if err := persistFileManifest(ctx, tx, tombstoneID, current, req.AllowWholeAudioRemoval); err != nil {
		return result, err
	}
	if err := tx.Commit(); err != nil {
		return result, err
	}
	result.TombstoneID = tombstoneID
	result.Status = "pending_external_cleanup"
	if err := s.redactForgetDatabase(ctx, tombstoneID); err != nil {
		return result, err
	}
	status, err := s.ForgetStatus(ctx, tombstoneID)
	if err != nil {
		return result, err
	}
	result.RedactionDone = status.RedactionDone
	return result, nil
}

func subtleHashMismatch(a, b string) bool {
	if len(a) != len(b) {
		return true
	}
	var diff byte
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	return diff != 0
}

func nullableInt(n sql.NullInt64) any {
	if !n.Valid {
		return nil
	}
	return n.Int64
}

func persistFileManifest(ctx context.Context, tx *sql.Tx, tombstoneID string, t forgetTarget, allowWhole bool) error {
	files, err := previewAudioFiles(ctx, tx, t)
	if err != nil {
		return err
	}
	for _, f := range files {
		remove := t.request.Scope != "turn" || (allowWhole && !f.OffsetsKnown)
		var a, b any
		if remove {
			a = int64(0)
			b = int64(0)
		} else if f.OffsetsKnown && f.StartMS != nil && f.EndMS != nil {
			a = *f.StartMS * 32
			b = *f.EndMS * 32
		} else {
			return errors.New("audio offsets cannot be mapped safely")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO forget_files(tombstone_id,path,start_byte,end_byte,remove_file,done) VALUES(?,?,?,?,?,0)`, tombstoneID, f.Path, a, b, boolInt(remove)); err != nil {
			return err
		}
	}
	return nil
}

// ResumeForget completes the local database redaction for a previously
// persisted tombstone. It is safe to call repeatedly.
func (s *Store) ResumeForget(ctx context.Context, tombstoneID string) error {
	return s.redactForgetDatabase(ctx, tombstoneID)
}

func (s *Store) resumeResult(ctx context.Context, tombstoneID string, already bool) (ForgetResult, error) {
	if err := s.redactForgetDatabase(ctx, tombstoneID); err != nil {
		return ForgetResult{TombstoneID: tombstoneID, AlreadyCommitted: already, Status: "pending_external_cleanup"}, err
	}
	status, err := s.ForgetStatus(ctx, tombstoneID)
	if err != nil {
		return ForgetResult{}, err
	}
	return resultFromStatus(status), nil
}

func resultFromStatus(s ForgetStatus) ForgetResult {
	return ForgetResult{TombstoneID: s.ID, AlreadyCommitted: true, RedactionDone: s.RedactionDone,
		ForgetPreview: ForgetPreview{Request: ForgetRequest{Scope: s.Scope, ConversationID: s.ConversationID, RecordingID: s.RecordingID, TurnID: func() string {
			if s.Scope == "turn" {
				return s.RequestedID
			}
			return ""
		}()}, Locator: s.ID, Status: s.Status}}
}

func (s *Store) statusByLocator(ctx context.Context, scope, turnID, recordingID, conversationID string) (ForgetStatus, error) {
	var target string
	switch scope {
	case "turn":
		target = turnID
	case "recording":
		target = recordingID
	case "conversation":
		target = conversationID
	default:
		return ForgetStatus{}, errors.New("invalid forget scope")
	}
	if target == "" {
		return ForgetStatus{}, sql.ErrNoRows
	}
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM forget_tombstones WHERE scope=? AND requested_id=?`, scope, target).Scan(&id)
	if err != nil {
		return ForgetStatus{}, err
	}
	return s.ForgetStatus(ctx, id)
}

func (s *Store) ForgetStatus(ctx context.Context, tombstoneID string) (ForgetStatus, error) {
	var status ForgetStatus
	var done int
	err := s.db.QueryRowContext(ctx, `SELECT id,scope,conversation_id,recording_id,requested_id,status,redaction_done FROM forget_tombstones WHERE id=?`, tombstoneID).Scan(&status.ID, &status.Scope, &status.ConversationID, &status.RecordingID, &status.RequestedID, &status.Status, &done)
	if err != nil {
		return status, err
	}
	status.RedactionDone = done == 1
	rows, err := s.db.QueryContext(ctx, `SELECT path,start_byte,end_byte,remove_file,done FROM forget_files WHERE tombstone_id=? ORDER BY path`, tombstoneID)
	if err != nil {
		return status, err
	}
	for rows.Next() {
		var f ForgetFile
		var start, end sql.NullInt64
		var remove, finished int
		if err := rows.Scan(&f.Path, &start, &end, &remove, &finished); err != nil {
			rows.Close()
			return status, err
		}
		if start.Valid {
			v := start.Int64
			f.StartByte = &v
		}
		if end.Valid {
			v := end.Int64
			f.EndByte = &v
		}
		f.RemoveFile = remove == 1
		f.Done = finished == 1
		status.Files = append(status.Files, f)
	}
	if err := rows.Close(); err != nil {
		return status, err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT provider_recording_id,device_id FROM forget_provider_fences WHERE tombstone_id=? ORDER BY provider_recording_id,device_id`, tombstoneID)
	if err != nil {
		return status, err
	}
	for rows.Next() {
		var f ForgetProviderFence
		if err := rows.Scan(&f.ProviderRecordingID, &f.DeviceID); err != nil {
			rows.Close()
			return status, err
		}
		status.ProviderFences = append(status.ProviderFences, f)
	}
	if err := rows.Close(); err != nil {
		return status, err
	}
	return status, nil
}

// MarkForgetFileDone records completion of one manifest item after the trusted
// audio coordinator has safely removed or rewritten the file.
func (s *Store) MarkForgetFileDone(ctx context.Context, tombstoneID, path string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE forget_files SET done=1 WHERE tombstone_id=? AND path=?`, tombstoneID, path)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return errors.New("forget file manifest entry not found")
	}
	return nil
}

// CompleteForget is called by the external cleanup coordinator only after
// plane/index/backup cleanup. It refuses completion until local redaction and
// every registered file manifest item are complete.
func (s *Store) CompleteForget(ctx context.Context, tombstoneID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var done int
	if err := tx.QueryRowContext(ctx, `SELECT redaction_done FROM forget_tombstones WHERE id=?`, tombstoneID).Scan(&done); err != nil {
		return err
	}
	if done != 1 {
		return errors.New("forget database redaction is pending")
	}
	var pending int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM forget_files WHERE tombstone_id=? AND done=0`, tombstoneID).Scan(&pending); err != nil {
		return err
	}
	if pending != 0 {
		return errors.New("forget audio cleanup is pending")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE forget_tombstones SET status='complete' WHERE id=?`, tombstoneID); err != nil {
		return err
	}
	return tx.Commit()
}

// ProviderFenceExists is the content-free anti-resync lookup used by sync.
func (s *Store) ProviderFenceExists(ctx context.Context, providerRecordingID, deviceID string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM forget_provider_fences WHERE provider_recording_id=? AND (device_id=? OR device_id='')`, providerRecordingID, deviceID).Scan(&n)
	return n > 0, err
}

func (s *Store) GuardForgottenSource(ctx context.Context, recordingID, sourceSegmentID string) error {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM forget_tombstones WHERE recording_id=? AND (scope IN ('recording','conversation') OR (scope='turn' AND source_segment_id=?))`, recordingID, sourceSegmentID).Scan(&n)
	if err != nil {
		return err
	}
	if n > 0 {
		return errors.New("source is permanently forgotten")
	}
	return nil
}

// GuardForgotten retains the older segment-index guard for callers which have
// not yet been upgraded to stable source segment IDs.
func (s *Store) GuardForgotten(ctx context.Context, recordingID string, segmentIndex int) error {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM forget_tombstones WHERE recording_id=? AND (scope IN ('recording','conversation') OR (scope='turn' AND segment_index=?))`, recordingID, segmentIndex).Scan(&n)
	if err != nil {
		return err
	}
	if n > 0 {
		return errors.New("source is permanently forgotten")
	}
	return nil
}

// SuppressAINote suppresses one note kind from a stable source segment. It
// deletes only derived annotations and keeps the original source turns.
func (s *Store) SuppressAINote(ctx context.Context, recordingID, sourceSegmentID, kind string) error {
	if strings.TrimSpace(recordingID) == "" || strings.TrimSpace(sourceSegmentID) == "" || strings.TrimSpace(kind) == "" {
		return errors.New("recording_id, source_segment_id, and kind required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO ai_note_suppressions(recording_id,source_segment_id,kind,created_at) VALUES(?,?,?,?)`, recordingID, sourceSegmentID, kind, time.Now().UnixMilli()); err != nil {
		return err
	}
	noteIDs, err := idsFromQuery(ctx, tx, `SELECT DISTINCT n.id FROM notes n JOIN note_evidence e ON e.note_id=n.id JOIN turns t ON t.id=e.turn_id JOIN attachment_turns at ON at.turn_id=t.id JOIN source_attachments a ON a.id=at.attachment_id WHERE n.kind=? AND t.recording_id=? AND a.segment_id=?`, kind, recordingID, sourceSegmentID)
	if err != nil {
		return err
	}
	turnIDs, err := idsFromQuery(ctx, tx, `SELECT DISTINCT t.id FROM turns t JOIN attachment_turns at ON at.turn_id=t.id JOIN source_attachments a ON a.id=at.attachment_id WHERE t.recording_id=? AND a.segment_id=?`, recordingID, sourceSegmentID)
	if err != nil {
		return err
	}
	candidateIDs, err := idsFromQuery(ctx, tx, `SELECT DISTINCT c.id FROM memory_candidates c,json_each(c.evidence_turn_ids_json) e JOIN turns t ON t.id=e.value JOIN attachment_turns at ON at.turn_id=t.id JOIN source_attachments a ON a.id=at.attachment_id WHERE c.kind=? AND t.recording_id=? AND a.segment_id=?`, kind, recordingID, sourceSegmentID)
	if err != nil {
		return err
	}
	if err := scrubAnnotationCopies(ctx, tx, noteIDs, turnIDs, false); err != nil {
		return err
	}
	if err := deleteIDs(ctx, tx, "note_evidence", "note_id", noteIDs); err != nil {
		return err
	}
	if err := deleteIDs(ctx, tx, "memory_embeddings", "artifact_id", noteIDs); err != nil {
		return err
	}
	if err := deleteIDs(ctx, tx, "derived_artifact_sources", "artifact_id", noteIDs); err != nil {
		return err
	}
	if err := deleteIDs(ctx, tx, "derived_artifacts", "artifact_id", noteIDs); err != nil {
		return err
	}
	if err := deleteIDs(ctx, tx, "memory_links", "from_id", noteIDs); err != nil {
		return err
	}
	if err := deleteIDs(ctx, tx, "memory_links", "to_id", noteIDs); err != nil {
		return err
	}
	if err := deleteIDs(ctx, tx, "memory_candidates", "id", candidateIDs); err != nil {
		return err
	}
	if err := deleteIDs(ctx, tx, "publication_runs", "id", []string{}); err != nil {
		return err
	}
	if err := deleteIDs(ctx, tx, "notes", "id", noteIDs); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) IsAINoteSuppressed(ctx context.Context, recordingID, sourceSegmentID, kind string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ai_note_suppressions WHERE recording_id=? AND source_segment_id=? AND kind=?`, recordingID, sourceSegmentID, kind).Scan(&n)
	return n > 0, err
}

// ---- resumable DB redaction implementation ----

func (s *Store) redactForgetDatabase(ctx context.Context, tombstoneID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var t forgetTarget
	var scope, conv, rec, segmentID, requested, locator string
	var segment int
	var start, end sql.NullInt64
	var redacted int
	err = tx.QueryRowContext(ctx, `SELECT scope,conversation_id,recording_id,source_segment_id,segment_index,requested_id,start_ms,end_ms,locator_key,redaction_done FROM forget_tombstones WHERE id=?`, tombstoneID).Scan(&scope, &conv, &rec, &segmentID, &segment, &requested, &start, &end, &locator, &redacted)
	if err != nil {
		return err
	}
	if redacted == 1 {
		return tx.Commit()
	}
	t = forgetTarget{request: ForgetRequest{Scope: scope, ConversationID: conv, RecordingID: rec, TurnID: func() string {
		if scope == "turn" {
			return requested
		}
		return ""
	}()}, conversationID: conv, recordingID: rec, sourceSegment: segmentID, targetID: requested, segmentIndex: segment, startMS: start, endMS: end, locator: locator}
	turnIDs, err := idsFromQuery(ctx, tx, turnSelectionSQL(t), turnSelectionArgs(t)...)
	if err != nil {
		return err
	}
	attachWhere := scopeAttachmentWhere(t)
	attachArgs := scopeAttachmentArgs(t)
	if scope == "turn" && segmentID != "" {
		attachWhere = "a.recording_id=? AND a.segment_id=?"
		attachArgs = []any{rec, segmentID}
	}
	attachmentIDs, err := idsFromQuery(ctx, tx, `SELECT a.id FROM source_attachments a WHERE `+attachWhere, attachArgs...)
	if err != nil {
		return err
	}
	noteIDs, err := idsFromQuery(ctx, tx, noteSelectionSQL(t, turnIDs), noteSelectionArgs(t, turnIDs)...)
	if err != nil {
		return err
	}
	candidateIDs, err := idsFromQuery(ctx, tx, candidateSelectionSQL(t, turnIDs), candidateSelectionArgs(t, turnIDs)...)
	if err != nil {
		return err
	}
	jobIDs, err := idsFromQuery(ctx, tx, `SELECT j.id FROM processing_jobs j WHERE `+forgetJobScope("j"), conv, conv, conv)
	if err != nil {
		return err
	}
	// Add jobs for attachments even when a legacy snapshot has no conversation ID.
	for _, aid := range attachmentIDs {
		more, e := idsFromQuery(ctx, tx, `SELECT id FROM processing_jobs WHERE input_snapshot_id=?`, aid)
		if e != nil {
			return e
		}
		jobIDs = append(jobIDs, more...)
	}
	jobIDs = uniqueStrings(jobIDs)
	if err := scrubAnnotationCopies(ctx, tx, noteIDs, turnIDs, false); err != nil {
		return err
	}
	originalTurnIDs, err := idsFromQuery(ctx, tx, `SELECT original_turn_id FROM turns WHERE id IN (`+placeholdersOrFalse(turnIDs)+`)`, stringsToAny(turnIDs)...)
	if err != nil {
		return err
	}
	if err := scrubTranscriptSnapshotIDs(ctx, tx, conv, originalTurnIDs); err != nil {
		return err
	}
	if scope == "turn" {
		if err := scrubAttachmentSnapshots(ctx, tx, t, attachmentIDs); err != nil {
			return err
		}
	}
	// Destroy all text-bearing decision and attempt payloads for affected jobs.
	if err := deleteIDs(ctx, tx, "decision_answers", "run_id", idsForRuns(ctx, tx, jobIDs, attachmentIDs, conv)); err != nil {
		return err
	}
	if err := deleteDecisionRuns(ctx, tx, jobIDs, attachmentIDs, conv, scope); err != nil {
		return err
	}
	if err := scrubAttempts(ctx, tx, jobIDs); err != nil {
		return err
	}
	if scope != "turn" {
		if err := deleteIDs(ctx, tx, "speaker_binding_events", "binding_id", idsForBindings(ctx, tx, rec, conv)); err != nil {
			return err
		}
		if err := deleteBindings(ctx, tx, rec, conv); err != nil {
			return err
		}
	} else {
		if err := deleteSpeakerEvidence(ctx, tx, requested); err != nil {
			return err
		}
	}
	if err := deleteIDs(ctx, tx, "note_evidence", "turn_id", turnIDs); err != nil {
		return err
	}
	if err := deleteIDs(ctx, tx, "turn_topics", "turn_id", turnIDs); err != nil {
		return err
	}
	if err := deleteIDs(ctx, tx, "turn_classes", "turn_id", turnIDs); err != nil {
		return err
	}
	if err := deleteIDs(ctx, tx, "analysis_coverage", "turn_id", turnIDs); err != nil {
		return err
	}
	if err := deleteEmbeddings(ctx, tx, turnIDs, noteIDs, conv, scope); err != nil {
		return err
	}
	if err := deleteLinks(ctx, tx, turnIDs, noteIDs, conv, scope); err != nil {
		return err
	}
	if err := deleteArtifacts(ctx, tx, noteIDs, candidateIDs, conv, scope); err != nil {
		return err
	}
	if err := deleteIDs(ctx, tx, "notes", "id", noteIDs); err != nil {
		return err
	}
	if err := forgetHighlights(ctx, tx, t); err != nil {
		return err
	}
	if err := deleteIDs(ctx, tx, "memory_candidates", "id", candidateIDs); err != nil {
		return err
	}
	if err := scrubJobs(ctx, tx, jobIDs); err != nil {
		return err
	}
	if scope == "turn" {
		if err := deleteIDs(ctx, tx, "attachment_turns", "turn_id", turnIDs); err != nil {
			return err
		}
	} else {
		if err := deleteIDs(ctx, tx, "derived_artifact_sources", "attachment_id", attachmentIDs); err != nil {
			return err
		}
		if err := deleteIDs(ctx, tx, "attachment_turns", "attachment_id", attachmentIDs); err != nil {
			return err
		}
	}
	if err := deleteIDs(ctx, tx, "turns", "id", turnIDs); err != nil {
		return err
	}
	if scope != "turn" {
		if err := deleteIDs(ctx, tx, "source_attachments", "id", attachmentIDs); err != nil {
			return err
		}
		if err := scrubBroadConversation(ctx, tx, t); err != nil {
			return err
		}
		if err := scrubSyncLedgers(ctx, tx, tombstoneID); err != nil {
			return err
		}
	} else {
		if err := scrubSyncLedgers(ctx, tx, tombstoneID); err != nil {
			return err
		}
	}
	if scope != "conversation" {
		if err := bumpForgetAnnotationVersion(ctx, tx, conv, locator); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE forget_tombstones SET redaction_done=1 WHERE id=?`, tombstoneID); err != nil {
		return err
	}
	return tx.Commit()
}

func turnSelectionSQL(t forgetTarget) string {
	where, _ := scopeTurnWhere(t)
	return `SELECT t.id FROM turns t WHERE ` + where
}
func turnSelectionArgs(t forgetTarget) []any { _, args := scopeTurnWhere(t); return args }

func idsFromQuery(ctx context.Context, q queryer, sqlText string, args ...any) ([]string, error) {
	rows, err := q.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func uniqueStrings(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

func placeholders(ids []string) string { return strings.TrimRight(strings.Repeat("?,", len(ids)), ",") }

func deleteIDs(ctx context.Context, tx *sql.Tx, table, column string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	// Table and column names are constants at all call sites; only values are bound.
	_, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE `+column+` IN (`+placeholders(ids)+`)`, stringsToAny(ids)...)
	return err
}
func stringsToAny(in []string) []any {
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}

func noteSelectionSQL(t forgetTarget, turnIDs []string) string {
	if t.request.Scope == "conversation" {
		return `SELECT id FROM notes WHERE conversation_id=?`
	}
	if len(turnIDs) == 0 {
		return `SELECT id FROM notes WHERE 1=0`
	}
	return `SELECT DISTINCT note_id FROM note_evidence WHERE turn_id IN (` + placeholders(turnIDs) + `)`
}
func noteSelectionArgs(t forgetTarget, turnIDs []string) []any {
	if t.request.Scope == "conversation" {
		return []any{t.conversationID}
	}
	return stringsToAny(turnIDs)
}
func candidateSelectionSQL(t forgetTarget, turnIDs []string) string {
	if t.request.Scope == "conversation" {
		return `SELECT id FROM memory_candidates WHERE conversation_id=?`
	}
	if len(turnIDs) == 0 {
		return `SELECT id FROM memory_candidates WHERE 1=0`
	}
	return `SELECT DISTINCT c.id FROM memory_candidates c,json_each(c.evidence_turn_ids_json) e WHERE e.value IN (` + placeholders(turnIDs) + `)`
}
func candidateSelectionArgs(t forgetTarget, turnIDs []string) []any {
	if t.request.Scope == "conversation" {
		return []any{t.conversationID}
	}
	return stringsToAny(turnIDs)
}

func scrubAnnotationCopies(ctx context.Context, tx *sql.Tx, noteIDs, turnIDs []string, clearAll bool) error {
	if clearAll {
		return nil
	}
	var convs []struct{ id, title, summary, raw string }
	rows, err := tx.QueryContext(ctx, `SELECT id,title,summary,annotations_json FROM conversations WHERE id IN (SELECT conversation_id FROM turns WHERE id IN (`+placeholdersOrFalse(turnIDs)+`))`, stringsToAny(turnIDs)...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var r struct{ id, title, summary, raw string }
		if err := rows.Scan(&r.id, &r.title, &r.summary, &r.raw); err != nil {
			rows.Close()
			return err
		}
		convs = append(convs, r)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	noteSet, turnSet := stringSet(noteIDs), stringSet(turnIDs)
	originalIDs, err := idsFromQuery(ctx, tx, `SELECT original_turn_id FROM turns WHERE id IN (`+placeholdersOrFalse(turnIDs)+`)`, stringsToAny(turnIDs)...)
	if err != nil {
		return err
	}
	for _, id := range originalIDs {
		turnSet[id] = true
	}
	for _, c := range convs {
		raw, e := scrubAnnotationsJSON(c.raw, noteSet, turnSet)
		if e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, `UPDATE conversations SET summary='',annotations_json=? WHERE id=?`, raw, c.id); e != nil {
			return e
		}
	}
	rows, err = tx.QueryContext(ctx, `SELECT conversation_id,annotation_version,summary,annotations_json FROM annotation_history WHERE conversation_id IN (SELECT conversation_id FROM turns WHERE id IN (`+placeholdersOrFalse(turnIDs)+`))`, stringsToAny(turnIDs)...)
	if err != nil {
		return err
	}
	type hist struct{ conv, version, raw string }
	var hs []hist
	for rows.Next() {
		var conv, version, summary, raw string
		if err := rows.Scan(&conv, &version, &summary, &raw); err != nil {
			rows.Close()
			return err
		}
		sanitized, e := scrubAnnotationsJSON(raw, noteSet, turnSet)
		if e != nil {
			rows.Close()
			return e
		}
		hs = append(hs, hist{conv, version, sanitized})
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, h := range hs {
		if _, err := tx.ExecContext(ctx, `UPDATE annotation_history SET summary='',annotations_json=? WHERE conversation_id=? AND annotation_version=?`, h.raw, h.conv, h.version); err != nil {
			return err
		}
	}
	return nil
}

func placeholdersOrFalse(ids []string) string {
	if len(ids) == 0 {
		return "SELECT NULL WHERE 0"
	}
	return placeholders(ids)
}
func stringSet(values []string) map[string]bool {
	m := map[string]bool{}
	for _, v := range values {
		m[v] = true
	}
	return m
}

func scrubAnnotationsJSON(raw string, noteIDs, turnIDs map[string]bool) (string, error) {
	var a Annotations
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		return "{}", nil
	}
	kept := a.Notes[:0]
	for _, note := range a.Notes {
		remove := noteIDs[note.ID]
		for _, evidence := range note.EvidenceTurnIDs {
			if turnIDs[evidence] {
				remove = true
			}
		}
		if !remove {
			kept = append(kept, note)
		}
	}
	a.Notes = kept
	a.Summary = ""
	filtered := a.TurnTopics[:0]
	for _, tt := range a.TurnTopics {
		if !turnIDs[tt.TurnID] {
			filtered = append(filtered, tt)
		}
	}
	a.TurnTopics = filtered
	b, err := json.Marshal(a)
	if err != nil {
		return "{}", err
	}
	return string(b), nil
}

func scrubTranscriptSnapshotIDs(ctx context.Context, tx *sql.Tx, conversationID string, originalIDs []string) error {
	if len(originalIDs) == 0 {
		return nil
	}
	var raw string
	if err := tx.QueryRowContext(ctx, `SELECT transcript_json FROM conversations WHERE id=?`, conversationID).Scan(&raw); err != nil {
		return err
	}
	var tr Transcript
	if err := json.Unmarshal([]byte(raw), &tr); err != nil {
		return nil
	}
	remove := stringSet(originalIDs)
	kept := tr.Turns[:0]
	changed := false
	for _, turn := range tr.Turns {
		if remove[turn.ID] {
			changed = true
			continue
		}
		kept = append(kept, turn)
	}
	if !changed {
		return nil
	}
	tr.Turns = kept
	b, err := json.Marshal(tr)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE conversations SET transcript_json=?,summary='' WHERE id=?`, string(b), conversationID)
	return err
}

func scrubTranscriptSnapshot(ctx context.Context, tx *sql.Tx, conversationID, originalID string) error {
	var raw string
	if err := tx.QueryRowContext(ctx, `SELECT transcript_json FROM conversations WHERE id=?`, conversationID).Scan(&raw); err != nil {
		return err
	}
	var tr Transcript
	if err := json.Unmarshal([]byte(raw), &tr); err != nil {
		return nil
	}
	kept := tr.Turns[:0]
	changed := false
	for _, turn := range tr.Turns {
		if turn.ID == originalID {
			changed = true
			continue
		}
		kept = append(kept, turn)
	}
	if !changed {
		return nil
	}
	tr.Turns = kept
	b, err := json.Marshal(tr)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE conversations SET transcript_json=?,summary='' WHERE id=?`, string(b), conversationID)
	return err
}

func scrubAttachmentSnapshots(ctx context.Context, tx *sql.Tx, t forgetTarget, attachmentIDs []string) error {
	if len(attachmentIDs) == 0 {
		return nil
	}
	for _, id := range attachmentIDs {
		var srcRaw, resultRaw string
		if err := tx.QueryRowContext(ctx, `SELECT source_json,result_json FROM source_attachments WHERE id=?`, id).Scan(&srcRaw, &resultRaw); err != nil {
			return err
		}
		var src asrinput.Source
		var result asrinput.Result
		if err := json.Unmarshal([]byte(srcRaw), &src); err != nil {
			src = asrinput.Source{}
		}
		if err := json.Unmarshal([]byte(resultRaw), &result); err != nil {
			result = asrinput.Result{}
		}
		if t.request.Scope == "turn" {
			kept := result.Turns[:0]
			for _, turn := range result.Turns {
				remove := t.originalID != "" && (turn.ID == t.originalID || strings.HasSuffix(t.originalID, "/"+turn.ID))
				start, end := turnGlobalRange(src, turn)
				if start != nil && end != nil && *start < t.endMS.Int64 && *end > t.startMS.Int64 {
					remove = true
				}
				if !remove {
					kept = append(kept, turn)
				}
			}
			result.Turns = kept
		}
		result.RawSource = nil
		result.Source = redactASRSource(result.Source)
		result.Source.RawEnvelope = nil
		b, err := json.Marshal(result)
		if err != nil {
			return err
		}
		digest, err := semanticJSONHash(result)
		if err != nil {
			return err
		}
		redactedSource := redactASRSource(src)
		srcBytes, err := json.Marshal(redactedSource)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE source_attachments SET source_json=?,result_json=?,content_sha256=? WHERE id=?`, string(srcBytes), string(b), digest, id); err != nil {
			return err
		}
	}
	return nil
}

func turnGlobalRange(src asrinput.Source, turn asrinput.Turn) (*int64, *int64) {
	start, end := turn.GlobalStartMS, turn.GlobalEndMS
	if start == nil && turn.LocalStartMS != nil {
		v := src.SegmentOffsetMS + *turn.LocalStartMS
		start = &v
	}
	if end == nil && turn.LocalEndMS != nil {
		v := src.SegmentOffsetMS + *turn.LocalEndMS
		end = &v
	}
	return start, end
}
func redactASRSource(src asrinput.Source) asrinput.Source {
	src.RawEnvelope = nil
	src.SourceAudioSHA256 = nil
	src.SourceRef = nil
	src.ProviderRequestID = ""
	src.ServerLogID = nil
	src.ResourceID = nil
	return src
}

func idsForRuns(ctx context.Context, tx *sql.Tx, jobs, attachments []string, conversation string) []string {
	ids := []string{}
	for _, job := range jobs {
		more, _ := idsFromQuery(ctx, tx, `SELECT id FROM decision_runs WHERE job_id=?`, job)
		ids = append(ids, more...)
	}
	for _, attachment := range attachments {
		more, _ := idsFromQuery(ctx, tx, `SELECT id FROM decision_runs WHERE input_snapshot_id=?`, attachment)
		ids = append(ids, more...)
	}
	more, _ := idsFromQuery(ctx, tx, `SELECT id FROM decision_runs WHERE input_snapshot_id=?`, conversation)
	ids = append(ids, more...)
	return uniqueStrings(ids)
}
func deleteDecisionRuns(ctx context.Context, tx *sql.Tx, jobs, attachments []string, conversation, scope string) error {
	ids := idsForRuns(ctx, tx, jobs, attachments, conversation)
	if err := deleteIDs(ctx, tx, "decision_answers", "run_id", ids); err != nil {
		return err
	}
	return deleteIDs(ctx, tx, "decision_runs", "id", ids)
}
func scrubAttempts(ctx context.Context, tx *sql.Tx, jobs []string) error {
	for _, id := range jobs {
		if _, err := tx.ExecContext(ctx, `UPDATE processing_attempts SET error_json=NULL,result_ref=NULL,usage_json=NULL WHERE job_id=?`, id); err != nil {
			return err
		}
	}
	return nil
}
func scrubJobs(ctx context.Context, tx *sql.Tx, jobs []string) error {
	for _, id := range jobs {
		if _, err := tx.ExecContext(ctx, `DELETE FROM processing_retry_requests WHERE job_id=?`, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE processing_jobs SET payload_json='{}',status='cancelled',lease_token=NULL,lease_expires_at=NULL,worker_id=NULL,updated_at=? WHERE id=?`, time.Now().UnixMilli(), id); err != nil {
			return err
		}
	}
	return nil
}
func idsForBindings(ctx context.Context, tx *sql.Tx, recording, conversation string) []string {
	ids, _ := idsFromQuery(ctx, tx, `SELECT id FROM speaker_bindings WHERE recording_id=? OR recording_id IN (SELECT id FROM recordings WHERE conversation_id=?)`, recording, conversation)
	return ids
}
func deleteBindings(ctx context.Context, tx *sql.Tx, recording, conversation string) error {
	ids := idsForBindings(ctx, tx, recording, conversation)
	if err := deleteIDs(ctx, tx, "speaker_binding_events", "binding_id", ids); err != nil {
		return err
	}
	return deleteIDs(ctx, tx, "speaker_bindings", "id", ids)
}
func deleteSpeakerEvidence(ctx context.Context, tx *sql.Tx, turnID string) error {
	ids, err := idsFromQuery(ctx, tx, `SELECT id FROM speaker_bindings WHERE evidence_json LIKE ?`, "%"+turnID+"%")
	if err != nil {
		return err
	}
	if err := deleteIDs(ctx, tx, "speaker_binding_events", "binding_id", ids); err != nil {
		return err
	}
	return deleteIDs(ctx, tx, "speaker_bindings", "id", ids)
}
func deleteEmbeddings(ctx context.Context, tx *sql.Tx, turnIDs, noteIDs []string, conversation, scope string) error {
	if scope != "turn" {
		_, err := tx.ExecContext(ctx, `DELETE FROM memory_embeddings WHERE conversation_id=?`, conversation)
		return err
	}
	if err := deleteIDs(ctx, tx, "memory_embeddings", "artifact_id", turnIDs); err != nil {
		return err
	}
	return deleteIDs(ctx, tx, "memory_embeddings", "artifact_id", noteIDs)
}
func deleteLinks(ctx context.Context, tx *sql.Tx, turnIDs, noteIDs []string, conversation, scope string) error {
	if scope != "turn" {
		_, err := tx.ExecContext(ctx, `DELETE FROM memory_links WHERE from_id=? OR to_id=? OR (from_kind='turn' AND from_id IN (SELECT id FROM turns WHERE conversation_id=?)) OR (to_kind='turn' AND to_id IN (SELECT id FROM turns WHERE conversation_id=?))`, conversation, conversation, conversation, conversation)
		return err
	}
	for _, id := range append(append([]string{}, turnIDs...), noteIDs...) {
		if _, err := tx.ExecContext(ctx, `DELETE FROM memory_links WHERE from_id=? OR to_id=? OR evidence_turn_ids_json LIKE ?`, id, id, "%"+id+"%"); err != nil {
			return err
		}
	}
	return nil
}
func deleteArtifacts(ctx context.Context, tx *sql.Tx, notes, candidates []string, conversation, scope string) error {
	if scope != "turn" {
		_, err := tx.ExecContext(ctx, `DELETE FROM derived_artifact_sources WHERE (kind='note' AND artifact_id IN (SELECT id FROM notes WHERE conversation_id=?)) OR (kind='candidate' AND artifact_id IN (SELECT id FROM memory_candidates WHERE conversation_id=?))`, conversation, conversation)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM derived_artifacts WHERE conversation_id=?`, conversation)
		return err
	}
	if err := deleteIDs(ctx, tx, "derived_artifact_sources", "artifact_id", notes); err != nil {
		return err
	}
	if err := deleteIDs(ctx, tx, "derived_artifact_sources", "artifact_id", candidates); err != nil {
		return err
	}
	if err := deleteIDs(ctx, tx, "derived_artifacts", "artifact_id", notes); err != nil {
		return err
	}
	return deleteIDs(ctx, tx, "derived_artifacts", "artifact_id", candidates)
}

func scrubBroadConversation(ctx context.Context, tx *sql.Tx, t forgetTarget) error {
	// Remove all annotation snapshots and other content copies for broad scopes.
	if t.request.Scope == "conversation" {
		if _, err := tx.ExecContext(ctx, `DELETE FROM annotation_history WHERE conversation_id=?`, t.conversationID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM topic_conversations WHERE conversation_id=?`, t.conversationID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM publication_runs WHERE conversation_id=?`, t.conversationID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM recordings WHERE conversation_id=?`, t.conversationID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM conversations WHERE id=?`, t.conversationID)
		return err
	}
	// A recording may share its conversation with other source recordings.
	if _, err := tx.ExecContext(ctx, `UPDATE conversations SET summary='',source_sha256='',transcript_version='' WHERE id=?`, t.conversationID); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM topic_conversations WHERE conversation_id=? AND topic_id NOT IN (SELECT je.value FROM notes n,json_each(n.topic_ids_json) je WHERE n.conversation_id=?) AND topic_id NOT IN (SELECT tt.topic_id FROM turn_topics tt JOIN turns t ON t.id=tt.turn_id WHERE t.conversation_id=?)`, t.conversationID, t.conversationID, t.conversationID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM publication_runs WHERE conversation_id=?`, t.conversationID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM recordings WHERE id=?`, t.recordingID)
	return err
}

func scrubSyncLedgers(ctx context.Context, tx *sql.Tx, tombstoneID string) error {
	var scope string
	if err := tx.QueryRowContext(ctx, `SELECT scope FROM forget_tombstones WHERE id=?`, tombstoneID).Scan(&scope); err != nil {
		return err
	}
	var ids []string
	rows, err := tx.QueryContext(ctx, `SELECT provider_recording_id FROM forget_provider_fences WHERE tombstone_id=?`, tombstoneID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE sync_events SET payload_json='{}' WHERE recording_id=?`, id); err != nil {
			return err
		}
		if scope == "turn" {
			// Local files have been silenced at exact sample intervals by the persisted
			// manifest. Keep paths and lengths so unrelated turns remain playable; erase
			// hashes that disclose the pre-redaction file content.
			if _, err := tx.ExecContext(ctx, `UPDATE sync_chunks SET sha256='' WHERE recording_id=?`, id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE sync_recordings SET last_error='source segment forgotten',updated_at=? WHERE recording_id=?`, time.Now().UnixMilli(), id); err != nil {
				return err
			}
		} else {
			if _, err := tx.ExecContext(ctx, `UPDATE sync_recordings SET state='deleted',stitched_path=NULL,last_error='forgotten',updated_at=? WHERE recording_id=?`, time.Now().UnixMilli(), id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM sync_chunks WHERE recording_id=?`, id); err != nil {
				return err
			}
		}
	}
	return nil
}

func bumpForgetAnnotationVersion(ctx context.Context, tx *sql.Tx, conversationID, salt string) error {
	if conversationID == "" {
		return nil
	}
	version := "forget-" + strings.TrimPrefix(stableForgetLocator("annotation", salt), "")[:24]
	var raw, basis, title string
	if err := tx.QueryRowContext(ctx, `SELECT annotation_basis,title,annotations_json FROM conversations WHERE id=?`, conversationID).Scan(&basis, &title, &raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	var a Annotations
	if json.Unmarshal([]byte(raw), &a) != nil {
		a = Annotations{ConversationID: conversationID, Title: title}
	}
	a.AnnotationVersion = version
	a.Summary = ""
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE conversations SET annotation_version=?,annotations_json=?,summary='' WHERE id=?`, version, string(b), conversationID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE notes SET annotation_version=? WHERE conversation_id=?`, version, conversationID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT OR REPLACE INTO annotation_history(conversation_id,annotation_version,annotation_basis,summary,annotations_json) VALUES(?,?,?,'',?)`, conversationID, version, basis, string(b)); err != nil {
		return err
	}
	return nil
}

// SuppressNote atomically retires an AI note and records a durable cleanup
// operation. The legacy error-only API remains as a convenience wrapper.
func (s *Store) SuppressNote(ctx context.Context, conversationID, noteID, expectedVersion string) error {
	_, err := s.SuppressNoteOperation(ctx, conversationID, noteID, expectedVersion)
	return err
}

// SuppressNoteOperation returns a stable operation ID. If the note was already
// suppressed, it returns the existing operation so external cleanup can retry.
func (s *Store) SuppressNoteOperation(ctx context.Context, conversationID, noteID, expectedVersion string) (string, error) {
	if strings.TrimSpace(conversationID) == "" || strings.TrimSpace(noteID) == "" {
		return "", errors.New("conversation_id and note_id required")
	}
	opID := "suppress-" + stableForgetLocator(conversationID, noteID)
	var existing string
	err := s.db.QueryRowContext(ctx, `SELECT operation_id FROM suppression_cleanup_operations WHERE conversation_id=? AND note_id=?`, conversationID, noteID).Scan(&existing)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if err = s.suppressNote(ctx, conversationID, noteID, expectedVersion, opID); err != nil {
		return "", err
	}
	return opID, nil
}

type SuppressionCleanupOperation struct {
	OperationID    string `json:"operation_id"`
	ConversationID string `json:"conversation_id"`
	NoteID         string `json:"note_id"`
	Status         string `json:"status"`
	CreatedAt      int64  `json:"created_at"`
	UpdatedAt      int64  `json:"updated_at"`
}

func (s *Store) SuppressionCleanupStatus(ctx context.Context, operationID string) (SuppressionCleanupOperation, error) {
	var op SuppressionCleanupOperation
	err := s.db.QueryRowContext(ctx, `SELECT operation_id,conversation_id,note_id,status,created_at,updated_at FROM suppression_cleanup_operations WHERE operation_id=?`, operationID).Scan(&op.OperationID, &op.ConversationID, &op.NoteID, &op.Status, &op.CreatedAt, &op.UpdatedAt)
	return op, err
}

func (s *Store) PendingSuppressionCleanups(ctx context.Context) ([]SuppressionCleanupOperation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT operation_id,conversation_id,note_id,status,created_at,updated_at FROM suppression_cleanup_operations WHERE status='pending' ORDER BY created_at,operation_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SuppressionCleanupOperation
	for rows.Next() {
		var op SuppressionCleanupOperation
		if err = rows.Scan(&op.OperationID, &op.ConversationID, &op.NoteID, &op.Status, &op.CreatedAt, &op.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, op)
	}
	return out, rows.Err()
}

func (s *Store) CompleteSuppressionCleanup(ctx context.Context, operationID string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE suppression_cleanup_operations SET status='complete',updated_at=? WHERE operation_id=? AND status='pending'`, time.Now().UnixMilli(), operationID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var status string
		if err = s.db.QueryRowContext(ctx, `SELECT status FROM suppression_cleanup_operations WHERE operation_id=?`, operationID).Scan(&status); err != nil {
			return err
		}
		if status != "complete" {
			return errors.New("suppression cleanup operation is not pending")
		}
	}
	return nil
}

// suppressNote retires a note and records the pending external-cleanup marker
// in the same transaction as all canonical redaction.
func (s *Store) suppressNote(ctx context.Context, conversationID, noteID, expectedVersion, operationID string) error {
	if strings.TrimSpace(conversationID) == "" || strings.TrimSpace(noteID) == "" || strings.TrimSpace(expectedVersion) == "" {
		return errors.New("conversation_id, note_id, and expected annotation version required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version, kind string
	if err = tx.QueryRowContext(ctx, `SELECT annotation_version FROM conversations WHERE id=?`, conversationID).Scan(&version); err != nil {
		return err
	}
	if version != expectedVersion {
		return fmt.Errorf("annotation version conflict: current version is %s", version)
	}
	if err = tx.QueryRowContext(ctx, `SELECT kind FROM notes WHERE id=? AND conversation_id=?`, noteID, conversationID).Scan(&kind); err != nil {
		return err
	}
	type source struct{ rec, segment string }
	var sources []source
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT t.recording_id,a.segment_id FROM note_evidence e JOIN turns t ON t.id=e.turn_id JOIN attachment_turns at ON at.turn_id=t.id JOIN source_attachments a ON a.id=at.attachment_id WHERE e.note_id=? ORDER BY t.recording_id,a.segment_id`, noteID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var x source
		if err = rows.Scan(&x.rec, &x.segment); err != nil {
			rows.Close()
			return err
		}
		sources = append(sources, x)
	}
	if err = rows.Close(); err != nil {
		return err
	}
	if len(sources) == 0 {
		return errors.New("note has no stable source locator; suppression cannot survive reprocessing")
	}
	now := time.Now().UnixMilli()
	for _, x := range sources {
		if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO ai_note_suppressions(recording_id,source_segment_id,kind,created_at) VALUES(?,?,?,?)`, x.rec, x.segment, kind, now); err != nil {
			return err
		}
	}
	turnIDs, err := idsFromQuery(ctx, tx, `SELECT turn_id FROM note_evidence WHERE note_id=?`, noteID)
	if err != nil {
		return err
	}
	candidateIDs, err := idsFromQuery(ctx, tx, `SELECT DISTINCT c.id FROM memory_candidates c,json_each(c.evidence_turn_ids_json) e JOIN turns t ON t.id=e.value WHERE c.conversation_id=? AND c.kind=? AND t.id IN (`+placeholdersOrFalse(turnIDs)+`)`, append([]any{conversationID, kind}, stringsToAny(turnIDs)...)...)
	if err != nil {
		return err
	}
	var raw string
	if err = tx.QueryRowContext(ctx, `SELECT annotations_json FROM conversations WHERE id=?`, conversationID).Scan(&raw); err != nil {
		return err
	}
	scrubbed, err := scrubAnnotationsJSON(raw, stringSet([]string{noteID}), map[string]bool{})
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE conversations SET summary='',annotations_json=? WHERE id=?`, scrubbed, conversationID); err != nil {
		return err
	}
	rows, err = tx.QueryContext(ctx, `SELECT annotation_version,annotations_json FROM annotation_history WHERE conversation_id=?`, conversationID)
	if err != nil {
		return err
	}
	type hist struct{ version, raw string }
	var history []hist
	for rows.Next() {
		var h hist
		if err = rows.Scan(&h.version, &h.raw); err != nil {
			rows.Close()
			return err
		}
		r, e := scrubAnnotationsJSON(h.raw, stringSet([]string{noteID}), map[string]bool{})
		if e != nil {
			rows.Close()
			return e
		}
		h.raw = r
		history = append(history, h)
	}
	if err = rows.Close(); err != nil {
		return err
	}
	for _, h := range history {
		if _, err = tx.ExecContext(ctx, `UPDATE annotation_history SET summary='',annotations_json=? WHERE conversation_id=? AND annotation_version=?`, h.raw, conversationID, h.version); err != nil {
			return err
		}
	}
	for _, table := range []string{"note_evidence"} {
		if err = deleteIDs(ctx, tx, table, "note_id", []string{noteID}); err != nil {
			return err
		}
	}
	for _, table := range []string{"memory_embeddings", "derived_artifact_sources", "derived_artifacts"} {
		if err = deleteIDs(ctx, tx, table, "artifact_id", []string{noteID}); err != nil {
			return err
		}
	}
	if err = deleteIDs(ctx, tx, "memory_links", "from_id", []string{noteID}); err != nil {
		return err
	}
	if err = deleteIDs(ctx, tx, "memory_links", "to_id", []string{noteID}); err != nil {
		return err
	}
	if err = deleteIDs(ctx, tx, "memory_embeddings", "artifact_id", candidateIDs); err != nil {
		return err
	}
	if err = deleteIDs(ctx, tx, "derived_artifact_sources", "artifact_id", candidateIDs); err != nil {
		return err
	}
	if err = deleteIDs(ctx, tx, "derived_artifacts", "artifact_id", candidateIDs); err != nil {
		return err
	}
	if err = deleteIDs(ctx, tx, "memory_candidates", "id", candidateIDs); err != nil {
		return err
	}
	if err = deleteIDs(ctx, tx, "notes", "id", []string{noteID}); err != nil {
		return err
	}
	if err = bumpForgetAnnotationVersion(ctx, tx, conversationID, "suppress-"+stableForgetLocator(conversationID, noteID)); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO suppression_cleanup_operations(operation_id,conversation_id,note_id,status,created_at,updated_at) VALUES(?,?,?,'pending',?,?)`, operationID, conversationID, noteID, now, now); err != nil {
		return err
	}
	return tx.Commit()
}
