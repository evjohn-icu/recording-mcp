package memory

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestApplyAnnotationsRevisionAndHistory(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	transcript, initial := synthetic()
	if _, err = s.Import(ctx, transcript, initial, "synthetic.wav", strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}

	next := Annotations{ConversationID: initial.ConversationID, Title: "修订标题", Summary: "新摘要", AnnotationVersion: "annotation-v2", AnnotationBasis: "人工注释；未核实", Topics: []TopicAnnotation{{ID: "topic:device", Title: "设备"}, {ID: "topic:new", Title: "新主题"}}, Notes: []NoteAnnotation{{ID: "note:revised", Kind: "observation", TopicIDs: []string{"topic:new"}, Summary: "记录观察", EvidenceTurnIDs: []string{"seg01-turn0001"}}}, TurnTopics: []TurnTopicAnnotation{{TurnID: turnKey(initial.RecordingID, "seg01-turn0002"), TopicIDs: []string{"topic:device"}}}}
	if err = s.ApplyAnnotations(ctx, initial.ConversationID, "annotation-v1", next); err != nil {
		t.Fatal(err)
	}
	// Old expected version is safe for an exact replay.
	if err = s.ApplyAnnotations(ctx, initial.ConversationID, "annotation-v1", next); err != nil {
		t.Fatalf("exact replay failed: %v", err)
	}
	changed := next
	changed.Summary = "different"
	if err = s.ApplyAnnotations(ctx, initial.ConversationID, "annotation-v1", changed); err == nil {
		t.Fatal("stale conflicting write accepted")
	}
	if err = s.ApplyAnnotations(ctx, initial.ConversationID, "annotation-v2", changed); err == nil {
		t.Fatal("reused version accepted")
	}

	history, err := s.GetAnnotationHistory(ctx, initial.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 || history[0].Version != "annotation-v1" || history[0].Current || history[1].Version != "annotation-v2" || !history[1].Current || history[0].Annotations.Title != initial.Title {
		t.Fatalf("bad history: %+v", history)
	}
	page, err := s.Query(ctx, Query{ConversationID: initial.ConversationID})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 2 {
		t.Fatalf("expected conversation and one current note after replace, got %d: %+v", page.Total, page.Items)
	}
	var currentNote Item
	for _, item := range page.Items {
		if item.Kind == "observation" {
			currentNote = item
		}
	}
	if currentNote.AnnotationVersion != "annotation-v2" {
		t.Fatalf("query omitted annotation version: %+v", currentNote)
	}
	detail, err := s.GetItem(ctx, currentNote.ID, false)
	if err != nil || detail.Item.AnnotationVersion != "annotation-v2" {
		t.Fatalf("GetItem omitted annotation version: %+v %v", detail, err)
	}
	if _, err = s.GetItem(ctx, noteKey(initial.ConversationID, "note:one"), false); err == nil {
		t.Fatal("old note remained in current view")
	}
}

func TestApplyAnnotationsRejectsBadEvidenceAndPreservesPriorRevision(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tr, a := synthetic()
	if _, err = s.Import(ctx, tr, a, "synthetic.wav", strings.Repeat("c", 64)); err != nil {
		t.Fatal(err)
	}
	bad := Annotations{ConversationID: a.ConversationID, Title: a.Title, AnnotationVersion: "annotation-v2", Topics: []TopicAnnotation{{ID: "topic:x", Title: "X"}}, Notes: []NoteAnnotation{{ID: "note:x", Kind: "request", TopicIDs: []string{"topic:x"}, Summary: "bad foreign evidence", EvidenceTurnIDs: []string{"not-this-turn"}}}}
	if err = s.ApplyAnnotations(ctx, a.ConversationID, "annotation-v1", bad); err == nil {
		t.Fatal("unknown evidence accepted")
	}
	history, err := s.GetAnnotationHistory(ctx, a.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || history[0].Version != "annotation-v1" {
		t.Fatalf("failed write changed state: %+v", history)
	}
	if err = s.ApplyAnnotations(ctx, a.ConversationID, "annotation-v1", Annotations{ConversationID: a.ConversationID, Title: a.Title, AnnotationVersion: "annotation-v2"}); err != nil {
		t.Fatalf("empty annotation set must be allowed: %v", err)
	}
}
