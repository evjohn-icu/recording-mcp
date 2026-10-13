package cloudsync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// syncRecordings is the per-recording download/ingestion state row.
type syncRecording struct {
	RecordingID        string
	Owner              string
	DeviceID           string
	State              string
	TotalChunks        *int64
	TotalSamples       *int64
	StartedAt          *string
	StitchedPath       *string
	TranscriptRevision string
	TranscriptionState string
	LastError          *string
	AckedAt            *int64
	UpdatedAt          int64
}

// Local chunk states mirror the cloud event stream; they exist so the engine
// can decide download idempotency purely from the local database.
const (
	stateDownloading          = "downloading"
	stateDownloaded           = "downloaded"
	stateTranscriptionPending = "transcription_pending"
	stateIngested             = "ingested"
	stateDeleted              = "deleted"
	stateFailed               = "failed"
)

// cursor reads the persisted cursor; a fresh database without a sync_state
// row reads as 0.
func cursor(ctx context.Context, db *sql.DB) (int64, error) {
	var cur int64
	err := db.QueryRowContext(ctx, `SELECT cursor FROM sync_state WHERE id=1`).Scan(&cur)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("cloudsync: read cursor: %w", err)
	}
	return cur, nil
}

// ensureStateTx inserts the single sync_state row if absent, inside tx.
func ensureStateTx(ctx context.Context, tx *sql.Tx, now int64) error {
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO sync_state(id,cursor,updated_at) VALUES(1,0,?)`, now)
	if err != nil {
		return fmt.Errorf("cloudsync: ensure sync_state: %w", err)
	}
	return nil
}

// advanceCursorTx persists the cursor for the event just processed. Callers
// must invoke it in the same transaction as the event's local effects and its
// sync_events row, so partial persistence is impossible.
func advanceCursorTx(ctx context.Context, tx *sql.Tx, eventID, now int64) error {
	if err := ensureStateTx(ctx, tx, now); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE sync_state SET cursor=MAX(cursor,?), last_success_at=?, updated_at=? WHERE id=1`, eventID, now, now)
	if err != nil {
		return fmt.Errorf("cloudsync: advance cursor to %d: %w", eventID, err)
	}
	return nil
}

// setLastError records a diagnostic in sync_state without advancing anything.
func setLastError(ctx context.Context, db *sql.DB, msg string, now int64) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := ensureStateTx(ctx, tx, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sync_state SET last_error=?, updated_at=? WHERE id=1`, msg, now); err != nil {
		return fmt.Errorf("cloudsync: set last_error: %w", err)
	}
	return tx.Commit()
}

// recordEventTx marks one event processed. INSERT OR IGNORE keeps the
// ledger append-only and idempotent under event replay.
func recordEventTx(ctx context.Context, tx *sql.Tx, ev Event, now int64) error {
	_, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO sync_events(event_id,type,recording_id,seq,payload_json,processed_at) VALUES(?,?,?,?,?,?)`,
		ev.ID, ev.Type, ev.RecordingID, ev.Seq, string(ev.Payload), now)
	if err != nil {
		return fmt.Errorf("cloudsync: record event %d: %w", ev.ID, err)
	}
	return nil
}

// eventProcessed reports whether an event id was already handled, which is
// how replayed pages (rewound cursor) skip without redoing work.
func eventProcessed(ctx context.Context, db *sql.DB, eventID int64) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sync_events WHERE event_id=?`, eventID).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("cloudsync: probe event %d: %w", eventID, err)
	}
	return n > 0, nil
}

// upsertRecordingTx inserts the recording row when absent (state downloading)
// and leaves an existing row untouched, so replayed created events never
// regress a recording that already progressed.
func upsertRecordingTx(ctx context.Context, tx *sql.Tx, recID, owner, deviceID string, now int64) error {
	if owner == "" {
		owner = "default"
	}
	if deviceID == "" {
		deviceID = "device:cloud"
	}
	startedAt := time.UnixMilli(now).UTC().Format(time.RFC3339)
	_, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO sync_recordings(recording_id,owner,device_id,state,started_at,transcript_revision,transcription_state,updated_at) VALUES(?,?,?,?,?,?,?,?)`,
		recID, owner, deviceID, stateDownloading, startedAt, "", "none", now)
	if err != nil {
		return fmt.Errorf("cloudsync: upsert recording %s: %w", recID, err)
	}
	return nil
}

func loadRecording(ctx context.Context, db *sql.DB, recID string) (*syncRecording, error) {
	row := db.QueryRowContext(ctx,
		`SELECT recording_id,owner,device_id,state,total_chunks,total_samples,started_at,stitched_path,transcript_revision,transcription_state,last_error,acked_at,updated_at FROM sync_recordings WHERE recording_id=?`, recID)
	return scanRecording(row)
}

func scanRecording(row *sql.Row) (*syncRecording, error) {
	var r syncRecording
	err := row.Scan(&r.RecordingID, &r.Owner, &r.DeviceID, &r.State, &r.TotalChunks, &r.TotalSamples, &r.StartedAt, &r.StitchedPath, &r.TranscriptRevision, &r.TranscriptionState, &r.LastError, &r.AckedAt, &r.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cloudsync: load recording: %w", err)
	}
	return &r, nil
}

// updateRecordingTx applies one mutation to the recording row inside tx.
// state/transcription_state/last_error are set only when the argument is
// non-empty, so callers patch exactly what they know.
func updateRecordingTx(ctx context.Context, tx *sql.Tx, recID string, state, transcriptionState, lastError *string, totalChunks, totalSamples *int64, stitchedPath *string, ackedAt *int64, now int64) error {
	if err := ensureRecordingTx(ctx, tx, recID, now); err != nil {
		return err
	}
	if state != nil {
		if _, err := tx.ExecContext(ctx, `UPDATE sync_recordings SET state=?, updated_at=? WHERE recording_id=?`, *state, now, recID); err != nil {
			return err
		}
	}
	if transcriptionState != nil {
		if _, err := tx.ExecContext(ctx, `UPDATE sync_recordings SET transcription_state=?, updated_at=? WHERE recording_id=?`, *transcriptionState, now, recID); err != nil {
			return err
		}
	}
	if lastError != nil {
		if _, err := tx.ExecContext(ctx, `UPDATE sync_recordings SET last_error=?, updated_at=? WHERE recording_id=?`, *lastError, now, recID); err != nil {
			return err
		}
	}
	if totalChunks != nil {
		if _, err := tx.ExecContext(ctx, `UPDATE sync_recordings SET total_chunks=?, updated_at=? WHERE recording_id=?`, *totalChunks, now, recID); err != nil {
			return err
		}
	}
	if totalSamples != nil {
		if _, err := tx.ExecContext(ctx, `UPDATE sync_recordings SET total_samples=?, updated_at=? WHERE recording_id=?`, *totalSamples, now, recID); err != nil {
			return err
		}
	}
	if stitchedPath != nil {
		if _, err := tx.ExecContext(ctx, `UPDATE sync_recordings SET stitched_path=?, updated_at=? WHERE recording_id=?`, *stitchedPath, now, recID); err != nil {
			return err
		}
	}
	if ackedAt != nil {
		if _, err := tx.ExecContext(ctx, `UPDATE sync_recordings SET acked_at=?, updated_at=? WHERE recording_id=?`, *ackedAt, now, recID); err != nil {
			return err
		}
	}
	return nil
}

// ensureRecordingTx guarantees the row exists before an UPDATE patches it.
func ensureRecordingTx(ctx context.Context, tx *sql.Tx, recID string, now int64) error {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sync_recordings WHERE recording_id=?`, recID).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	return upsertRecordingTx(ctx, tx, recID, "", "", now)
}

// chunkRow is one downloaded chunk's local provenance. SampleCount is derived
// from Bytes (two bytes per sample) whenever callers need it.
type chunkRow struct {
	Seq         int64
	SHA256      string
	Bytes       int64
	SampleCount int64
	FilePath    string
}

// addChunkTx records one downloaded chunk inside tx.
func addChunkTx(ctx context.Context, tx *sql.Tx, recID string, c chunkRow, now int64) error {
	_, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO sync_chunks(recording_id,seq,sha256,bytes,file_path,downloaded_at) VALUES(?,?,?,?,?,?)`,
		recID, c.Seq, c.SHA256, c.Bytes, c.FilePath, now)
	if err != nil {
		return fmt.Errorf("cloudsync: record chunk %s/%d: %w", recID, c.Seq, err)
	}
	return nil
}

func hasChunk(ctx context.Context, db *sql.DB, recID string, seq int64) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sync_chunks WHERE recording_id=? AND seq=?`, recID, seq).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("cloudsync: probe chunk %s/%d: %w", recID, seq, err)
	}
	return n > 0, nil
}

// listChunks returns the recording's downloaded chunks ordered by seq.
// sample_count derives from bytes: each sample is exactly two S16LE bytes, a
// relation verified at download time.
func listChunks(ctx context.Context, db *sql.DB, recID string) ([]chunkRow, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT seq,sha256,bytes,file_path FROM sync_chunks WHERE recording_id=? ORDER BY seq`, recID)
	if err != nil {
		return nil, fmt.Errorf("cloudsync: list chunks %s: %w", recID, err)
	}
	defer rows.Close()
	var out []chunkRow
	for rows.Next() {
		var c chunkRow
		if err := rows.Scan(&c.Seq, &c.SHA256, &c.Bytes, &c.FilePath); err != nil {
			return nil, err
		}
		c.SampleCount = c.Bytes / 2
		out = append(out, c)
	}
	return out, rows.Err()
}

// deleteChunksTx removes every chunk row for the recording inside tx.
func deleteChunksTx(ctx context.Context, tx *sql.Tx, recID string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM sync_chunks WHERE recording_id=?`, recID); err != nil {
		return fmt.Errorf("cloudsync: delete chunks %s: %w", recID, err)
	}
	return nil
}

// Status is the --status snapshot: sync_state plus a per-recording summary.
type Status struct {
	Cursor        int64             `json:"cursor"`
	LastSuccessAt *int64            `json:"last_success_at"`
	LastError     *string           `json:"last_error"`
	UpdatedAt     *int64            `json:"updated_at"`
	Recordings    []StatusRecording `json:"recordings"`
}

// StatusRecording summarizes one sync_recordings row.
type StatusRecording struct {
	RecordingID        string  `json:"recording_id"`
	Owner              string  `json:"owner"`
	DeviceID           string  `json:"device_id"`
	State              string  `json:"state"`
	TotalChunks        *int64  `json:"total_chunks"`
	TotalSamples       *int64  `json:"total_samples"`
	StitchedPath       *string `json:"stitched_path"`
	TranscriptRevision string  `json:"transcript_revision"`
	TranscriptionState string  `json:"transcription_state"`
	LastError          *string `json:"last_error"`
	AckedAt            *int64  `json:"acked_at"`
	UpdatedAt          int64   `json:"updated_at"`
}

// Snapshot reads the local sync state for human/CI inspection.
func Snapshot(ctx context.Context, db *sql.DB) (Status, error) {
	var st Status
	err := db.QueryRowContext(ctx, `SELECT cursor,last_success_at,last_error,updated_at FROM sync_state WHERE id=1`).
		Scan(&st.Cursor, &st.LastSuccessAt, &st.LastError, &st.UpdatedAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return st, fmt.Errorf("cloudsync: read sync_state: %w", err)
	}
	rows, err := db.QueryContext(ctx,
		`SELECT recording_id,owner,device_id,state,total_chunks,total_samples,stitched_path,transcript_revision,transcription_state,last_error,acked_at,updated_at FROM sync_recordings ORDER BY updated_at,recording_id`)
	if err != nil {
		return st, fmt.Errorf("cloudsync: read sync_recordings: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var r StatusRecording
		if err := rows.Scan(&r.RecordingID, &r.Owner, &r.DeviceID, &r.State, &r.TotalChunks, &r.TotalSamples, &r.StitchedPath, &r.TranscriptRevision, &r.TranscriptionState, &r.LastError, &r.AckedAt, &r.UpdatedAt); err != nil {
			return st, err
		}
		st.Recordings = append(st.Recordings, r)
	}
	if err := rows.Err(); err != nil {
		return st, err
	}
	if st.Recordings == nil {
		st.Recordings = []StatusRecording{}
	}
	return st, nil
}
