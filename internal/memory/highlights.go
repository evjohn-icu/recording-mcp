package memory

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"recordingmcp/internal/asrinput"
)

var (
	ErrHighlightInvalid         = errors.New("invalid memory highlight")
	ErrProfileRetired           = errors.New("profile is permanently retired")
	ErrHighlightVersionConflict = errors.New("memory highlight version conflict")
	ErrHighlightRequestConflict = errors.New("memory highlight request conflict")
	ErrHighlightGone            = errors.New("memory highlight no longer exists")
	ErrHighlightNotFound        = errors.New("memory highlight not found")
	ErrHighlightSourceMismatch  = errors.New("memory highlight evidence does not match source")
	ErrHighlightSourceForgotten = errors.New("memory highlight source is permanently forgotten")
)

type Highlight struct {
	ContractVersion    string  `json:"contract_version"`
	ID                 string  `json:"id"`
	ConversationID     string  `json:"conversation_id"`
	RecordingID        string  `json:"recording_id"`
	AnchorKind         string  `json:"anchor_kind"`
	StartMS            *int64  `json:"start_ms,omitempty"`
	EndMS              *int64  `json:"end_ms,omitempty"`
	TurnID             string  `json:"turn_id,omitempty"`
	SourceAttachmentID string  `json:"source_attachment_id,omitempty"`
	TranscriptRevision string  `json:"transcript_revision,omitempty"`
	Tag                string  `json:"tag"`
	NoteText           string  `json:"note_text"`
	Version            int64   `json:"version"`
	CreatedAtMS        int64   `json:"created_at_ms"`
	UpdatedAtMS        int64   `json:"updated_at_ms"`
	State              string  `json:"state"`
	QuoteText          *string `json:"quote_text,omitempty"`
}

type HighlightCreate struct {
	RequestID          string `json:"request_id"`
	ConversationID     string `json:"conversation_id,omitempty"`
	RecordingID        string `json:"recording_id"`
	AnchorKind         string `json:"anchor_kind"`
	StartMS            *int64 `json:"start_ms,omitempty"`
	EndMS              *int64 `json:"end_ms,omitempty"`
	TurnID             string `json:"turn_id,omitempty"`
	SourceAttachmentID string `json:"source_attachment_id,omitempty"`
	TranscriptRevision string `json:"transcript_revision,omitempty"`
	Tag                string `json:"tag"`
	NoteText           string `json:"note_text,omitempty"`
}

type HighlightRelink struct {
	TurnID             string `json:"turn_id"`
	SourceAttachmentID string `json:"source_attachment_id"`
	TranscriptRevision string `json:"transcript_revision"`
}

type HighlightUpdate struct {
	ExpectedVersion int64            `json:"expected_version"`
	Tag             *string          `json:"tag,omitempty"`
	NoteText        *string          `json:"note_text,omitempty"`
	Relink          *HighlightRelink `json:"relink,omitempty"`
}

func (s *Store) CreateHighlight(ctx context.Context, in HighlightCreate) (Highlight, error) {
	if in.AnchorKind == "point" && in.StartMS != nil && in.EndMS != nil && *in.StartMS == *in.EndMS {
		in.EndMS = nil
	}
	if !validID(in.RequestID) || !validID(in.RecordingID) || !validHighlight(in.AnchorKind, in.StartMS, in.EndMS, in.TurnID, in.Tag, in.NoteText) {
		return Highlight{}, ErrHighlightInvalid
	}
	payload, err := json.Marshal(in)
	if err != nil {
		return Highlight{}, err
	}
	sum := sha256.Sum256(payload)
	payloadHash := hex.EncodeToString(sum[:])
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Highlight{}, err
	}
	defer tx.Rollback()
	if err = checkProfileRetired(ctx, tx); err != nil {
		return Highlight{}, err
	}
	var oldHash, oldID, oldStatus string
	err = tx.QueryRowContext(ctx, `SELECT payload_sha256,highlight_id,status FROM memory_highlight_requests WHERE request_id=?`, in.RequestID).Scan(&oldHash, &oldID, &oldStatus)
	if err == nil {
		if oldStatus != "active" || oldHash == "" {
			return Highlight{}, ErrHighlightGone
		}
		if oldHash != payloadHash {
			return Highlight{}, ErrHighlightRequestConflict
		}
		return getHighlight(ctx, tx, oldID)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Highlight{}, err
	}
	if err = checkHighlightForgotten(ctx, tx, in.RecordingID, in.ConversationID, in.AnchorKind, in.StartMS, in.EndMS); err != nil {
		return Highlight{}, err
	}
	var conversation string
	if err = tx.QueryRowContext(ctx, `SELECT conversation_id FROM recordings WHERE id=?`, in.RecordingID).Scan(&conversation); err != nil {
		return Highlight{}, ErrHighlightSourceMismatch
	}
	if in.ConversationID != "" && in.ConversationID != conversation {
		return Highlight{}, ErrHighlightSourceMismatch
	}
	if err = validateHighlightSource(ctx, tx, conversation, in.RecordingID, in.AnchorKind, in.StartMS, in.EndMS, in.TurnID, in.SourceAttachmentID, in.TranscriptRevision); err != nil {
		return Highlight{}, err
	}
	if err = checkHighlightForgotten(ctx, tx, in.RecordingID, conversation, in.AnchorKind, in.StartMS, in.EndMS); err != nil {
		return Highlight{}, err
	}
	id := stableKey("highlight", in.RequestID)
	now := time.Now().UnixMilli()
	_, err = tx.ExecContext(ctx, `INSERT INTO memory_highlights(id,conversation_id,recording_id,anchor_kind,start_ms,end_ms,turn_id,source_attachment_id,transcript_revision,tag,note_text,version,created_at_ms,updated_at_ms) VALUES(?,?,?,?,?,?,?,?,?,?,?,1,?,?)`, id, conversation, in.RecordingID, in.AnchorKind, nullablePtrInt(in.StartMS), nullablePtrInt(in.EndMS), nullableString(in.TurnID), nullableString(in.SourceAttachmentID), nullableString(in.TranscriptRevision), in.Tag, in.NoteText, now, now)
	if err != nil {
		return Highlight{}, mapHighlightDBError(err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO memory_highlight_requests(request_id,conversation_id,recording_id,anchor_kind,start_ms,end_ms,turn_id,payload_sha256,highlight_id,status,created_at_ms) VALUES(?,?,?,?,?,?,?,?,?,'active',?)`, in.RequestID, conversation, in.RecordingID, in.AnchorKind, nullablePtrInt(in.StartMS), nullablePtrInt(in.EndMS), nullableString(in.TurnID), payloadHash, id, now); err != nil {
		return Highlight{}, err
	}
	if err = tx.Commit(); err != nil {
		return Highlight{}, err
	}
	return s.GetHighlight(ctx, id)
}

func (s *Store) ListHighlights(ctx context.Context, recordingID string) ([]Highlight, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = checkProfileRetired(ctx, tx); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id FROM memory_highlights WHERE recording_id=? ORDER BY created_at_ms,id`, recordingID)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	out := make([]Highlight, 0, len(ids))
	for _, id := range ids {
		h, e := getHighlight(ctx, tx, id)
		if e != nil {
			return nil, e
		}
		out = append(out, h)
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) GetHighlight(ctx context.Context, id string) (Highlight, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Highlight{}, err
	}
	defer tx.Rollback()
	if err = checkProfileRetired(ctx, tx); err != nil {
		return Highlight{}, err
	}
	h, err := getHighlight(ctx, tx, id)
	if err != nil {
		return Highlight{}, err
	}
	if err = tx.Commit(); err != nil {
		return Highlight{}, err
	}
	return h, nil
}

func (s *Store) UpdateHighlight(ctx context.Context, id string, u HighlightUpdate) (Highlight, error) {
	if u.ExpectedVersion < 1 || u.Tag == nil && u.NoteText == nil && u.Relink == nil {
		return Highlight{}, ErrHighlightInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Highlight{}, err
	}
	defer tx.Rollback()
	if err = checkProfileRetired(ctx, tx); err != nil {
		return Highlight{}, err
	}
	h, err := getHighlight(ctx, tx, id)
	if err != nil {
		return Highlight{}, err
	}
	if h.Version != u.ExpectedVersion {
		return Highlight{}, ErrHighlightVersionConflict
	}
	tag, note := h.Tag, h.NoteText
	if u.Tag != nil {
		tag = *u.Tag
	}
	if u.NoteText != nil {
		note = *u.NoteText
	}
	if !validHighlight(h.AnchorKind, h.StartMS, h.EndMS, h.TurnID, tag, note) {
		return Highlight{}, ErrHighlightInvalid
	}
	if u.Relink != nil {
		if !validID(u.Relink.TurnID) || !validID(u.Relink.SourceAttachmentID) || !validID(u.Relink.TranscriptRevision) {
			return Highlight{}, ErrHighlightInvalid
		}
		if err = validateHighlightSource(ctx, tx, h.ConversationID, h.RecordingID, h.AnchorKind, h.StartMS, h.EndMS, u.Relink.TurnID, u.Relink.SourceAttachmentID, u.Relink.TranscriptRevision); err != nil {
			return Highlight{}, err
		}
		h.TurnID = u.Relink.TurnID
		h.SourceAttachmentID = u.Relink.SourceAttachmentID
		h.TranscriptRevision = u.Relink.TranscriptRevision
	}
	res, err := tx.ExecContext(ctx, `UPDATE memory_highlights SET tag=?,note_text=?,turn_id=?,source_attachment_id=?,transcript_revision=?,version=version+1,updated_at_ms=? WHERE id=? AND version=?`, tag, note, nullableString(h.TurnID), nullableString(h.SourceAttachmentID), nullableString(h.TranscriptRevision), time.Now().UnixMilli(), id, u.ExpectedVersion)
	if err != nil {
		return Highlight{}, mapHighlightDBError(err)
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return Highlight{}, ErrHighlightVersionConflict
	}
	if err = tx.Commit(); err != nil {
		return Highlight{}, err
	}
	return s.GetHighlight(ctx, id)
}

func (s *Store) DeleteHighlight(ctx context.Context, id string, expectedVersion int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := checkProfileRetired(ctx, tx); err != nil {
		return err
	}
	var version int64
	if err = tx.QueryRowContext(ctx, `SELECT version FROM memory_highlights WHERE id=?`, id).Scan(&version); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			var status string
			if e := tx.QueryRowContext(ctx, `SELECT status FROM memory_highlight_requests WHERE highlight_id=? LIMIT 1`, id).Scan(&status); e == nil {
				return ErrHighlightGone
			}
			return ErrHighlightNotFound
		}
		return err
	}
	if version != expectedVersion {
		return ErrHighlightVersionConflict
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM memory_highlights WHERE id=?`, id); err != nil {
		return mapHighlightDBError(err)
	}
	_, err = tx.ExecContext(ctx, `UPDATE memory_highlight_requests SET status='deleted',payload_sha256='' WHERE highlight_id=? AND status='active'`, id)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func getHighlight(ctx context.Context, q queryer, id string) (Highlight, error) {
	h := Highlight{ContractVersion: "highlights/0.1"}
	var start, end sql.NullInt64
	var turn, att, rev sql.NullString
	err := q.QueryRowContext(ctx, `SELECT id,conversation_id,recording_id,anchor_kind,start_ms,end_ms,turn_id,source_attachment_id,transcript_revision,tag,note_text,version,created_at_ms,updated_at_ms FROM memory_highlights WHERE id=?`, id).Scan(&h.ID, &h.ConversationID, &h.RecordingID, &h.AnchorKind, &start, &end, &turn, &att, &rev, &h.Tag, &h.NoteText, &h.Version, &h.CreatedAtMS, &h.UpdatedAtMS)
	if errors.Is(err, sql.ErrNoRows) {
		var status string
		if e := q.QueryRowContext(ctx, `SELECT status FROM memory_highlight_requests WHERE highlight_id=? ORDER BY created_at_ms DESC LIMIT 1`, id).Scan(&status); e == nil {
			return Highlight{}, ErrHighlightGone
		}
		return Highlight{}, ErrHighlightNotFound
	}
	if err != nil {
		return Highlight{}, err
	}
	if start.Valid {
		v := start.Int64
		h.StartMS = &v
	}
	if end.Valid {
		v := end.Int64
		h.EndMS = &v
	}
	h.TurnID = turn.String
	h.SourceAttachmentID = att.String
	h.TranscriptRevision = rev.String
	if err := checkHighlightForgotten(ctx, q, h.RecordingID, h.ConversationID, h.AnchorKind, h.StartMS, h.EndMS); err != nil {
		return Highlight{}, err
	}
	var active int
	var currentRevision string
	e := q.QueryRowContext(ctx, `SELECT active,transcript_revision FROM source_attachments WHERE id=? AND recording_id=? AND conversation_id=?`, h.SourceAttachmentID, h.RecordingID, h.ConversationID).Scan(&active, &currentRevision)
	if e == nil && active == 1 && currentRevision == h.TranscriptRevision {
		if h.TurnID != "" {
			var class string
			if e = q.QueryRowContext(ctx, `SELECT COALESCE(tc.class,'speech') FROM turns t JOIN attachment_turns at ON at.turn_id=t.id LEFT JOIN turn_classes tc ON tc.turn_id=t.id WHERE t.id=? AND at.attachment_id=?`, h.TurnID, h.SourceAttachmentID).Scan(&class); e != nil || class == "ambient" {
				h.State = "needs_relink"
				return h, nil
			}
		}
		h.State = "current"
		quoteSQL := `SELECT t.content FROM turns t JOIN attachment_turns at ON at.turn_id=t.id LEFT JOIN turn_classes tc ON tc.turn_id=t.id WHERE at.attachment_id=? AND t.recording_id=? AND t.conversation_id=? AND COALESCE(tc.class,'speech')!='ambient' AND ` + CurrentTurnPredicate("t")
		args := []any{h.SourceAttachmentID, h.RecordingID, h.ConversationID}
		switch h.AnchorKind {
		case "turn":
			quoteSQL += ` AND t.id=?`
			args = append(args, h.TurnID)
		case "point":
			quoteSQL += ` AND t.global_start_ms IS NOT NULL AND t.global_end_ms IS NOT NULL AND t.global_start_ms<=? AND t.global_end_ms>?`
			args = append(args, *h.StartMS, *h.StartMS)
		case "range":
			quoteSQL += ` AND t.global_start_ms IS NOT NULL AND t.global_end_ms IS NOT NULL AND t.global_start_ms<? AND t.global_end_ms>?`
			args = append(args, *h.EndMS, *h.StartMS)
		}
		quoteSQL += ` ORDER BY t.global_start_ms,t.id`
		rows, qe := q.QueryContext(ctx, quoteSQL, args...)
		if qe == nil {
			var pieces []string
			for rows.Next() {
				var v string
				if rows.Scan(&v) == nil && strings.TrimSpace(v) != "" {
					pieces = append(pieces, v)
				}
			}
			_ = rows.Close()
			if len(pieces) > 0 {
				v := strings.Join(pieces, " ")
				h.QuoteText = &v
			}
		}
	} else {
		h.State = "needs_relink"
	}
	return h, nil
}

func validHighlight(kind string, start, end *int64, turn, tag, note string) bool {
	if len(note) > 8192 {
		return false
	}
	switch tag {
	case "important", "follow_up", "question":
	default:
		return false
	}
	switch kind {
	case "point":
		return start != nil && *start >= 0 && (end == nil || *end == *start) && (turn == "" || validID(turn))
	case "range":
		return start != nil && end != nil && *start >= 0 && *end > *start && (turn == "" || validID(turn))
	case "turn":
		return validID(turn) && ((start == nil && end == nil) || (start != nil && end != nil && *start >= 0 && *end > *start))
	default:
		return false
	}
}
func nullablePtrInt(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}
func nullableString(v string) any {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	return v
}

func validateHighlightSource(ctx context.Context, q queryer, conversation, recording, kind string, start, end *int64, turnID, attachmentID, revision string) error {
	if attachmentID == "" || revision == "" {
		return ErrHighlightSourceMismatch
	}
	var active int
	var attConversation string
	var attRevision string
	var sourceJSON string
	err := q.QueryRowContext(ctx, `SELECT conversation_id,transcript_revision,active,source_json FROM source_attachments WHERE id=? AND recording_id=?`, attachmentID, recording).Scan(&attConversation, &attRevision, &active, &sourceJSON)
	if err != nil || attConversation != conversation || attRevision != revision || active != 1 {
		return ErrHighlightSourceMismatch
	}
	var source asrinput.Source
	if json.Unmarshal([]byte(sourceJSON), &source) != nil || source.SegmentOffsetMS < 0 || source.SegmentDurationMS <= 0 {
		return ErrHighlightSourceMismatch
	}
	segmentEnd := source.SegmentOffsetMS + source.SegmentDurationMS
	if kind == "turn" && start == nil && end == nil {
		// The current attachment and turn are still validated below.
	} else if start == nil || *start < source.SegmentOffsetMS || *start >= segmentEnd || end != nil && (*end > segmentEnd || *end < *start || (kind != "point" && *end == *start)) {
		return ErrHighlightSourceMismatch
	}
	if kind == "turn" && turnID == "" {
		return ErrHighlightSourceMismatch
	}
	if turnID != "" {
		var tConversation, tRecording string
		var ts, te sql.NullInt64
		err = q.QueryRowContext(ctx, `SELECT conversation_id,recording_id,global_start_ms,global_end_ms FROM turns WHERE id=?`, turnID).Scan(&tConversation, &tRecording, &ts, &te)
		if err != nil || tConversation != conversation || tRecording != recording {
			return ErrHighlightSourceMismatch
		}
		var ambient int
		if err = q.QueryRowContext(ctx, `SELECT COUNT(*) FROM turn_classes WHERE turn_id=? AND class='ambient'`, turnID).Scan(&ambient); err != nil {
			return err
		}
		if ambient > 0 {
			return ErrHighlightSourceMismatch
		}
		var linked int
		if err = q.QueryRowContext(ctx, `SELECT COUNT(*) FROM attachment_turns WHERE attachment_id=? AND turn_id=?`, attachmentID, turnID).Scan(&linked); err != nil || linked != 1 {
			return ErrHighlightSourceMismatch
		}
		if kind != "turn" && (!ts.Valid || !te.Valid) || kind == "turn" && start != nil && (!ts.Valid || !te.Valid || *start < ts.Int64 || *end > te.Int64) || kind == "point" && (*start < ts.Int64 || *start >= te.Int64) || kind == "range" && (*start >= te.Int64 || *end <= ts.Int64) {
			return ErrHighlightSourceMismatch
		}
	}
	if start != nil && end != nil {
		var duration int64
		if err = q.QueryRowContext(ctx, `SELECT COALESCE(MAX(global_end_ms),0) FROM turns WHERE recording_id=?`, recording).Scan(&duration); err != nil {
			return err
		}
		if duration > 0 && *end > duration {
			return ErrHighlightInvalid
		}
	}
	return nil
}

func checkHighlightForgotten(ctx context.Context, q queryer, recording, conversation, kind string, start, end *int64) error {
	var n int
	unknownTiming := kind == "turn" && start == nil || kind != "point" && end == nil
	err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM forget_tombstones f WHERE (f.recording_id=? OR (f.scope='conversation' AND f.conversation_id=?)) AND (f.scope IN ('recording','conversation') OR f.scope='turn' AND (f.start_ms IS NULL OR f.end_ms IS NULL OR ?=1 OR ? IS NULL OR (? IS NULL AND ? >= f.start_ms AND ? < f.end_ms) OR (? IS NOT NULL AND ? < f.end_ms AND ? > f.start_ms)))`, recording, conversation, boolInt(unknownTiming), nullablePtrInt(start), nullablePtrInt(end), nullablePtrInt(start), nullablePtrInt(start), nullablePtrInt(end), nullablePtrInt(start), nullablePtrInt(end)).Scan(&n)
	if err != nil {
		return err
	}
	if n > 0 {
		return ErrHighlightSourceForgotten
	}
	return nil
}

func countHighlightsForForget(ctx context.Context, q queryer, t forgetTarget, out *int) error {
	where, args := highlightsForForgetWhere(t)
	return q.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_highlights h WHERE `+where, args...).Scan(out)
}

func forgetHighlights(ctx context.Context, tx *sql.Tx, t forgetTarget) error {
	where, args := highlightsForForgetWhere(t)
	ids, err := idsFromQuery(ctx, tx, `SELECT h.id FROM memory_highlights h WHERE `+where, args...)
	if err != nil {
		return err
	}
	ledgerWhere, ledgerArgs := highlightRequestsForForgetWhere(t)
	if _, err = tx.ExecContext(ctx, `UPDATE memory_highlight_requests SET status='forgotten',payload_sha256='' WHERE `+ledgerWhere, ledgerArgs...); err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	return deleteIDs(ctx, tx, "memory_highlights", "id", ids)
}

func highlightRequestsForForgetWhere(t forgetTarget) (string, []any) {
	switch t.request.Scope {
	case "conversation":
		return "conversation_id=?", []any{t.conversationID}
	case "recording":
		return "recording_id=?", []any{t.recordingID}
	default:
		return `recording_id=? AND (turn_id=? OR start_ms IS NULL OR (anchor_kind!='point' AND end_ms IS NULL) OR (anchor_kind='point' AND start_ms>=? AND start_ms<?) OR (anchor_kind!='point' AND start_ms<? AND end_ms>?))`, []any{t.recordingID, t.targetID, t.startMS.Int64, t.endMS.Int64, t.endMS.Int64, t.startMS.Int64}
	}
}

func highlightsForForgetWhere(t forgetTarget) (string, []any) {
	switch t.request.Scope {
	case "conversation":
		return "h.conversation_id=?", []any{t.conversationID}
	case "recording":
		return "h.recording_id=?", []any{t.recordingID}
	default:
		return `h.recording_id=? AND (h.turn_id=? OR h.start_ms IS NULL OR (h.anchor_kind!='point' AND h.end_ms IS NULL) OR (h.anchor_kind='point' AND h.start_ms>=? AND h.start_ms<?) OR (h.anchor_kind!='point' AND h.start_ms<? AND h.end_ms>?))`, []any{t.recordingID, t.targetID, t.startMS.Int64, t.endMS.Int64, t.endMS.Int64, t.startMS.Int64}
	}
}

func mapHighlightDBError(err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(strings.ToLower(err.Error()), "permanently retired") {
		return ErrProfileRetired
	}
	if strings.Contains(strings.ToLower(err.Error()), "permanently forgotten") {
		return ErrHighlightSourceForgotten
	}
	return fmt.Errorf("memory highlight: %w", err)
}

func checkProfileRetired(ctx context.Context, q queryer) error {
	var retired int
	err := q.QueryRowContext(ctx, `SELECT retired FROM profile_control WHERE id=1`).Scan(&retired)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if retired == 1 {
		return ErrProfileRetired
	}
	return nil
}
