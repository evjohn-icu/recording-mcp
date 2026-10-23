package process

import (
	"context"
	"fmt"
	"testing"
	"time"

	"recordingmcp/internal/migrations"
)

func TestRetryFailedIsRequestIdempotentAndAttemptGuarded(t *testing.T) {
	db := newDB(t)
	if _, err := db.Exec(migrations.ProcessingRuntimeDDL); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	id, _, err := Enqueue(context.Background(), db, Spec{Stage: "extract", InputSnapshotID: "conv", IdempotencyKey: "retry-me", MaxAttempts: 3}, now)
	if err != nil {
		t.Fatal(err)
	}
	j, err := Claim(context.Background(), db, ClaimOptions{WorkerID: "w", Lease: time.Minute, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if err := Fail(context.Background(), db, j.ID, j.LeaseToken, "{}", false, now); err != nil {
		t.Fatal(err)
	}
	retried, err := RetryFailed(context.Background(), db, id, 1, "req-1", now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if retried.Status != "queued" {
		t.Fatalf("status=%s", retried.Status)
	}
	replay, err := RetryFailed(context.Background(), db, id, 1, "req-1", now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if replay.ID != id || replay.Status != "queued" {
		t.Fatalf("replay=%+v", replay)
	}
	if _, err := RetryFailed(context.Background(), db, id, 0, "req-stale", now); err == nil {
		t.Fatal("stale expected attempt accepted")
	}
	if _, err := RetryFailed(context.Background(), db, id, 0, "req-1", now); err == nil {
		t.Fatal("request id reuse with different expected attempt accepted")
	}
}

func TestHeartbeatReportsGeneratorAndStaleness(t *testing.T) {
	db := newDB(t)
	if _, err := db.Exec(migrations.ProcessingRuntimeDDL); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := WriteHeartbeat(context.Background(), db, Heartbeat{WorkerID: "w", PID: 123, Stages: []string{"extract"}, GeneratorEnabled: true}, now); err != nil {
		t.Fatal(err)
	}
	got, err := ReadHeartbeats(context.Background(), db, now.Add(time.Second), 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].Alive || !got[0].GeneratorEnabled {
		t.Fatalf("heartbeat=%+v", got)
	}
	got, err = ReadHeartbeats(context.Background(), db, now.Add(time.Minute), 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Alive {
		t.Fatal("stale worker reported alive")
	}
}

func TestListForConversationFiltersBeforeLimit(t *testing.T) {
	db := newDB(t)
	now := time.Now()
	for i := 0; i < 105; i++ {
		if _, _, err := Enqueue(context.Background(), db, Spec{Stage: "extract", InputSnapshotID: "other", IdempotencyKey: fmt.Sprintf("other-%03d", i), PayloadJSON: `{"conversation_id":"other"}`}, now.Add(time.Duration(i)*time.Millisecond)); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := Enqueue(context.Background(), db, Spec{Stage: "extract", InputSnapshotID: "attachment", IdempotencyKey: "target", PayloadJSON: `{"conversation_id":"wanted"}`}, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	jobs, err := ListForConversation(context.Background(), db, "wanted", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].InputSnapshotID != "attachment" {
		t.Fatalf("target job omitted: %+v", jobs)
	}
}
