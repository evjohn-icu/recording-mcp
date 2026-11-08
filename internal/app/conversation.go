package app

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"
	"time"

	"recordingmcp/internal/memory"
)

// Audio availability literals. They are the only values the web layer renders,
// so a missing file, a deleted recording and an unknown offset stay
// distinguishable instead of collapsing into "unavailable".
const (
	AudioAvailable     = "available"
	AudioDeleted       = "deleted"
	AudioMissing       = "missing"
	AudioUnavailable   = "unavailable"
	AudioOffsetUnknown = "offset_unknown"
	AudioOutOfRange    = "out_of_range"
)

// ConversationView is the web detail view of one conversation: the stored
// summary, the logical annotation view, its recordings and processing state.
type ConversationView struct {
	ConversationID     string                  `json:"conversation_id"`
	Title              string                  `json:"title"`
	Summary            string                  `json:"summary"`
	AnnotationVersion  string                  `json:"annotation_version"`
	AnnotationBasis    string                  `json:"annotation_basis"`
	RecordingStartedAt *time.Time              `json:"recording_started_at"` // null = 时间未知
	Recordings         []RecordingView         `json:"recordings"`
	Topics             []memory.Topic          `json:"topics"`
	Notes              []NoteView              `json:"notes"`
	Processing         memory.ProcessingStatus `json:"processing"`
}

// RecordingView is one stored recording of the conversation, including its
// primary one, with the local audio availability of each.
type RecordingView struct {
	RecordingID       string     `json:"recording_id"`
	StartedAt         *time.Time `json:"started_at"`
	AbsoluteTimeKnown bool       `json:"absolute_time_known"`
	LocalAudio        string     `json:"local_audio"` // available|deleted|missing|unavailable
	// SourceFile, SourceBytes and DurationMS describe the imported file. They
	// stay zero for recordings that were not imported through the web path.
	SourceFile  string `json:"source_file,omitempty"`
	SourceBytes int64  `json:"source_bytes,omitempty"`
	DurationMS  int64  `json:"duration_ms,omitempty"`
}

// NoteView is one note of the conversation's current logical annotation view.
// NoteID is the logical id that annotations_json stores; RowID is notes.id.
type NoteView struct {
	NoteID            string   `json:"note_id"`
	RowID             string   `json:"row_id"`
	Kind              string   `json:"kind"`
	Summary           string   `json:"summary"`
	TopicIDs          []string `json:"topic_ids"`
	EvidenceTurnIDs   []string `json:"evidence_turn_ids"`
	AnnotationVersion string   `json:"annotation_version"`
	Edited            bool     `json:"edited"`
}

// TurnView is one transcript turn plus the audio availability of its offset.
type TurnView struct {
	memory.Item
	SpeakerConfirmed bool   `json:"speaker_confirmed"`
	AudioStatus      string `json:"audio_status"` // available|offset_unknown|out_of_range|deleted|missing|unavailable
}

// TurnPage is one offset page of turns of a single recording.
type TurnPage struct {
	ConversationID string     `json:"conversation_id"`
	RecordingID    string     `json:"recording_id"`
	Items          []TurnView `json:"items"`
	NextOffset     *int       `json:"next_offset"` // nil = 没有更多
}

// ConversationView assembles the web detail view. Every slice is initialized
// so the JSON carries [] instead of null.
func (s *Service) ConversationView(ctx context.Context, conversationID string) (ConversationView, error) {
	view := ConversationView{
		ConversationID: conversationID,
		Recordings:     []RecordingView{},
		Topics:         []memory.Topic{},
		Notes:          []NoteView{},
	}
	if strings.TrimSpace(conversationID) == "" {
		return view, validationError("conversation_id is required")
	}
	if err := s.requireConversation(ctx, conversationID); err != nil {
		return view, err
	}
	title, err := s.Store.Conversation(ctx, conversationID)
	if err != nil {
		return view, err
	}
	current, version, err := s.Store.CurrentAnnotations(ctx, conversationID)
	if err != nil {
		return view, err
	}
	summary := current.Summary
	if strings.TrimSpace(summary) == "" {
		summary = title
	}
	started, err := s.Store.ConversationStartedAt(ctx, conversationID)
	if err != nil {
		return view, err
	}
	existing, err := s.currentNoteRowIDs(ctx, conversationID)
	if err != nil {
		return view, err
	}
	evidence, err := s.noteEvidence(ctx, conversationID)
	if err != nil {
		return view, err
	}
	for _, n := range current.Notes {
		rowID := memory.NoteRowID(conversationID, n.ID)
		if !existing[rowID] {
			// The logical view still names a note that no current note row
			// backs (superseded or deleted): it stays edit-mergeable but is not
			// shown as if it were current.
			continue
		}
		view.Notes = append(view.Notes, NoteView{
			NoteID:            n.ID,
			RowID:             rowID,
			Kind:              n.Kind,
			Summary:           n.Summary,
			TopicIDs:          nonNilStrings(n.TopicIDs),
			EvidenceTurnIDs:   nonNilStrings(evidence[rowID]),
			AnnotationVersion: version,
			Edited:            n.Edited,
		})
	}
	recordings, err := s.recordingViews(ctx, conversationID)
	if err != nil {
		return view, err
	}
	topics, err := s.Topics(ctx, conversationID)
	if err != nil {
		return view, err
	}
	processing, err := s.ProcessingStatus(ctx, conversationID)
	if err != nil {
		return view, err
	}
	view.Title = title
	view.Summary = summary
	view.AnnotationVersion = version
	view.AnnotationBasis = current.AnnotationBasis
	view.RecordingStartedAt = started
	view.Recordings = recordings
	view.Topics = nonNilTopics(topics.Topics)
	view.Processing = processing
	return view, nil
}

type WebEvidenceTurn struct {
	ItemView
	Text         string `json:"text"`
	SegmentIndex int    `json:"segment_index"`
	IsTarget     bool   `json:"is_target"`
}

type WebEvidenceContext struct {
	RecordingID string            `json:"recording_id"`
	Turns       []WebEvidenceTurn `json:"turns"`
	Truncated   bool              `json:"truncated"`
}

// WebEvidenceContext returns only current, non-ambient evidence and enriches
// each turn with confirmed speaker and playable-audio metadata.
func (s *Service) WebEvidenceContext(ctx context.Context, turnID string, radius, maxBytes int) (WebEvidenceContext, error) {
	ev, err := s.Store.GetCurrentEvidenceContext(ctx, turnID, radius, maxBytes)
	if err != nil {
		return WebEvidenceContext{}, err
	}
	out := WebEvidenceContext{RecordingID: ev.RecordingID, Turns: []WebEvidenceTurn{}, Truncated: ev.Truncated}
	for _, turn := range ev.Turns {
		var ambient bool
		if err := s.Store.DB().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM turn_classes WHERE turn_id=? AND class='ambient')`, turn.ID).Scan(&ambient); err != nil {
			return WebEvidenceContext{}, err
		}
		if ambient {
			if turn.IsTarget {
				return WebEvidenceContext{}, notFoundError("turn not found in current spoken evidence: " + turn.ID)
			}
			continue
		}
		detail, err := s.Store.GetItem(ctx, turn.ID, false)
		if err != nil {
			if errors.Is(err, memory.ErrNotFound) {
				continue // a concurrent revision change can invalidate a window row
			}
			return WebEvidenceContext{}, err
		}
		itemView, err := s.webItemView(ctx, detail.Item)
		if err != nil {
			return WebEvidenceContext{}, err
		}
		out.Turns = append(out.Turns, WebEvidenceTurn{ItemView: itemView, Text: turn.Text, SegmentIndex: turn.SegmentIndex, IsTarget: turn.IsTarget})
	}
	return out, nil
}

// ItemView is the web-safe item representation. AudioStatus is populated for
// turns and empty for non-turn items; embedded memory.Item fields retain the
// current recording, transcript revision and confirmed speaker projection.
type ItemView struct {
	memory.Item
	SpeakerConfirmed bool   `json:"speaker_confirmed"`
	AudioStatus      string `json:"audio_status"`
}

type WebItemDetail struct {
	Experimental bool       `json:"experimental"`
	Item         ItemView   `json:"item"`
	Evidence     []ItemView `json:"evidence,omitempty"`
}

// GetWebItem returns current-state item data for browser views. MCP's GetItem
// remains unchanged; this projection adds explicit audio availability and
// removes stale/ambient evidence from note neighborhoods.
func (s *Service) GetWebItem(ctx context.Context, id string) (WebItemDetail, error) {
	detail, err := s.GetItem(ctx, id, false)
	if err != nil {
		return WebItemDetail{}, err
	}
	ambient, err := s.Store.ItemAmbient(ctx, detail.Item)
	if err != nil {
		return WebItemDetail{}, err
	}
	if ambient {
		return WebItemDetail{}, notFoundError("item not found in current spoken view: " + id)
	}
	view := WebItemDetail{Experimental: detail.Experimental, Evidence: []ItemView{}}
	view.Item, err = s.webItemView(ctx, detail.Item)
	if err != nil {
		return WebItemDetail{}, err
	}
	if detail.Item.Kind != "turn" {
		current, err := s.currentTurnItems(ctx, detail.Item.ConversationID)
		if err != nil {
			return WebItemDetail{}, err
		}
		for _, evidence := range detail.Evidence {
			if item, ok := current[evidence.ID]; ok {
				wrapped, err := s.webItemView(ctx, item)
				if err != nil {
					return WebItemDetail{}, err
				}
				view.Evidence = append(view.Evidence, wrapped)
			}
		}
	}
	return view, nil
}

func (s *Service) webItemView(ctx context.Context, item memory.Item) (ItemView, error) {
	view := ItemView{Item: item, SpeakerConfirmed: item.SpeakerPersonID != nil && *item.SpeakerPersonID != ""}
	if item.Kind != "turn" {
		return view, nil
	}
	source, err := s.newAudioSource(ctx, item.ConversationID, item.RecordingID)
	if err != nil {
		return ItemView{}, err
	}
	view.AudioStatus = source.status(item.GlobalStartMS, item.GlobalEndMS)
	return view, nil
}

// currentTurnItems uses Store.Query for the authoritative current-revision and
// speaker-binding projection, while its default ambient policy matches the
// browser's spoken-evidence view.
func (s *Service) currentTurnItems(ctx context.Context, conversationID string) (map[string]memory.Item, error) {
	out := map[string]memory.Item{}
	offset := 0
	for {
		page, err := s.Store.Query(ctx, memory.Query{ConversationID: conversationID, IncludeTurns: true, Limit: 500, Offset: offset})
		if err != nil {
			return nil, err
		}
		for _, item := range page.Items {
			if item.Kind == "turn" {
				out[item.ID] = item
			}
		}
		if page.NextOffset == nil {
			break
		}
		offset = *page.NextOffset
	}
	return out, nil
}

// TurnPage returns one page of transcript turns of a single recording. A
// recording id is mandatory: the store's turn ordering does not separate
// recordings, so a conversation-wide page would interleave them.
func (s *Service) TurnPage(ctx context.Context, conversationID, recordingID string, includeAmbient bool, offset, limit int) (TurnPage, error) {
	page := TurnPage{ConversationID: conversationID, RecordingID: recordingID, Items: []TurnView{}}
	if strings.TrimSpace(conversationID) == "" {
		return page, validationError("conversation_id is required")
	}
	if strings.TrimSpace(recordingID) == "" {
		return page, validationError("recording_id is required")
	}
	if offset < 0 {
		return page, validationError("offset must be nonnegative")
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	if err := s.requireConversation(ctx, conversationID); err != nil {
		return page, err
	}
	if err := s.requireRecording(ctx, conversationID, recordingID); err != nil {
		return page, err
	}
	// Store.Query paginates the combined conversation/note/turn result. Page
	// through that result and apply the recording/turn projection before
	// applying the caller's turn offset; otherwise notes and the conversation
	// row consume slots and create empty or skipped transcript pages.
	const scanPageSize = 500
	turns := make([]memory.Item, 0, limit+1)
	scanOffset := 0
	turnOffset := 0
	for len(turns) < offset+limit+1 {
		memPage, err := s.Store.Query(ctx, memory.Query{
			ConversationID: conversationID,
			RecordingID:    recordingID,
			IncludeTurns:   true,
			IncludeAmbient: includeAmbient,
			Limit:          scanPageSize,
			Offset:         scanOffset,
		})
		if err != nil {
			return page, err
		}
		for _, it := range memPage.Items {
			if it.Kind != "turn" || it.RecordingID != recordingID {
				continue
			}
			if turnOffset < offset {
				turnOffset++
				continue
			}
			turns = append(turns, it)
			if len(turns) == offset+limit+1 {
				break
			}
		}
		if memPage.NextOffset == nil || len(turns) == offset+limit+1 {
			break
		}
		scanOffset = *memPage.NextOffset
	}
	more := len(turns) > limit
	if more {
		turns = turns[:limit]
	}
	audio, err := s.newAudioSource(ctx, conversationID, recordingID)
	if err != nil {
		return page, err
	}
	for _, it := range turns {
		page.Items = append(page.Items, TurnView{Item: it, SpeakerConfirmed: it.SpeakerPersonID != nil && *it.SpeakerPersonID != "", AudioStatus: audio.status(it.GlobalStartMS, it.GlobalEndMS)})
	}
	if more {
		next := offset + len(page.Items)
		page.NextOffset = &next
	}
	return page, nil
}

// requireConversation maps a missing conversation row onto a typed not_found.
// Store.Conversation returns a plain string error for it, so this existence
// probe is the only reliable signal.
func (s *Service) requireConversation(ctx context.Context, conversationID string) error {
	var one int
	err := s.Store.DB().QueryRowContext(ctx, `SELECT 1 FROM conversations WHERE id=?`, conversationID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return notFoundError("conversation not found: " + conversationID)
	}
	if err != nil {
		return err
	}
	return nil
}

func (s *Service) requireRecording(ctx context.Context, conversationID, recordingID string) error {
	var one int
	err := s.Store.DB().QueryRowContext(ctx, `SELECT 1 FROM recordings WHERE id=? AND conversation_id=?`, recordingID, conversationID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return notFoundError("recording not found: " + recordingID)
	}
	if err != nil {
		return err
	}
	return nil
}

// currentNoteRowIDs collects the ids of the notes the store currently reports
// for the conversation. The page total is not a note count: it also counts the
// conversation row and, when turns are requested, every turn.
func (s *Service) currentNoteRowIDs(ctx context.Context, conversationID string) (map[string]bool, error) {
	existing := map[string]bool{}
	offset := 0
	for {
		page, err := s.Store.Query(ctx, memory.Query{ConversationID: conversationID, Limit: 500, Offset: offset})
		if err != nil {
			return nil, err
		}
		for _, it := range page.Items {
			switch it.Kind {
			case "turn", "conversation":
			default:
				existing[it.ID] = true
			}
		}
		if page.NextOffset == nil {
			break
		}
		offset = *page.NextOffset
	}
	return existing, nil
}

// noteEvidence maps notes.id to its ordered evidence turn ids.
func (s *Service) noteEvidence(ctx context.Context, conversationID string) (map[string][]string, error) {
	rows, err := s.Store.DB().QueryContext(ctx,
		`SELECT ne.note_id, ne.turn_id FROM note_evidence ne
		 JOIN notes n ON n.id = ne.note_id JOIN turns t ON t.id=ne.turn_id
		 WHERE n.conversation_id=? AND `+memory.CurrentTurnPredicate("t")+`
		 AND NOT EXISTS(SELECT 1 FROM turn_classes tc WHERE tc.turn_id=t.id AND tc.class='ambient')
		 ORDER BY ne.note_id, ne.ordinal`, conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var noteID, turnID string
		if err := rows.Scan(&noteID, &turnID); err != nil {
			return nil, err
		}
		out[noteID] = append(out[noteID], turnID)
	}
	return out, rows.Err()
}

func (s *Service) recordingViews(ctx context.Context, conversationID string) ([]RecordingView, error) {
	var primary string
	if err := s.Store.DB().QueryRowContext(ctx, `SELECT recording_id FROM conversations WHERE id=?`, conversationID).Scan(&primary); err != nil {
		return nil, err
	}
	// The store owns a single SQLite connection, so the rows are fully read
	// before any per-recording query runs; a nested query while iterating
	// would deadlock on that connection.
	rows, err := s.Store.DB().QueryContext(ctx,
		`SELECT id, recording_started_at, absolute_time_known, source_file, source_bytes, duration_ms FROM recordings WHERE conversation_id=? ORDER BY id`, conversationID)
	if err != nil {
		return nil, err
	}
	out := []RecordingView{}
	for rows.Next() {
		var rec RecordingView
		var started sql.NullString
		if err := rows.Scan(&rec.RecordingID, &started, &rec.AbsoluteTimeKnown, &rec.SourceFile, &rec.SourceBytes, &rec.DurationMS); err != nil {
			rows.Close()
			return nil, err
		}
		parsed, err := memory.ParseStartedAt(started.String)
		if err != nil {
			rows.Close()
			return nil, err
		}
		rec.StartedAt = parsed
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range out {
		src, err := s.newAudioSource(ctx, conversationID, out[i].RecordingID)
		if err != nil {
			return nil, err
		}
		out[i].LocalAudio = src.state
	}
	// The conversation's own recording is the one its start time belongs to;
	// it stays first while the rest keep the stable id order.
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].RecordingID == primary && out[j].RecordingID != primary
	})
	return out, nil
}

func nonNilStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

func nonNilTopics(in []memory.Topic) []memory.Topic {
	if in == nil {
		return []memory.Topic{}
	}
	return in
}
