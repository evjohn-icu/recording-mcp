// Package pipeline implements the extract-stage processing pipeline: it loads a
// conversation's turns from the store, builds deterministic windows, renders the
// model state, runs the decision engine and records the outcome in the decision
// log.
package pipeline

import (
	"context"
	"database/sql"
	"encoding/json"
	"sort"
	"strings"

	"recordingmcp/internal/decision"
)

// Turn is one persisted conversation turn in deterministic display order.
type Turn struct {
	ID              string
	Text            string
	SpeakerScopedID *string
	GlobalStartMS   *int64
	GlobalEndMS     *int64
	SegmentIndex    int
	OriginalIndex   int
}

// Window is a contiguous slice of turns passed to one engine evaluation.
type Window struct {
	TurnIDs []string
	StartMS *int64
	EndMS   *int64
}

// turnRow carries the columns needed for ordering that are not part of the
// public Turn shape (the original turn id is the final tie-breaker).
type turnRow struct {
	turn           Turn
	originalTurnID string
}

// turnsSelect is the shared column list for every turn read.
const turnsSelect = `SELECT id,content,speaker_scoped_id,global_start_ms,global_end_ms,segment_index,original_turn_id,turn_json FROM turns`

// LoadConversationTurns reads every turn of a conversation and returns them in
// deterministic order: turns with a global start time first (ascending), then
// by segment index, then by original turn id. OriginalIndex is parsed from
// turn_json's original_index field; when that is absent or invalid the row
// order is used instead.
func LoadConversationTurns(ctx context.Context, db *sql.DB, conversationID string) ([]Turn, error) {
	rows, err := db.QueryContext(ctx, turnsSelect+` WHERE conversation_id=?`, conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTurns(rows)
}

// LoadTurnsByIDs reads only the turns of a conversation whose ids appear in
// turnIDs and returns them in the same deterministic order as
// LoadConversationTurns. Ids absent from the conversation are omitted; an empty
// turnIDs yields no turns.
func LoadTurnsByIDs(ctx context.Context, db *sql.DB, conversationID string, turnIDs []string) ([]Turn, error) {
	if len(turnIDs) == 0 {
		return []Turn{}, nil
	}
	placeholders := make([]string, len(turnIDs))
	args := make([]any, 0, len(turnIDs)+1)
	args = append(args, conversationID)
	for i, id := range turnIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}
	query := turnsSelect + ` WHERE conversation_id=? AND id IN (` + strings.Join(placeholders, ",") + `)`
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTurns(rows)
}

// scanTurns consumes turn rows and applies the canonical ordering shared by
// every turn loader.
func scanTurns(rows *sql.Rows) ([]Turn, error) {
	ordered := make([]turnRow, 0)
	rowIndex := 0
	for rows.Next() {
		var (
			id          string
			content     string
			speaker     sql.NullString
			globalStart sql.NullInt64
			globalEnd   sql.NullInt64
			segmentIdx  int
			origTurnID  string
			turnJSON    string
		)
		if err := rows.Scan(&id, &content, &speaker, &globalStart, &globalEnd, &segmentIdx, &origTurnID, &turnJSON); err != nil {
			return nil, err
		}
		t := Turn{
			ID:           id,
			Text:         content,
			SegmentIndex: segmentIdx,
		}
		if speaker.Valid {
			v := speaker.String
			t.SpeakerScopedID = &v
		}
		if globalStart.Valid {
			v := globalStart.Int64
			t.GlobalStartMS = &v
		}
		if globalEnd.Valid {
			v := globalEnd.Int64
			t.GlobalEndMS = &v
		}
		t.OriginalIndex = parseOriginalIndex(turnJSON)
		if t.OriginalIndex == 0 {
			t.OriginalIndex = rowIndex + 1
		}
		ordered = append(ordered, turnRow{turn: t, originalTurnID: origTurnID})
		rowIndex++
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	sort.SliceStable(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		aHas, bHas := a.turn.GlobalStartMS != nil, b.turn.GlobalStartMS != nil
		if aHas != bHas {
			return aHas
		}
		if aHas && bHas && *a.turn.GlobalStartMS != *b.turn.GlobalStartMS {
			return *a.turn.GlobalStartMS < *b.turn.GlobalStartMS
		}
		if a.turn.SegmentIndex != b.turn.SegmentIndex {
			return a.turn.SegmentIndex < b.turn.SegmentIndex
		}
		return a.originalTurnID < b.originalTurnID
	})

	turns := make([]Turn, len(ordered))
	for i, r := range ordered {
		turns[i] = r.turn
	}
	return turns, nil
}

// parseOriginalIndex extracts the original_index field from a stored turn_json
// blob, returning 0 when the blob is absent or does not carry a positive index.
func parseOriginalIndex(raw string) int {
	if strings.TrimSpace(raw) == "" {
		return 0
	}
	var payload struct {
		OriginalIndex int `json:"original_index"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return 0
	}
	if payload.OriginalIndex < 0 {
		return 0
	}
	return payload.OriginalIndex
}

// BuildWindows splits turns into evaluation windows. v1 always uses a single
// window unless the turn count exceeds maxTurnsPerWindow, in which case turns
// are chunked sequentially.
func BuildWindows(turns []Turn, maxTurnsPerWindow int) []Window {
	if len(turns) == 0 {
		return nil
	}
	if maxTurnsPerWindow <= 0 {
		maxTurnsPerWindow = len(turns)
	}
	windows := make([]Window, 0, (len(turns)+maxTurnsPerWindow-1)/maxTurnsPerWindow)
	for start := 0; start < len(turns); start += maxTurnsPerWindow {
		end := start + maxTurnsPerWindow
		if end > len(turns) {
			end = len(turns)
		}
		chunk := turns[start:end]
		w := Window{TurnIDs: make([]string, 0, len(chunk))}
		for _, t := range chunk {
			w.TurnIDs = append(w.TurnIDs, t.ID)
			if t.GlobalStartMS != nil && (w.StartMS == nil || *t.GlobalStartMS < *w.StartMS) {
				v := *t.GlobalStartMS
				w.StartMS = &v
			}
			if t.GlobalEndMS != nil && (w.EndMS == nil || *t.GlobalEndMS > *w.EndMS) {
				v := *t.GlobalEndMS
				w.EndMS = &v
			}
		}
		windows = append(windows, w)
	}
	return windows
}

// BuildQuestions returns the fixed set of independent yes/no judgments the
// extract stage asks about one window.
func BuildQuestions() []decision.Question {
	return []decision.Question{
		{ID: "q_suggestion", Type: decision.TypeNoul, Instructions: "这段对话中，说话者是否向他人提出了建议或推荐（而不是仅仅陈述事实）？"},
		{ID: "q_request", Type: decision.TypeNoul, Instructions: "这段对话中，说话者是否向他人提出了请求或要求对方做某事？"},
		{ID: "q_intention", Type: decision.TypeNoul, Instructions: "这段对话中，说话者是否表达了自己未来的计划、意图或打算？"},
		{ID: "q_multitopic", Type: decision.TypeNoul, Instructions: "这段对话是否涉及两个或更多互不相关的主题？"},
		{ID: "q_need_context", Type: decision.TypeNoul, Instructions: "这段对话是否缺少理解所必需的上下文，需要补充前后文才能正确解读？"},
	}
}

// RenderState renders a window as one line per turn using the format
// "[<turnID>] <speaker|未知>: <text>". When the rendered text exceeds maxChars
// characters it is truncated and annotated. A non-positive maxChars disables
// truncation.
func RenderState(turns []Turn, w Window, maxChars int) string {
	byID := make(map[string]Turn, len(turns))
	for _, t := range turns {
		byID[t.ID] = t
	}
	var b strings.Builder
	for _, id := range w.TurnIDs {
		t, ok := byID[id]
		if !ok {
			continue
		}
		speaker := "未知"
		if t.SpeakerScopedID != nil && strings.TrimSpace(*t.SpeakerScopedID) != "" {
			speaker = *t.SpeakerScopedID
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString("[")
		b.WriteString(t.ID)
		b.WriteString("] ")
		b.WriteString(speaker)
		b.WriteString(": ")
		b.WriteString(t.Text)
	}
	out := b.String()
	if maxChars <= 0 {
		return out
	}
	runes := []rune(out)
	if len(runes) <= maxChars {
		return out
	}
	return string(runes[:maxChars]) + "…(truncated)"
}
