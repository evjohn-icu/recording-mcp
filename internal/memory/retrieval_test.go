package memory

import (
	"context"
	"encoding/base64"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mustExec(t *testing.T, s *Store, query string, args ...any) {
	t.Helper()
	if _, err := s.db.Exec(query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

func seedTurn(t *testing.T, s *Store, id, conv, rec string, seg int, start int64, content string) {
	t.Helper()
	mustExec(t, s, `INSERT INTO turns(id,conversation_id,recording_id,segment_index,global_start_ms,global_end_ms,local_start_ms,local_end_ms,speaker_scoped_id,speaker_entity_id,content,definite,source_sha256,transcript_version,original_turn_id,turn_json)
 VALUES(?,?,?,?,?,?,NULL,NULL,NULL,NULL,?,NULL,?,?,?, '{}')`,
		id, conv, rec, seg, start, start+500, content, strings.Repeat("a", 64), "rev-1", id)
}

// seedRetrieval builds three conversations:
//   - conv:a (rec:a, known start 2026-03) with five turns and no notes;
//   - conv:b (rec:b, known start 2026-01) with two turns, one note, one topic;
//   - conv:c (rec:c, NULL start) with one turn and one note.
func seedRetrieval(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "retrieval.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	conv := `INSERT INTO conversations(id,title,summary,annotation_version,annotation_basis,recording_id,source_sha256,transcript_version,duration_ms,transcript_json,annotations_json) VALUES(?,?,'','v1','',?,?,?,?, '{}','{}')`
	mustExec(t, s, conv, "conv:a", "Alpha", "rec:a", "sha", "tv", 6000)
	mustExec(t, s, conv, "conv:b", "Beta", "rec:b", "sha", "tv", 3000)
	mustExec(t, s, conv, "conv:c", "Gamma", "rec:c", "sha", "tv", 2000)

	rec := `INSERT INTO recordings(id,conversation_id,source_file,source_sha256,duration_ms,recording_started_at,absolute_time_known) VALUES(?,?,'file','sha',?,?,?)`
	mustExec(t, s, rec, "rec:a", "conv:a", 6000, "2026-03-01T10:00:00Z", 1)
	mustExec(t, s, rec, "rec:b", "conv:b", 3000, "2026-01-01T00:00:00Z", 1)
	mustExec(t, s, rec, "rec:c", "conv:c", 2000, nil, 0)

	seedTurn(t, s, "t:a1", "conv:a", "rec:a", 0, 1000, "Android deployment plan 100% ready")
	seedTurn(t, s, "t:a2", "conv:a", "rec:a", 1, 2000, "Second turn about budget")
	seedTurn(t, s, "t:a3", "conv:a", "rec:a", 2, 3000, "Third turn discussing android again")
	seedTurn(t, s, "t:a4", "conv:a", "rec:a", 3, 4000, "Fourth turn")
	seedTurn(t, s, "t:a5", "conv:a", "rec:a", 4, 5000, "Fifth turn underscore a_b marker")
	seedTurn(t, s, "t:b1", "conv:b", "rec:b", 0, 1000, "Budget review for android project")
	seedTurn(t, s, "t:b2", "conv:b", "rec:b", 1, 2000, "unrelated text")
	seedTurn(t, s, "t:c1", "conv:c", "rec:c", 0, 1000, "android without a known start time")

	note := `INSERT INTO notes(id,conversation_id,kind,summary,topic_ids_json,claim_mode,interpretation_note,annotation_version) VALUES(?,?,'observation',?,?,'inference','','v1')`
	mustExec(t, s, note, "n:b1", "conv:b", "Android roadmap note 100% done", `["topic:dev"]`)
	mustExec(t, s, note, "n:c1", "conv:c", "android note without time", `[]`)

	mustExec(t, s, `INSERT INTO note_evidence(note_id,turn_id,ordinal) VALUES('n:b1','t:b1',0)`)
	mustExec(t, s, `INSERT INTO note_evidence(note_id,turn_id,ordinal) VALUES('n:c1','t:c1',0)`)

	mustExec(t, s, `INSERT INTO topics(id,title) VALUES('topic:dev','开发')`)
	mustExec(t, s, `INSERT INTO topic_conversations(topic_id,conversation_id) VALUES('topic:dev','conv:b')`)
	mustExec(t, s, `INSERT INTO turn_topics(turn_id,topic_id) VALUES('t:b1','topic:dev')`)
	return s
}

func idsOf(page ConversationPage) []string {
	out := make([]string, 0, len(page.Items))
	for _, item := range page.Items {
		out = append(out, item.ConversationID)
	}
	return out
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestListConversationsOrderingFilteringAndCursor(t *testing.T) {
	s := seedRetrieval(t)
	ctx := context.Background()
	at := func(v string) *time.Time {
		parsed, err := time.Parse(time.RFC3339, v)
		if err != nil {
			t.Fatal(err)
		}
		return &parsed
	}

	page, err := s.ListConversations(ctx, ConversationFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if got := idsOf(page); !sameStrings(got, []string{"conv:a", "conv:b", "conv:c"}) {
		t.Fatalf("default order = %v", got)
	}
	if page.NextCursor != "" {
		t.Fatalf("unexpected cursor %q for a single full page", page.NextCursor)
	}
	if page.Items[0].RecordingStartedAt == nil || page.Items[2].RecordingStartedAt != nil {
		t.Fatalf("recording_started_at not populated: %+v", page.Items)
	}
	if page.Items[0].OriginalTurns != 5 || page.Items[1].OriginalTurns != 2 || page.Items[2].OriginalTurns != 1 {
		t.Fatalf("turn counts wrong: %+v", page.Items)
	}
	if page.Items[1].TopicLinkedTurns != 1 || page.Items[1].AnalysisStatus != "partial" {
		t.Fatalf("topic-linked partial expected: %+v", page.Items[1])
	}
	if page.Items[0].AnalysisStatus != "pending" {
		t.Fatalf("conv:a should be pending: %+v", page.Items[0])
	}

	// Time bounds apply only to known start times; NULL start is excluded.
	page, err = s.ListConversations(ctx, ConversationFilter{StartedAfter: at("2026-02-01T00:00:00Z")})
	if err != nil {
		t.Fatal(err)
	}
	if got := idsOf(page); !sameStrings(got, []string{"conv:a"}) {
		t.Fatalf("started_after filter = %v", got)
	}
	page, err = s.ListConversations(ctx, ConversationFilter{StartedBefore: at("2026-02-01T00:00:00Z")})
	if err != nil {
		t.Fatal(err)
	}
	if got := idsOf(page); !sameStrings(got, []string{"conv:b"}) {
		t.Fatalf("started_before filter = %v", got)
	}

	page, err = s.ListConversations(ctx, ConversationFilter{TopicID: "topic:dev"})
	if err != nil {
		t.Fatal(err)
	}
	if got := idsOf(page); !sameStrings(got, []string{"conv:b"}) {
		t.Fatalf("topic filter = %v", got)
	}

	page, err = s.ListConversations(ctx, ConversationFilter{Status: "partial"})
	if err != nil {
		t.Fatal(err)
	}
	if got := idsOf(page); !sameStrings(got, []string{"conv:b", "conv:c"}) {
		t.Fatalf("status partial = %v", got)
	}
	page, err = s.ListConversations(ctx, ConversationFilter{Status: "PENDING"})
	if err != nil || !sameStrings(idsOf(page), []string{"conv:a"}) {
		t.Fatalf("status pending = %v %v", idsOf(page), err)
	}
	if _, err = s.ListConversations(ctx, ConversationFilter{Status: "bogus"}); err == nil {
		t.Fatal("invalid status must error")
	}

	// Cursor walk with limit 1 visits every row exactly once.
	seen := []string{}
	cursor := ""
	for range 5 {
		p, err := s.ListConversations(ctx, ConversationFilter{Limit: 1, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		seen = append(seen, idsOf(p)...)
		if p.NextCursor == "" {
			break
		}
		cursor = p.NextCursor
	}
	if !sameStrings(seen, []string{"conv:a", "conv:b", "conv:c"}) {
		t.Fatalf("cursor walk = %v", seen)
	}

	if _, err = s.ListConversations(ctx, ConversationFilter{Cursor: "!!!"}); err == nil {
		t.Fatal("malformed cursor must error")
	}
	outOfRange := base64.StdEncoding.EncodeToString([]byte(`{"t":-2,"id":"x"}`))
	if _, err = s.ListConversations(ctx, ConversationFilter{Cursor: outOfRange}); err == nil {
		t.Fatal("out-of-range cursor must error")
	}
}

func TestGetEvidenceContextRadiusAndTruncation(t *testing.T) {
	s := seedRetrieval(t)
	ctx := context.Background()

	ev, err := s.GetEvidenceContext(ctx, "t:a3", 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if ev.RecordingID != "rec:a" {
		t.Fatalf("recording = %q", ev.RecordingID)
	}
	var ids []string
	for _, turn := range ev.Turns {
		ids = append(ids, turn.ID)
	}
	if !sameStrings(ids, []string{"t:a2", "t:a3", "t:a4"}) {
		t.Fatalf("radius 1 window = %v", ids)
	}
	if !ev.Turns[1].IsTarget || ev.Turns[0].IsTarget || ev.Turns[2].IsTarget {
		t.Fatalf("target flag wrong: %+v", ev.Turns)
	}
	if ev.Turns[1].GlobalStartMS == nil || *ev.Turns[1].GlobalStartMS != 3000 || ev.Turns[1].SegmentIndex != 2 {
		t.Fatalf("target fields wrong: %+v", ev.Turns[1])
	}

	// radius 0 means "use the default 3"; negative means zero neighbors.
	ev, err = s.GetEvidenceContext(ctx, "t:a3", 0, 0)
	if err != nil || len(ev.Turns) != 5 {
		t.Fatalf("default radius = %+v %v", ev.Turns, err)
	}
	ev, err = s.GetEvidenceContext(ctx, "t:a3", -1, 0)
	if err != nil || len(ev.Turns) != 1 || ev.Turns[0].ID != "t:a3" || !ev.Turns[0].IsTarget {
		t.Fatalf("negative radius = %+v %v", ev.Turns, err)
	}

	// Budget accumulates whole turns in transcript order and stops on overflow.
	ev, err = s.GetEvidenceContext(ctx, "t:a1", 3, 60)
	if err != nil {
		t.Fatal(err)
	}
	ids = ids[:0]
	for _, turn := range ev.Turns {
		ids = append(ids, turn.ID)
	}
	if !sameStrings(ids, []string{"t:a1", "t:a2"}) || !ev.Truncated {
		t.Fatalf("truncation = %v truncated=%v", ids, ev.Truncated)
	}
	if !ev.Turns[0].IsTarget {
		t.Fatalf("target should lead a truncated chronological window: %+v", ev.Turns)
	}

	if _, err = s.GetEvidenceContext(ctx, "turn:missing", 1, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing turn err = %v", err)
	}
	if _, err = s.GetEvidenceContext(ctx, "  ", 1, 0); err == nil {
		t.Fatal("blank turn id must error")
	}
}

func TestSearchMemoryScopesCaseEscapesAndCursor(t *testing.T) {
	s := seedRetrieval(t)
	ctx := context.Background()

	derived, err := s.SearchMemory(ctx, MemorySearch{Text: "android", Scope: "derived"})
	if err != nil {
		t.Fatal(err)
	}
	if len(derived.Hits) != 2 || derived.Hits[0].Kind != "note" || derived.Hits[0].ID != "n:b1" || derived.Hits[1].ID != "n:c1" {
		t.Fatalf("derived hits = %+v", derived.Hits)
	}
	if derived.Hits[0].Score != 1.0 || !sameStrings(derived.Hits[0].EvidenceTurnIDs, []string{"t:b1"}) {
		t.Fatalf("derived hit metadata = %+v", derived.Hits[0])
	}
	if derived.Hits[0].ConversationID != "conv:b" {
		t.Fatalf("conversation id = %q", derived.Hits[0].ConversationID)
	}

	raw, err := s.SearchMemory(ctx, MemorySearch{Text: "android", Scope: "raw"})
	if err != nil {
		t.Fatal(err)
	}
	var rawIDs []string
	for _, hit := range raw.Hits {
		if hit.Kind != "turn" || hit.Score != 0.5 {
			t.Fatalf("raw hit kind/score = %+v", hit)
		}
		rawIDs = append(rawIDs, hit.ID)
	}
	if !sameStrings(rawIDs, []string{"t:a1", "t:a3", "t:b1", "t:c1"}) {
		t.Fatalf("raw hits = %v", rawIDs)
	}
	if raw.Hits[0].GlobalStartMS == nil || *raw.Hits[0].GlobalStartMS != 1000 {
		t.Fatalf("raw global start = %+v", raw.Hits[0])
	}

	// Both scopes: derived precedes raw, each group ordered by id.
	all, err := s.SearchMemory(ctx, MemorySearch{Text: "ANDROID"})
	if err != nil {
		t.Fatal(err)
	}
	var allKeys []string
	for _, hit := range all.Hits {
		allKeys = append(allKeys, hit.Kind+"/"+hit.ID)
	}
	want := []string{"note/n:b1", "note/n:c1", "turn/t:a1", "turn/t:a3", "turn/t:b1", "turn/t:c1"}
	if !sameStrings(allKeys, want) {
		t.Fatalf("combined hits = %v", allKeys)
	}

	// % and _ are literals, never wildcards.
	pct, err := s.SearchMemory(ctx, MemorySearch{Text: "%"})
	if err != nil {
		t.Fatal(err)
	}
	if len(pct.Hits) != 2 || pct.Hits[0].ID != "n:b1" || pct.Hits[1].ID != "t:a1" {
		t.Fatalf("percent literal hits = %+v", pct.Hits)
	}
	under, err := s.SearchMemory(ctx, MemorySearch{Text: "_"})
	if err != nil || len(under.Hits) != 1 || under.Hits[0].ID != "t:a5" {
		t.Fatalf("underscore literal hits = %+v %v", under.Hits, err)
	}
	if no, err := s.SearchMemory(ctx, MemorySearch{Text: "axb"}); err != nil || len(no.Hits) != 0 {
		t.Fatalf("underscore must not match axb: %+v %v", no.Hits, err)
	}

	// Cursor walk with limit 1 yields the combined ordering.
	seen := []string{}
	cursor := ""
	for range len(want) + 1 {
		p, err := s.SearchMemory(ctx, MemorySearch{Text: "android", Limit: 1, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, hit := range p.Hits {
			seen = append(seen, hit.Kind+"/"+hit.ID)
		}
		if p.NextCursor == "" {
			break
		}
		cursor = p.NextCursor
	}
	if !sameStrings(seen, want) {
		t.Fatalf("cursor walk = %v", seen)
	}

	// Time and topic filters constrain both scopes.
	after, _ := time.Parse(time.RFC3339, "2026-02-01T00:00:00Z")
	timed, err := s.SearchMemory(ctx, MemorySearch{Text: "android", StartedAfter: &after})
	if err != nil {
		t.Fatal(err)
	}
	var timedIDs []string
	for _, hit := range timed.Hits {
		timedIDs = append(timedIDs, hit.ID)
	}
	if !sameStrings(timedIDs, []string{"t:a1", "t:a3"}) {
		t.Fatalf("time-filtered hits = %v", timedIDs)
	}
	topiced, err := s.SearchMemory(ctx, MemorySearch{Text: "android", TopicID: "topic:dev"})
	if err != nil {
		t.Fatal(err)
	}
	var topicedKeys []string
	for _, hit := range topiced.Hits {
		topicedKeys = append(topicedKeys, hit.Kind+"/"+hit.ID)
	}
	if !sameStrings(topicedKeys, []string{"note/n:b1", "turn/t:b1"}) {
		t.Fatalf("topic-filtered hits = %v", topicedKeys)
	}

	if _, err = s.SearchMemory(ctx, MemorySearch{Text: "  "}); err == nil {
		t.Fatal("blank text must error")
	}
	if _, err = s.SearchMemory(ctx, MemorySearch{Text: "android", Scope: "everything"}); err == nil {
		t.Fatal("invalid scope must error")
	}
	badCursor := base64.StdEncoding.EncodeToString([]byte(`{"kind":"bogus","id":"x"}`))
	if _, err = s.SearchMemory(ctx, MemorySearch{Text: "android", Cursor: badCursor}); err == nil {
		t.Fatal("invalid cursor kind must error")
	}
	if _, err = s.SearchMemory(ctx, MemorySearch{Text: "android", Cursor: "###"}); err == nil {
		t.Fatal("malformed cursor must error")
	}
}

// TestRetrievalExcludesStaleDerivedMemory proves every current-state retrieval
// surface hides memory derived from a superseded transcript revision, while
// the explicit audit switches surface it with honest stale markers and
// derived_from provenance.
func TestRetrievalExcludesStaleDerivedMemory(t *testing.T) {
	ctx := context.Background()
	s := openTempStore(t)

	v1 := sampleResult("seg-a", "rev-1", 0)
	v1.Turns[0].Text = "superseded phrase alpha"
	if _, err := s.IngestTranscript(ctx, v1); err != nil {
		t.Fatal(err)
	}
	var conv, v1TurnID string
	if err := s.db.QueryRow(`SELECT conversation_id, id FROM turns`).Scan(&conv, &v1TurnID); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyAnnotations(ctx, conv, "", Annotations{
		ConversationID: conv, AnnotationVersion: "rev-1", Title: "stale conversation",
		Topics: []TopicAnnotation{{ID: "topic:stale", Title: "stale topic"}},
		Notes: []NoteAnnotation{{
			ID: "note:stale", Kind: "observation", TopicIDs: []string{"topic:stale"},
			Summary: "stale summary phrase", EvidenceTurnIDs: []string{v1TurnID},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	var noteID string
	if err := s.db.QueryRow(`SELECT id FROM notes`).Scan(&noteID); err != nil {
		t.Fatal(err)
	}

	// While rev-1 is active the note is current.
	if page, err := s.SearchMemory(ctx, MemorySearch{Text: "stale summary", Scope: "both"}); err != nil || len(page.Hits) != 1 || page.Hits[0].Stale {
		t.Fatalf("current note not visible before supersede: %+v %v", page, err)
	}

	v2 := sampleResult("seg-a", "rev-2", 0)
	v2.Turns[0].Text = "current phrase beta"
	v2.Source.SupersedesRevision = "rev-1"
	if _, err := s.IngestTranscript(ctx, v2); err != nil {
		t.Fatal(err)
	}

	// SearchMemory: stale note and stale turn are gone by default.
	if page, err := s.SearchMemory(ctx, MemorySearch{Text: "stale summary", Scope: "both"}); err != nil || len(page.Hits) != 0 {
		t.Fatalf("stale note surfaced by default: %+v %v", page, err)
	}
	if page, err := s.SearchMemory(ctx, MemorySearch{Text: "superseded phrase", Scope: "raw"}); err != nil || len(page.Hits) != 0 {
		t.Fatalf("stale turn surfaced by default: %+v %v", page, err)
	}
	// include_stale surfaces them with stale + derived_from.
	page, err := s.SearchMemory(ctx, MemorySearch{Text: "stale summary", Scope: "both", IncludeStale: true})
	if err != nil || len(page.Hits) != 1 {
		t.Fatalf("include_stale note hits = %+v %v, want the stale note", page, err)
	}
	if h := page.Hits[0]; !h.Stale || h.ID != noteID || len(h.DerivedFrom) != 1 || h.DerivedFrom[0] != "rev-1" {
		t.Fatalf("stale note marker/provenance wrong: %+v", h)
	}
	turnPage, err := s.SearchMemory(ctx, MemorySearch{Text: "superseded phrase", Scope: "raw", IncludeStale: true})
	if err != nil || len(turnPage.Hits) != 1 {
		t.Fatalf("include_stale turn hits = %+v %v, want the stale turn", turnPage, err)
	}
	if h := turnPage.Hits[0]; !h.Stale || h.ID != v1TurnID {
		t.Fatalf("stale turn marker wrong: %+v", h)
	}

	// store.Query hides both rows; IncludeStale exposes them.
	qp, err := s.Query(ctx, Query{ConversationID: conv, IncludeTurns: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range qp.Items {
		if it.ID == v1TurnID || it.ID == noteID {
			t.Fatalf("stale item %s surfaced by Query: %+v", it.ID, qp.Items)
		}
	}
	if len(qp.Items) != 2 { // conversation + rev-2 turn
		t.Fatalf("Query items = %d, want conversation + current turn: %+v", len(qp.Items), qp.Items)
	}
	qpStale, err := s.Query(ctx, Query{ConversationID: conv, IncludeTurns: true, IncludeStale: true})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, it := range qpStale.Items {
		seen[it.ID] = true
	}
	if !seen[v1TurnID] || !seen[noteID] {
		t.Fatalf("IncludeStale query missing audit rows: %+v", qpStale.Items)
	}

	// GetItem: default hides the stale note, includeStale returns its marker.
	if _, err := s.GetItem(ctx, noteID, false); err == nil {
		t.Fatal("stale note resolved without includeStale")
	}
	detail, err := s.GetItem(ctx, noteID, true)
	if err != nil {
		t.Fatal(err)
	}
	if !detail.Item.Stale || len(detail.Item.DerivedFrom) != 1 || detail.Item.DerivedFrom[0] != "rev-1" {
		t.Fatalf("stale note detail wrong: %+v", detail.Item)
	}
	if _, err := s.GetItem(ctx, v1TurnID, false); err == nil {
		t.Fatal("stale turn resolved without includeStale")
	}
	turnDetail, err := s.GetItem(ctx, v1TurnID, true)
	if err != nil {
		t.Fatal(err)
	}
	if !turnDetail.Item.Stale {
		t.Fatalf("stale turn detail not marked: %+v", turnDetail.Item)
	}

	// ListConversations counts only the active revision and no stale notes.
	list, err := s.ListConversations(ctx, ConversationFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("conversations = %d, want 1", len(list.Items))
	}
	sum := list.Items[0]
	if sum.ReceivedSegments != 1 || sum.OriginalTurns != 1 || sum.TopicLinkedTurns != 0 || sum.AnalysisStatus != "pending" {
		t.Fatalf("summary not active-scoped: %+v", sum)
	}
}
