package pipeline

import (
	"context"
	"testing"

	"recordingmcp/internal/memory"
)

func TestRebuildAutomaticTopicMembershipDropsSupersededOnly(t *testing.T) {
	ctx := context.Background()
	db, _ := openTestDBAndStore(t)
	seedTwoTurns(t, db, "membership-conv")
	insertSourceAttachment(t, db, "membership-conv", "old", 0, "rev-1", false)
	linkAttachmentTurn(t, db, "old", "t1")

	input := memory.Annotations{TurnTopics: []memory.TurnTopicAnnotation{
		{TurnID: "t1", TopicIDs: []string{autoTopicPrefix + "auto"}},
		{TurnID: "t1", TopicIDs: []string{"human-topic"}},
		{TurnID: "t2", TopicIDs: []string{autoTopicPrefix + "auto"}},
	}}
	got, err := rebuildAutomaticTopicMembership(ctx, db, "membership-conv", input)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.TurnTopics) != 2 || got.TurnTopics[0].TurnID != "t1" || got.TurnTopics[0].TopicIDs[0] != "human-topic" || got.TurnTopics[1].TurnID != "t2" {
		t.Fatalf("rebuilt memberships = %+v; expected stale automatic link removed and human/a current links retained", got.TurnTopics)
	}
	if len(input.TurnTopics) != 3 {
		t.Fatalf("rebuild mutated input annotations: %+v", input.TurnTopics)
	}
}
