package app

import (
	"context"
	"testing"
	"time"

	"recordingmcp/internal/process"
)

func TestRetryProcessingRejectsSupersededExtractSnapshot(t *testing.T) {
	f := newFixture(t)
	var attachmentID string
	if err := f.store.DB().QueryRow(`SELECT id FROM source_attachments WHERE conversation_id=? LIMIT 1`, f.conversationID).Scan(&attachmentID); err != nil {
		t.Fatal(err)
	}
	id, _, err := process.Enqueue(context.Background(), f.store.DB(), process.Spec{Stage: "extract", InputSnapshotID: attachmentID, IdempotencyKey: "stale-extract", PayloadJSON: `{"conversation_id":"` + f.conversationID + `"}`}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.DB().Exec(`UPDATE processing_jobs SET status='failed' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.DB().Exec(`UPDATE source_attachments SET active=0 WHERE id=?`, attachmentID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.RetryProcessing(context.Background(), f.conversationID, id, 0, "stale-request"); err == nil {
		t.Fatal("retry accepted superseded transcript snapshot")
	}
}
