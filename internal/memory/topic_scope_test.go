package memory

import (
	"context"
	"testing"
)

// TestStaleTopicMembershipDoesNotSurfaceCurrentContent proves that a topic
// whose membership comes only from superseded turn_topics rows cannot route
// current retrieval back to historical content: current-state queries see no
// members of the dead mapping, while the audit switches can still inspect it.
// The topic row itself remains historical taxonomy (auditable via list_topics);
// only content retrieval is revision-scoped.
func TestStaleTopicMembershipDoesNotSurfaceCurrentContent(t *testing.T) {
	ctx := context.Background()
	s := openTempStore(t)

	v1 := sampleResult("seg-a", "rev-1", 0)
	v1.Turns[0].Text = "historical topic phrase"
	if _, err := s.IngestTranscript(ctx, v1); err != nil {
		t.Fatal(err)
	}
	var conv, v1TurnID string
	if err := s.db.QueryRow(`SELECT conversation_id, id FROM turns`).Scan(&conv, &v1TurnID); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyAnnotations(ctx, conv, "", Annotations{
		ConversationID: conv, AnnotationVersion: "rev-1", Title: "topic scope",
		Topics: []TopicAnnotation{{ID: "topic:hist", Title: "historical"}},
		Notes: []NoteAnnotation{{
			ID: "note:hist", Kind: "observation", TopicIDs: []string{"topic:hist"},
			Summary: "historical note phrase", EvidenceTurnIDs: []string{v1TurnID},
		}},
		TurnTopics: []TurnTopicAnnotation{{TurnID: v1TurnID, TopicIDs: []string{"topic:hist"}}},
	}); err != nil {
		t.Fatal(err)
	}

	// Sanity: while rev-1 is active the topic has members.
	if page, err := s.Query(ctx, Query{TopicID: "topic:hist", IncludeTurns: true}); err != nil || len(page.Items) == 0 {
		t.Fatalf("live topic has no members before supersede: %+v %v", page, err)
	}

	v2 := sampleResult("seg-a", "rev-2", 0)
	v2.Turns[0].Text = "current phrase"
	v2.Source.SupersedesRevision = "rev-1"
	if _, err := s.IngestTranscript(ctx, v2); err != nil {
		t.Fatal(err)
	}

	// The stale mapping must not surface current content: its only members are
	// the superseded turn and its stale note.
	if page, err := s.Query(ctx, Query{TopicID: "topic:hist", IncludeTurns: true}); err != nil || len(page.Items) != 0 {
		t.Fatalf("stale topic mapping surfaced current content: %+v %v", page, err)
	}
	if page, err := s.SearchMemory(ctx, MemorySearch{Text: "phrase", Scope: "both", TopicID: "topic:hist"}); err != nil || len(page.Hits) != 0 {
		t.Fatalf("stale topic mapping surfaced search hits: %+v %v", page, err)
	}
	// Audit switches still see the historical membership.
	audit, err := s.Query(ctx, Query{TopicID: "topic:hist", IncludeTurns: true, IncludeStale: true})
	if err != nil || len(audit.Items) == 0 {
		t.Fatalf("audit lookup lost historical topic membership: %+v %v", audit, err)
	}
}
