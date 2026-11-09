package process

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

var (
	ErrRetryConflict = errors.New("process: retry state conflict")
	ErrStaleSource   = errors.New("process: source snapshot is stale")
)

type Heartbeat struct {
	WorkerID         string    `json:"worker_id"`
	PID              int       `json:"pid"`
	Stages           []string  `json:"stages"`
	GeneratorEnabled bool      `json:"generator_enabled"`
	ASRProvider      string    `json:"asr_provider,omitempty"`
	ASRAvailable     bool      `json:"asr_available"`
	EmbedProvider    string    `json:"embed_provider,omitempty"`
	LastSeenAt       time.Time `json:"last_seen_at"`
	Alive            bool      `json:"alive"`
}

func WriteHeartbeat(ctx context.Context, db *sql.DB, h Heartbeat, now time.Time) error {
	stages, err := json.Marshal(h.Stages)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `INSERT INTO worker_heartbeats(worker_id,pid,stages_json,generator_enabled,asr_provider,asr_available,embed_provider,last_seen_at) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(worker_id) DO UPDATE SET pid=excluded.pid,stages_json=excluded.stages_json,generator_enabled=excluded.generator_enabled,asr_provider=excluded.asr_provider,asr_available=excluded.asr_available,embed_provider=excluded.embed_provider,last_seen_at=excluded.last_seen_at`, h.WorkerID, h.PID, string(stages), boolInt(h.GeneratorEnabled), h.ASRProvider, boolInt(h.ASRAvailable), h.EmbedProvider, now.UnixMilli())
	return err
}

func RemoveHeartbeat(ctx context.Context, db *sql.DB, workerID string) error {
	_, err := db.ExecContext(ctx, `DELETE FROM worker_heartbeats WHERE worker_id=?`, workerID)
	return err
}

func ReadHeartbeats(ctx context.Context, db *sql.DB, now time.Time, staleAfter time.Duration) ([]Heartbeat, error) {
	rows, err := db.QueryContext(ctx, `SELECT worker_id,pid,stages_json,generator_enabled,asr_provider,asr_available,embed_provider,last_seen_at FROM worker_heartbeats ORDER BY worker_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Heartbeat
	for rows.Next() {
		var h Heartbeat
		var raw string
		var enabled, asrAvailable int
		var stamp int64
		if err := rows.Scan(&h.WorkerID, &h.PID, &raw, &enabled, &h.ASRProvider, &asrAvailable, &h.EmbedProvider, &stamp); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(raw), &h.Stages)
		h.GeneratorEnabled = enabled != 0
		h.ASRAvailable = asrAvailable != 0
		h.LastSeenAt = fromMS(stamp)
		h.Alive = now.Sub(h.LastSeenAt) <= staleAfter
		out = append(out, h)
	}
	return out, rows.Err()
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

// RetryFailed requeues a failed or retry-wait job exactly once per requestID.
// expectedAttempt prevents a stale UI from retrying after another attempt ran.
func RetryFailed(ctx context.Context, db *sql.DB, id string, expectedAttempt int, requestID string, now time.Time) (*Job, error) {
	return retryFailed(ctx, db, id, expectedAttempt, requestID, "", now)
}

// RetryFailedForConversation performs its current-source check in the same
// SQLite transaction that requeues the job.
func RetryFailedForConversation(ctx context.Context, db *sql.DB, conversationID, id string, expectedAttempt int, requestID string, now time.Time) (*Job, error) {
	return retryFailed(ctx, db, id, expectedAttempt, requestID, conversationID, now)
}

func retryFailed(ctx context.Context, db *sql.DB, id string, expectedAttempt int, requestID, conversationID string, now time.Time) (*Job, error) {
	if strings.TrimSpace(requestID) == "" {
		return nil, errors.New("process: request_id is required")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var prior string
	var priorAttempt int
	err = tx.QueryRowContext(ctx, `SELECT job_id,expected_attempt FROM processing_retry_requests WHERE request_id=?`, requestID).Scan(&prior, &priorAttempt)
	if err == nil {
		if prior != id || priorAttempt != expectedAttempt {
			return nil, ErrRetryConflict
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return Get(ctx, db, id)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	var attempt int
	var status, snapshot, stage, payload string
	if err = tx.QueryRowContext(ctx, `SELECT attempt_count,status,input_snapshot_id,stage,payload_json FROM processing_jobs WHERE id=?`, id).Scan(&attempt, &status, &snapshot, &stage, &payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if attempt != expectedAttempt {
		return nil, ErrRetryConflict
	}
	if status != "failed" && status != "retry_wait" {
		return nil, ErrRetryConflict
	}
	if strings.TrimSpace(snapshot) == "" {
		return nil, errors.New("process: job has no source snapshot")
	}
	if conversationID != "" {
		var present int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversations WHERE id=?`, conversationID).Scan(&present); err != nil {
			return nil, err
		}
		if present != 1 {
			return nil, ErrStaleSource
		}
		if stage == "extract" {
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM source_attachments WHERE id=? AND conversation_id=? AND active=1`, snapshot, conversationID).Scan(&present); err != nil {
				return nil, err
			}
			if present != 1 {
				return nil, ErrStaleSource
			}
		} else {
			var p struct {
				ConversationID string `json:"conversation_id"`
			}
			if json.Unmarshal([]byte(payload), &p) != nil || snapshot != conversationID || p.ConversationID != conversationID {
				return nil, ErrStaleSource
			}
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO processing_retry_requests(request_id,job_id,expected_attempt,created_at) VALUES(?,?,?,?)`, requestID, id, expectedAttempt, ms(now)); err != nil {
		return nil, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE processing_jobs SET status='queued',available_at=?,max_attempts=MAX(max_attempts,attempt_count+1),updated_at=? WHERE id=? AND attempt_count=? AND status IN ('failed','retry_wait')`, ms(now), ms(now), id, expectedAttempt)
	if err != nil {
		return nil, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if affected != 1 {
		return nil, ErrRetryConflict
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return Get(ctx, db, id)
}
