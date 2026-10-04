// Package process implements the durable processing-job queue used by workers.
//
// Jobs move through the state machine queued -> running -> succeeded; retryable
// failures enter retry_wait until available_at, unrecoverable or exhausted
// failures enter failed. Every state transition is guarded by the current lease
// token so a worker that lost its lease can never publish a result.
package process

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	// ErrNoJob reports that no claimable job matched the request.
	ErrNoJob = errors.New("process: no job available")
	// ErrLeaseLost reports that the lease token no longer owns a running job.
	ErrLeaseLost = errors.New("process: lease lost")
	// ErrNotFound reports that a job id does not exist.
	ErrNotFound = errors.New("process: job not found")
)

// Spec describes a job to enqueue. IdempotencyKey is mandatory and determines
// the resulting job id.
type Spec struct {
	Stage           string
	InputSnapshotID string
	IdempotencyKey  string
	PayloadJSON     string
	Priority        int
	MaxAttempts     int
}

// Job is a snapshot of a processing job row.
type Job struct {
	ID              string
	Stage           string
	InputSnapshotID string
	Status          string
	PayloadJSON     string
	WorkerID        string
	LeaseToken      string
	AttemptCount    int
	MaxAttempts     int
	LeaseExpiresAt  time.Time
}

// ClaimOptions configures a Claim call.
type ClaimOptions struct {
	WorkerID         string
	Stages           []string
	Lease            time.Duration
	ProcessorVersion string
	Now              time.Time
}

const (
	defaultMaxAttempts = 3
	defaultLease       = 30 * time.Second
	defaultListLimit   = 100
	backoffBase        = 5 * time.Second
	backoffCap         = 60 * time.Second
)

func ms(t time.Time) int64 { return t.UnixMilli() }

func fromMS(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.UnixMilli(v)
}

// jobID derives a stable, collision-resistant id from the idempotency key.
func jobID(idempotencyKey string) string {
	sum := sha256.Sum256([]byte(idempotencyKey))
	return "job:" + hex.EncodeToString(sum[:])[:32]
}

// attemptID derives a stable id for one execution attempt of a job.
func attemptID(jobID string, attempt int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s/%d", jobID, attempt)))
	return "attempt:" + hex.EncodeToString(sum[:])[:32]
}

// newLeaseToken returns a fresh random lease token.
func newLeaseToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// RetryBackoff returns 5s*2^(attempt-1), capped at 60s.
func RetryBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := backoffBase
	for i := 1; i < attempt; i++ {
		if d >= backoffCap {
			return backoffCap
		}
		d *= 2
	}
	if d > backoffCap {
		return backoffCap
	}
	return d
}

// EnqueueTx inserts a queued job inside the caller's transaction. It is
// idempotent on IdempotencyKey: when the job already exists it returns the
// existing id with inserted=false.
func EnqueueTx(ctx context.Context, tx *sql.Tx, spec Spec, now time.Time) (jobIDOut string, inserted bool, err error) {
	if strings.TrimSpace(spec.Stage) == "" {
		return "", false, errors.New("process: stage is required")
	}
	if strings.TrimSpace(spec.IdempotencyKey) == "" {
		return "", false, errors.New("process: idempotency key is required")
	}
	payload := spec.PayloadJSON
	if strings.TrimSpace(payload) == "" {
		payload = "{}"
	}
	maxAttempts := spec.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = defaultMaxAttempts
	}
	id := jobID(spec.IdempotencyKey)
	nowMS := ms(now)

	res, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO processing_jobs
		 (id, stage, input_snapshot_id, idempotency_key, status, priority, payload_json,
		  max_attempts, attempt_count, available_at, created_at, updated_at)
		 VALUES (?, ?, ?, ?, 'queued', ?, ?, ?, 0, ?, ?, ?)`,
		id, spec.Stage, spec.InputSnapshotID, spec.IdempotencyKey, spec.Priority, payload,
		maxAttempts, nowMS, nowMS, nowMS)
	if err != nil {
		return "", false, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return "", false, err
	}
	if affected > 0 {
		return id, true, nil
	}

	// The row already existed; return the persisted id for this key.
	var existing string
	if err := tx.QueryRowContext(ctx,
		`SELECT id FROM processing_jobs WHERE idempotency_key = ?`, spec.IdempotencyKey).Scan(&existing); err != nil {
		return "", false, err
	}
	return existing, false, nil
}

// Claim atomically recovers expired leases and claims the next eligible job.
func Claim(ctx context.Context, db *sql.DB, opts ClaimOptions) (*Job, error) {
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	lease := opts.Lease
	if lease <= 0 {
		lease = defaultLease
	}
	nowMS := ms(now)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	// (a)+(b) Expire leases. Record the abandoned running attempt first, then
	// move exhausted jobs to failed and the rest back to retry_wait.
	if _, err := tx.ExecContext(ctx,
		`UPDATE processing_attempts SET outcome = 'lease_lost', finished_at = ?
		 WHERE outcome = 'running' AND job_id IN (
		   SELECT id FROM processing_jobs
		   WHERE status = 'running' AND lease_expires_at IS NOT NULL AND lease_expires_at <= ?)`,
		nowMS, nowMS); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE processing_jobs
		 SET status = 'failed', lease_token = NULL, lease_expires_at = NULL, worker_id = NULL, updated_at = ?
		 WHERE status = 'running' AND lease_expires_at IS NOT NULL AND lease_expires_at <= ?
		   AND attempt_count >= max_attempts`,
		nowMS, nowMS); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE processing_jobs
		 SET status = 'retry_wait', available_at = ?, lease_token = NULL, lease_expires_at = NULL,
		     worker_id = NULL, updated_at = ?
		 WHERE status = 'running' AND lease_expires_at IS NOT NULL AND lease_expires_at <= ?
		   AND attempt_count < max_attempts`,
		nowMS, nowMS, nowMS); err != nil {
		return nil, err
	}

	// (c) Select the next eligible job.
	stageClause := ""
	args := []any{nowMS}
	if len(opts.Stages) > 0 {
		placeholders := make([]string, len(opts.Stages))
		for i, s := range opts.Stages {
			placeholders[i] = "?"
			args = append(args, s)
		}
		stageClause = " AND stage IN (" + strings.Join(placeholders, ",") + ")"
	}

	var (
		id, stage, inputSnapshotID, status, payload string
		workerID, leaseToken                        sql.NullString
		leaseExpiresAt                              sql.NullInt64
		attemptCount, maxAttempts                   int
	)
	row := tx.QueryRowContext(ctx,
		`SELECT id, stage, input_snapshot_id, status, payload_json, attempt_count, max_attempts,
		        worker_id, lease_token, lease_expires_at
		 FROM processing_jobs
		 WHERE status IN ('queued', 'retry_wait') AND available_at <= ?`+stageClause+`
		 ORDER BY priority DESC, available_at ASC, created_at ASC
		 LIMIT 1`, args...)
	if err := row.Scan(&id, &stage, &inputSnapshotID, &status, &payload, &attemptCount, &maxAttempts,
		&workerID, &leaseToken, &leaseExpiresAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Persist the lease recovery performed above before reporting no job.
			if cerr := tx.Commit(); cerr != nil {
				return nil, cerr
			}
			return nil, ErrNoJob
		}
		return nil, err
	}

	// (d) Atomically take ownership.
	token, err := newLeaseToken()
	if err != nil {
		return nil, err
	}
	newAttempt := attemptCount + 1
	expiresAt := nowMS + lease.Milliseconds()
	res, err := tx.ExecContext(ctx,
		`UPDATE processing_jobs
		 SET status = 'running', attempt_count = attempt_count + 1, lease_token = ?,
		     lease_expires_at = ?, worker_id = ?, updated_at = ?
		 WHERE id = ? AND status IN ('queued', 'retry_wait') AND available_at <= ?`,
		token, expiresAt, opts.WorkerID, nowMS, id, nowMS)
	if err != nil {
		return nil, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if affected == 0 {
		if cerr := tx.Commit(); cerr != nil {
			return nil, cerr
		}
		return nil, ErrNoJob
	}

	// (e) Record the running attempt.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO processing_attempts
		 (id, job_id, attempt, worker_id, lease_token, processor_version, started_at, outcome)
		 VALUES (?, ?, ?, ?, ?, ?, ?, 'running')`,
		attemptID(id, newAttempt), id, newAttempt, opts.WorkerID, token, opts.ProcessorVersion, nowMS); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return &Job{
		ID:              id,
		Stage:           stage,
		InputSnapshotID: inputSnapshotID,
		Status:          "running",
		PayloadJSON:     payload,
		WorkerID:        opts.WorkerID,
		LeaseToken:      token,
		AttemptCount:    newAttempt,
		MaxAttempts:     maxAttempts,
		LeaseExpiresAt:  fromMS(expiresAt),
	}, nil
}

// Renew extends the lease of a running job owned by token.
func Renew(ctx context.Context, db *sql.DB, jobID, token string, lease time.Duration, now time.Time) error {
	if lease <= 0 {
		lease = defaultLease
	}
	nowMS := ms(now)
	res, err := db.ExecContext(ctx,
		`UPDATE processing_jobs SET lease_expires_at = ?, updated_at = ?
		 WHERE id = ? AND lease_token = ? AND status = 'running'`,
		nowMS+lease.Milliseconds(), nowMS, jobID, token)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrLeaseLost
	}
	return nil
}

// Complete marks a running job succeeded and records the result reference.
func Complete(ctx context.Context, db *sql.DB, jobID, token, resultRef string, now time.Time) error {
	nowMS := ms(now)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		`UPDATE processing_jobs
		 SET status = 'succeeded', lease_token = NULL, lease_expires_at = NULL, worker_id = NULL, updated_at = ?
		 WHERE id = ? AND lease_token = ? AND status = 'running'`,
		nowMS, jobID, token)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrLeaseLost
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE processing_attempts SET outcome = 'succeeded', result_ref = ?, finished_at = ?
		 WHERE job_id = ? AND outcome = 'running'`,
		resultRef, nowMS, jobID); err != nil {
		return err
	}
	return tx.Commit()
}

// Fail terminates a running attempt. Retryable failures within the attempt
// budget return the job to retry_wait; otherwise the job becomes failed.
func Fail(ctx context.Context, db *sql.DB, jobID, token, errorJSON string, retryable bool, now time.Time) error {
	nowMS := ms(now)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var attemptCount, maxAttempts int
	err = tx.QueryRowContext(ctx,
		`SELECT attempt_count, max_attempts FROM processing_jobs
		 WHERE id = ? AND lease_token = ? AND status = 'running'`,
		jobID, token).Scan(&attemptCount, &maxAttempts)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrLeaseLost
		}
		return err
	}

	outcome := "fatal_error"
	if retryable {
		outcome = "retryable_error"
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE processing_attempts SET outcome = ?, error_json = ?, finished_at = ?
		 WHERE job_id = ? AND outcome = 'running'`,
		outcome, errorJSON, nowMS, jobID); err != nil {
		return err
	}

	if retryable && attemptCount < maxAttempts {
		if _, err := tx.ExecContext(ctx,
			`UPDATE processing_jobs
			 SET status = 'retry_wait', available_at = ?, lease_token = NULL, lease_expires_at = NULL,
			     worker_id = NULL, updated_at = ?
			 WHERE id = ? AND lease_token = ? AND status = 'running'`,
			nowMS+RetryBackoff(attemptCount).Milliseconds(), nowMS, jobID, token); err != nil {
			return err
		}
	} else {
		if _, err := tx.ExecContext(ctx,
			`UPDATE processing_jobs
			 SET status = 'failed', lease_token = NULL, lease_expires_at = NULL, worker_id = NULL, updated_at = ?
			 WHERE id = ? AND lease_token = ? AND status = 'running'`,
			nowMS, jobID, token); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func scanJob(row interface{ Scan(...any) error }) (*Job, error) {
	var (
		j                    Job
		workerID, leaseToken sql.NullString
		leaseExpiresAt       sql.NullInt64
	)
	if err := row.Scan(&j.ID, &j.Stage, &j.InputSnapshotID, &j.Status, &j.PayloadJSON,
		&j.AttemptCount, &j.MaxAttempts, &workerID, &leaseToken, &leaseExpiresAt); err != nil {
		return nil, err
	}
	j.WorkerID = workerID.String
	j.LeaseToken = leaseToken.String
	j.LeaseExpiresAt = fromMS(leaseExpiresAt.Int64)
	return &j, nil
}

const jobColumns = `id, stage, input_snapshot_id, status, payload_json, attempt_count, max_attempts,
	worker_id, lease_token, lease_expires_at`

// Get returns one job by id, or ErrNotFound.
func Get(ctx context.Context, db *sql.DB, jobID string) (*Job, error) {
	row := db.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM processing_jobs WHERE id = ?`, jobID)
	job, err := scanJob(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return job, nil
}

// List returns jobs, optionally filtered by status, for debugging.
func List(ctx context.Context, db *sql.DB, status string, limit int) ([]*Job, error) {
	if limit <= 0 {
		limit = defaultListLimit
	}
	query := `SELECT ` + jobColumns + ` FROM processing_jobs`
	args := []any{}
	if strings.TrimSpace(status) != "" {
		query += ` WHERE status = ?`
		args = append(args, status)
	}
	query += ` ORDER BY created_at ASC, id ASC LIMIT ?`
	args = append(args, limit)

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []*Job
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return jobs, nil
}
