package contextbuild

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"recordingmcp/internal/asrinput"
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
	pkg, err := Build(context.Background(), Deps{Store: f.store, DB: f.db, Reranker: rerank}, req)
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

// ingest stores one final ASR segment through the real ingestion path so
// supersede and staleness semantics are exercised rather than simulated.
func (f *fixture) ingest(segment, revision, supersedes, turnID, text string) memory.IngestReport {
	f.t.Helper()
	start, end := int64(10), int64(900)
	speaker := "spk:device-1:1"
	report, err := f.store.IngestTranscript(context.Background(), asrinput.Result{
		ContractVersion: asrinput.ContractVersion,
		Source: asrinput.Source{
			Provider:           "test-provider",
			ProviderRequestID:  "req-" + segment + "-" + revision,
			DeviceID:           "device-1",
			RecordingID:        "rec-scope",
			ConversationID:     "meeting-scope",
			SegmentID:          segment,
			SegmentOffsetMS:    0,
			SegmentDurationMS:  1000,
			TranscriptRevision: revision,
			SupersedesRevision: supersedes,
			CompletionStatus:   "final",
		},
		Turns: []asrinput.Turn{{
			ID:              turnID,
			OriginalIndex:   1,
			Text:            text,
			LocalStartMS:    &start,
			LocalEndMS:      &end,
			GlobalStartMS:   &start,
			GlobalEndMS:     &end,
			SpeakerScopedID: &speaker,
		}},
	})
	if err != nil {
		f.t.Fatalf("IngestTranscript(%s/%s): %v", segment, revision, err)
	}
	return report
}

// ingestedTurnID returns the stored turn id of one ingested revision.
func (f *fixture) ingestedTurnID(segment, revision string) string {
	f.t.Helper()
	var id string
	if err := f.db.QueryRow(`SELECT at.turn_id FROM attachment_turns at
 JOIN source_attachments a ON a.id=at.attachment_id
 WHERE a.segment_id=? AND a.transcript_revision=? LIMIT 1`, segment, revision).Scan(&id); err != nil {
		f.t.Fatalf("ingested turn %s/%s: %v", segment, revision, err)
	}
	return id
}

// attachmentID returns the source_attachments row id of one ingested revision.
func (f *fixture) attachmentID(segment, revision string) string {
	f.t.Helper()
	var id string
	if err := f.db.QueryRow(`SELECT id FROM source_attachments WHERE segment_id=? AND transcript_revision=?`, segment, revision).Scan(&id); err != nil {
		f.t.Fatalf("attachment %s/%s: %v", segment, revision, err)
	}
	return id
}

// markNoteSourced records the derived-artifact bookkeeping the publish path
// writes, so a later supersede marks the note stale through the real trigger.
func (f *fixture) markNoteSourced(noteID, conversationID, attachmentID string) {
	f.t.Helper()
	f.exec(`INSERT INTO derived_artifacts(kind,artifact_id,conversation_id,stale,stale_reason,stale_at)
 VALUES('note',?,?,0,'',NULL)`, noteID, conversationID)
	f.exec(`INSERT INTO derived_artifact_sources(kind,artifact_id,attachment_id) VALUES('note',?,?)`, noteID, attachmentID)
}

func TestBuildContextScopeQueryAndConversation(t *testing.T) {
	f := newFixture(t)
	start := "2026-03-01T10:00:00Z"
	f.conversation("conv:a", "Alpha", "rec:a")
	f.recording("rec:a", "conv:a", &start)
	f.turn("t:a", "conv:a", "rec:a", 0, 1000, nil, "scope query marker alpha")
	f.conversation("conv:b", "Beta", "rec:b")
	f.recording("rec:b", "conv:b", &start)
	f.turn("t:b", "conv:b", "rec:b", 0, 1000, nil, "scope query marker beta")

	control := f.build(nil, Request{Query: "scope query marker"})
	if !containsString(itemIDs(control.Items), "t:a") || !containsString(itemIDs(control.Items), "t:b") {
		t.Fatalf("control build missing query hits: %v", itemIDs(control.Items))
	}

	scoped := f.build(nil, Request{Query: "scope query marker", ConversationID: "conv:a"})
	if !containsString(itemIDs(scoped.Items), "t:a") {
		t.Fatalf("in-scope conversation item missing: %v", itemIDs(scoped.Items))
	}
	if containsString(itemIDs(scoped.Items), "t:b") {
		t.Fatalf("out-of-scope conversation leaked: %v", itemIDs(scoped.Items))
	}
	for _, item := range scoped.Items {
		if item.ConversationID != "conv:a" {
			t.Fatalf("item %s escaped conversation scope: %q", item.ID, item.ConversationID)
		}
	}
}

func TestBuildContextScopeQueryAndTopic(t *testing.T) {
	f := newFixture(t)
	start := "2026-03-01T10:00:00Z"
	f.conversation("conv:a", "Alpha", "rec:a")
	f.recording("rec:a", "conv:a", &start)
	f.turn("t:a", "conv:a", "rec:a", 0, 1000, nil, "topic scope marker alpha")
	f.topic("topic:a", "Alpha topic")
	f.turnTopic("t:a", "topic:a")
	f.conversation("conv:b", "Beta", "rec:b")
	f.recording("rec:b", "conv:b", &start)
	f.turn("t:b", "conv:b", "rec:b", 0, 1000, nil, "topic scope marker beta")
	f.topic("topic:b", "Beta topic")
	f.turnTopic("t:b", "topic:b")

	scoped := f.build(nil, Request{Query: "topic scope marker", TopicIDs: []string{"topic:a"}})
	if !containsString(itemIDs(scoped.Items), "t:a") {
		t.Fatalf("in-scope topic item missing: %v", itemIDs(scoped.Items))
	}
	if containsString(itemIDs(scoped.Items), "t:b") {
		t.Fatalf("out-of-scope topic leaked: %v", itemIDs(scoped.Items))
	}
	for _, item := range scoped.Items {
		if !containsString(item.TopicIDs, "topic:a") {
			t.Fatalf("item %s does not carry the requested topic: %v", item.ID, item.TopicIDs)
		}
	}
}

func TestBuildContextScopeQueryAndStartedAfter(t *testing.T) {
	f := newFixture(t)
	january := "2026-01-01T00:00:00Z"
	march := "2026-03-01T10:00:00Z"
	f.conversation("conv:early", "Early", "rec:early")
	f.recording("rec:early", "conv:early", &january)
	f.turn("t:early", "conv:early", "rec:early", 0, 1000, nil, "time scope marker early")
	f.conversation("conv:late", "Late", "rec:late")
	f.recording("rec:late", "conv:late", &march)
	f.turn("t:late", "conv:late", "rec:late", 0, 1000, nil, "time scope marker late")

	after := mustTime(t, "2026-02-01T00:00:00Z")
	scoped := f.build(nil, Request{Query: "time scope marker", StartedAfter: after})
	if !containsString(itemIDs(scoped.Items), "t:late") {
		t.Fatalf("in-range item missing: %v", itemIDs(scoped.Items))
	}
	if containsString(itemIDs(scoped.Items), "t:early") {
		t.Fatalf("global lexical branch leaked out-of-range item: %v", itemIDs(scoped.Items))
	}
	for _, item := range scoped.Items {
		if item.ConversationID != "conv:late" {
			t.Fatalf("item %s outside the time bound: %q", item.ID, item.ConversationID)
		}
	}
}

func TestBuildContextScopeQueryAndStartedBefore(t *testing.T) {
	f := newFixture(t)
	january := "2026-01-01T00:00:00Z"
	march := "2026-03-01T10:00:00Z"
	f.conversation("conv:early", "Early", "rec:early")
	f.recording("rec:early", "conv:early", &january)
	f.turn("t:early", "conv:early", "rec:early", 0, 1000, nil, "before scope marker early")
	f.conversation("conv:late", "Late", "rec:late")
	f.recording("rec:late", "conv:late", &march)
	f.turn("t:late", "conv:late", "rec:late", 0, 1000, nil, "before scope marker late")

	before := mustTime(t, "2026-02-01T00:00:00Z")
	scoped := f.build(nil, Request{Query: "before scope marker", StartedBefore: before})
	if !containsString(itemIDs(scoped.Items), "t:early") {
		t.Fatalf("in-range item missing: %v", itemIDs(scoped.Items))
	}
	if containsString(itemIDs(scoped.Items), "t:late") {
		t.Fatalf("global lexical branch leaked out-of-range item: %v", itemIDs(scoped.Items))
	}
	for _, item := range scoped.Items {
		if item.ConversationID != "conv:early" {
			t.Fatalf("item %s outside the time bound: %q", item.ID, item.ConversationID)
		}
	}
}

func TestBuildContextScopeQueryTopicAndTime(t *testing.T) {
	f := newFixture(t)
	january := "2026-01-01T00:00:00Z"
	march := "2026-03-01T10:00:00Z"
	f.conversation("conv:old", "Old", "rec:old")
	f.recording("rec:old", "conv:old", &january)
	f.turn("t:old", "conv:old", "rec:old", 0, 1000, nil, "combined marker old")
	f.topic("topic:combined", "Combined")
	f.turnTopic("t:old", "topic:combined")
	f.conversation("conv:new", "New", "rec:new")
	f.recording("rec:new", "conv:new", &march)
	f.turn("t:new", "conv:new", "rec:new", 0, 1000, nil, "combined marker new")
	f.turnTopic("t:new", "topic:combined")

	after := mustTime(t, "2026-02-01T00:00:00Z")
	scoped := f.build(nil, Request{Query: "combined marker", TopicIDs: []string{"topic:combined"}, StartedAfter: after})
	if !containsString(itemIDs(scoped.Items), "t:new") {
		t.Fatalf("in-range topic item missing: %v", itemIDs(scoped.Items))
	}
	if containsString(itemIDs(scoped.Items), "t:old") {
		t.Fatalf("query+topic item outside the time bound leaked: %v", itemIDs(scoped.Items))
	}
	for _, item := range scoped.Items {
		if !containsString(item.TopicIDs, "topic:combined") {
			t.Fatalf("item %s escaped the topic scope: %v", item.ID, item.TopicIDs)
		}
		if item.ConversationID != "conv:new" {
			t.Fatalf("item %s escaped the time scope: %q", item.ID, item.ConversationID)
		}
	}
}

func TestBuildContextScopeQueryConversationAndTime(t *testing.T) {
	f := newFixture(t)
	january := "2026-01-01T00:00:00Z"
	march := "2026-03-01T10:00:00Z"
	f.conversation("conv:a", "Alpha", "rec:a")
	f.recording("rec:a", "conv:a", &march)
	f.turn("t:a", "conv:a", "rec:a", 0, 1000, nil, "conv time marker alpha")
	f.conversation("conv:b", "Beta", "rec:b")
	f.recording("rec:b", "conv:b", &january)
	f.turn("t:b", "conv:b", "rec:b", 0, 1000, nil, "conv time marker beta")
	f.conversation("conv:c", "Gamma", "rec:c")
	f.recording("rec:c", "conv:c", &march)
	f.turn("t:c", "conv:c", "rec:c", 0, 1000, nil, "conv time marker gamma")

	after := mustTime(t, "2026-02-01T00:00:00Z")
	scoped := f.build(nil, Request{Query: "conv time marker", ConversationID: "conv:a", StartedAfter: after})
	if !containsString(itemIDs(scoped.Items), "t:a") {
		t.Fatalf("in-scope, in-range item missing: %v", itemIDs(scoped.Items))
	}
	if containsString(itemIDs(scoped.Items), "t:b") {
		t.Fatalf("out-of-range conversation leaked: %v", itemIDs(scoped.Items))
	}
	if containsString(itemIDs(scoped.Items), "t:c") {
		t.Fatalf("in-range but out-of-scope conversation leaked: %v", itemIDs(scoped.Items))
	}
	for _, item := range scoped.Items {
		if item.ConversationID != "conv:a" {
			t.Fatalf("item %s escaped the conversation scope: %q", item.ID, item.ConversationID)
		}
	}
}

func TestBuildContextLinkExpansionCannotEscapeConversationScope(t *testing.T) {
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
	f.note("n:a", "conv:a", "observation", "alpha rollout decision", []string{"topic:a"}, []string{"t:a"})
	f.note("n:b", "conv:b", "observation", "beta rollout decision", []string{"topic:b"}, []string{"t:b"})
	f.topicLink("topic:a", "topic:b", "follow_up", []string{"t:a"}, pipeline.ReviewConfirm)

	scoped := f.build(nil, Request{Query: "alpha", ConversationID: "conv:a"})
	if !containsString(itemIDs(scoped.Items), "n:a") {
		t.Fatalf("in-scope item missing: %v", itemIDs(scoped.Items))
	}
	if containsString(itemIDs(scoped.Items), "n:b") {
		t.Fatalf("link expansion escaped the conversation scope: %v", itemIDs(scoped.Items))
	}
	for _, item := range scoped.Items {
		if item.ConversationID != "conv:a" {
			t.Fatalf("item %s escaped the conversation scope: %q", item.ID, item.ConversationID)
		}
	}
}

func TestBuildContextLinkExpansionCannotEscapeTimeScope(t *testing.T) {
	f := newFixture(t)
	march := "2026-03-01T10:00:00Z"
	january := "2026-01-01T00:00:00Z"
	f.conversation("conv:a", "Alpha", "rec:a")
	f.recording("rec:a", "conv:a", &march)
	f.turn("t:a", "conv:a", "rec:a", 0, 1000, nil, "alpha planning")
	f.conversation("conv:b", "Beta", "rec:b")
	f.recording("rec:b", "conv:b", &january)
	f.turn("t:b", "conv:b", "rec:b", 0, 1000, nil, "beta planning")
	f.topic("topic:a", "Alpha topic")
	f.topic("topic:b", "Beta topic")
	f.note("n:a", "conv:a", "observation", "alpha rollout decision", []string{"topic:a"}, []string{"t:a"})
	f.note("n:b", "conv:b", "observation", "beta rollout decision", []string{"topic:b"}, []string{"t:b"})
	f.topicLink("topic:a", "topic:b", "follow_up", []string{"t:a"}, pipeline.ReviewConfirm)

	after := mustTime(t, "2026-02-01T00:00:00Z")
	scoped := f.build(nil, Request{Query: "alpha", StartedAfter: after})
	if !containsString(itemIDs(scoped.Items), "n:a") {
		t.Fatalf("in-range item missing: %v", itemIDs(scoped.Items))
	}
	if containsString(itemIDs(scoped.Items), "n:b") {
		t.Fatalf("link expansion escaped the time scope: %v", itemIDs(scoped.Items))
	}
	for _, item := range scoped.Items {
		if item.ConversationID == "conv:b" {
			t.Fatalf("out-of-range item returned: %s", item.ID)
		}
	}
}

func TestBuildContextNavigationCannotEscapeTopicScope(t *testing.T) {
	f := newFixture(t)
	start := "2026-03-01T10:00:00Z"
	f.conversation("conv:a", "Alpha", "rec:a")
	f.recording("rec:a", "conv:a", &start)
	f.turn("t:multi", "conv:a", "rec:a", 0, 1000, nil, "nav scope marker")
	f.topic("topic:a", "Alpha topic")
	f.topic("topic:z", "Zeta topic")
	f.turnTopic("t:multi", "topic:a")
	f.turnTopic("t:multi", "topic:z")
	f.note("n:z", "conv:a", "observation", "zeta only note", []string{"topic:z"}, nil)

	control := f.build(nil, Request{Query: "nav scope marker"})
	if !containsString(itemIDs(control.Items), "n:z") {
		t.Fatalf("control navigation did not surface the topic:z note: %v", itemIDs(control.Items))
	}

	scoped := f.build(nil, Request{Query: "nav scope marker", TopicIDs: []string{"topic:a"}})
	if !containsString(itemIDs(scoped.Items), "t:multi") {
		t.Fatalf("in-scope item missing: %v", itemIDs(scoped.Items))
	}
	if containsString(itemIDs(scoped.Items), "n:z") {
		t.Fatalf("navigation escaped the topic scope: %v", itemIDs(scoped.Items))
	}
	for _, item := range scoped.Items {
		if !containsString(item.TopicIDs, "topic:a") {
			t.Fatalf("item %s escaped the topic scope: %v", item.ID, item.TopicIDs)
		}
	}
}

func TestBuildContextEvidenceContainsCurrentTurnsOnly(t *testing.T) {
	f := newFixture(t)
	report := f.ingest("seg-1", "rev-1", "", "utt-1", "superseded alpha text")
	v1Turn := f.ingestedTurnID("seg-1", "rev-1")
	f.ingest("seg-1", "rev-2", "rev-1", "utt-1", "current alpha text")
	v2Turn := f.ingestedTurnID("seg-1", "rev-2")
	if v1Turn == v2Turn {
		t.Fatalf("revisions share a turn id: %s", v1Turn)
	}
	f.note("n:cur", report.ConversationID, "observation", "alpha evidence note", nil, []string{v2Turn})

	pkg := f.build(nil, Request{Query: "alpha evidence note", IncludeRawEvidence: true})
	if !containsString(itemIDs(pkg.Items), "n:cur") {
		t.Fatalf("current note missing: %v", itemIDs(pkg.Items))
	}
	if len(pkg.Evidence) == 0 {
		t.Fatalf("no evidence window emitted")
	}
	current := false
	for _, ref := range pkg.Evidence {
		if ref.TurnID == v1Turn || strings.Contains(ref.Text, "superseded alpha text") {
			t.Fatalf("superseded turn leaked into the evidence window: %+v", ref)
		}
		if ref.TurnID == v2Turn {
			current = true
		}
	}
	if !current {
		t.Fatalf("current turn absent from its own evidence window: %+v", pkg.Evidence)
	}
}

func TestBuildContextStaleHistoricalEvidenceNeverAppears(t *testing.T) {
	f := newFixture(t)
	report := f.ingest("seg-1", "rev-1", "", "utt-1", "historical alpha evidence")
	v1Turn := f.ingestedTurnID("seg-1", "rev-1")
	v1Attachment := f.attachmentID("seg-1", "rev-1")
	f.turn("t:cur-ev", report.ConversationID, report.RecordingID, 0, 5000, nil, "current alpha evidence")
	f.topic("topic:a", "Alpha topic")
	f.topic("topic:b", "Beta topic")
	f.note("n:a", report.ConversationID, "observation", "alpha current note", []string{"topic:a"}, []string{"t:cur-ev"})
	f.note("n:stale", report.ConversationID, "observation", "alpha stale note", []string{"topic:b"}, []string{v1Turn})
	f.markNoteSourced("n:stale", report.ConversationID, v1Attachment)
	f.topicLink("topic:a", "topic:b", "follow_up", []string{v1Turn}, pipeline.ReviewConfirm)

	// Before the supersede both the note and its link are current.
	control := f.build(nil, Request{Query: "alpha"})
	if !containsString(itemIDs(control.Items), "n:stale") {
		t.Fatalf("control build should surface the not-yet-stale note: %v", itemIDs(control.Items))
	}

	f.ingest("seg-1", "rev-2", "rev-1", "utt-1", "current alpha evidence")

	pkg := f.build(nil, Request{Query: "alpha"})
	if containsString(itemIDs(pkg.Items), "n:stale") {
		t.Fatalf("stale note returned as current: %v", itemIDs(pkg.Items))
	}
	if len(pkg.Trace.LinksExpanded) != 0 {
		t.Fatalf("stale link traversed: %+v", pkg.Trace.LinksExpanded)
	}
	for _, item := range pkg.Items {
		if item.ID == v1Turn || strings.Contains(item.Content, "historical alpha evidence") {
			t.Fatalf("superseded turn surfaced as an item: %+v", item)
		}
	}

	withEvidence := f.build(nil, Request{Query: "alpha", IncludeRawEvidence: true})
	for _, ref := range withEvidence.Evidence {
		if ref.TurnID == v1Turn || strings.Contains(ref.Text, "historical alpha evidence") {
			t.Fatalf("superseded turn surfaced as evidence: %+v", ref)
		}
	}
}

func TestBuildContextEvidenceOrderingDeterministic(t *testing.T) {
	f := newFixture(t)
	start := "2026-03-01T10:00:00Z"
	f.conversation("conv:a", "Alpha", "rec:a")
	f.recording("rec:a", "conv:a", &start)
	f.turn("t:1", "conv:a", "rec:a", 0, 1000, nil, "deterministic marker one")
	f.turn("t:2", "conv:a", "rec:a", 1, 2000, nil, "deterministic marker two")
	f.note("n:1", "conv:a", "observation", "deterministic marker note", nil, []string{"t:1"})
	f.note("n:2", "conv:a", "observation", "deterministic marker note", nil, []string{"t:2"})

	req := Request{Query: "deterministic marker", IncludeRawEvidence: true}
	first := f.build(nil, req)
	second := f.build(nil, req)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("identical builds differ:\nfirst=%+v\nsecond=%+v", first, second)
	}
	if len(first.Evidence) == 0 {
		t.Fatalf("determinism fixture produced no evidence to compare")
	}
}

func TestBuildContextConfirmedLinkStopsAfterEvidenceSuperseded(t *testing.T) {
	f := newFixture(t)
	report := f.ingest("seg-1", "rev-1", "", "utt-1", "alpha planning")
	v1Turn := f.ingestedTurnID("seg-1", "rev-1")
	// The link-discovered note lives in a different conversation so that
	// navigation of the recalled conversation cannot surface it: only the
	// confirmed link can.
	f.conversation("conv:linked", "Linked", "rec:linked")
	f.recording("rec:linked", "conv:linked", nil)
	f.turn("t:bev", "conv:linked", "rec:linked", 0, 5000, nil, "beta evidence")
	f.topic("topic:a", "Alpha topic")
	f.topic("topic:b", "Beta topic")
	f.note("n:a", report.ConversationID, "observation", "alpha rollout decision", []string{"topic:a"}, []string{v1Turn})
	f.note("n:b", "conv:linked", "observation", "beta followup decision", []string{"topic:b"}, []string{"t:bev"})
	f.topicLink("topic:a", "topic:b", "follow_up", []string{v1Turn}, pipeline.ReviewConfirm)

	control := f.build(nil, Request{Query: "alpha rollout"})
	if !containsString(itemIDs(control.Items), "n:b") {
		t.Fatalf("control build missing the link-discovered item: %v", itemIDs(control.Items))
	}
	if len(control.Trace.LinksExpanded) == 0 {
		t.Fatalf("control build did not record the confirmed link")
	}

	f.ingest("seg-1", "rev-2", "rev-1", "utt-1", "alpha planning revised")

	after := f.build(nil, Request{Query: "alpha rollout"})
	if !containsString(itemIDs(after.Items), "n:a") {
		t.Fatalf("current recalled item missing: %v", itemIDs(after.Items))
	}
	if containsString(itemIDs(after.Items), "n:b") {
		t.Fatalf("link with superseded evidence still navigated: %v", itemIDs(after.Items))
	}
	if len(after.Trace.LinksExpanded) != 0 {
		t.Fatalf("stale link recorded as expanded: %+v", after.Trace.LinksExpanded)
	}
}
