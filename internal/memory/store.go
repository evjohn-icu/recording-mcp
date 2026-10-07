package memory

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	_ "modernc.org/sqlite"

	"recordingmcp/internal/migrations"
)

type Store struct{ db *sql.DB }

func Open(path string) (*Store, error) {
	if path != "::memory:" && path != "" && path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000`); err != nil {
		db.Close()
		return nil, err
	}
	if err := migrations.Apply(context.Background(), db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db}, nil
}
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the underlying connection pool so packages that operate on the
// same database (for example link curation) share one SQLite connection
// instead of opening a competing pool. Callers must not close the handle;
// Store.Close owns it.
func (s *Store) DB() *sql.DB { return s.db }

func hashJSON(v any) (string, string, error) {
	b, e := json.Marshal(v)
	if e != nil {
		return "", "", e
	}
	h := sha256.Sum256(b)
	return string(b), hex.EncodeToString(h[:]), nil
}
func validID(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" || len(v) > 256 {
		return false
	}
	for _, r := range v {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
func ptrText(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}
func ptrInt(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

// Import validates the whole source pair before writing any row. Same IDs and bytes replay idempotently.
func (s *Store) Import(ctx context.Context, t Transcript, a Annotations, sourceFile, sourceHash string) (ImportReport, error) {
	if a.Version != 0 && a.Version != 1 {
		return ImportReport{}, fmt.Errorf("annotation version must be 1")
	}
	if !validID(a.ConversationID) || !validID(a.RecordingID) || strings.TrimSpace(a.Title) == "" {
		return ImportReport{}, errors.New("conversation_id, recording_id and title are required")
	}
	if sourceHash == "" {
		return ImportReport{}, errors.New("source file SHA-256 is required")
	}
	if math.IsNaN(t.DurationSeconds) || math.IsInf(t.DurationSeconds, 0) || t.DurationSeconds <= 0 {
		return ImportReport{}, errors.New("transcript duration must be positive")
	}
	duration := int64(math.Round(t.DurationSeconds * 1000))
	if t.Coverage.EndMS != 0 {
		if math.Abs(float64(t.Coverage.EndMS)-float64(duration)) > 1 {
			return ImportReport{}, errors.New("transcript coverage end does not match duration")
		}
		duration = t.Coverage.EndMS
	}
	if t.Coverage.StartMS < 0 {
		return ImportReport{}, errors.New("negative coverage start")
	}
	if len(a.Topics) == 0 {
		return ImportReport{}, errors.New("at least one topic is required")
	}
	topicMap := map[string]TopicAnnotation{}
	for _, x := range a.Topics {
		if !validID(x.ID) || strings.TrimSpace(x.Title) == "" {
			return ImportReport{}, fmt.Errorf("invalid topic %q", x.ID)
		}
		if _, ok := topicMap[x.ID]; ok {
			return ImportReport{}, fmt.Errorf("duplicate topic ID %q", x.ID)
		}
		topicMap[x.ID] = x
	}
	turnMap := map[string]Turn{}
	retained := []Turn{}
	overlap, blank := 0, 0
	for _, x := range t.Turns {
		if !validID(x.ID) {
			return ImportReport{}, fmt.Errorf("invalid turn id %q", x.ID)
		}
		if _, ok := turnMap[x.ID]; ok {
			return ImportReport{}, fmt.Errorf("duplicate turn id %q", x.ID)
		}
		turnMap[x.ID] = x
		if x.SegmentIndex < 1 || x.SegmentStartMS < 0 || x.SegmentEndMS <= x.SegmentStartMS || x.SegmentEndMS > duration {
			return ImportReport{}, fmt.Errorf("invalid segment boundary for %s", x.ID)
		}
		for name, v := range map[string]*int64{"local_start_ms": x.LocalStartMS, "local_end_ms": x.LocalEndMS, "global_start_ms": x.GlobalStartMS, "global_end_ms": x.GlobalEndMS} {
			if v != nil && (*v < 0 || *v > duration) {
				return ImportReport{}, fmt.Errorf("%s out of range for %s", name, x.ID)
			}
		}
		segmentMaxEnd := x.SegmentEndMS
		if segmentMaxEnd < duration {
			segmentMaxEnd += 5
		}
		if x.GlobalStartMS != nil && (*x.GlobalStartMS < x.SegmentStartMS || *x.GlobalStartMS > segmentMaxEnd || *x.GlobalStartMS > duration) || x.GlobalEndMS != nil && (*x.GlobalEndMS < x.SegmentStartMS || *x.GlobalEndMS > segmentMaxEnd || *x.GlobalEndMS > duration) {
			return ImportReport{}, fmt.Errorf("global time outside segment for %s", x.ID)
		}
		if x.GlobalStartMS != nil && x.GlobalEndMS != nil && *x.GlobalEndMS < *x.GlobalStartMS {
			return ImportReport{}, fmt.Errorf("end precedes start for %s", x.ID)
		}
		if x.OverlapDuplicate {
			overlap++
			continue
		}
		if strings.TrimSpace(x.Text) == "" {
			blank++
			continue
		}
		retained = append(retained, x)
	}
	kept := map[string]bool{}
	for _, x := range retained {
		kept[x.ID] = true
	}
	if len(a.Notes) == 0 {
		return ImportReport{}, errors.New("at least one annotated discussion note is required")
	}
	noteMap := map[string]NoteAnnotation{}
	for _, n := range a.Notes {
		if !validID(n.ID) || strings.TrimSpace(n.Summary) == "" {
			return ImportReport{}, fmt.Errorf("invalid note %q", n.ID)
		}
		switch n.Kind {
		case "observation", "suggestion", "request", "constraint", "intention":
		default:
			return ImportReport{}, fmt.Errorf("unsupported note kind %q", n.Kind)
		}
		if _, ok := noteMap[n.ID]; ok {
			return ImportReport{}, fmt.Errorf("duplicate note id %q", n.ID)
		}
		if len(n.EvidenceTurnIDs) == 0 {
			return ImportReport{}, fmt.Errorf("note %s needs evidence turns", n.ID)
		}
		if len(unique(n.EvidenceTurnIDs)) != len(n.EvidenceTurnIDs) {
			return ImportReport{}, fmt.Errorf("duplicate evidence turn in note %s", n.ID)
		}
		for _, topic := range n.TopicIDs {
			if _, ok := topicMap[topic]; !ok {
				return ImportReport{}, fmt.Errorf("note %s references unknown topic %s", n.ID, topic)
			}
		}
		for _, id := range n.EvidenceTurnIDs {
			if !kept[id] {
				return ImportReport{}, fmt.Errorf("note %s references unknown, blank or excluded turn %s", n.ID, id)
			}
		}
		noteMap[n.ID] = n
	}
	links := map[string][]string{}
	for _, tt := range a.TurnTopics {
		if !kept[tt.TurnID] {
			return ImportReport{}, fmt.Errorf("turn_topics references unknown, blank or excluded turn %s", tt.TurnID)
		}
		if len(unique(tt.TopicIDs)) != len(tt.TopicIDs) {
			return ImportReport{}, fmt.Errorf("duplicate topic link for turn %s", tt.TurnID)
		}
		for _, tid := range tt.TopicIDs {
			if _, ok := topicMap[tid]; !ok {
				return ImportReport{}, fmt.Errorf("turn %s references unknown topic %s", tt.TurnID, tid)
			}
		}
		links[tt.TurnID] = append(links[tt.TurnID], tt.TopicIDs...)
	}
	for _, n := range a.Notes {
		for _, id := range n.EvidenceTurnIDs {
			links[id] = append(links[id], n.TopicIDs...)
		}
	}
	for k, v := range links {
		links[k] = unique(v)
		sort.Strings(links[k])
	}
	transcriptJSON, transcriptHash, err := hashJSON(t)
	if err != nil {
		return ImportReport{}, err
	}
	annotationJSON, _, err := hashJSON(a)
	if err != nil {
		return ImportReport{}, err
	}
	transcriptVersion := "transcript-" + transcriptHash[:12]
	basis := a.AnnotationBasis
	if basis == "" {
		basis = "人工解释的讨论脉络；非经核实事实，不代表承诺"
	}
	summary := a.Summary
	if summary == "" {
		summary = a.Title
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ImportReport{}, err
	}
	defer tx.Rollback()
	var oldTranscript, oldAnnotation, oldSourceHash, oldRecording string
	err = tx.QueryRowContext(ctx, "SELECT transcript_json,annotations_json,source_sha256,recording_id FROM conversations WHERE id=?", a.ConversationID).Scan(&oldTranscript, &oldAnnotation, &oldSourceHash, &oldRecording)
	if err == nil {
		if oldTranscript != transcriptJSON || oldAnnotation != annotationJSON || oldSourceHash != sourceHash || oldRecording != a.RecordingID {
			return ImportReport{}, fmt.Errorf("conflicting import for conversation %s", a.ConversationID)
		}
		return ImportReport{ConversationID: a.ConversationID, RecordingID: a.RecordingID, Inserted: false, RetainedTurns: len(retained), ExcludedOverlapDuplicates: overlap, ExcludedBlankTurns: blank, Topics: len(topicMap), Notes: len(noteMap), SourceSHA256: sourceHash, TranscriptVersion: transcriptVersion}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return ImportReport{}, err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO conversations VALUES(?,?,?,?,?,?,?,?,?,?,?)", a.ConversationID, a.Title, summary, a.AnnotationVersion, basis, a.RecordingID, sourceHash, transcriptVersion, duration, transcriptJSON, annotationJSON)
	if err != nil {
		return ImportReport{}, err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO recordings(id,conversation_id,source_file,source_sha256,duration_ms,recording_started_at,absolute_time_known) VALUES(?,?,?,?,?,NULL,0)", a.RecordingID, a.ConversationID, filepath.Base(sourceFile), sourceHash, duration)
	if err != nil {
		return ImportReport{}, err
	}
	for _, topic := range a.Topics {
		var oldTitle string
		e := tx.QueryRowContext(ctx, "SELECT title FROM topics WHERE id=?", topic.ID).Scan(&oldTitle)
		if e == nil && oldTitle != topic.Title {
			return ImportReport{}, fmt.Errorf("topic ID %s already has conflicting title", topic.ID)
		}
		if errors.Is(e, sql.ErrNoRows) {
			if _, err = tx.ExecContext(ctx, "INSERT INTO topics VALUES(?,?)", topic.ID, topic.Title); err != nil {
				return ImportReport{}, err
			}
		} else if e != nil {
			return ImportReport{}, e
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO topic_conversations VALUES(?,?)", topic.ID, a.ConversationID); err != nil {
			return ImportReport{}, err
		}
	}
	turnDB := map[string]string{}
	for _, turn := range retained {
		turnDB[turn.ID] = turnKey(a.RecordingID, turn.ID)
	}
	for _, turn := range retained {
		raw, _, e := hashJSON(turn)
		if e != nil {
			return ImportReport{}, e
		}
		definite := any(nil)
		if turn.Definite != nil {
			definite = *turn.Definite
		}
		dbid := turnDB[turn.ID]
		_, err = tx.ExecContext(ctx, `INSERT INTO turns(id,conversation_id,recording_id,segment_index,global_start_ms,global_end_ms,local_start_ms,local_end_ms,speaker_scoped_id,speaker_entity_id,content,definite,source_sha256,transcript_version,original_turn_id,turn_json) VALUES(?,?,?,?,?,?,?,?,?,NULL,?,?,?,?,?,?)`, dbid, a.ConversationID, a.RecordingID, turn.SegmentIndex, ptrInt(turn.GlobalStartMS), ptrInt(turn.GlobalEndMS), ptrInt(turn.LocalStartMS), ptrInt(turn.LocalEndMS), ptrText(turn.SpeakerScopedID), turn.Text, definite, sourceHash, transcriptVersion, turn.ID, raw)
		if err != nil {
			return ImportReport{}, err
		}
		for _, topic := range links[turn.ID] {
			if _, err = tx.ExecContext(ctx, "INSERT INTO turn_topics VALUES(?,?)", dbid, topic); err != nil {
				return ImportReport{}, err
			}
		}
	}
	for _, n := range a.Notes {
		noteID := noteKey(a.ConversationID, n.ID)
		ids, _ := json.Marshal(n.TopicIDs)
		_, err = tx.ExecContext(ctx, "INSERT INTO notes VALUES(?,?,?,?,?,?,?,?)", noteID, a.ConversationID, n.Kind, n.Summary, string(ids), "inference", basis, a.AnnotationVersion)
		if err != nil {
			return ImportReport{}, err
		}
		for ix, id := range n.EvidenceTurnIDs {
			if _, err = tx.ExecContext(ctx, "INSERT INTO note_evidence VALUES(?,?,?)", noteID, turnDB[id], ix); err != nil {
				return ImportReport{}, err
			}
		}
	}
	if err = tx.Commit(); err != nil {
		return ImportReport{}, err
	}
	return ImportReport{ConversationID: a.ConversationID, RecordingID: a.RecordingID, Inserted: true, RetainedTurns: len(retained), ExcludedOverlapDuplicates: overlap, ExcludedBlankTurns: blank, Topics: len(topicMap), Notes: len(noteMap), SourceSHA256: sourceHash, TranscriptVersion: transcriptVersion}, nil
}
func unique(in []string) []string {
	m := map[string]bool{}
	o := []string{}
	for _, v := range in {
		if !m[v] {
			o = append(o, v)
			m[v] = true
		}
	}
	return o
}
func turnKey(recording, id string) string    { return "turn:" + hashID(recording+"\x00"+id) }
func noteKey(conversation, id string) string { return "note:" + hashID(conversation+"\x00"+id) }
func hashID(s string) string                 { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

func (s *Store) Conversation(ctx context.Context, id string) (string, error) {
	var title string
	err := s.db.QueryRowContext(ctx, "SELECT title FROM conversations WHERE id=?", id).Scan(&title)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("conversation not found: %s", id)
	}
	return title, err
}
func (s *Store) Query(ctx context.Context, q Query) (Page, error) {
	if q.Limit < 0 || q.Limit > 500 || q.Offset < 0 {
		return Page{}, errors.New("limit must be 0..500 and offset nonnegative")
	}
	if q.Limit == 0 {
		q.Limit = 100
	}
	if strings.TrimSpace(q.Query) == "" && q.TopicID == "" && q.ConversationID == "" && q.SpeakerScopedID == "" && q.RecordingID == "" {
		return Page{}, errors.New("at least one filter is required")
	}
	if q.SpeakerScopedID != "" && q.RecordingID == "" {
		return Page{}, errors.New("recording_id is required with speaker_scoped_id to prevent cross-recording identity joins")
	}
	var convo string
	if q.ConversationID != "" {
		if _, e := s.Conversation(ctx, q.ConversationID); e != nil {
			return Page{}, e
		}
		convo = q.ConversationID
	}
	items := []Item{}
	turnSQL := `SELECT t.id,t.conversation_id,t.recording_id,t.content,t.speaker_scoped_id,t.global_start_ms,t.global_end_ms,t.source_sha256,t.original_turn_id,t.transcript_version,c.title FROM turns t JOIN conversations c ON c.id=t.conversation_id WHERE (?='' OR t.conversation_id=?) AND (?='' OR t.recording_id=?) AND (?='' OR t.speaker_scoped_id=?) AND (?='' OR EXISTS(SELECT 1 FROM turn_topics z WHERE z.turn_id=t.id AND z.topic_id=?))`
	if !q.IncludeStale {
		turnSQL += ` AND ` + CurrentTurnPredicate("t")
	}
	if !q.IncludeAmbient {
		turnSQL += ` AND NOT EXISTS(SELECT 1 FROM turn_classes tc WHERE tc.turn_id=t.id AND tc.class='ambient')`
	}
	turnSQL += ` ORDER BY CASE WHEN t.global_start_ms IS NULL THEN 1 ELSE 0 END,t.global_start_ms,t.id`
	rows, err := s.db.QueryContext(ctx, turnSQL, convo, convo, q.RecordingID, q.RecordingID, q.SpeakerScopedID, q.SpeakerScopedID, q.TopicID, q.TopicID)
	if err != nil {
		return Page{}, err
	}
	defer rows.Close()
	turnItems := []Item{}
	for rows.Next() {
		var it Item
		var sp sql.NullString
		var st, en sql.NullInt64
		if err = rows.Scan(&it.ID, &it.ConversationID, &it.RecordingID, &it.Content, &sp, &st, &en, &it.SourceSHA256, &it.TurnID, &it.TranscriptVersion, &it.Title); err != nil {
			rows.Close()
			return Page{}, err
		}
		it.Kind = "turn"
		it.SpeakerEntityID = nil
		if sp.Valid {
			v := sp.String
			it.SpeakerScopedID = &v
		}
		if st.Valid {
			v := st.Int64
			it.GlobalStartMS = &v
		}
		if en.Valid {
			v := en.Int64
			it.GlobalEndMS = &v
		}
		if q.Query != "" && !literalMatch(it.Content, q.Query) {
			continue
		}
		turnItems = append(turnItems, it)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return Page{}, err
	}
	if err = rows.Close(); err != nil {
		return Page{}, err
	}
	for i := range turnItems {
		turnItems[i].TopicIDs, _ = s.turnTopics(ctx, turnItems[i].ID)
		turnItems[i].Stale, _ = s.turnStale(ctx, turnItems[i].ID)
		turnItems[i].DerivedFrom, _ = s.turnDerivedFrom(ctx, turnItems[i].ID)
		if q.IncludeAmbient {
			turnItems[i].Ambient, _ = s.turnAmbient(ctx, turnItems[i].ID)
		}
	}
	if q.IncludeTurns {
		if err := s.resolveSpeakerIdentities(ctx, turnItems); err != nil {
			return Page{}, err
		}
		items = append(items, turnItems...)
	}
	// Conversation and notes appear by default; turns only when explicitly requested.
	noteSQL := `SELECT n.id,n.conversation_id,n.kind,n.summary,n.topic_ids_json,n.claim_mode,n.interpretation_note,n.annotation_version,c.title FROM notes n JOIN conversations c ON c.id=n.conversation_id WHERE (?='' OR n.conversation_id=?) AND (?='' OR EXISTS(SELECT 1 FROM json_each(n.topic_ids_json) WHERE value=?)) AND (?='' OR EXISTS(SELECT 1 FROM note_evidence e JOIN turns t ON t.id=e.turn_id WHERE e.note_id=n.id AND t.speaker_scoped_id=? AND t.recording_id=?)) AND (?='' OR EXISTS(SELECT 1 FROM note_evidence e JOIN turns t ON t.id=e.turn_id WHERE e.note_id=n.id AND t.recording_id=?))`
	if !q.IncludeStale {
		noteSQL += ` AND NOT ` + NoteStalePredicate("n")
	}
	if !q.IncludeAmbient {
		// A note is ambient when its primary evidence turn (ordinal 0) is
		// classified ambient; unclassified turns never hide a note. Matches
		// SearchMemory exactly.
		noteSQL += ` AND NOT EXISTS(SELECT 1 FROM note_evidence ne JOIN turn_classes tc ON tc.turn_id=ne.turn_id WHERE ne.note_id=n.id AND ne.ordinal=0 AND tc.class='ambient')`
	}
	noteSQL += ` ORDER BY n.id`
	nrows, err := s.db.QueryContext(ctx, noteSQL, convo, convo, q.TopicID, q.TopicID, q.SpeakerScopedID, q.SpeakerScopedID, q.RecordingID, q.RecordingID, q.RecordingID)
	if err != nil {
		return Page{}, err
	}
	noteItems := []Item{}
	for nrows.Next() {
		var it Item
		var ids string
		if err = nrows.Scan(&it.ID, &it.ConversationID, &it.Kind, &it.Summary, &ids, &it.ClaimMode, &it.InterpretationNote, &it.AnnotationVersion, &it.Title); err != nil {
			nrows.Close()
			return Page{}, err
		}
		_ = json.Unmarshal([]byte(ids), &it.TopicIDs)
		it.SpeakerEntityID = nil
		if q.Query != "" && !literalMatch(it.Summary, q.Query) {
			continue
		}
		noteItems = append(noteItems, it)
	}
	if err = nrows.Err(); err != nil {
		nrows.Close()
		return Page{}, err
	}
	nrows.Close()
	for i := range noteItems {
		noteItems[i].Stale, _ = s.noteStale(ctx, noteItems[i].ID)
		noteItems[i].DerivedFrom, _ = s.noteDerivedFrom(ctx, noteItems[i].ID)
		if q.IncludeAmbient {
			ev, _ := s.noteEvidenceTurnIDs(ctx, noteItems[i].ID)
			if len(ev) > 0 {
				noteItems[i].Ambient, _ = s.turnAmbient(ctx, ev[0])
			}
		}
	}
	items = append(items, noteItems...)
	if q.ConversationID != "" && q.TopicID == "" && q.SpeakerScopedID == "" && q.RecordingID == "" {
		var id, title, summary, recording string
		err = s.db.QueryRowContext(ctx, "SELECT id,title,summary,recording_id FROM conversations WHERE id=?", q.ConversationID).Scan(&id, &title, &summary, &recording)
		if err != nil {
			return Page{}, err
		}
		it := Item{ID: id, Kind: "conversation", ConversationID: id, RecordingID: recording, Title: title, Summary: summary, SpeakerEntityID: nil}
		if q.Query == "" || literalMatch(summary+" "+title, q.Query) {
			items = append(items, it)
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		ai, aj := items[i], items[j]
		rank := func(k string) int {
			switch k {
			case "conversation":
				return 0
			case "observation", "suggestion", "request", "constraint", "intention":
				return 1
			case "turn":
				return 2
			}
			return 3
		}
		ri, rj := rank(ai.Kind), rank(aj.Kind)
		if ri != rj {
			return ri < rj
		}
		if ai.GlobalStartMS == nil && aj.GlobalStartMS != nil {
			return false
		}
		if ai.GlobalStartMS != nil && aj.GlobalStartMS == nil {
			return true
		}
		if ai.GlobalStartMS != nil && aj.GlobalStartMS != nil && *ai.GlobalStartMS != *aj.GlobalStartMS {
			return *ai.GlobalStartMS < *aj.GlobalStartMS
		}
		return ai.ID < aj.ID
	})
	total := len(items)
	if q.Offset > total {
		q.Offset = total
	}
	end := q.Offset + q.Limit
	if end > total {
		end = total
	}
	page := Page{Experimental: true, Items: append([]Item{}, items[q.Offset:end]...), Total: total}
	if end < total {
		v := end
		page.NextOffset = &v
	}
	return page, nil
}
func literalMatch(s, q string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(strings.TrimSpace(q)))
}
func (s *Store) turnTopics(ctx context.Context, id string) ([]string, error) {
	rows, e := s.db.QueryContext(ctx, "SELECT topic_id FROM turn_topics WHERE turn_id=? ORDER BY topic_id", id)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var x string
		if e = rows.Scan(&x); e != nil {
			return nil, e
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// turnStale reports whether any attachment that materialized a turn is
// superseded (active=0). Legacy turns with no attachment rows are current.
func (s *Store) turnStale(ctx context.Context, id string) (bool, error) {
	var stale bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM attachment_turns at JOIN source_attachments a ON a.id=at.attachment_id WHERE at.turn_id=? AND a.active=0)`, id).Scan(&stale)
	return stale, err
}

// noteStale reports whether a derived note has been marked stale.
func (s *Store) noteStale(ctx context.Context, id string) (bool, error) {
	var stale bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM derived_artifacts d WHERE d.kind='note' AND d.artifact_id=? AND d.stale=1)`, id).Scan(&stale)
	return stale, err
}

// turnDerivedFrom returns the transcript_revision values of the attachments a
// turn was ingested from, sorted ascending. Legacy turns with no attachment
// rows yield an empty slice.
func (s *Store) turnDerivedFrom(ctx context.Context, id string) ([]string, error) {
	return s.transcriptRevisions(ctx, `SELECT DISTINCT a.transcript_revision FROM attachment_turns at JOIN source_attachments a ON a.id=at.attachment_id WHERE at.turn_id=? ORDER BY a.transcript_revision`, id)
}

// resolveSpeakerIdentities fills SpeakerPersonID on turn items from the
// confirmed speaker bindings of each turn's own transcript revision. Identity
// is revision-scoped, so a turn resolves only when it carries exactly one
// source revision and a speaker label: legacy turns without attachment rows
// have no revision and stay nil, and a label whose binding is proposed,
// rejected or revoked has no confirmed identity and also stays nil. The
// SpeakerProjection lookup is cached per call keyed by (recording, revision),
// so a page of turns costs one query per distinct pair and no query at all
// when no turn carries a speaker label.
func (s *Store) resolveSpeakerIdentities(ctx context.Context, items []Item) error {
	cache := newSpeakerProjectionCache(s)
	for i := range items {
		it := &items[i]
		if it.Kind != "turn" || it.SpeakerScopedID == nil {
			continue
		}
		person, err := cache.person(ctx, it.RecordingID, it.DerivedFrom, *it.SpeakerScopedID)
		if err != nil {
			return err
		}
		it.SpeakerPersonID = person
	}
	return nil
}

// speakerProjectionCache memoizes confirmed speaker projections for one
// retrieval call, keyed by (recording_id, transcript_revision). It is shared by
// the Item and SearchHit turn paths so a page of turns costs one
// SpeakerProjection query per distinct pair and no query at all when no turn
// carries a speaker label.
type speakerProjectionCache struct {
	store *Store
	cache map[string]map[string]string
}

func newSpeakerProjectionCache(s *Store) *speakerProjectionCache {
	return &speakerProjectionCache{store: s, cache: map[string]map[string]string{}}
}

// person returns the confirmed person id for a speaker label on one transcript
// revision, or nil when the label is empty or unresolved. Identity is
// revision-scoped, so a label only resolves when it has exactly one source
// revision: legacy turns without attachment rows (no revision) and turns
// spanning multiple revisions are never projected, and a label whose binding is
// proposed, rejected or revoked has no confirmed identity either.
func (c *speakerProjectionCache) person(ctx context.Context, recordingID string, revisions []string, label string) (*string, error) {
	if label == "" || len(revisions) != 1 {
		return nil, nil
	}
	key := recordingID + "\x00" + revisions[0]
	proj, ok := c.cache[key]
	if !ok {
		var err error
		proj, err = c.store.SpeakerProjection(ctx, recordingID, revisions[0])
		if err != nil {
			return nil, err
		}
		c.cache[key] = proj
	}
	if person, found := proj[label]; found && person != "" {
		v := person
		return &v, nil
	}
	return nil, nil
}

// noteDerivedFrom returns the transcript_revision values of the attachments a
// note was derived from, sorted ascending. Legacy notes yield an empty slice.
func (s *Store) noteDerivedFrom(ctx context.Context, id string) ([]string, error) {
	return s.transcriptRevisions(ctx, `SELECT DISTINCT a.transcript_revision FROM derived_artifact_sources s JOIN source_attachments a ON a.id=s.attachment_id WHERE s.kind='note' AND s.artifact_id=? ORDER BY a.transcript_revision`, id)
}

func (s *Store) transcriptRevisions(ctx context.Context, query, id string) ([]string, error) {
	rows, e := s.db.QueryContext(ctx, query, id)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var x string
		if e = rows.Scan(&x); e != nil {
			return nil, e
		}
		out = append(out, x)
	}
	return out, rows.Err()
}
func (s *Store) Topics(ctx context.Context, conversation string) (TopicList, error) {
	rows, e := s.db.QueryContext(ctx, "SELECT t.id,t.title,tc.conversation_id FROM topics t JOIN topic_conversations tc ON tc.topic_id=t.id WHERE (?='' OR tc.conversation_id=?) ORDER BY tc.conversation_id,t.id", conversation, conversation)
	if e != nil {
		return TopicList{}, e
	}
	defer rows.Close()
	out := TopicList{Experimental: true, Topics: []Topic{}}
	for rows.Next() {
		var x Topic
		if e = rows.Scan(&x.ID, &x.Title, &x.ConversationID); e != nil {
			return TopicList{}, e
		}
		out.Topics = append(out.Topics, x)
	}
	return out, rows.Err()
}
func (s *Store) GetItem(ctx context.Context, id string, includeStale bool) (ItemDetail, error) {
	var it Item
	var sp, record, content, source, version sql.NullString
	var st, en sql.NullInt64
	turnSQL := `SELECT id,conversation_id,recording_id,content,speaker_scoped_id,global_start_ms,global_end_ms,source_sha256,original_turn_id,transcript_version FROM turns WHERE id=?`
	if !includeStale {
		turnSQL += ` AND ` + CurrentTurnPredicate("turns")
	}
	err := s.db.QueryRowContext(ctx, turnSQL, id).Scan(&it.ID, &it.ConversationID, &record, &content, &sp, &st, &en, &source, &it.TurnID, &version)
	if err == nil {
		it.Kind = "turn"
		it.RecordingID = record.String
		it.Content = content.String
		it.SourceSHA256 = source.String
		it.TranscriptVersion = version.String
		it.SpeakerEntityID = nil
		if sp.Valid {
			v := sp.String
			it.SpeakerScopedID = &v
		}
		if st.Valid {
			v := st.Int64
			it.GlobalStartMS = &v
		}
		if en.Valid {
			v := en.Int64
			it.GlobalEndMS = &v
		}
		it.TopicIDs, _ = s.turnTopics(ctx, id)
		it.Stale, _ = s.turnStale(ctx, id)
		it.DerivedFrom, _ = s.turnDerivedFrom(ctx, id)
		resolved := []Item{it}
		if err := s.resolveSpeakerIdentities(ctx, resolved); err != nil {
			return ItemDetail{}, err
		}
		it = resolved[0]
		return ItemDetail{Experimental: true, Item: it}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return ItemDetail{}, err
	}
	var topics string
	noteSQL := "SELECT id,conversation_id,kind,summary,topic_ids_json,claim_mode,interpretation_note,annotation_version FROM notes WHERE id=?"
	if !includeStale {
		noteSQL += ` AND NOT ` + NoteStalePredicate("notes")
	}
	err = s.db.QueryRowContext(ctx, noteSQL, id).Scan(&it.ID, &it.ConversationID, &it.Kind, &it.Summary, &topics, &it.ClaimMode, &it.InterpretationNote, &it.AnnotationVersion)
	if err == nil {
		_ = json.Unmarshal([]byte(topics), &it.TopicIDs)
		it.SpeakerEntityID = nil
		it.Stale, _ = s.noteStale(ctx, id)
		it.DerivedFrom, _ = s.noteDerivedFrom(ctx, id)
		ev, er := s.noteEvidence(ctx, id)
		if er != nil {
			return ItemDetail{}, er
		}
		return ItemDetail{Experimental: true, Item: it, Evidence: ev}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return ItemDetail{}, err
	}
	var title, summary, recording string
	err = s.db.QueryRowContext(ctx, "SELECT title,summary,recording_id FROM conversations WHERE id=?", id).Scan(&title, &summary, &recording)
	if errors.Is(err, sql.ErrNoRows) {
		return ItemDetail{}, fmt.Errorf("context item not found: %s", id)
	}
	it = Item{ID: id, Kind: "conversation", ConversationID: id, Title: title, Summary: summary, RecordingID: recording, SpeakerEntityID: nil}
	return ItemDetail{Experimental: true, Item: it}, err
}
func (s *Store) noteEvidence(ctx context.Context, id string) ([]Item, error) {
	rows, e := s.db.QueryContext(ctx, "SELECT t.id,t.conversation_id,t.recording_id,t.content,t.speaker_scoped_id,t.global_start_ms,t.global_end_ms,t.source_sha256,t.transcript_version,t.original_turn_id FROM note_evidence n JOIN turns t ON t.id=n.turn_id WHERE n.note_id=? ORDER BY n.ordinal", id)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Item{}
	for rows.Next() {
		var x Item
		var sp sql.NullString
		var st, en sql.NullInt64
		if e = rows.Scan(&x.ID, &x.ConversationID, &x.RecordingID, &x.Content, &sp, &st, &en, &x.SourceSHA256, &x.TranscriptVersion, &x.TurnID); e != nil {
			return nil, e
		}
		x.Kind = "turn"
		x.SpeakerEntityID = nil
		if sp.Valid {
			v := sp.String
			x.SpeakerScopedID = &v
		}
		if st.Valid {
			v := st.Int64
			x.GlobalStartMS = &v
		}
		if en.Valid {
			v := en.Int64
			x.GlobalEndMS = &v
		}
		out = append(out, x)
	}
	return out, rows.Err()
}
