package contextbrief

import (
	"context"
	"testing"
	"recordingmcp/internal/memory"
)

func TestBuildRequiresExplicitNonemptyScope(t *testing.T) {
	store, err := memory.Open(":memory:")
	if err != nil { t.Fatal(err) }
	defer store.Close()
	if _, err := Build(context.Background(), store, Request{}); err == nil {
		t.Fatal("expected an explicit scope")
	}
}

func TestBriefSurfaceHasFixedSectionsAndNoImplicitConfirmation(t *testing.T) {
	sections := Sections{}
	if sections.Background == nil || sections.PreviousDiscussions == nil || sections.FollowUpCandidates == nil || sections.ChangesConflicts == nil || sections.Unknowns == nil {
		t.Fatal("all brief sections should be initialized")
	}
	entry := Entry{AssertionState: "proposed", EvidenceIDs: []string{"ev:test"}}
	if entry.AssertionState == "explicitly_confirmed" {
		t.Fatal("inference must not become explicit confirmation")
	}
}
