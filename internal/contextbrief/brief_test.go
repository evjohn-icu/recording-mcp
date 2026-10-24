package contextbrief

import (
	"context"
	"recordingmcp/internal/memory"
	"testing"
)

func TestBuildRequiresExplicitNonemptyScope(t *testing.T) {
	store, err := memory.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := Build(context.Background(), store, Request{}); err == nil {
		t.Fatal("expected an explicit scope")
	}
}

func TestBriefSurfaceHasFixedSectionsAndNoImplicitConfirmation(t *testing.T) {
	sections := Sections{Background: []Entry{}, PreviousDiscussions: []Entry{}, FollowUpCandidates: []Entry{}, ChangesConflicts: []Entry{}, Unknowns: []Entry{}}
	if sections.Background == nil || sections.PreviousDiscussions == nil || sections.FollowUpCandidates == nil || sections.ChangesConflicts == nil || sections.Unknowns == nil {
		t.Fatal("all brief sections should be initialized")
	}
	entry := Entry{AssertionState: "proposed", EvidenceIDs: []string{"ev:test"}}
	if entry.AssertionState == "explicitly_confirmed" {
		t.Fatal("inference must not become explicit confirmation")
	}
}

func TestExplicitTopicConversationUnionAndPersonIntersection(t *testing.T) {
	x := rawTurn{conv: "c1", topics: []string{"t1"}, people: []string{"p1"}}
	if !scopeMatches(Scope{Kind: "project", TopicIDs: []string{"t1"}, ConversationIDs: []string{"c2"}}, x) {
		t.Fatal("topic/conversation scopes must be unioned")
	}
	if validate(Request{Scope: Scope{Kind: "project", TopicIDs: []string{"t1"}, PersonIDs: []string{"p1"}}}) == nil {
		t.Fatal("project scope must reject person ids")
	}
	if scopeMatches(Scope{Kind: "project", TopicIDs: []string{"t2"}, ConversationIDs: []string{"c2"}}, x) {
		t.Fatal("scope expanded beyond explicit ids")
	}
}
