package pipeline

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"recordingmcp/internal/memory"
	"recordingmcp/internal/process"
)

// A transcript correction must not rewrite human-authored note text or retarget
// its evidence to the replacement turn. The old link remains auditable.
func TestHumanRevisionPreservesEditedNoteAndHistoricalEvidence(t *testing.T) {
	ctx := context.Background()
	store, err := memory.Open(filepath.Join(t.TempDir(), "human-revision.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	v1 := e2eResult("human-seg", "rev-1", "human-turn-v1", "原文预算下周批准", 0)
	report, err := store.IngestTranscript(ctx, v1)
	if err != nil {
		t.Fatal(err)
	}
	var turnID string
	if err := store.DB().QueryRowContext(ctx, `SELECT id FROM turns WHERE conversation_id=?`, report.ConversationID).Scan(&turnID); err != nil {
		t.Fatal(err)
	}
	annotations := memory.Annotations{
		ConversationID: report.ConversationID, AnnotationVersion: "human-v1", Title: "人工修订",
		Topics: []memory.TopicAnnotation{{ID: "human-topic", Title: "预算"}},
		Notes:  []memory.NoteAnnotation{{ID: "human-note", Kind: "observation", TopicIDs: []string{"human-topic"}, Summary: "人工确认：预算在周三批准", Edited: true, EvidenceTurnIDs: []string{turnID}}},
	}
	if err := store.ApplyAnnotations(ctx, report.ConversationID, "", annotations); err != nil {
		t.Fatalf("apply human edit: %v", err)
	}
	v2 := e2eResult("human-seg", "rev-2", "human-turn-v2", "修正稿预算下周三批准", 0)
	v2.Source.SupersedesRevision = "rev-1"
	if _, err := store.IngestTranscript(ctx, v2); err != nil {
		t.Fatalf("ingest correction: %v", err)
	}
	corrected, _, err := store.CurrentAnnotations(ctx, report.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	if len(corrected.Notes) != 1 || corrected.Notes[0].Summary != annotations.Notes[0].Summary || !corrected.Notes[0].Edited {
		t.Fatalf("human note changed during transcript correction: %+v", corrected.Notes)
	}
	if len(corrected.Notes[0].EvidenceTurnIDs) != 1 || corrected.Notes[0].EvidenceTurnIDs[0] != turnID {
		t.Fatalf("correction silently retargeted human evidence: %+v", corrected.Notes[0].EvidenceTurnIDs)
	}
	var currentTurn string
	if err := store.DB().QueryRowContext(ctx, `SELECT t.id FROM turns t JOIN source_attachments a ON a.id IN (SELECT attachment_id FROM attachment_turns WHERE turn_id=t.id) WHERE t.conversation_id=? AND a.active=1`, report.ConversationID).Scan(&currentTurn); err != nil {
		t.Fatal(err)
	}
	if _, err := InsertCandidates(ctx, store.DB(), report.ConversationID, "run-current", "job-current", "test-generator", []ValidatedDraft{{CandidateDraft: CandidateDraft{Kind: "observation", Summary: "当前转写摘要", TopicTitle: "当前主题", EvidenceTurnIDs: []string{currentTurn}}, Status: "validated"}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	payload := `{"conversation_id":"` + report.ConversationID + `"}`
	if _, err := (&PublishHandler{DB: store.DB(), Store: store}).Execute(ctx, process.Job{Stage: "publish", InputSnapshotID: report.ConversationID, PayloadJSON: payload}); err != nil {
		t.Fatalf("reprocess corrected transcript: %v", err)
	}
	current, _, err := store.CurrentAnnotations(ctx, report.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	foundHuman := false
	for _, note := range current.Notes {
		if note.ID == "human-note" {
			foundHuman = note.Summary == annotations.Notes[0].Summary && note.Edited && len(note.EvidenceTurnIDs) == 1 && note.EvidenceTurnIDs[0] == turnID
		}
	}
	if !foundHuman {
		t.Fatalf("reprocessing changed edited note/evidence: %+v", current.Notes)
	}
	foundCurrent := false
	for _, note := range current.Notes {
		if note.Summary == "当前转写摘要" {
			foundCurrent = true
		}
	}
	if !foundCurrent {
		t.Fatalf("reprocess did not publish current-source candidate: %+v", current.Notes)
	}
	ctxResult, err := store.GetEvidenceContext(ctx, turnID, 1, 4000)
	if err != nil {
		t.Fatal(err)
	}
	if !ctxResult.Stale {
		t.Fatalf("superseded human evidence was not reported stale: %+v", ctxResult)
	}
	history, err := store.GetAnnotationHistory(ctx, report.ConversationID)
	if err != nil || len(history) < 1 {
		t.Fatalf("human annotation history missing: %+v %v", history, err)
	}
}
