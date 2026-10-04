package worker

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"recordingmcp/internal/migrations"
	"recordingmcp/internal/process"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", t.TempDir()+"/worker.sqlite")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		t.Fatalf("set busy timeout: %v", err)
	}
	if err := migrations.Apply(context.Background(), db); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	return db
}

func enqueueJob(t *testing.T, db *sql.DB, key string) string {
	t.Helper()
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	id, inserted, err := process.EnqueueTx(ctx, tx, process.Spec{
		Stage:           "extract",
		InputSnapshotID: "snap-" + key,
		IdempotencyKey:  key,
		PayloadJSON:     `{"key":"` + key + `"}`,
		MaxAttempts:     3,
	}, time.Now())
	if err != nil {
		tx.Rollback()
		t.Fatalf("enqueue: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if !inserted {
		t.Fatalf("expected a newly inserted job for key %q", key)
	}
	return id
}

type executorFunc func(ctx context.Context, job process.Job) (string, error)

func (f executorFunc) Execute(ctx context.Context, job process.Job) (string, error) {
	return f(ctx, job)
}

func TestRunOnceClaimsExecutesAndCompletes(t *testing.T) {
	db := openTestDB(t)
	id := enqueueJob(t, db, "complete")

	var got process.Job
	cfg := Config{
		DB:               db,
		WorkerID:         "w1",
		Stages:           []string{"extract"},
		Lease:            5 * time.Second,
		ProcessorVersion: "test-v1",
		Executor: executorFunc(func(ctx context.Context, job process.Job) (string, error) {
			got = job
			return "ref-" + job.ID, nil
		}),
	}
	ran, err := RunOnce(context.Background(), cfg)
	if err != nil || !ran {
		t.Fatalf("RunOnce = (%v, %v), want (true, nil)", ran, err)
	}
	if got.ID != id {
		t.Fatalf("executor saw job %q, want %q", got.ID, id)
	}
	job, err := process.Get(context.Background(), db, id)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.Status != "succeeded" {
		t.Fatalf("status = %q, want %q", job.Status, "succeeded")
	}
	if job.AttemptCount < 1 {
		t.Fatalf("attempt count = %d, want >= 1", job.AttemptCount)
	}
}

func TestRunOnceFailureSchedulesRetry(t *testing.T) {
	db := openTestDB(t)
	id := enqueueJob(t, db, "retry")

	cfg := Config{
		DB:       db,
		WorkerID: "w1",
		Executor: executorFunc(func(ctx context.Context, job process.Job) (string, error) {
			return "", errors.New("boom")
		}),
	}
	ran, err := RunOnce(context.Background(), cfg)
	if err != nil || !ran {
		t.Fatalf("RunOnce = (%v, %v), want (true, nil)", ran, err)
	}
	job, err := process.Get(context.Background(), db, id)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.Status != "retry_wait" {
		t.Fatalf("status = %q, want %q", job.Status, "retry_wait")
	}
	if job.AttemptCount < 1 {
		t.Fatalf("attempt count = %d, want >= 1", job.AttemptCount)
	}
}

func TestRunOnceEmptyQueue(t *testing.T) {
	db := openTestDB(t)
	cfg := Config{
		DB:       db,
		WorkerID: "w1",
		Executor: executorFunc(func(ctx context.Context, job process.Job) (string, error) {
			t.Fatalf("executor must not run on an empty queue")
			return "", nil
		}),
	}
	ran, err := RunOnce(context.Background(), cfg)
	if err != nil || ran {
		t.Fatalf("RunOnce = (%v, %v), want (false, nil)", ran, err)
	}
}

// TestRunOnceLeaseLostSkipsSubmit starts an execution with an already-expired
// lease, preempts it from inside the executor, and verifies the heartbeat
// detects the loss, cancels the execution and never submits a result.
func TestRunOnceLeaseLostSkipsSubmit(t *testing.T) {
	db := openTestDB(t)
	id := enqueueJob(t, db, "lease")

	cancelled := make(chan struct{})
	cfg := Config{
		DB:               db,
		WorkerID:         "old",
		Stages:           []string{"extract"},
		Lease:            time.Nanosecond, // expires immediately; heartbeat is floored
		ProcessorVersion: "old-v1",
		Executor: executorFunc(func(ctx context.Context, job process.Job) (string, error) {
			if job.ID != id {
				t.Errorf("executor saw job %q, want %q", job.ID, id)
			}
			// Let the 1ns lease expire, then preempt it from another worker.
			time.Sleep(2 * time.Millisecond)
			thief, err := process.Claim(context.Background(), db, process.ClaimOptions{
				WorkerID:         "thief",
				Stages:           []string{"extract"},
				Lease:            10 * time.Second,
				ProcessorVersion: "thief-v1",
				Now:              time.Now(),
			})
			if err != nil {
				t.Errorf("thief claim: %v", err)
			} else if thief == nil || thief.ID != id {
				t.Errorf("thief did not preempt job: %+v", thief)
			}
			select {
			case <-ctx.Done():
				close(cancelled)
			case <-time.After(2 * time.Second):
			}
			return "old-ref", nil
		}),
	}
	ran, err := RunOnce(context.Background(), cfg)
	if err != nil || !ran {
		t.Fatalf("RunOnce = (%v, %v), want (true, nil)", ran, err)
	}
	select {
	case <-cancelled:
	default:
		t.Fatal("worker did not cancel execution after losing its lease")
	}
	job, err := process.Get(context.Background(), db, id)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.Status != "running" {
		t.Fatalf("status = %q, want %q (old worker must not submit)", job.Status, "running")
	}
	if job.WorkerID != "thief" {
		t.Fatalf("worker = %q, want %q", job.WorkerID, "thief")
	}
}

func TestRunStopsOnContextCancel(t *testing.T) {
	db := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cfg := Config{
		DB:           db,
		WorkerID:     "w1",
		PollInterval: time.Millisecond,
		Executor:     executorFunc(func(ctx context.Context, job process.Job) (string, error) { return "", nil }),
	}
	if err := Run(ctx, cfg); err != nil {
		t.Fatalf("Run on cancelled ctx = %v, want nil", err)
	}
}
