package memory

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func i64(v int64) *int64   { return &v }
func str(v string) *string { return &v }
func yes(v bool) *bool     { return &v }
func synthetic() (Transcript, Annotations) {
	t := Transcript{AudioFileName: "synthetic.wav", DurationSeconds: 1.001}
	t.Coverage.StartMS = 0
	t.Coverage.EndMS = 1001
	t.Turns = []Turn{
		{ID: "seg01-turn0001", SegmentIndex: 1, SegmentStartMS: 0, SegmentEndMS: 1001, GlobalStartMS: nil, GlobalEndMS: i64(500), SpeakerLocalID: str("0"), SpeakerScopedID: str("segment-01:speaker-0"), Text: "安卓直接读取设备文件", Definite: yes(true)},
		{ID: "seg01-turn0002", SegmentIndex: 1, SegmentStartMS: 0, SegmentEndMS: 1001, GlobalStartMS: i64(500), GlobalEndMS: i64(900), SpeakerScopedID: nil, Text: "工作方向需要讨论", Definite: yes(true)},
		{ID: "seg01-turn0003", SegmentIndex: 1, SegmentStartMS: 0, SegmentEndMS: 1001, GlobalStartMS: i64(901), GlobalEndMS: i64(950), Text: "", Definite: yes(true)},
		{ID: "seg01-turn0004", SegmentIndex: 1, SegmentStartMS: 0, SegmentEndMS: 1001, GlobalStartMS: i64(950), GlobalEndMS: i64(999), Text: "overlap", OverlapDuplicate: true},
	}
	a := Annotations{Version: 1, ConversationID: "conv:test", Title: "测试会话", RecordingID: "rec:test", Summary: "设备与工作方向讨论", AnnotationVersion: "annotation-v1", AnnotationBasis: "人工解释；未核实", Topics: []TopicAnnotation{{ID: "topic:device", Title: "设备"}, {ID: "topic:career", Title: "工作"}}, Notes: []NoteAnnotation{{ID: "note:one", Kind: "suggestion", TopicIDs: []string{"topic:device"}, Summary: "提出设备读取设想", EvidenceTurnIDs: []string{"seg01-turn0001"}}, {ID: "note:two", Kind: "intention", TopicIDs: []string{"topic:career"}, Summary: "讨论工作方向", EvidenceTurnIDs: []string{"seg01-turn0002"}}}, TurnTopics: []TurnTopicAnnotation{{TurnID: "seg01-turn0001", TopicIDs: []string{"topic:device"}}, {TurnID: "seg01-turn0002", TopicIDs: []string{"topic:career"}}}}
	return t, a
}

func TestImportAtomicIdempotentAndPreservesUncertainty(t *testing.T) {
	db := filepath.Join(t.TempDir(), "memory.db")
	ctx := context.Background()
	s, e := Open(db)
	if e != nil {
		t.Fatal(e)
	}
	tr, a := synthetic()
	bad := a
	bad.Notes = append([]NoteAnnotation(nil), a.Notes...)
	bad.Notes[0].EvidenceTurnIDs = []string{"not-a-turn"}
	if _, e = s.Import(ctx, tr, bad, "synthetic.wav", strings.Repeat("a", 64)); e == nil {
		t.Fatal("unknown evidence accepted")
	}
	if _, e = s.Conversation(ctx, a.ConversationID); e == nil {
		t.Fatal("failed import partially created conversation")
	}
	r, e := s.Import(ctx, tr, a, "synthetic.wav", strings.Repeat("a", 64))
	if e != nil {
		t.Fatal(e)
	}
	if !r.Inserted || r.RetainedTurns != 2 || r.ExcludedBlankTurns != 1 || r.ExcludedOverlapDuplicates != 1 {
		t.Fatalf("bad import report: %+v", r)
	}
	r, e = s.Import(ctx, tr, a, "synthetic.wav", strings.Repeat("a", 64))
	if e != nil || r.Inserted {
		t.Fatalf("replay not idempotent: %+v %v", r, e)
	}
	conflict := tr
	conflict.Turns = append([]Turn(nil), tr.Turns...)
	conflict.Turns[0].Text = "changed"
	if _, e = s.Import(ctx, conflict, a, "synthetic.wav", strings.Repeat("a", 64)); e == nil {
		t.Fatal("conflicting import accepted")
	}
	p, e := s.Query(ctx, Query{ConversationID: a.ConversationID, IncludeTurns: true, Limit: 1})
	if e != nil {
		t.Fatal(e)
	}
	if p.Total != 5 || len(p.Items) != 1 || p.NextOffset == nil {
		t.Fatalf("bad page: %+v", p)
	}
	turn, err := s.GetItem(ctx, turnKey(a.RecordingID, "seg01-turn0001"), false)
	if err != nil {
		t.Fatal(err)
	}
	if turn.Item.GlobalStartMS != nil || turn.Item.SpeakerEntityID != nil {
		t.Fatalf("unknown metadata was invented: %+v", turn.Item)
	}
	if _, e = s.Query(ctx, Query{ConversationID: a.ConversationID, SpeakerScopedID: "segment-01:speaker-0"}); e == nil {
		t.Fatal("unscoped speaker search accepted")
	}
	sp, e := s.Query(ctx, Query{ConversationID: a.ConversationID, RecordingID: a.RecordingID, SpeakerScopedID: "segment-01:speaker-0", IncludeTurns: true})
	turnResults := 0
	for _, item := range sp.Items {
		if item.Kind == "turn" {
			turnResults++
		}
	}
	if e != nil || turnResults != 1 {
		t.Fatalf("scoped speaker search: %+v %v", sp, e)
	}
	page, e := s.Query(ctx, Query{Query: "直接读取", IncludeTurns: true, Limit: 1})
	if e != nil || len(page.Items) != 1 || page.Items[0].Kind != "turn" {
		t.Fatalf("literal text query: %+v %v", page, e)
	}
	notes, e := s.Query(ctx, Query{TopicID: "topic:device"})
	if e != nil || len(notes.Items) != 1 || notes.Items[0].Kind != "suggestion" {
		t.Fatalf("topic filter: %+v %v", notes, e)
	}
	if _, e = s.GetItem(ctx, "note:"+hashID(a.ConversationID+"\x00note:one"), false); e != nil {
		t.Fatalf("note evidence lookup: %v", e)
	}
	if e = s.Close(); e != nil {
		t.Fatal(e)
	}
	s, e = Open(db)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	p, e = s.Query(ctx, Query{ConversationID: a.ConversationID, IncludeTurns: true})
	if e != nil || p.Total != 5 {
		t.Fatalf("reopen lost records: %+v %v", p, e)
	}
}

func TestTurnAndTopicIDsScopeCorrectlyAcrossRecordings(t *testing.T) {
	s, e := Open(filepath.Join(t.TempDir(), "memory.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	ctx := context.Background()
	for _, pair := range []struct{ conv, record string }{{"conv:a", "rec:a"}, {"conv:b", "rec:b"}} {
		tr, a := synthetic()
		a.ConversationID = pair.conv
		a.RecordingID = pair.record
		if _, e = s.Import(ctx, tr, a, pair.record, strings.Repeat("b", 64)); e != nil {
			t.Fatal(e)
		}
	}
	p, e := s.Query(ctx, Query{Query: "安卓", IncludeTurns: true})
	if e != nil || p.Total != 2 {
		t.Fatalf("turn IDs should be recording-scoped: %+v %v", p, e)
	}
	topics, e := s.Topics(ctx, "")
	if e != nil || len(topics.Topics) != 4 {
		t.Fatalf("topic reuse listing: %+v %v", topics, e)
	}
	// A shared global topic keeps one canonical row while linking both conversations.
	tr, a := synthetic()
	a.ConversationID = "conv:c"
	a.RecordingID = "rec:c"
	a.Topics = a.Topics[:1]
	a.Notes = a.Notes[:1]
	a.Notes[0].TopicIDs = []string{"topic:device"}
	a.TurnTopics = a.TurnTopics[:1]
	if _, e = s.Import(ctx, tr, a, "rec:c", strings.Repeat("c", 64)); e != nil {
		t.Fatal(e)
	}
	list, e := s.Topics(ctx, "")
	if e != nil {
		t.Fatal(e)
	}
	cnt := 0
	for _, x := range list.Topics {
		if x.ID == "topic:device" {
			cnt++
		}
	}
	if cnt != 3 {
		t.Fatalf("expected one stable topic linked to three conversations, got %+v", list)
	}
}

func TestValidationRejectsInvalidTypeAndTime(t *testing.T) {
	s, e := Open(filepath.Join(t.TempDir(), "m.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	tr, a := synthetic()
	a.Notes[0].Kind = "commitment"
	if _, e = s.Import(context.Background(), tr, a, "x", "h"); e == nil {
		t.Fatal("commitment should not be inferred")
	}
	tr, a = synthetic()
	tr.Turns[0].GlobalEndMS = i64(2000)
	if _, e = s.Import(context.Background(), tr, a, "x", "h"); e == nil {
		t.Fatal("out of range accepted")
	}
	if _, e = s.Conversation(context.Background(), a.ConversationID); e == nil {
		t.Fatal("invalid batch partially persisted")
	}
	if _, e = s.GetItem(context.Background(), "missing", false); e == nil || !strings.Contains(e.Error(), "not found") {
		t.Fatalf("missing item error=%v", e)
	}
	if !errors.Is(nil, nil) {
		t.Fatal("unreachable")
	}
}

func TestFiveMillisecondSegmentEndTolerancePreservesOriginalValue(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tr, a := synthetic()
	tr.DurationSeconds = 2
	tr.Coverage.EndMS = 2000
	tr.Turns = tr.Turns[:2]
	tr.Turns[0].SegmentEndMS = 1000
	tr.Turns[1].SegmentStartMS = 1000
	tr.Turns[1].SegmentEndMS = 2000
	tr.Turns[1].GlobalStartMS = i64(1003)
	tr.Turns[1].GlobalEndMS = i64(1500)
	tr.Turns[0].GlobalEndMS = i64(1002)
	a.Notes = a.Notes[:1]
	a.TurnTopics = a.TurnTopics[:1]
	if _, err = s.Import(context.Background(), tr, a, "audio", strings.Repeat("d", 64)); err != nil {
		t.Fatalf("2ms rounding variance should be accepted: %v", err)
	}
	it, err := s.GetItem(context.Background(), turnKey(a.RecordingID, tr.Turns[0].ID), false)
	if err != nil || it.Item.GlobalEndMS == nil || *it.Item.GlobalEndMS != 1002 {
		t.Fatalf("raw out-of-segment timestamp must be preserved: %+v %v", it, err)
	}
}
