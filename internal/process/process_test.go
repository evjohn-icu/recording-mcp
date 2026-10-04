package process

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"recordingmcp/internal/migrations"
)

func newDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "process.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := migrations.Apply(context.Background(), db); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	return db
}

func enqueue(t *testing.T, db *sql.DB, spec Spec, now time.Time) (string, bool) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	id, inserted, err := EnqueueTx(context.Background(), tx, spec, now)
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("enqueue: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return id, inserted
}

func countJobs(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM processing_jobs`).Scan(&n); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	return n
}

func attemptOutcome(t *testing.T, db *sql.DB, jobID string, attempt int) string {
	t.Helper()
	var outcome string
	if err := db.QueryRow(`SELECT outcome FROM processing_attempts WHERE job_id = ? AND attempt = ?`,
		jobID, attempt).Scan(&outcome); err != nil {
		t.Fatalf("attempt outcome: %v", err)
	}
	return outcome
}

func TestEnqueueIdempotent(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	now := time.Now()

	spec := Spec{
		Stage:           "extract",
		InputSnapshotID: "att-1",
		IdempotencyKey:  "key-1",
		PayloadJSON:     `{"a":1}`,
		Priority:        5,
		MaxAttempts:     3,
	}
	id1, inserted1 := enqueue(t, db, spec, now)
	if !inserted1 {
		t.Fatalf("first enqueue should insert")
	}
	if id1 != jobID("key-1") {
		t.Fatalf("unexpected derived id %q", id1)
	}

	id2, inserted2 := enqueue(t, db, spec, now.Add(time.Second))
	if inserted2 {
		t.Fatalf("second enqueue should be a no-op")
	}
	if id2 != id1 {
		t.Fatalf("idempotent enqueue returned %q, want %q", id2, id1)
	}
	if got := countJobs(t, db); got != 1 {
		t.Fatalf("job count = %d, want 1", got)
	}

	job, err := Get(ctx, db, id1)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if job.Status != "queued" || job.AttemptCount != 0 || job.MaxAttempts != 3 {
		t.Fatalf("unexpected job %+v", job)
	}
}

func TestEnqueueDefaultsAndValidation(t *testing.T) {
	db := newDB(t)
	now := time.Now()

	// Empty payload and non-positive MaxAttempts fall back to defaults.
	id, _ := enqueue(t, db, Spec{Stage: "extract", IdempotencyKey: "k", MaxAttempts: 0}, now)
	job, err := Get(context.Background(), db, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if job.PayloadJSON != "{}" || job.MaxAttempts != 3 {
		t.Fatalf("defaults not applied: %+v", job)
	}

	for _, spec := range []Spec{
		{Stage: "", IdempotencyKey: "k"},
		{Stage: "extract", IdempotencyKey: ""},
	} {
		tx, err := db.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, _, err := EnqueueTx(context.Background(), tx, spec, now); err == nil {
			t.Fatalf("expected validation error for %+v", spec)
		}
		_ = tx.Rollback()
	}
}

func TestClaimEmptyQueue(t *testing.T) {
	db := newDB(t)
	_, err := Claim(context.Background(), db, ClaimOptions{WorkerID: "w1", Stages: []string{"extract"}})
	if !errors.Is(err, ErrNoJob) {
		t.Fatalf("err = %v, want ErrNoJob", err)
	}
}

func TestClaimComplete(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	now := time.Now()
	id, _ := enqueue(t, db, Spec{Stage: "extract", IdempotencyKey: "k", InputSnapshotID: "att"}, now)

	job, err := Claim(ctx, db, ClaimOptions{
		WorkerID: "w1", Stages: []string{"extract"}, Lease: 30 * time.Second,
		ProcessorVersion: "pv1", Now: now,
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if job.ID != id || job.Status != "running" || job.AttemptCount != 1 || job.LeaseToken == "" {
		t.Fatalf("unexpected claimed job %+v", job)
	}
	if got := attemptOutcome(t, db, id, 1); got != "running" {
		t.Fatalf("attempt outcome = %q, want running", got)
	}

	if err := Complete(ctx, db, job.ID, job.LeaseToken, "result:1", now.Add(time.Second)); err != nil {
		t.Fatalf("complete: %v", err)
	}
	stored, err := Get(ctx, db, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if stored.Status != "succeeded" || stored.LeaseToken != "" {
		t.Fatalf("unexpected job after complete: %+v", stored)
	}
	if got := attemptOutcome(t, db, id, 1); got != "succeeded" {
		t.Fatalf("attempt outcome = %q, want succeeded", got)
	}
	var ref string
	if err := db.QueryRow(`SELECT result_ref FROM processing_attempts WHERE job_id = ? AND attempt = 1`, id).Scan(&ref); err != nil {
		t.Fatalf("result ref: %v", err)
	}
	if ref != "result:1" {
		t.Fatalf("result_ref = %q, want result:1", ref)
	}

	// A completed job is no longer claimable.
	if _, err := Claim(ctx, db, ClaimOptions{WorkerID: "w1", Stages: []string{"extract"}, Now: now}); !errors.Is(err, ErrNoJob) {
		t.Fatalf("claim after complete err = %v, want ErrNoJob", err)
	}
}

func TestRetryableFailureBackoff(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	now := time.Now()
	id, _ := enqueue(t, db, Spec{Stage: "extract", IdempotencyKey: "k", MaxAttempts: 3}, now)

	job, err := Claim(ctx, db, ClaimOptions{WorkerID: "w1", Stages: []string{"extract"}, Now: now})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := Fail(ctx, db, job.ID, job.LeaseToken, `{"error":"boom"}`, true, now); err != nil {
		t.Fatalf("fail: %v", err)
	}

	stored, err := Get(ctx, db, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if stored.Status != "retry_wait" {
		t.Fatalf("status = %q, want retry_wait", stored.Status)
	}
	if got := attemptOutcome(t, db, id, 1); got != "retryable_error" {
		t.Fatalf("attempt outcome = %q, want retryable_error", got)
	}

	// Before available_at the job is not claimable.
	if _, err := Claim(ctx, db, ClaimOptions{WorkerID: "w1", Stages: []string{"extract"}, Now: now}); !errors.Is(err, ErrNoJob) {
		t.Fatalf("early claim err = %v, want ErrNoJob", err)
	}
	// RetryBackoff(1) = 5s; once elapsed the job is claimable again.
	later := now.Add(RetryBackoff(1) + time.Millisecond)
	job2, err := Claim(ctx, db, ClaimOptions{WorkerID: "w1", Stages: []string{"extract"}, Now: later})
	if err != nil {
		t.Fatalf("late claim: %v", err)
	}
	if job2.ID != id || job2.AttemptCount != 2 {
		t.Fatalf("unexpected retried job %+v", job2)
	}
}

func TestFailureExhaustedBecomesFailed(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	now := time.Now()
	id, _ := enqueue(t, db, Spec{Stage: "extract", IdempotencyKey: "k", MaxAttempts: 1}, now)

	job, err := Claim(ctx, db, ClaimOptions{WorkerID: "w1", Stages: []string{"extract"}, Now: now})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := Fail(ctx, db, job.ID, job.LeaseToken, `{"error":"boom"}`, true, now); err != nil {
		t.Fatalf("fail: %v", err)
	}
	stored, err := Get(ctx, db, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if stored.Status != "failed" {
		t.Fatalf("status = %q, want failed", stored.Status)
	}
}

func TestFatalFailureBecomesFailed(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	now := time.Now()
	id, _ := enqueue(t, db, Spec{Stage: "extract", IdempotencyKey: "k", MaxAttempts: 5}, now)

	job, err := Claim(ctx, db, ClaimOptions{WorkerID: "w1", Stages: []string{"extract"}, Now: now})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := Fail(ctx, db, job.ID, job.LeaseToken, `{"error":"fatal"}`, false, now); err != nil {
		t.Fatalf("fail: %v", err)
	}
	stored, err := Get(ctx, db, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if stored.Status != "failed" {
		t.Fatalf("status = %q, want failed", stored.Status)
	}
	if got := attemptOutcome(t, db, id, 1); got != "fatal_error" {
		t.Fatalf("attempt outcome = %q, want fatal_error", got)
	}
}

func TestLeasePreemption(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	t0 := time.Now()
	id, _ := enqueue(t, db, Spec{Stage: "extract", IdempotencyKey: "k", MaxAttempts: 3}, t0)

	first, err := Claim(ctx, db, ClaimOptions{WorkerID: "w1", Stages: []string{"extract"}, Lease: time.Second, Now: t0})
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if first.AttemptCount != 1 {
		t.Fatalf("attempt = %d, want 1", first.AttemptCount)
	}

	// After the short lease expires another worker takes over.
	takeover := t0.Add(2 * time.Second)
	second, err := Claim(ctx, db, ClaimOptions{WorkerID: "w2", Stages: []string{"extract"}, Lease: 30 * time.Second, Now: takeover})
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if second.ID != id || second.AttemptCount != 2 {
		t.Fatalf("unexpected takeover job %+v", second)
	}
	if second.LeaseToken == first.LeaseToken {
		t.Fatalf("lease token was not rotated")
	}
	if got := attemptOutcome(t, db, id, 1); got != "lease_lost" {
		t.Fatalf("abandoned attempt outcome = %q, want lease_lost", got)
	}

	// The old worker can no longer act.
	if err := Renew(ctx, db, id, first.LeaseToken, 30*time.Second, takeover); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("renew old lease err = %v, want ErrLeaseLost", err)
	}
	if err := Complete(ctx, db, id, first.LeaseToken, "stale", takeover); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("complete old lease err = %v, want ErrLeaseLost", err)
	}
	if err := Fail(ctx, db, id, first.LeaseToken, "{}", true, takeover); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("fail old lease err = %v, want ErrLeaseLost", err)
	}
	if got := attemptOutcome(t, db, id, 2); got != "running" {
		t.Fatalf("new attempt outcome = %q, want running", got)
	}
}

func TestExpiredLeaseExhaustedBecomesFailed(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	t0 := time.Now()
	id, _ := enqueue(t, db, Spec{Stage: "extract", IdempotencyKey: "k", MaxAttempts: 1}, t0)

	if _, err := Claim(ctx, db, ClaimOptions{WorkerID: "w1", Stages: []string{"extract"}, Lease: time.Second, Now: t0}); err != nil {
		t.Fatalf("claim: %v", err)
	}

	// Lease expires with the attempt budget already exhausted: no new claim.
	_, err := Claim(ctx, db, ClaimOptions{WorkerID: "w2", Stages: []string{"extract"}, Now: t0.Add(2 * time.Second)})
	if !errors.Is(err, ErrNoJob) {
		t.Fatalf("err = %v, want ErrNoJob", err)
	}
	stored, err := Get(ctx, db, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if stored.Status != "failed" {
		t.Fatalf("status = %q, want failed", stored.Status)
	}
	if got := attemptOutcome(t, db, id, 1); got != "lease_lost" {
		t.Fatalf("attempt outcome = %q, want lease_lost", got)
	}
}

func TestRenewExtendsLease(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	now := time.Now()
	id, _ := enqueue(t, db, Spec{Stage: "extract", IdempotencyKey: "k"}, now)

	job, err := Claim(ctx, db, ClaimOptions{WorkerID: "w1", Stages: []string{"extract"}, Lease: 10 * time.Second, Now: now})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := Renew(ctx, db, id, job.LeaseToken, 60*time.Second, now); err != nil {
		t.Fatalf("renew: %v", err)
	}
	stored, err := Get(ctx, db, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got := stored.LeaseExpiresAt.Sub(now); got < 59*time.Second || got > 61*time.Second {
		t.Fatalf("lease expiry delta = %v, want ~60s", got)
	}
}

func TestClaimStageFilter(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	now := time.Now()
	enqueue(t, db, Spec{Stage: "extract", IdempotencyKey: "k1"}, now)
	enqueue(t, db, Spec{Stage: "decision", IdempotencyKey: "k2"}, now)

	job, err := Claim(ctx, db, ClaimOptions{WorkerID: "w1", Stages: []string{"decision"}, Now: now})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if job.Stage != "decision" {
		t.Fatalf("stage = %q, want decision", job.Stage)
	}
}

func TestGetNotFound(t *testing.T) {
	db := newDB(t)
	if _, err := Get(context.Background(), db, "job:missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestList(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	now := time.Now()
	enqueue(t, db, Spec{Stage: "extract", IdempotencyKey: "k1"}, now)
	enqueue(t, db, Spec{Stage: "extract", IdempotencyKey: "k2"}, now)
	id3, _ := enqueue(t, db, Spec{Stage: "extract", IdempotencyKey: "k3"}, now)

	job, err := Claim(ctx, db, ClaimOptions{WorkerID: "w1", Stages: []string{"extract"}, Now: now})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := Complete(ctx, db, job.ID, job.LeaseToken, "ref", now); err != nil {
		t.Fatalf("complete: %v", err)
	}

	queued, err := List(ctx, db, "queued", 0)
	if err != nil {
		t.Fatalf("list queued: %v", err)
	}
	if len(queued) != 2 {
		t.Fatalf("queued count = %d, want 2", len(queued))
	}
	all, err := List(ctx, db, "", 0)
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("all count = %d, want 3", len(all))
	}
	_ = id3
}

func TestRetryBackoff(t *testing.T) {
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{0, 5 * time.Second},
		{1, 5 * time.Second},
		{2, 10 * time.Second},
		{3, 20 * time.Second},
		{4, 40 * time.Second},
		{5, 60 * time.Second},
		{6, 60 * time.Second},
	}
	for _, c := range cases {
		if got := RetryBackoff(c.attempt); got != c.want {
			t.Errorf("RetryBackoff(%d) = %v, want %v", c.attempt, got, c.want)
		}
	}
}
