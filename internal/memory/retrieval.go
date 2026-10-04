package memory

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Retrieval paging defaults. Limits are clamped (0 -> default, >max -> max);
// the evidence radius uses the same convention so an omitted MCP field maps to
// the documented default instead of zero context.
const (
	defaultPageLimit       = 20
	maxPageLimit           = 100
	defaultEvidenceRadius  = 3
	defaultEvidenceMaxByte = 16000
)

// ErrNotFound reports that a requested record does not exist. Retrieval errors
// are wrapped around it so callers can use errors.Is while still getting a
// human-readable message.
var ErrNotFound = errors.New("memory: not found")

// ConversationFilter bounds ListConversations. Time bounds apply only to rows
// whose recording has a known recording_started_at; unknown times are excluded
// whenever either bound is set.
type ConversationFilter struct {
	Limit         int
	Cursor        string
	TopicID       string
	Status        string
	StartedAfter  *time.Time
	StartedBefore *time.Time
}

// ConversationSummary is one conversation row in a ListConversations page.
type ConversationSummary struct {
	ConversationID     string     `json:"conversation_id"`
	Title              string     `json:"title"`
	RecordingID        string     `json:"recording_id"`
	RecordingStartedAt *time.Time `json:"recording_started_at"`
	ReceivedSegments   int        `json:"received_segments"`
	OriginalTurns      int        `json:"original_turns"`
	TopicLinkedTurns   int        `json:"topic_linked_turns"`
	AnalysisStatus     string     `json:"analysis_status"`
	AnnotationVersion  string     `json:"annotation_version"`
}

// ConversationPage is one cursor page of conversation summaries.
type ConversationPage struct {
	Items      []ConversationSummary `json:"items"`
	NextCursor string                `json:"next_cursor,omitempty"`
}

// TurnEvidence is one transcript turn returned as part of an evidence window.
type TurnEvidence struct {
	ID              string  `json:"id"`
	Text            string  `json:"text"`
	SpeakerScopedID *string `json:"speaker_scoped_id"`
	GlobalStartMS   *int64  `json:"global_start_ms"`
	GlobalEndMS     *int64  `json:"global_end_ms"`
	SegmentIndex    int     `json:"segment_index"`
	IsTarget        bool    `json:"is_target"`
}

// EvidenceContext is the ordered neighborhood around one target turn.
type EvidenceContext struct {
	RecordingID string         `json:"recording_id"`
	Turns       []TurnEvidence `json:"turns"`
	Truncated   bool           `json:"truncated"`
	// Stale reports that the target turn derives from a superseded transcript
	// attachment (active=0). This lookup is an explicit audit path and returns
	// any turn by id; legacy turns with no attachment rows are never stale.
	Stale bool `json:"stale"`
}

// MemorySearch bounds SearchMemory. Text is matched as a case-insensitive
// literal substring; % and _ never act as wildcards.
type MemorySearch struct {
	Text          string
	Scope         string
	Limit         int
	Cursor        string
	TopicID       string
	StartedAfter  *time.Time
	StartedBefore *time.Time
	// IncludeStale opts into superseded notes and turns, which are excluded
	// from current-state results by default.
	IncludeStale bool
}

// SearchHit is one matched note or turn.
type SearchHit struct {
	Kind            string   `json:"kind"`
	ID              string   `json:"id"`
	Text            string   `json:"text"`
	ConversationID  string   `json:"conversation_id"`
	Why             string   `json:"why"`
	Score           float64  `json:"score"`
	EvidenceTurnIDs []string `json:"evidence_turn_ids,omitempty"`
	GlobalStartMS   *int64   `json:"global_start_ms"`
	// Stale reports that the hit derives from a superseded transcript
	// attachment; it is only ever true when IncludeStale was requested.
	Stale bool `json:"stale,omitempty"`
	// DerivedFrom lists the transcript_revision values of the hit's source
	// attachments, sorted ascending; empty for legacy hits.
	DerivedFrom []string `json:"derived_from,omitempty"`
}

// SearchPage is one cursor page of search hits. Degraded is reserved for future
// ranked backends; the literal backend always serves complete results.
type SearchPage struct {
	Hits          []SearchHit `json:"hits"`
	NextCursor    string      `json:"next_cursor,omitempty"`
	Degraded      bool        `json:"degraded"`
	DegradeReason string      `json:"degrade_reason,omitempty"`
}

// listCursor is the decoded ListConversations cursor. T is the recording start
// in Unix milliseconds, or -1 for an unknown start time.
type listCursor struct {
	T  int64  `json:"t"`
	ID string `json:"id"`
}

// searchCursor is the decoded SearchMemory cursor: kind is "note" or "turn",
// matching SearchHit.Kind.
type searchCursor struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

func clampPageLimit(limit int) int {
	if limit <= 0 {
		return defaultPageLimit
	}
	if limit > maxPageLimit {
		return maxPageLimit
	}
	return limit
}

// normalizeStatus maps an optional status filter onto the GetProcessingStatus
// vocabulary ("pending" / "partial"). Empty means no filter.
func normalizeStatus(status string) (string, error) {
	switch v := strings.ToLower(strings.TrimSpace(status)); v {
	case "", "pending", "partial":
		return v, nil
	default:
		return "", fmt.Errorf("invalid status: %s", status)
	}
}

// parseStartedAt converts a stored RFC3339 recording_started_at into a time.
// NULL and empty values yield nil.
func parseStartedAt(raw sql.NullString) (*time.Time, error) {
	if !raw.Valid || strings.TrimSpace(raw.String) == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, raw.String)
	if err != nil {
		return nil, fmt.Errorf("invalid recording_started_at %q: %w", raw.String, err)
	}
	return &t, nil
}

// withinStartedRange reports whether started satisfies the inclusive bounds.
// Rows with an unknown start time fail any set bound.
func withinStartedRange(started *time.Time, after, before *time.Time) bool {
	if after == nil && before == nil {
		return true
	}
	if started == nil {
		return false
	}
	if after != nil && started.Before(*after) {
		return false
	}
	if before != nil && started.After(*before) {
		return false
	}
	return true
}

func decodeListCursor(raw string) (*listCursor, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	b, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid cursor: %w", err)
	}
	var c listCursor
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("invalid cursor: %w", err)
	}
	if c.ID == "" || c.T < -1 {
		return nil, errors.New("invalid cursor: out of range")
	}
	return &c, nil
}

func encodeListCursor(item ConversationSummary) string {
	t := int64(-1)
	if item.RecordingStartedAt != nil {
		t = item.RecordingStartedAt.UnixMilli()
	}
	b, err := json.Marshal(listCursor{T: t, ID: item.ConversationID})
	if err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(b)
}

// conversationKeyLess orders summaries by (unknown time last, start desc, id asc).
func conversationKeyLess(a, b ConversationSummary) bool {
	an, bn := a.RecordingStartedAt == nil, b.RecordingStartedAt == nil
	if an != bn {
		return !an
	}
	if !an && !a.RecordingStartedAt.Equal(*b.RecordingStartedAt) {
		return a.RecordingStartedAt.After(*b.RecordingStartedAt)
	}
	return a.ConversationID < b.ConversationID
}

// conversationAfterCursor reports whether item sorts strictly after the cursor.
func conversationAfterCursor(item ConversationSummary, c *listCursor) bool {
	if c == nil {
		return true
	}
	null := item.RecordingStartedAt == nil
	if null != (c.T == -1) {
		return null
	}
	if !null {
		ms := item.RecordingStartedAt.UnixMilli()
		if ms != c.T {
			return ms < c.T
		}
	}
	return item.ConversationID > c.ID
}

// ListConversations returns conversations ordered by recording start time
// descending (unknown time last), then by conversation id ascending.
func (s *Store) ListConversations(ctx context.Context, f ConversationFilter) (ConversationPage, error) {
	if err := ctx.Err(); err != nil {
		return ConversationPage{}, err
	}
	status, err := normalizeStatus(f.Status)
	if err != nil {
		return ConversationPage{}, err
	}
	limit := clampPageLimit(f.Limit)
	cur, err := decodeListCursor(f.Cursor)
	if err != nil {
		return ConversationPage{}, err
	}

	rows, err := s.db.QueryContext(ctx, `SELECT c.id,c.title,c.recording_id,c.annotation_version,r.recording_started_at,
 (SELECT COUNT(*) FROM source_attachments a WHERE a.conversation_id=c.id AND a.active=1) +
 (SELECT COUNT(*) FROM recordings r2 WHERE r2.conversation_id=c.id AND NOT EXISTS(SELECT 1 FROM source_attachments a WHERE a.recording_id=r2.id)),
 (SELECT COUNT(*) FROM turns t WHERE t.conversation_id=c.id AND NOT EXISTS(SELECT 1 FROM attachment_turns at JOIN source_attachments a ON a.id=at.attachment_id WHERE at.turn_id=t.id AND a.active=0)),
 (SELECT COUNT(*) FROM turns t WHERE t.conversation_id=c.id AND NOT EXISTS(SELECT 1 FROM attachment_turns at JOIN source_attachments a ON a.id=at.attachment_id WHERE at.turn_id=t.id AND a.active=0) AND EXISTS(SELECT 1 FROM turn_topics tt WHERE tt.turn_id=t.id)),
 (SELECT COUNT(*) FROM notes n WHERE n.conversation_id=c.id AND NOT EXISTS(SELECT 1 FROM derived_artifacts d WHERE d.kind='note' AND d.artifact_id=n.id AND d.stale=1))
 FROM conversations c LEFT JOIN recordings r ON r.id=c.recording_id
 WHERE (?='' OR EXISTS(SELECT 1 FROM topic_conversations tc WHERE tc.topic_id=? AND tc.conversation_id=c.id))`,
		f.TopicID, f.TopicID)
	if err != nil {
		return ConversationPage{}, err
	}
	defer rows.Close()

	items := []ConversationSummary{}
	for rows.Next() {
		var item ConversationSummary
		var started sql.NullString
		var notes int
		if err = rows.Scan(&item.ConversationID, &item.Title, &item.RecordingID, &item.AnnotationVersion, &started,
			&item.ReceivedSegments, &item.OriginalTurns, &item.TopicLinkedTurns, &notes); err != nil {
			return ConversationPage{}, err
		}
		item.RecordingStartedAt, err = parseStartedAt(started)
		if err != nil {
			return ConversationPage{}, err
		}
		item.AnalysisStatus = "pending"
		if notes > 0 || item.TopicLinkedTurns > 0 {
			item.AnalysisStatus = "partial"
		}
		if status != "" && item.AnalysisStatus != status {
			continue
		}
		if !withinStartedRange(item.RecordingStartedAt, f.StartedAfter, f.StartedBefore) {
			continue
		}
		if !conversationAfterCursor(item, cur) {
			continue
		}
		items = append(items, item)
	}
	if err = rows.Err(); err != nil {
		return ConversationPage{}, err
	}
	sort.SliceStable(items, func(i, j int) bool { return conversationKeyLess(items[i], items[j]) })

	page := ConversationPage{Items: items}
	if len(items) > limit {
		page.Items = items[:limit]
		page.NextCursor = encodeListCursor(page.Items[len(page.Items)-1])
	}
	return page, nil
}

// GetEvidenceContext returns the target turn together with up to radius turns
// before and after it within the same recording, in transcript order.
func (s *Store) GetEvidenceContext(ctx context.Context, turnID string, radius, maxBytes int) (EvidenceContext, error) {
	if err := ctx.Err(); err != nil {
		return EvidenceContext{}, err
	}
	if strings.TrimSpace(turnID) == "" {
		return EvidenceContext{}, errors.New("turn id is required")
	}
	if radius < 0 {
		radius = 0
	} else if radius == 0 {
		radius = defaultEvidenceRadius
	}
	if maxBytes <= 0 {
		maxBytes = defaultEvidenceMaxByte
	}

	var recordingID string
	err := s.db.QueryRowContext(ctx, `SELECT recording_id FROM turns WHERE id=?`, turnID).Scan(&recordingID)
	if errors.Is(err, sql.ErrNoRows) {
		return EvidenceContext{}, fmt.Errorf("turn not found: %s: %w", turnID, ErrNotFound)
	}
	if err != nil {
		return EvidenceContext{}, err
	}
	stale, err := s.turnStale(ctx, turnID)
	if err != nil {
		return EvidenceContext{}, err
	}

	rows, err := s.db.QueryContext(ctx, `SELECT id,content,speaker_scoped_id,global_start_ms,global_end_ms,segment_index
 FROM turns WHERE recording_id=?
 ORDER BY (global_start_ms IS NULL),global_start_ms,segment_index,original_turn_id`, recordingID)
	if err != nil {
		return EvidenceContext{}, err
	}
	defer rows.Close()

	all := []TurnEvidence{}
	target := -1
	for rows.Next() {
		var ev TurnEvidence
		var speaker sql.NullString
		var start, end sql.NullInt64
		if err = rows.Scan(&ev.ID, &ev.Text, &speaker, &start, &end, &ev.SegmentIndex); err != nil {
			return EvidenceContext{}, err
		}
		if speaker.Valid {
			v := speaker.String
			ev.SpeakerScopedID = &v
		}
		if start.Valid {
			v := start.Int64
			ev.GlobalStartMS = &v
		}
		if end.Valid {
			v := end.Int64
			ev.GlobalEndMS = &v
		}
		if ev.ID == turnID {
			target = len(all)
		}
		all = append(all, ev)
	}
	if err = rows.Err(); err != nil {
		return EvidenceContext{}, err
	}
	if target < 0 {
		return EvidenceContext{}, fmt.Errorf("turn not found: %s: %w", turnID, ErrNotFound)
	}

	lo := target - radius
	if lo < 0 {
		lo = 0
	}
	hi := target + radius
	if hi >= len(all) {
		hi = len(all) - 1
	}

	out := []TurnEvidence{}
	used, truncated := 0, false
	for i := lo; i <= hi; i++ {
		ev := all[i]
		ev.IsTarget = ev.ID == turnID
		size := len([]byte(ev.Text))
		if used+size > maxBytes {
			truncated = true
			break
		}
		used += size
		out = append(out, ev)
	}
	return EvidenceContext{RecordingID: recordingID, Turns: out, Truncated: truncated, Stale: stale}, nil
}

func decodeSearchCursor(raw string) (*searchCursor, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	b, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid cursor: %w", err)
	}
	var c searchCursor
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("invalid cursor: %w", err)
	}
	if c.ID == "" || (c.Kind != "note" && c.Kind != "turn") {
		return nil, errors.New("invalid cursor: out of range")
	}
	return &c, nil
}

func encodeSearchCursor(hit SearchHit) string {
	b, err := json.Marshal(searchCursor{Kind: hit.Kind, ID: hit.ID})
	if err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(b)
}

// searchKindRank orders derived notes ahead of raw turns.
func searchKindRank(kind string) int {
	if kind == "note" {
		return 0
	}
	return 1
}

func searchHitAfterCursor(hit SearchHit, c *searchCursor) bool {
	if c == nil {
		return true
	}
	hr, cr := searchKindRank(hit.Kind), searchKindRank(c.Kind)
	if hr != cr {
		return hr > cr
	}
	return hit.ID > c.ID
}

// SearchMemory performs case-insensitive literal substring search across
// derived notes and raw transcript turns.
func (s *Store) SearchMemory(ctx context.Context, q MemorySearch) (SearchPage, error) {
	if err := ctx.Err(); err != nil {
		return SearchPage{}, err
	}
	text := strings.TrimSpace(q.Text)
	if text == "" {
		return SearchPage{}, errors.New("text is required")
	}
	scope := strings.ToLower(strings.TrimSpace(q.Scope))
	switch scope {
	case "", "both", "raw", "derived":
	default:
		return SearchPage{}, fmt.Errorf("invalid scope: %s", q.Scope)
	}
	limit := clampPageLimit(q.Limit)
	cur, err := decodeSearchCursor(q.Cursor)
	if err != nil {
		return SearchPage{}, err
	}
	lower := strings.ToLower(text)

	hits := []SearchHit{}
	if scope == "" || scope == "both" || scope == "derived" {
		noteHits, err := s.searchNotes(ctx, q, lower)
		if err != nil {
			return SearchPage{}, err
		}
		hits = append(hits, noteHits...)
	}
	if scope == "" || scope == "both" || scope == "raw" {
		turnHits, err := s.searchTurns(ctx, q, lower)
		if err != nil {
			return SearchPage{}, err
		}
		hits = append(hits, turnHits...)
	}

	sort.SliceStable(hits, func(i, j int) bool {
		ri, rj := searchKindRank(hits[i].Kind), searchKindRank(hits[j].Kind)
		if ri != rj {
			return ri < rj
		}
		return hits[i].ID < hits[j].ID
	})

	if cur != nil {
		kept := hits[:0]
		for _, hit := range hits {
			if searchHitAfterCursor(hit, cur) {
				kept = append(kept, hit)
			}
		}
		hits = kept
	}

	page := SearchPage{Hits: hits}
	if len(hits) > limit {
		page.Hits = hits[:limit]
		page.NextCursor = encodeSearchCursor(page.Hits[len(page.Hits)-1])
	}
	return page, nil
}

func (s *Store) searchNotes(ctx context.Context, q MemorySearch, lower string) ([]SearchHit, error) {
	noteSQL := `SELECT n.id,n.conversation_id,n.summary,r.recording_started_at
 FROM notes n JOIN conversations c ON c.id=n.conversation_id
 LEFT JOIN recordings r ON r.id=c.recording_id
 WHERE (?='' OR EXISTS(SELECT 1 FROM json_each(n.topic_ids_json) WHERE value=?))`
	if !q.IncludeStale {
		noteSQL += ` AND NOT EXISTS(SELECT 1 FROM derived_artifacts d WHERE d.kind='note' AND d.artifact_id=n.id AND d.stale=1)`
	}
	noteSQL += ` ORDER BY n.id`
	rows, err := s.db.QueryContext(ctx, noteSQL, q.TopicID, q.TopicID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Collect matches first and close the cursor: the store uses a single open
	// connection, so a nested evidence query while rows are open would deadlock.
	type matchedNote struct{ id, conv, summary string }
	matched := []matchedNote{}
	for rows.Next() {
		var id, conv, summary string
		var started sql.NullString
		if err := rows.Scan(&id, &conv, &summary, &started); err != nil {
			return nil, err
		}
		if !strings.Contains(strings.ToLower(summary), lower) {
			continue
		}
		at, err := parseStartedAt(started)
		if err != nil {
			return nil, err
		}
		if !withinStartedRange(at, q.StartedAfter, q.StartedBefore) {
			continue
		}
		matched = append(matched, matchedNote{id: id, conv: conv, summary: summary})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()

	out := []SearchHit{}
	for _, note := range matched {
		evidence, err := s.noteEvidenceTurnIDs(ctx, note.id)
		if err != nil {
			return nil, err
		}
		stale, err := s.noteStale(ctx, note.id)
		if err != nil {
			return nil, err
		}
		derivedFrom, err := s.noteDerivedFrom(ctx, note.id)
		if err != nil {
			return nil, err
		}
		out = append(out, SearchHit{
			Kind: "note", ID: note.id, Text: note.summary, ConversationID: note.conv,
			Why: "note summary contains text", Score: 1.0, EvidenceTurnIDs: evidence,
			Stale: stale, DerivedFrom: derivedFrom,
		})
	}
	return out, nil
}

func (s *Store) searchTurns(ctx context.Context, q MemorySearch, lower string) ([]SearchHit, error) {
	turnSQL := `SELECT t.id,t.conversation_id,t.content,t.global_start_ms,r.recording_started_at
 FROM turns t JOIN recordings r ON r.id=t.recording_id
 WHERE (?='' OR EXISTS(SELECT 1 FROM turn_topics tt WHERE tt.turn_id=t.id AND tt.topic_id=?))`
	if !q.IncludeStale {
		turnSQL += ` AND NOT EXISTS(SELECT 1 FROM attachment_turns at JOIN source_attachments a ON a.id=at.attachment_id WHERE at.turn_id=t.id AND a.active=0)`
	}
	turnSQL += ` ORDER BY t.id`
	rows, err := s.db.QueryContext(ctx, turnSQL, q.TopicID, q.TopicID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Collect matches first and close the cursor: the store uses a single open
	// connection, so a nested marker query while rows are open would deadlock.
	type matchedTurn struct {
		id, conv, content string
		globalStart       *int64
	}
	matched := []matchedTurn{}
	for rows.Next() {
		var id, conv, content string
		var start sql.NullInt64
		var started sql.NullString
		if err := rows.Scan(&id, &conv, &content, &start, &started); err != nil {
			return nil, err
		}
		if !strings.Contains(strings.ToLower(content), lower) {
			continue
		}
		at, err := parseStartedAt(started)
		if err != nil {
			return nil, err
		}
		if !withinStartedRange(at, q.StartedAfter, q.StartedBefore) {
			continue
		}
		var globalStart *int64
		if start.Valid {
			v := start.Int64
			globalStart = &v
		}
		matched = append(matched, matchedTurn{id: id, conv: conv, content: content, globalStart: globalStart})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()

	out := []SearchHit{}
	for _, turn := range matched {
		stale, err := s.turnStale(ctx, turn.id)
		if err != nil {
			return nil, err
		}
		derivedFrom, err := s.turnDerivedFrom(ctx, turn.id)
		if err != nil {
			return nil, err
		}
		out = append(out, SearchHit{
			Kind: "turn", ID: turn.id, Text: turn.content, ConversationID: turn.conv,
			Why: "transcript turn contains text", Score: 0.5, GlobalStartMS: turn.globalStart,
			Stale: stale, DerivedFrom: derivedFrom,
		})
	}
	return out, nil
}

func (s *Store) noteEvidenceTurnIDs(ctx context.Context, noteID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT turn_id FROM note_evidence WHERE note_id=? ORDER BY ordinal`, noteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
