package projection

import "testing"

func TestPersonScopeDoesNotCountUnrelatedNotesOrBindings(t *testing.T) {
	f := newProjectionFixture(t)
	f.conversation("conv", "rec")
	f.recording("rec", "conv", nil)
	f.turn("turn", "conv", "rec", new(int64(0)), nil, "原话")
	f.note("note", "conv", "suggestion", "建议", nil, []string{"turn"})
	f.binding("binding", "rec", "rev", "speaker", "person:other", "confirmed", 1)
	result := f.build(Request{PersonIDs: []string{"person:requested"}})
	if len(result.Unresolved) != 0 || result.Coverage.NotesScanned != 0 || result.Coverage.BindingsScanned != 0 || result.Coverage.TopicsScanned != 0 {
		t.Fatalf("person scope leaked counts or unresolved: %+v", result)
	}
}

func TestHistoricalTopicConversationMembershipDoesNotProjectTopic(t *testing.T) {
	f := newProjectionFixture(t)
	f.conversation("conv-old", "rec-old")
	f.recording("rec-old", "conv-old", nil)
	f.topic("topic-old", "旧主题")
	f.topicConversation("topic-old", "conv-old")

	result := f.build(Request{})
	if hasObject(result, "topic-old") {
		t.Fatalf("topic projected from historical topic_conversations membership: %+v", objectOf(t, result, "topic-old"))
	}
}

// A conversation-scoped projection must not scan or report notes belonging to
// a different conversation, even when those notes would otherwise be
// unresolved.
func TestConversationScopeExcludesOtherConversationNotesFromAudit(t *testing.T) {
	f := newProjectionFixture(t)
	f.conversation("conv-a", "rec-a")
	f.recording("rec-a", "conv-a", nil)
	f.turn("turn-a", "conv-a", "rec-a", new(int64(1000)), nil, "A 侧转写")

	f.conversation("conv-b", "rec-b")
	f.recording("rec-b", "conv-b", nil)
	f.turn("turn-b", "conv-b", "rec-b", new(int64(1000)), nil, "B 侧真实证据转写")
	f.note("note-b-suggestion", "conv-b", "suggestion", "建议后续跟进", nil, []string{"turn-b"})

	result := f.build(Request{ConversationIDs: []string{"conv-a"}})
	for _, unresolved := range result.Unresolved {
		if unresolved.Subject == "note-b-suggestion" {
			t.Errorf("out-of-scope note appeared in unresolved results: %+v", result.Unresolved)
			break
		}
	}
	if result.Coverage.NotesScanned != 0 {
		t.Errorf("notes_scanned = %d, want 0 for scope containing no notes", result.Coverage.NotesScanned)
	}
}
