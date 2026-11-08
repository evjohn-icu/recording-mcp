package app

import (
	"context"
	"fmt"
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

func TestProcessingSnapshotProjectsPhases(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	// The fixture itself queued extract/embed jobs; this test projects only the
	// two stages it drives.
	if _, err := f.store.DB().Exec(`DELETE FROM processing_jobs`); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	enqueue := func(stage, key string) string {
		id, _, err := process.Enqueue(ctx, f.store.DB(), process.Spec{
			Stage: stage, InputSnapshotID: f.conversationID, IdempotencyKey: key,
			PayloadJSON: `{"conversation_id":"` + f.conversationID + `"}`, MaxAttempts: 3,
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	ingestID := enqueue("ingest", "phase-ingest")
	job, err := process.Claim(ctx, f.store.DB(), process.ClaimOptions{WorkerID: "w", Stages: []string{"ingest"}, Lease: time.Minute, Now: now})
	if err != nil || job == nil || job.ID != ingestID {
		t.Fatalf("claim ingest: %+v %v", job, err)
	}
	if err := process.Complete(ctx, f.store.DB(), job.ID, job.LeaseToken, "ingested", now); err != nil {
		t.Fatal(err)
	}

	asrID := enqueue("asr", "phase-asr")
	job, err = process.Claim(ctx, f.store.DB(), process.ClaimOptions{WorkerID: "w", Stages: []string{"asr"}, Lease: time.Minute, Now: now.Add(time.Second)})
	if err != nil || job == nil || job.ID != asrID {
		t.Fatalf("claim asr: %+v %v", job, err)
	}
	if err := process.Fail(ctx, f.store.DB(), job.ID, job.LeaseToken,
		`{"error":"asr backend unavailable","code":"asr_failed","action":"check credentials"}`, false, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}

	snap, err := f.svc.ProcessingSnapshot(ctx, f.conversationID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Phases) != 5 {
		t.Fatalf("phases=%d", len(snap.Phases))
	}
	wantNames := []string{"ingest", "asr", "extract", "publish", "index"}
	wantStates := []string{"done", "failed", "skipped", "skipped", "skipped"}
	for i, phase := range snap.Phases {
		if phase.Name != wantNames[i] || phase.State != wantStates[i] {
			t.Fatalf("phase %d = %+v, want %s/%s", i, phase, wantNames[i], wantStates[i])
		}
	}
	if snap.CurrentStage != "asr" {
		t.Fatalf("current_stage=%q", snap.CurrentStage)
	}
	asr := snap.Phases[1]
	if asr.Error == "" || asr.ErrorCode != "asr_failed" || asr.ErrorAction != "check credentials" {
		t.Fatalf("asr error projection: %+v", asr)
	}
	if asr.JobID != asrID || asr.Attempt != 1 {
		t.Fatalf("asr job projection: %+v", asr)
	}
}

func TestProcessingPhasesSurviveMoreThanListLimitJobs(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.store.DB().Exec(`DELETE FROM processing_jobs`); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for i := 0; i < 120; i++ {
		if _, _, err := process.Enqueue(ctx, f.store.DB(), process.Spec{
			Stage: "embed", InputSnapshotID: f.conversationID,
			IdempotencyKey: fmt.Sprintf("bulk-embed-%03d", i), PayloadJSON: `{}`,
		}, now.Add(time.Duration(i)*time.Millisecond)); err != nil {
			t.Fatal(err)
		}
	}
	for _, stage := range []string{"ingest", "asr", "extract"} {
		if _, _, err := process.Enqueue(ctx, f.store.DB(), process.Spec{
			Stage: stage, InputSnapshotID: f.conversationID,
			IdempotencyKey: "phase-" + stage, PayloadJSON: `{"conversation_id":"` + f.conversationID + `"}`,
		}, now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.store.DB().Exec(`UPDATE processing_jobs SET status='succeeded'`); err != nil {
		t.Fatal(err)
	}
	snapshot, err := f.svc.ProcessingSnapshot(ctx, f.conversationID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.CurrentStage != "completed" {
		t.Fatalf("current_stage=%q with %d jobs", snapshot.CurrentStage, len(snapshot.Jobs))
	}
	for _, phase := range snapshot.Phases {
		if phase.Name == "ingest" || phase.Name == "asr" {
			if phase.State != "done" {
				t.Fatalf("phase %s=%s, want done", phase.Name, phase.State)
			}
		}
	}
}
