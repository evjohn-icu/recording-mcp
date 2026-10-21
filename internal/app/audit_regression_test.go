package app

import (
	"context"
	"testing"

	"recordingmcp/internal/memory"
)

// Editing a sibling note must not revive a stale note merely because the
// current annotation revision is replaced. The source attachment remains
// active; staleness is an explicit artifact-level decision.
func TestEditNoteDoesNotReviveExplicitlyStaleSibling(t *testing.T) {
	f := newFixture(t)
	var activeAttachments int
	var jobs, embeddings int
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM source_attachments WHERE conversation_id=? AND active=1`, f.conversationID).Scan(&activeAttachments); err != nil {
		t.Fatal(err)
	}
	if activeAttachments == 0 {
		t.Fatal("fixture must retain an active source attachment")
	}
	staleRowID := memory.NoteRowID(f.conversationID, "note:one")
	if _, err := f.store.DB().Exec(
		`UPDATE derived_artifacts SET stale=1, stale_reason='explicit' WHERE kind='note' AND artifact_id=?`,
		staleRowID,
	); err != nil {
		t.Fatal(err)
	}

	rowID := memory.NoteRowID(f.conversationID, "note:two")
	if _, err := f.svc.EditNote(context.Background(), f.conversationID, rowID, "v1", "web-1", "更新后的第二条归纳"); err != nil {
		t.Fatalf("EditNote: %v", err)
	}

	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM processing_jobs WHERE stage='embed' AND idempotency_key=?`, "embed|"+f.conversationID+"|web-1").Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if jobs != 1 {
		t.Fatalf("annotation edit embed jobs = %d, want 1", jobs)
	}
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM memory_embeddings WHERE kind='note' AND conversation_id=?`, f.conversationID).Scan(&embeddings); err != nil {
		t.Fatal(err)
	}
	if embeddings != 0 {
		t.Fatalf("note embeddings after annotation replacement = %d, want invalidated", embeddings)
	}
	view, err := f.svc.ConversationView(context.Background(), f.conversationID)
	if err != nil {
		t.Fatal(err)
	}
	if view.AnnotationVersion != "web-1" {
		t.Fatalf("annotation version = %q, want web-1", view.AnnotationVersion)
	}
	for _, note := range view.Notes {
		if note.NoteID == "note:one" {
			t.Fatalf("explicitly stale note was revived by sibling edit: %+v", note)
		}
	}
	if len(view.Notes) != 1 || view.Notes[0].NoteID != "note:two" || view.Notes[0].Summary != "更新后的第二条归纳" {
		t.Fatalf("unexpected current notes after edit: %+v", view.Notes)
	}
}
