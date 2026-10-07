package projection

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"recordingmcp/internal/memory"
)

// pfixture seeds a real SQLite database through memory.Open and exposes the
// shared handle for raw SQL, so every test exercises real rows.
type pfixture struct {
	t     *testing.T
	store *memory.Store
	db    *sql.DB
}

func newProjectionFixture(t *testing.T) *pfixture {
	t.Helper()
	store, err := memory.Open(filepath.Join(t.TempDir(), "projection.db"))
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return &pfixture{t: t, store: store, db: store.DB()}
}

func (f *pfixture) exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.db.Exec(query, args...); err != nil {
		f.t.Fatalf("exec %q: %v", query, err)
	}
}

func (f *pfixture) conversation(id, recID string) {
	f.t.Helper()
	f.exec(`INSERT INTO conversations(id,title,summary,annotation_version,annotation_basis,recording_id,source_sha256,transcript_version,duration_ms,transcript_json,annotations_json)
 VALUES(?,?,'','v1','',?,'sha','tv',1000,'{}','{}')`, id, id, recID)
}

// recording seeds a recording; startedAt nil means unknown start.
func (f *pfixture) recording(id, convID string, startedAt *string) {
	f.t.Helper()
	known := 0
	if startedAt != nil {
		known = 1
	}
	f.exec(`INSERT INTO recordings(id,conversation_id,source_file,source_sha256,duration_ms,recording_started_at,absolute_time_known)
 VALUES(?,?,'file','sha',1000,?,?)`, id, convID, startedAt, known)
}

// turn seeds a turn; startMS nil means no global time anchor.
func (f *pfixture) turn(id, convID, recID string, startMS *int64, speaker *string, content string) {
	f.t.Helper()
	f.exec(`INSERT INTO turns(id,conversation_id,recording_id,segment_index,global_start_ms,global_end_ms,speaker_scoped_id,speaker_entity_id,content,source_sha256,transcript_version,original_turn_id,turn_json)
 VALUES(?,?,?,0,?,NULL,?,NULL,?,'sha','rev-1',?,'{}')`, id, convID, recID, startMS, speaker, content, id)
}

func (f *pfixture) topic(id, title string) {
	f.t.Helper()
	f.exec(`INSERT INTO topics(id,title) VALUES(?,?)`, id, title)
}

func (f *pfixture) topicConversation(topicID, convID string) {
	f.t.Helper()
	f.exec(`INSERT INTO topic_conversations(topic_id,conversation_id) VALUES(?,?)`, topicID, convID)
}

func (f *pfixture) turnTopic(turnID, topicID string) {
	f.t.Helper()
	f.exec(`INSERT INTO turn_topics(turn_id,topic_id) VALUES(?,?)`, turnID, topicID)
}

// note seeds a notes row plus its evidence in ordinal order.
func (f *pfixture) note(id, convID, kind, summary string, topicIDs, evidenceTurnIDs []string) {
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
 VALUES(?,?,?,?,?,'inference','','v1')`, id, convID, kind, summary, topics)
	for i, turnID := range evidenceTurnIDs {
		f.exec(`INSERT INTO note_evidence(note_id,turn_id,ordinal) VALUES(?,?,?)`, id, turnID, i)
	}
}

func (f *pfixture) markStaleNote(noteID, convID string) {
	f.t.Helper()
	f.exec(`INSERT INTO derived_artifacts(kind,artifact_id,conversation_id,stale,stale_reason,stale_at)
 VALUES('note',?,?,1,'superseded by newer transcript',NULL)`, noteID, convID)
}

func (f *pfixture) markAmbientTurn(turnID string) {
	f.t.Helper()
	f.exec(`INSERT INTO turn_classes(turn_id,class,subtype,method,created_at) VALUES(?,'ambient','kiosk_prompt','rule',0)`, turnID)
}

// attachment links turns to one source attachment; active=0 marks superseded.
func (f *pfixture) attachment(id, convID, recID, revision string, active int, turnIDs ...string) {
	f.t.Helper()
	f.exec(`INSERT INTO source_attachments(id,conversation_id,recording_id,segment_index,segment_id,transcript_revision,source_json,result_json,content_sha256,active,supersedes_revision)
 VALUES(?,?,?,0,?,?, '{}','{}','sha',?,NULL)`, id, convID, recID, "seg-"+id, revision, active)
	for _, turnID := range turnIDs {
		f.exec(`INSERT INTO attachment_turns(attachment_id,turn_id) VALUES(?,?)`, id, turnID)
	}
}

func (f *pfixture) binding(id, recID, revision, speaker, personID, status string, version int) {
	f.t.Helper()
	f.exec(`INSERT INTO speaker_bindings(id,recording_id,transcript_revision,speaker_scoped_id,person_id,status,evidence_json,reviewer,version,created_at,updated_at)
 VALUES(?,?,?,?,?,?, '[]','',?,0,0)`, id, recID, revision, speaker, personID, status, version)
}

func (f *pfixture) build(req Request) Result {
	f.t.Helper()
	result, err := Build(context.Background(), f.store, f.db, req)
	if err != nil {
		f.t.Fatalf("Build: %v", err)
	}
	return result
}

func objectOf(t *testing.T, result Result, id string) Object {
	t.Helper()
	for _, obj := range result.Objects {
		if obj.ID == id {
			return obj
		}
	}
	t.Fatalf("no object %q in %d objects", id, len(result.Objects))
	return Object{}
}

func hasObject(result Result, id string) bool {
	for _, obj := range result.Objects {
		if obj.ID == id {
			return true
		}
	}
	return false
}

func unresolvedReason(result Result, subject, reason string) bool {
	for _, u := range result.Unresolved {
		if u.Subject == subject && u.Reason == reason {
			return true
		}
	}
	return false
}

func TestNoteKindMappingProjectsCommitmentPreferenceEvent(t *testing.T) {
	f := newProjectionFixture(t)
	f.conversation("conv-1", "rec-1")
	f.recording("rec-1", "conv-1", new("2026-03-01T09:00:00+08:00"))
	f.turn("t:1", "conv-1", "rec-1", new(int64(1000)), nil, "讨论出差与预算")
	f.turn("t:untimed", "conv-1", "rec-1", nil, nil, "没有时间锚的观察")

	f.topic("topic:move", "搬家")
	f.topic("topic:report", "报告")
	f.topic("topic:budget", "预算")
	f.topic("topic:visit", "参观")

	f.note("n:int", "conv-1", "intention", "我打算下周搬去珠海出差", []string{"topic:move"}, []string{"t:1"})
	f.note("n:req", "conv-1", "request", "请把报告发给我", []string{"topic:report"}, []string{"t:1"})
	f.note("n:cons", "conv-1", "constraint", "预算不能超过十万", []string{"topic:budget"}, []string{"t:1"})
	f.note("n:obs", "conv-1", "observation", "参观了珠海的新办公室", []string{"topic:visit"}, []string{"t:1"})
	f.note("n:sug", "conv-1", "suggestion", "建议多招两个人", nil, []string{"t:1"})
	f.note("n:untimed", "conv-1", "observation", "有人提到旧系统", nil, []string{"t:untimed"})

	result := f.build(Request{})

	intention := objectOf(t, result, "n:int")
	if intention.Kind != KindCommitment || intention.Confidence != 0.6 || intention.Status != StatusCurrent {
		t.Fatalf("n:int = %+v", intention)
	}
	if intention.Subject != "topic:move" {
		t.Fatalf("n:int subject = %q", intention.Subject)
	}
	request := objectOf(t, result, "n:req")
	if request.Kind != KindCommitment || request.Confidence != 0.5 {
		t.Fatalf("n:req = %+v", request)
	}
	constraint := objectOf(t, result, "n:cons")
	if constraint.Kind != KindPreference || constraint.Confidence != 0.6 {
		t.Fatalf("n:cons = %+v", constraint)
	}
	obs := objectOf(t, result, "n:obs")
	if obs.Kind != KindEvent || obs.Confidence != 0.5 {
		t.Fatalf("n:obs = %+v", obs)
	}
	if want := []string{"t:1"}; len(obs.EvidenceTurnIDs) != 1 || obs.EvidenceTurnIDs[0] != want[0] {
		t.Fatalf("n:obs evidence = %v, want %v", obs.EvidenceTurnIDs, want)
	}
	if intention.DerivedFrom[0] != "note:n:int" {
		t.Fatalf("n:int derived_from = %v", intention.DerivedFrom)
	}
	if !unresolvedReason(result, "n:sug", "kind_not_projected_in_v0") {
		t.Fatalf("suggestion not reported unresolved: %+v", result.Unresolved)
	}
	if !unresolvedReason(result, "n:untimed", "kind_not_projected_in_v0") {
		t.Fatalf("untimed observation not reported unresolved: %+v", result.Unresolved)
	}
	if hasObject(result, "n:sug") || hasObject(result, "n:untimed") {
		t.Fatal("unprojected note produced an object")
	}
	if result.Coverage.NotesUnprojected != 2 {
		t.Fatalf("notes_unprojected = %d, want 2", result.Coverage.NotesUnprojected)
	}
}

func TestConflictRuleMarksEarlierCommitmentSuperseded(t *testing.T) {
	f := newProjectionFixture(t)
	f.conversation("conv-1", "rec-1")
	f.recording("rec-1", "conv-1", new("2026-03-01T09:00:00+08:00"))
	f.turn("t:early", "conv-1", "rec-1", new(int64(1000)), nil, "我们讨论搬家")
	f.turn("t:later", "conv-1", "rec-1", new(int64(2000)), nil, "计划又变了")
	f.topic("topic:move", "搬家")
	f.turnTopic("t:early", "topic:move")
	f.turnTopic("t:later", "topic:move")
	f.note("n:early", "conv-1", "intention", "计划搬去珠海定居", []string{"topic:move"}, []string{"t:early"})
	f.note("n:later", "conv-1", "intention", "暂时不搬珠海了", []string{"topic:move"}, []string{"t:later"})

	result := f.build(Request{})

	earlier := objectOf(t, result, "n:early")
	if earlier.Status != StatusSuperseded || earlier.StatusReason != "negated_by_later_note" {
		t.Fatalf("n:early = %+v", earlier)
	}
	if len(earlier.ConflictWith) != 1 || earlier.ConflictWith[0] != "n:later" {
		t.Fatalf("n:early conflict_with = %v", earlier.ConflictWith)
	}
	later := objectOf(t, result, "n:later")
	if later.Status != StatusCurrent {
		t.Fatalf("n:later = %+v", later)
	}
	if len(later.ConflictWith) != 1 || later.ConflictWith[0] != "n:early" {
		t.Fatalf("n:later conflict_with = %v", later.ConflictWith)
	}
	if len(result.Conflicts) != 1 {
		t.Fatalf("conflicts = %+v, want exactly one", result.Conflicts)
	}
	conflict := result.Conflicts[0]
	if conflict.EarlierID != "n:early" || conflict.LaterID != "n:later" || conflict.Kind != KindCommitment {
		t.Fatalf("conflict = %+v", conflict)
	}
	if conflict.Cue != "暂时不" {
		t.Fatalf("cue = %q, want 暂时不", conflict.Cue)
	}
	if conflict.Subject != "topic:move" {
		t.Fatalf("conflict subject = %q", conflict.Subject)
	}
}

func TestConflictRuleIgnoresNonCueNegation(t *testing.T) {
	f := newProjectionFixture(t)
	f.conversation("conv-1", "rec-1")
	f.recording("rec-1", "conv-1", new("2026-03-01T09:00:00+08:00"))
	f.turn("t:early", "conv-1", "rec-1", new(int64(1000)), nil, "聊计划")
	f.turn("t:later", "conv-1", "rec-1", new(int64(2000)), nil, "继续聊计划")
	f.topic("topic:move", "搬家")
	f.note("n:early", "conv-1", "intention", "计划搬去珠海定居", []string{"topic:move"}, []string{"t:early"})
	f.note("n:later", "conv-1", "intention", "只是不喜欢搬来搬去", []string{"topic:move"}, []string{"t:later"})

	result := f.build(Request{})

	for _, id := range []string{"n:early", "n:later"} {
		if obj := objectOf(t, result, id); obj.Status != StatusCurrent {
			t.Fatalf("%s = %+v, want current", id, obj)
		}
	}
	if len(result.Conflicts) != 0 {
		t.Fatalf("conflicts = %+v, want none", result.Conflicts)
	}
}

func TestPersonObjectFromConfirmedBindingOnly(t *testing.T) {
	f := newProjectionFixture(t)
	f.conversation("conv-p", "rec-p")
	f.recording("rec-p", "conv-p", new("2026-03-01T09:00:00+08:00"))
	speaker := "spk:dev:1"
	f.turn("t:p1", "conv-p", "rec-p", new(int64(1000)), &speaker, "我同意这个方案")
	f.turn("t:p2", "conv-p", "rec-p", new(int64(2000)), &speaker, "我下周跟进")
	f.attachment("att-1", "conv-p", "rec-p", "rev-1", 1, "t:p1", "t:p2")
	f.binding("sb-1", "rec-p", "rev-1", speaker, "person:alice", "confirmed", 1)
	f.binding("sb-2", "rec-p", "rev-1", speaker, "", "proposed", 2)
	f.binding("sb-3", "rec-p", "rev-1", speaker, "person:carol", "rejected", 3)

	result := f.build(Request{})

	alice := objectOf(t, result, "person:alice")
	if alice.Kind != KindPerson || alice.Subject != "person:alice" || alice.Status != StatusCurrent {
		t.Fatalf("alice = %+v", alice)
	}
	if alice.Confidence != 1.0 {
		t.Fatalf("alice confidence = %f", alice.Confidence)
	}
	if len(alice.EvidenceTurnIDs) != 2 {
		t.Fatalf("alice evidence = %v", alice.EvidenceTurnIDs)
	}
	if alice.DerivedFrom[0] != "binding:sb-1" {
		t.Fatalf("alice derived_from = %v", alice.DerivedFrom)
	}
	if len(alice.ConversationIDs) != 1 || alice.ConversationIDs[0] != "conv-p" {
		t.Fatalf("alice conversations = %v", alice.ConversationIDs)
	}
	if alice.Summary != "confirmed speaker label spk:dev:1 in recording rec-p" {
		t.Fatalf("alice summary = %q", alice.Summary)
	}
	for _, absent := range []string{"person:bob", "person:carol", "spk:dev:1"} {
		if hasObject(result, absent) {
			t.Fatalf("unconfirmed binding projected as %q", absent)
		}
	}
	if result.Coverage.BindingsScanned != 1 {
		t.Fatalf("bindings_scanned = %d, want 1", result.Coverage.BindingsScanned)
	}
}

func TestPersonObjectUnresolvedWhenEvidenceSuperseded(t *testing.T) {
	f := newProjectionFixture(t)
	f.conversation("conv-u", "rec-u")
	f.recording("rec-u", "conv-u", new("2026-03-01T09:00:00+08:00"))
	speaker := "spk:dev:2"
	f.turn("t:u1", "conv-u", "rec-u", new(int64(1000)), &speaker, "旧修订里的发言")
	// The only attachment backing the turn is superseded, so the binding's
	// evidence has no current turn left.
	f.attachment("att-sup", "conv-u", "rec-u", "rev-1", 0, "t:u1")
	f.binding("sb-u", "rec-u", "rev-1", speaker, "person:dave", "confirmed", 1)

	result := f.build(Request{})

	dave := objectOf(t, result, "person:dave")
	if dave.Status != StatusUnresolved || dave.StatusReason != "evidence_superseded" {
		t.Fatalf("dave = %+v", dave)
	}
	if len(dave.EvidenceTurnIDs) != 0 {
		t.Fatalf("dave evidence = %v, want none", dave.EvidenceTurnIDs)
	}
}

func TestStaleNoteAbsentUnlessAudit(t *testing.T) {
	f := newProjectionFixture(t)
	f.conversation("conv-1", "rec-1")
	f.recording("rec-1", "conv-1", new("2026-03-01T09:00:00+08:00"))
	f.turn("t:1", "conv-1", "rec-1", new(int64(1000)), nil, "聊预算")
	f.topic("topic:budget", "预算")
	f.note("n:stale", "conv-1", "constraint", "预算不能超过十万", []string{"topic:budget"}, []string{"t:1"})
	f.markStaleNote("n:stale", "conv-1")

	deflt := f.build(Request{})
	if hasObject(deflt, "n:stale") {
		t.Fatal("stale note projected without audit")
	}
	audit := f.build(Request{Audit: true})
	obj := objectOf(t, audit, "n:stale")
	if obj.Status != StatusSuperseded || obj.StatusReason != "stale_evidence" {
		t.Fatalf("audit object = %+v", obj)
	}
	if obj.Kind != KindPreference {
		t.Fatalf("audit kind = %q", obj.Kind)
	}
}

func TestAmbientPrimaryEvidenceNoteNotProjected(t *testing.T) {
	f := newProjectionFixture(t)
	f.conversation("conv-a", "rec-a")
	f.recording("rec-a", "conv-a", new("2026-03-01T09:00:00+08:00"))
	f.turn("t:amb", "conv-a", "rec-a", new(int64(1000)), nil, "欢迎光临，请扫码点餐")
	f.markAmbientTurn("t:amb")
	f.turn("t:speech", "conv-a", "rec-a", new(int64(2000)), nil, "聊点正事")
	f.topic("topic:food", "点餐")
	f.note("n:amb", "conv-a", "intention", "打算换一家餐厅", []string{"topic:food"}, []string{"t:amb"})
	f.note("n:speech", "conv-a", "intention", "打算认真聊合作", []string{"topic:food"}, []string{"t:speech"})

	result := f.build(Request{})
	if hasObject(result, "n:amb") {
		t.Fatal("ambient-primary note projected")
	}
	if !hasObject(result, "n:speech") {
		t.Fatal("speech note missing")
	}
	for _, u := range result.Unresolved {
		if u.Subject == "n:amb" {
			t.Fatalf("ambient note reported unresolved: %+v", u)
		}
	}
}

func TestScopeAndLimitAreHardFilters(t *testing.T) {
	f := newProjectionFixture(t)
	march := "2026-03-01T09:00:00+08:00"
	for _, conv := range []string{"conv-1", "conv-2", "conv-3"} {
		f.conversation(conv, "rec-"+conv)
		f.recording("rec-"+conv, conv, new(march))
		f.turn("t:"+conv, conv, "rec-"+conv, new(int64(1000)), nil, "范围探针")
	}
	f.conversation("conv-unknown", "rec-unknown")
	f.recording("rec-unknown", "conv-unknown", nil)
	f.turn("t:unknown", "conv-unknown", "rec-unknown", new(int64(1000)), nil, "范围探针")

	topicTurns := map[string]string{
		"topic:1":       "t:conv-1",
		"topic:2":       "t:conv-2",
		"topic:3":       "t:conv-3",
		"topic:unknown": "t:unknown",
	}
	for _, topic := range []string{"topic:1", "topic:2", "topic:3", "topic:unknown"} {
		f.topic(topic, "主题"+topic)
		f.turnTopic(topicTurns[topic], topic)
	}
	f.note("n:1", "conv-1", "intention", "计划一", []string{"topic:1"}, []string{"t:conv-1"})
	f.note("n:2", "conv-2", "intention", "计划二", []string{"topic:2"}, []string{"t:conv-2"})
	f.note("n:3", "conv-3", "intention", "计划三", []string{"topic:3"}, []string{"t:conv-3"})
	f.note("n:u", "conv-unknown", "intention", "未知时间计划", []string{"topic:unknown"}, []string{"t:unknown"})

	after := mustTime(t, "2026-01-01T00:00:00Z")
	before := mustTime(t, "2026-12-31T00:00:00Z")

	convScoped := f.build(Request{ConversationIDs: []string{"conv-2"}})
	if hasObject(convScoped, "n:1") || hasObject(convScoped, "n:3") {
		t.Fatalf("conversation filter leaked: %+v", convScoped.Objects)
	}
	if !hasObject(convScoped, "n:2") {
		t.Fatal("in-scope object missing")
	}

	topicScoped := f.build(Request{TopicIDs: []string{"topic:3"}})
	if !hasObject(topicScoped, "n:3") {
		t.Fatal("topic-scoped object missing")
	}
	if hasObject(topicScoped, "n:1") || hasObject(topicScoped, "n:2") {
		t.Fatal("topic filter leaked")
	}

	timeScoped := f.build(Request{StartedAfter: after, StartedBefore: before})
	if hasObject(timeScoped, "n:u") {
		t.Fatal("unknown-start conversation passed a bounded request")
	}
	if !hasObject(timeScoped, "n:1") {
		t.Fatal("known-start object dropped by bounded request")
	}

	limited := f.build(Request{Limit: 2})
	if len(limited.Objects) != 2 {
		t.Fatalf("limit = %d objects, want 2", len(limited.Objects))
	}
	if limited.Coverage.ObjectsProjected < 2 {
		t.Fatalf("objects_projected = %d", limited.Coverage.ObjectsProjected)
	}
	// Ordering is (kind asc, subject asc) so a limit keeps the head.
	for i := 1; i < len(limited.Objects); i++ {
		prev, cur := limited.Objects[i-1], limited.Objects[i]
		if prev.Kind > cur.Kind || (prev.Kind == cur.Kind && prev.Subject > cur.Subject) {
			t.Fatalf("objects not sorted: %+v", limited.Objects)
		}
	}
}

func TestProjectionIsDeterministic(t *testing.T) {
	f := newProjectionFixture(t)
	f.conversation("conv-1", "rec-1")
	f.recording("rec-1", "conv-1", new("2026-03-01T09:00:00+08:00"))
	f.turn("t:1", "conv-1", "rec-1", new(int64(1000)), nil, "聊计划")
	f.turn("t:2", "conv-1", "rec-1", new(int64(2000)), nil, "又聊计划")
	speaker := "spk:dev:1"
	f.turn("t:3", "conv-1", "rec-1", new(int64(3000)), &speaker, "我发言")
	f.attachment("att-1", "conv-1", "rec-1", "rev-1", 1, "t:3")
	f.binding("sb-1", "rec-1", "rev-1", speaker, "person:alice", "confirmed", 1)
	f.topic("topic:a", "甲")
	f.topic("topic:b", "乙")
	f.turnTopic("t:1", "topic:a")
	f.turnTopic("t:2", "topic:b")
	f.note("n:int", "conv-1", "intention", "计划搬去珠海定居", []string{"topic:a"}, []string{"t:1"})
	f.note("n:req", "conv-1", "request", "请把报告发给我", []string{"topic:b"}, []string{"t:2"})
	f.note("n:sug", "conv-1", "suggestion", "建议加一条", nil, []string{"t:2"})

	first := f.build(Request{})
	second := f.build(Request{})
	a, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatalf("two builds differ:\n%s\n%s", a, b)
	}
}

func TestEveryObjectEvidenceResolvesToCurrentTurn(t *testing.T) {
	f := newProjectionFixture(t)
	f.conversation("conv-1", "rec-1")
	f.recording("rec-1", "conv-1", new("2026-03-01T09:00:00+08:00"))
	f.turn("t:current", "conv-1", "rec-1", new(int64(1000)), nil, "当前证据")
	f.turn("t:stale", "conv-1", "rec-1", new(int64(2000)), nil, "过期证据")
	f.attachment("att-old", "conv-1", "rec-1", "rev-1", 0, "t:stale")
	f.turn("t:amb", "conv-1", "rec-1", new(int64(3000)), nil, "欢迎光临，请扫码点餐")
	f.markAmbientTurn("t:amb")
	f.turn("t:speech", "conv-1", "rec-1", new(int64(4000)), nil, "另一段当前证据")
	f.topic("topic:a", "甲")
	f.note("n:cur", "conv-1", "intention", "计划一", []string{"topic:a"}, []string{"t:current"})
	f.note("n:speech", "conv-1", "intention", "计划二", []string{"topic:a"}, []string{"t:speech"})

	result := f.build(Request{})
	if len(result.Objects) == 0 {
		t.Fatal("no objects projected")
	}
	for _, obj := range result.Objects {
		for _, turnID := range obj.EvidenceTurnIDs {
			var ok bool
			err := f.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM turns t WHERE t.id=? AND `+
				memory.CurrentTurnPredicate("t")+
				` AND NOT EXISTS(SELECT 1 FROM turn_classes tc WHERE tc.turn_id=t.id AND tc.class='ambient'))`, turnID).Scan(&ok)
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				t.Fatalf("object %s carries non-current evidence turn %q", obj.ID, turnID)
			}
		}
	}
}

func mustTime(t *testing.T, value string) *time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("parse time %q: %v", value, err)
	}
	return &parsed
}
