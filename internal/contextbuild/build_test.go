package contextbuild

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"recordingmcp/internal/memory"
	"recordingmcp/internal/pipeline"
)

// fixture seeds a real SQLite database through memory.Open and exposes the
// shared handle for raw SQL, so every test exercises real rows rather than
// mocks.
type fixture struct {
	t     *testing.T
	store *memory.Store
	db    *sql.DB
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	store, err := memory.Open(filepath.Join(t.TempDir(), "contextbuild.db"))
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return &fixture{t: t, store: store, db: store.DB()}
}

func (f *fixture) exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.db.Exec(query, args...); err != nil {
		f.t.Fatalf("exec %q: %v", query, err)
	}
}

func (f *fixture) conversation(id, title, recordingID string) {
	f.t.Helper()
	f.exec(`INSERT INTO conversations(id,title,summary,annotation_version,annotation_basis,recording_id,source_sha256,transcript_version,duration_ms,transcript_json,annotations_json)
 VALUES(?,?,'','v1','',?,'sha','tv',1000,'{}','{}')`, id, title, recordingID)
}

func (f *fixture) recording(id, conversationID string, startedAt *string) {
	f.t.Helper()
	known := 0
	if startedAt != nil {
		known = 1
	}
	f.exec(`INSERT INTO recordings(id,conversation_id,source_file,source_sha256,duration_ms,recording_started_at,absolute_time_known)
 VALUES(?,?,'file','sha',1000,?,?)`, id, conversationID, startedAt, known)
}

func (f *fixture) turn(id, conversationID, recordingID string, segment int, startMS int64, speaker *string, content string) {
	f.t.Helper()
	f.exec(`INSERT INTO turns(id,conversation_id,recording_id,segment_index,global_start_ms,global_end_ms,speaker_scoped_id,speaker_entity_id,content,source_sha256,transcript_version,original_turn_id,turn_json)
 VALUES(?,?,?,?,?,?,?,NULL,?,'sha','rev-1',?, '{}')`,
		id, conversationID, recordingID, segment, startMS, startMS+500, speaker, content, id)
}

func (f *fixture) topic(id, title string) {
	f.t.Helper()
	f.exec(`INSERT INTO topics(id,title) VALUES(?,?)`, id, title)
}

func (f *fixture) topicConversation(topicID, conversationID string) {
	f.t.Helper()
	f.exec(`INSERT INTO topic_conversations(topic_id,conversation_id) VALUES(?,?)`, topicID, conversationID)
}

func (f *fixture) turnTopic(turnID, topicID string) {
	f.t.Helper()
	f.exec(`INSERT INTO turn_topics(turn_id,topic_id) VALUES(?,?)`, turnID, topicID)
}

func (f *fixture) note(id, conversationID, kind, summary string, topicIDs, evidenceTurnIDs []string) {
	f.t.Helper()
	topics := "[]"
	if len(topicIDs) > 0 {
		raw, err := json.Marshal(topicIDs)
		if err != nil {
			f.t.Fatalf("marshal topic ids: %v", err)
		}
		topics = string(raw)
	}
	f.exec(`INSERT INTO notes(id,conversation_id,kind,summary,topic_ids_json,claim_mode,interpretation_note,annotation_version)
 VALUES(?,?,?,?,?,'inference','','v1')`, id, conversationID, kind, summary, topics)
	for i, turnID := range evidenceTurnIDs {
		f.exec(`INSERT INTO note_evidence(note_id,turn_id,ordinal) VALUES(?,?,?)`, id, turnID, i)
	}
}

// markStaleNote records a stale derived_artifact for a note.
func (f *fixture) markStaleNote(noteID, conversationID string) {
	f.t.Helper()
	f.exec(`INSERT INTO derived_artifacts(kind,artifact_id,conversation_id,stale,stale_reason,stale_at)
 VALUES('note',?,?,1,'superseded by newer transcript',NULL)`, noteID, conversationID)
}

// topicLink inserts a real link through pipeline and optionally reviews it.
// action is "" (leave proposed), pipeline.ReviewConfirm or pipeline.ReviewReject.
func (f *fixture) topicLink(from, to, relation string, evidenceTurnIDs []string, action string) {
	f.t.Helper()
	proposals := []pipeline.LinkProposal{{
		Kind:            "topic_link",
		FromTopicID:     from,
		ToTopicID:       to,
		Relation:        relation,
		Reason:          "shared evidence in the test fixture",
		EvidenceTurnIDs: evidenceTurnIDs,
		Confidence:      0.5,
	}}
	validated, err := pipeline.ValidateTopicLinks(context.Background(), f.db, proposals)
	if err != nil {
		f.t.Fatalf("ValidateTopicLinks: %v", err)
	}
	if validated[0].Status != "validated" {
		f.t.Fatalf("link rejected: %s", validated[0].RejectionReason)
	}
	inserted, err := pipeline.InsertLinks(context.Background(), f.db, "test-model", validated, time.UnixMilli(1_700_000_000_000))
	if err != nil {
		f.t.Fatalf("InsertLinks: %v", err)
	}
	if action == "" {
		return
	}
	if _, err := pipeline.ReviewLink(context.Background(), f.db, inserted[0].ID, action); err != nil {
		f.t.Fatalf("ReviewLink(%s): %v", action, err)
	}
}

func (f *fixture) build(rerank Reranker, req Request) Package {
	f.t.Helper()
	pkg, err := Build(context.Background(), f.store, f.db, rerank, req)
	if err != nil {
		f.t.Fatalf("Build: %v", err)
	}
	return pkg
}

func mustTime(t *testing.T, value string) *time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("parse time %q: %v", value, err)
	}
	return &parsed
}

func itemIDs(items []memory.Item) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.ID)
	}
	return out
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func hasGap(pkg Package, kind string) bool {
	for _, gap := range pkg.Gaps {
		if gap.Kind == kind {
			return true
		}
	}
	return false
}

func countKind(pkg Package, kind string) int {
	count := 0
	for _, item := range pkg.Items {
		if item.Kind == kind {
			count++
		}
	}
	return count
}

func TestBuildContextLiteralLexicalHit(t *testing.T) {
	f := newFixture(t)
	start := "2026-03-01T10:00:00Z"
	f.conversation("conv:a", "Alpha", "rec:a")
	f.recording("rec:a", "conv:a", &start)
	f.turn("t:match", "conv:a", "rec:a", 0, 1000, nil, "deployment plan alpha ready")
	f.turn("t:other", "conv:a", "rec:a", 1, 2000, nil, "unrelated chatter")

	pkg := f.build(nil, Request{Query: "deployment plan"})

	if !containsString(itemIDs(pkg.Items), "t:match") {
		t.Fatalf("literal turn not recalled: %v", itemIDs(pkg.Items))
	}
	if containsString(itemIDs(pkg.Items), "t:other") {
		t.Fatalf("non-matching turn leaked: %v", itemIDs(pkg.Items))
	}
	if pkg.Trace.Lexical.Query != "deployment plan" {
		t.Fatalf("lexical query = %q", pkg.Trace.Lexical.Query)
	}
	if len(pkg.Trace.Lexical.HitIDs) != 1 || pkg.Trace.Lexical.HitIDs[0] != "t:match" {
		t.Fatalf("lexical hit ids = %v, want [t:match]", pkg.Trace.Lexical.HitIDs)
	}
	if !pkg.Coverage.Branches.Lexical {
		t.Fatalf("lexical branch not recorded")
	}
	if pkg.Coverage.Scanned.Turns == 0 {
		t.Fatalf("scanned turns = 0, want the recalled turn counted")
	}
}

func TestBuildContextDerivedNoteHit(t *testing.T) {
	f := newFixture(t)
	start := "2026-03-01T10:00:00Z"
	f.conversation("conv:a", "Alpha", "rec:a")
	f.recording("rec:a", "conv:a", &start)
	f.turn("t:raw", "conv:a", "rec:a", 0, 1000, nil, "raw transcript words here")
	f.note("n:1", "conv:a", "observation", "roadmap decision recorded", nil, []string{"t:raw"})

	pkg := f.build(nil, Request{Query: "roadmap"})

	if !containsString(itemIDs(pkg.Items), "n:1") {
		t.Fatalf("derived note not recalled: %v", itemIDs(pkg.Items))
	}
	for _, item := range pkg.Items {
		if item.ID == "n:1" && !strings.Contains(item.Summary, "roadmap") {
			t.Fatalf("note summary mismatch: %+v", item)
		}
	}
	if containsString(itemIDs(pkg.Items), "t:raw") {
		t.Fatalf("raw turn surfaced without matching the query: %v", itemIDs(pkg.Items))
	}
	if len(pkg.Evidence) != 0 {
		t.Fatalf("evidence emitted without include_raw_evidence: %+v", pkg.Evidence)
	}
}

func TestBuildContextConfirmedRelationEnablesIndirectDiscovery(t *testing.T) {
	f := newFixture(t)
	start := "2026-03-01T10:00:00Z"
	f.conversation("conv:a", "Alpha", "rec:a")
	f.recording("rec:a", "conv:a", &start)
	f.turn("t:a", "conv:a", "rec:a", 0, 1000, nil, "alpha planning")
	f.conversation("conv:b", "Beta", "rec:b")
	f.recording("rec:b", "conv:b", &start)
	f.turn("t:b", "conv:b", "rec:b", 0, 1000, nil, "beta planning")
	f.topic("topic:a", "Alpha topic")
	f.topic("topic:b", "Beta topic")
	f.topicConversation("topic:a", "conv:a")
	f.topicConversation("topic:b", "conv:b")
	f.note("n:a", "conv:a", "observation", "alpha rollout decision", []string{"topic:a"}, []string{"t:a"})
	f.note("n:b", "conv:b", "observation", "beta rollout decision", []string{"topic:b"}, []string{"t:b"})
	f.topicLink("topic:a", "topic:b", "follow_up", []string{"t:a"}, pipeline.ReviewConfirm)

	pkg := f.build(nil, Request{Query: "alpha"})

	if !containsString(itemIDs(pkg.Items), "n:b") {
		t.Fatalf("indirectly linked item missing: %v", itemIDs(pkg.Items))
	}
	if len(pkg.Trace.LinksExpanded) == 0 {
		t.Fatalf("links_expanded is empty")
	}
	expansion := pkg.Trace.LinksExpanded[0]
	if expansion.Relation != "follow_up" || expansion.FromID != "topic:a" || expansion.ToID != "topic:b" {
		t.Fatalf("link expansion = %+v", expansion)
	}
	if !pkg.Coverage.Branches.Links {
		t.Fatalf("links branch not recorded")
	}

	// The indirectly discovered item must carry its source evidence, and the
	// raw-evidence windows must be able to cite it.
	for _, it := range pkg.Items {
		if it.ID != "n:b" {
			continue
		}
		if len(it.EvidenceTurnIDs) != 1 || it.EvidenceTurnIDs[0] != "t:b" {
			t.Fatalf("link-discovered item evidence = %v, want [t:b]", it.EvidenceTurnIDs)
		}
	}
	withEvidence := f.build(nil, Request{Query: "alpha", IncludeRawEvidence: true})
	cited := false
	for _, ref := range withEvidence.Evidence {
		for _, usedBy := range ref.UsedByItemIDs {
			if usedBy == "n:b" {
				cited = true
			}
		}
	}
	if !cited {
		t.Fatalf("raw evidence does not cite the link-discovered item: %+v", withEvidence.Evidence)
	}
}

func TestBuildContextProposedRelationIgnored(t *testing.T) {
	f := newFixture(t)
	start := "2026-03-01T10:00:00Z"
	f.conversation("conv:a", "Alpha", "rec:a")
	f.recording("rec:a", "conv:a", &start)
	f.turn("t:a", "conv:a", "rec:a", 0, 1000, nil, "alpha planning")
	f.conversation("conv:b", "Beta", "rec:b")
	f.recording("rec:b", "conv:b", &start)
	f.turn("t:b", "conv:b", "rec:b", 0, 1000, nil, "beta planning")
	f.topic("topic:a", "Alpha topic")
	f.topic("topic:b", "Beta topic")
	f.topicConversation("topic:a", "conv:a")
	f.topicConversation("topic:b", "conv:b")
	f.note("n:a", "conv:a", "observation", "alpha rollout decision", []string{"topic:a"}, []string{"t:a"})
	f.note("n:b", "conv:b", "observation", "beta rollout decision", []string{"topic:b"}, []string{"t:b"})
	f.topicLink("topic:a", "topic:b", "follow_up", []string{"t:a"}, "")

	pkg := f.build(nil, Request{Query: "alpha"})

	if containsString(itemIDs(pkg.Items), "n:b") {
		t.Fatalf("proposed link expanded: %v", itemIDs(pkg.Items))
	}
	if len(pkg.Trace.LinksExpanded) != 0 {
		t.Fatalf("proposed link recorded: %+v", pkg.Trace.LinksExpanded)
	}
}

func TestBuildContextRejectedRelationIgnored(t *testing.T) {
	f := newFixture(t)
	start := "2026-03-01T10:00:00Z"
	f.conversation("conv:a", "Alpha", "rec:a")
	f.recording("rec:a", "conv:a", &start)
	f.turn("t:a", "conv:a", "rec:a", 0, 1000, nil, "alpha planning")
	f.conversation("conv:b", "Beta", "rec:b")
	f.recording("rec:b", "conv:b", &start)
	f.turn("t:b", "conv:b", "rec:b", 0, 1000, nil, "beta planning")
	f.topic("topic:a", "Alpha topic")
	f.topic("topic:b", "Beta topic")
	f.topicConversation("topic:a", "conv:a")
	f.topicConversation("topic:b", "conv:b")
	f.note("n:a", "conv:a", "observation", "alpha rollout decision", []string{"topic:a"}, []string{"t:a"})
	f.note("n:b", "conv:b", "observation", "beta rollout decision", []string{"topic:b"}, []string{"t:b"})
	f.topicLink("topic:a", "topic:b", "follow_up", []string{"t:a"}, pipeline.ReviewReject)

	pkg := f.build(nil, Request{Query: "alpha"})

	if containsString(itemIDs(pkg.Items), "n:b") {
		t.Fatalf("rejected link expanded: %v", itemIDs(pkg.Items))
	}
	if len(pkg.Trace.LinksExpanded) != 0 {
		t.Fatalf("rejected link recorded: %+v", pkg.Trace.LinksExpanded)
	}
}

func TestBuildContextStaleNeverCurrent(t *testing.T) {
	f := newFixture(t)
	start := "2026-03-01T10:00:00Z"
	f.conversation("conv:a", "Alpha", "rec:a")
	f.recording("rec:a", "conv:a", &start)
	f.turn("t:1", "conv:a", "rec:a", 0, 1000, nil, "alpha evidence one")
	f.turn("t:2", "conv:a", "rec:a", 1, 2000, nil, "alpha evidence two")
	f.note("n:stale", "conv:a", "observation", "alpha stale note", nil, []string{"t:1"})
	f.note("n:current", "conv:a", "observation", "alpha current note", nil, []string{"t:2"})
	f.markStaleNote("n:stale", "conv:a")

	pkg := f.build(nil, Request{Query: "alpha"})

	if containsString(itemIDs(pkg.Items), "n:stale") {
		t.Fatalf("stale note returned as current: %v", itemIDs(pkg.Items))
	}
	if !containsString(pkg.Trace.StaleFiltered, "n:stale") {
		t.Fatalf("stale artifact not recorded: %v", pkg.Trace.StaleFiltered)
	}
	if !hasGap(pkg, "stale_pending_reprocessing") {
		t.Fatalf("missing stale_pending_reprocessing gap: %+v", pkg.Gaps)
	}
}

func TestBuildContextDedupesSharedEvidence(t *testing.T) {
	f := newFixture(t)
	start := "2026-03-01T10:00:00Z"
	f.conversation("conv:a", "Alpha", "rec:a")
	f.recording("rec:a", "conv:a", &start)
	f.turn("t:1", "conv:a", "rec:a", 0, 1000, nil, "shared evidence alpha")
	f.turn("t:2", "conv:a", "rec:a", 1, 2000, nil, "secondary source")
	f.note("n:1", "conv:a", "observation", "shared evidence plan", nil, []string{"t:1", "t:2"})

	pkg := f.build(nil, Request{Query: "shared evidence"})

	if !containsString(itemIDs(pkg.Items), "n:1") {
		t.Fatalf("note with shared evidence missing: %v", itemIDs(pkg.Items))
	}
	if countKind(pkg, "turn") != 0 {
		t.Fatalf("evidence-shared turns not deduped: %v", itemIDs(pkg.Items))
	}
}

func TestBuildContextFiltersStrictly(t *testing.T) {
	f := newFixture(t)
	march := "2026-03-01T10:00:00Z"
	january := "2026-01-01T00:00:00Z"
	f.conversation("conv:a", "Alpha", "rec:a")
	f.recording("rec:a", "conv:a", &march)
	f.conversation("conv:b", "Beta", "rec:b")
	f.recording("rec:b", "conv:b", &january)
	f.turn("t:a", "conv:a", "rec:a", 0, 1000, nil, "filter marker alpha")
	f.turn("t:b", "conv:b", "rec:b", 0, 1000, nil, "filter marker alpha")
	f.topic("topic:filter", "Filter")
	f.turnTopic("t:a", "topic:filter")

	after := mustTime(t, "2026-02-01T00:00:00Z")
	bounded := f.build(nil, Request{Query: "filter marker", StartedAfter: after})
	if containsString(itemIDs(bounded.Items), "t:b") {
		t.Fatalf("time bound leaked conv:b: %v", itemIDs(bounded.Items))
	}
	if !containsString(itemIDs(bounded.Items), "t:a") {
		t.Fatalf("time bound dropped conv:a: %v", itemIDs(bounded.Items))
	}
	if bounded.Coverage.TimeRange.StartedAfter == nil || !bounded.Coverage.TimeRange.StartedAfter.Equal(*after) {
		t.Fatalf("coverage time range = %+v", bounded.Coverage.TimeRange)
	}

	topicScoped := f.build(nil, Request{TopicIDs: []string{"topic:filter"}})
	for _, item := range topicScoped.Items {
		if item.ConversationID != "conv:a" {
			t.Fatalf("topic filter leaked %q: %v", item.ConversationID, itemIDs(topicScoped.Items))
		}
	}
	if !containsString(itemIDs(topicScoped.Items), "t:a") {
		t.Fatalf("topic filter dropped t:a: %v", itemIDs(topicScoped.Items))
	}

	conversationScoped := f.build(nil, Request{ConversationID: "conv:a"})
	if len(conversationScoped.Items) == 0 {
		t.Fatalf("conversation filter returned nothing")
	}
	for _, item := range conversationScoped.Items {
		if item.ConversationID != "conv:a" {
			t.Fatalf("conversation filter leaked %q: %v", item.ConversationID, itemIDs(conversationScoped.Items))
		}
	}
}

func TestBuildContextByteBudgetEnforced(t *testing.T) {
	f := newFixture(t)
	start := "2026-03-01T10:00:00Z"
	f.conversation("conv:a", "Alpha", "rec:a")
	f.recording("rec:a", "conv:a", &start)
	f.note("n:1", "conv:a", "observation", "budget probe note", nil, nil)
	f.note("n:2", "conv:a", "observation", "budget probe note", nil, nil)
	f.note("n:3", "conv:a", "observation", "budget probe note", nil, nil)

	const maxBytes = 20
	pkg := f.build(nil, Request{Query: "budget probe", MaxBytes: maxBytes})

	if !pkg.Truncated {
		t.Fatalf("tiny budget did not truncate: items=%v", itemIDs(pkg.Items))
	}
	if !hasGap(pkg, "budget_exhausted") {
		t.Fatalf("missing budget_exhausted gap: %+v", pkg.Gaps)
	}
	if pkg.Trace.Budget.BytesUsed > maxBytes {
		t.Fatalf("bytes_used = %d exceeds budget %d", pkg.Trace.Budget.BytesUsed, maxBytes)
	}
	if len(pkg.Items) != 1 {
		t.Fatalf("items = %v, want a single item inside the budget", itemIDs(pkg.Items))
	}
	if pkg.Trace.Budget.ItemsUsed != len(pkg.Items) {
		t.Fatalf("budget items_used = %d, items = %d", pkg.Trace.Budget.ItemsUsed, len(pkg.Items))
	}
}

func TestBuildContextDegradedWithoutDecision(t *testing.T) {
	f := newFixture(t)
	start := "2026-03-01T10:00:00Z"
	f.conversation("conv:a", "Alpha", "rec:a")
	f.recording("rec:a", "conv:a", &start)
	f.turn("t:1", "conv:a", "rec:a", 0, 1000, nil, "degraded probe turn")

	pkg := f.build(nil, Request{Query: "degraded probe"})

	if !pkg.Degraded {
		t.Fatalf("nil reranker did not degrade the package")
	}
	if pkg.DegradeReason == "" {
		t.Fatalf("degrade reason is empty")
	}
	if pkg.Trace.DecisionRerank {
		t.Fatalf("decision_rerank = true without a reranker")
	}
	if len(pkg.Items) == 0 {
		t.Fatalf("degraded package dropped deterministic items")
	}
}

func TestBuildContextUnknownTimeNotInBoundedQuery(t *testing.T) {
	f := newFixture(t)
	march := "2026-03-01T10:00:00Z"
	f.conversation("conv:known", "Known", "rec:known")
	f.recording("rec:known", "conv:known", &march)
	f.conversation("conv:unknown", "Unknown", "rec:unknown")
	f.recording("rec:unknown", "conv:unknown", nil)
	f.turn("t:known", "conv:known", "rec:known", 0, 1000, nil, "unknown time marker")
	f.turn("t:unknown", "conv:unknown", "rec:unknown", 0, 1000, nil, "unknown time marker")

	after := mustTime(t, "2026-02-01T00:00:00Z")
	bounded := f.build(nil, Request{Query: "unknown time marker", StartedAfter: after})
	if containsString(itemIDs(bounded.Items), "t:unknown") {
		t.Fatalf("unknown-time turn included under bounds: %v", itemIDs(bounded.Items))
	}
	if bounded.Coverage.TimeRange.UnknownTimeIncluded {
		t.Fatalf("unknown_time_included = true for a bounded query")
	}

	unbounded := f.build(nil, Request{Query: "unknown time marker"})
	if !containsString(itemIDs(unbounded.Items), "t:unknown") {
		t.Fatalf("unknown-time turn missing without bounds: %v", itemIDs(unbounded.Items))
	}
	if !unbounded.Coverage.TimeRange.UnknownTimeIncluded {
		t.Fatalf("unknown_time_included = false although unknown-time content returned")
	}
}

func TestBuildContextNoMatchesReturnsEmptyWithCoverage(t *testing.T) {
	f := newFixture(t)
	start := "2026-03-01T10:00:00Z"
	f.conversation("conv:a", "Alpha", "rec:a")
	f.recording("rec:a", "conv:a", &start)
	f.turn("t:1", "conv:a", "rec:a", 0, 1000, nil, "totally unrelated text")

	pkg := f.build(nil, Request{Query: "zzz nothing matches"})

	if len(pkg.Items) != 0 {
		t.Fatalf("invented context: %v", itemIDs(pkg.Items))
	}
	if len(pkg.Evidence) != 0 {
		t.Fatalf("invented evidence: %+v", pkg.Evidence)
	}
	if !hasGap(pkg, "no_matches") {
		t.Fatalf("missing no_matches gap: %+v", pkg.Gaps)
	}
	// An empty scan is still a recorded scan: the zero coverage proves no
	// candidate was fabricated to fill the package.
	if pkg.Coverage.Scanned != (Scanned{}) {
		t.Fatalf("scanned = %+v, want an empty scan", pkg.Coverage.Scanned)
	}
	if pkg.Trace.Lexical.Query != "zzz nothing matches" {
		t.Fatalf("lexical trace missing: %+v", pkg.Trace.Lexical)
	}
}
