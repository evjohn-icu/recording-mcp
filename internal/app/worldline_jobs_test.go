package app

import (
	"context"
	"encoding/json"
	"testing"

	"recordingmcp/internal/plane"
	"recordingmcp/internal/process"
)

func TestWorldlineAnalysisEnqueueIsDurableIdempotentAndScoped(t *testing.T) {
	f := newFixture(t)
	events, err := plane.Open(t.TempDir() + "/events.db")
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	svc := &WorldlineService{Time: &TimeService{Store: f.store, Events: events}}
	first, err := svc.EnqueueAnalysis(context.Background(), "profile-jobs", "request-1")
	if err != nil {
		t.Fatal(err)
	}
	again, err := svc.EnqueueAnalysis(context.Background(), "profile-jobs", "request-1")
	if err != nil {
		t.Fatal(err)
	}
	if first.JobID != again.JobID || first.Status != "queued" {
		t.Fatalf("enqueue not durable/idempotent: first=%+v again=%+v", first, again)
	}
	original, err := process.Get(context.Background(), f.store.DB(), first.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.DB().Exec(`UPDATE notes SET summary=summary||' edited',annotation_version='v2'`); err != nil {
		t.Fatal(err)
	}
	replayed, err := svc.EnqueueAnalysis(context.Background(), "profile-jobs", "request-1")
	if err != nil {
		t.Fatal(err)
	}
	if replayed.JobID != first.JobID {
		t.Fatalf("same request id created a new snapshot job: %+v", replayed)
	}
	afterEdit, err := process.Get(context.Background(), f.store.DB(), first.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if afterEdit.PayloadJSON != original.PayloadJSON {
		t.Fatal("idempotent request replaced its frozen source snapshot after edit")
	}
	var payload struct {
		Snapshot      string              `json:"source_snapshot_version"`
		Pairs         []semanticPair      `json:"pairs"`
		References    []EvidenceReference `json:"references"`
		Notes         map[string]string   `json:"source_note_versions"`
		Conversations []string            `json:"source_conversation_ids"`
	}
	job, err := process.Get(context.Background(), f.store.DB(), first.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(job.PayloadJSON), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Snapshot == "" || payload.Pairs == nil || payload.References == nil || payload.Notes == nil || payload.Conversations == nil {
		t.Fatalf("worldline queue snapshot is incomplete: %+v", payload)
	}
	if _, err := WorldlineJobStatus(context.Background(), f.store.DB(), "another-profile", first.JobID); err == nil {
		t.Fatal("job status leaked across profile scopes")
	}
	status, err := WorldlineJobStatus(context.Background(), f.store.DB(), "profile-jobs", first.JobID)
	if err != nil || status.Status != "queued" {
		t.Fatalf("status=%+v err=%v", status, err)
	}
}
