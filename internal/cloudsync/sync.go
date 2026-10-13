package cloudsync

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	"recordingmcp/internal/asrinput"
	"recordingmcp/internal/memory"
)

// syncPageLimit is the event page size used by the engine loop; the server
// clamps it to its own maximum.
const syncPageLimit = 200

// checksumRetries is the verification attempt count for one chunk download:
// the initial fetch plus one retry, per the plan's mismatch rule.
const checksumRetries = 2

// Engine drives one sync pass over the ingest-v1 event log. Events are
// processed in strict ascending id order; every event's local effects land in
// the same SQLite transaction that advances the cursor past it.
type Engine struct {
	DB     *sql.DB
	Store  *memory.Store
	Client *Client
	// AudioDir is the controlled directory for downloaded chunk files and
	// stitched recordings.
	AudioDir string
	// Transcriber is nil when --asr off: recordings park in
	// transcription_pending instead of being silently dropped.
	Transcriber Transcriber
	// Revision is the transcript_revision stamped onto ingested segments
	// ("asr-v1" when empty).
	Revision string
	// DeviceIDFallback backs device_id when the cloud event carries none.
	DeviceIDFallback string
	// DisableAck skips best-effort transcription-state reports to the server
	// (tests and --no-ack).
	DisableAck bool
	// Log receives human-readable progress lines; nil drops them.
	Log *log.Logger
}

// Report is one RunOnce outcome, printed as one JSON line by the CLI.
type Report struct {
	Cursor             int64 `json:"cursor"`
	EventsProcessed    int   `json:"events_processed"`
	ChunksDownloaded   int   `json:"chunks_downloaded"`
	RecordingsIngested int   `json:"recordings_ingested"`
	Errors             int   `json:"errors"`
}

func (e *Engine) logf(format string, args ...any) {
	if e.Log != nil {
		e.Log.Printf(format, args...)
	}
}

func (e *Engine) revision() string {
	if e.Revision == "" {
		return "asr-v1"
	}
	return e.Revision
}

// RunOnce performs one pass: drain the event page stream (strict id order),
// then sweep recordings parked in transcription_pending (a previous pass ran
// with --asr off). Network and protocol failures return as errors; callers
// retry with the configured poll interval and never skip events.
func (e *Engine) RunOnce(ctx context.Context) (Report, error) {
	var rep Report
	cur, err := cursor(ctx, e.DB)
	if err != nil {
		return rep, err
	}
	page, err := e.Client.SyncEvents(ctx, cur, syncPageLimit)
	if err != nil {
		if IsCursorExpired(err) {
			if err := e.recoverFromSnapshot(ctx, &rep); err != nil {
				return rep, err
			}
			rep.Cursor, _ = cursor(ctx, e.DB)
			return rep, nil
		}
		return rep, err
	}
	for _, ev := range page.Events {
		// Replayed pages (cursor rewound behind already-handled events) skip
		// idempotently: the sync_events ledger is the processed marker.
		done, err := eventProcessed(ctx, e.DB, ev.ID)
		if err != nil {
			return rep, err
		}
		if done {
			if err := e.commitEvent(ctx, &ev, nil); err != nil {
				return rep, err
			}
			continue
		}
		advance, err := e.handleEvent(ctx, ev, &rep)
		if err != nil {
			// Unexpected local failure: leave the cursor where it is so the
			// next pass re-fetches the same event (never skip).
			return rep, err
		}
		if !advance {
			// The event could not be settled (incomplete finish, unknown or
			// malformed event). Stop the page here so the cursor invariant
			// "every event at or before the cursor is settled" holds; the
			// next pass retries this event first.
			break
		}
	}
	rep.Cursor, err = cursor(ctx, e.DB)
	if err != nil {
		return rep, err
	}
	if err := e.sweepPendingTranscriptions(ctx, &rep); err != nil {
		return rep, err
	}
	return rep, nil
}

// commitEvent settles one event: apply extra local effects, record the event
// as processed, and advance the cursor — all in one transaction. A nil event
// (snapshot recovery and pending sweeps, which have no event id) records no
// ledger row and leaves the cursor untouched.
func (e *Engine) commitEvent(ctx context.Context, ev *Event, extra func(ctx context.Context, tx *sql.Tx) error) error {
	tx, err := e.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UnixMilli()
	if extra != nil {
		if err := extra(ctx, tx); err != nil {
			return err
		}
	}
	if ev != nil {
		if err := recordEventTx(ctx, tx, *ev, now); err != nil {
			return err
		}
		if err := advanceCursorTx(ctx, tx, ev.ID, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// handleEvent dispatches one unprocessed event. The bool reports whether the
// event was settled (mark processed + advance cursor).
func (e *Engine) handleEvent(ctx context.Context, ev Event, rep *Report) (bool, error) {
	switch ev.Type {
	case "recording.created":
		return e.handleCreated(ctx, ev, rep)
	case "chunk.committed":
		return e.handleChunk(ctx, ev, rep)
	case "recording.finished":
		return e.handleFinished(ctx, ev, rep)
	case "recording.deleted":
		return e.handleDeleted(ctx, ev, rep)
	case "transcript.available":
		// Reserved event type with no v1 producer; log and move on.
		e.logf("cloudsync: event %d: unsupported event type (reserved): transcript.available", ev.ID)
		return true, e.commitEvent(ctx, &ev, nil)
	default:
		return e.holdEvent(ctx, rep, &ev, fmt.Sprintf("event %d: unknown event type %q; cursor held", ev.ID, ev.Type))
	}
}

// holdEvent records a permanent, operator-visible problem and holds the
// cursor: the event is not settled, later events wait, and the next pass
// retries it first (never silently skipped).
func (e *Engine) holdEvent(ctx context.Context, rep *Report, ev *Event, msg string) (bool, error) {
	e.logf("cloudsync: %s", msg)
	rep.Errors++
	if err := setLastError(ctx, e.DB, msg, time.Now().UnixMilli()); err != nil {
		return false, err
	}
	return false, nil
}

func (e *Engine) handleCreated(ctx context.Context, ev Event, rep *Report) (bool, error) {
	var p RecordingCreatedPayload
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return e.holdEvent(ctx, rep, &ev, fmt.Sprintf("event %d: malformed recording.created payload: %v", ev.ID, err))
	}
	owner := "default"
	if p.Owner != nil && *p.Owner != "" {
		owner = *p.Owner
	}
	deviceID := e.DeviceIDFallback
	if p.DeviceID != nil && *p.DeviceID != "" {
		deviceID = *p.DeviceID
	}
	err := e.commitEvent(ctx, &ev, func(ctx context.Context, tx *sql.Tx) error {
		return upsertRecordingTx(ctx, tx, ev.RecordingID, owner, deviceID, time.Now().UnixMilli())
	})
	if err != nil {
		return false, err
	}
	rep.EventsProcessed++
	return true, nil
}

func (e *Engine) handleChunk(ctx context.Context, ev Event, rep *Report) (bool, error) {
	var p ChunkCommittedPayload
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return e.holdEvent(ctx, rep, &ev, fmt.Sprintf("event %d: malformed chunk.committed payload: %v", ev.ID, err))
	}
	// Defensive: a chunk event must never arrive before its recording row.
	if err := e.ensureRecording(ctx, ev.RecordingID, time.Now().UnixMilli()); err != nil {
		return false, err
	}
	seen, err := hasChunk(ctx, e.DB, ev.RecordingID, p.Seq)
	if err != nil {
		return false, err
	}
	if seen {
		// Re-delivered event for an already-downloaded chunk.
		rep.EventsProcessed++
		return true, e.commitEvent(ctx, &ev, nil)
	}
	if err := e.downloadChunkVerified(ctx, ev.RecordingID, p); err != nil {
		var cm *checksumMismatchError
		if errors.As(err, &cm) {
			// Two verified-bad attempts: fail the recording but keep the
			// stream flowing — never wedge the cursor on one bad chunk.
			e.logf("cloudsync: recording %s chunk %d: %v; recording marked failed", ev.RecordingID, p.Seq, cm)
			rep.Errors++
			return true, e.commitEvent(ctx, &ev, func(ctx context.Context, tx *sql.Tx) error {
				return failRecordingTx(ctx, tx, ev.RecordingID, time.Now().UnixMilli())
			})
		}
		// Transport or local I/O error: abort the pass; the whole event is
		// retried on the next pass.
		return false, err
	}
	err = e.commitEvent(ctx, &ev, func(ctx context.Context, tx *sql.Tx) error {
		return addChunkTx(ctx, tx, ev.RecordingID, chunkRow{
			Seq:         p.Seq,
			SHA256:      p.ContentSHA256,
			Bytes:       p.Bytes,
			SampleCount: p.SampleCount,
			FilePath:    e.chunkPath(ev.RecordingID, p.Seq),
		}, time.Now().UnixMilli())
	})
	if err != nil {
		return false, err
	}
	rep.EventsProcessed++
	rep.ChunksDownloaded++
	return true, nil
}

// failRecordingTx marks a recording failed unless it already reached a
// terminal state, so a late event never regresses ingested data.
func failRecordingTx(ctx context.Context, tx *sql.Tx, recID string, now int64) error {
	var state string
	err := tx.QueryRowContext(ctx, `SELECT state FROM sync_recordings WHERE recording_id=?`, recID).Scan(&state)
	if err == nil {
		switch state {
		case stateIngested, stateTranscriptionPending, stateDeleted:
			return nil
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	failed := stateFailed
	msg := "checksum_mismatch"
	return updateRecordingTx(ctx, tx, recID, &failed, nil, &msg, nil, nil, nil, nil, now)
}

type checksumMismatchError struct{ msg string }

func (e *checksumMismatchError) Error() string { return e.msg }

// downloadChunkVerified fetches one chunk to its .part path, verifying the
// computed SHA-256 and byte count against the committed payload (one retry
// after a verification failure), then fsyncs and atomically renames to the
// final chunk file name. Transport errors abort the pass; only verified
// content mismatches surface as checksumMismatchError.
func (e *Engine) downloadChunkVerified(ctx context.Context, recID string, p ChunkCommittedPayload) error {
	dir := filepath.Join(e.AudioDir, recID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	part := e.chunkPartPath(recID, p.Seq)
	final := e.chunkPath(recID, p.Seq)
	var lastErr error
	for attempt := range checksumRetries {
		if attempt > 0 {
			e.logf("cloudsync: recording %s chunk %d verification failed (%v); retrying once", recID, p.Seq, lastErr)
		}
		got, n, err := e.fetchChunk(ctx, recID, p.Seq, part)
		if err != nil {
			os.Remove(part)
			return err
		}
		switch {
		case got != p.ContentSHA256:
			lastErr = fmt.Errorf("sha256 mismatch: got %s want %s", got, p.ContentSHA256)
		case n != p.Bytes:
			lastErr = fmt.Errorf("byte count mismatch: got %d want %d", n, p.Bytes)
		case p.SampleCount >= 0 && n != p.SampleCount*2:
			lastErr = fmt.Errorf("sample count mismatch: %d bytes imply %d samples, event says %d", n, n/2, p.SampleCount)
		default:
			if err := os.Rename(part, final); err != nil {
				os.Remove(part)
				return err
			}
			return nil
		}
		os.Remove(part)
	}
	return &checksumMismatchError{msg: fmt.Sprintf("checksum_mismatch: recording %s chunk %d: %v", recID, p.Seq, lastErr)}
}

// fetchChunk streams one chunk into path, hashing the received bytes and
// fsyncing the file before close.
func (e *Engine) fetchChunk(ctx context.Context, recID string, seq int64, path string) (string, int64, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	meta, err := e.Client.DownloadChunk(ctx, recID, int(seq), f)
	if err != nil {
		return "", 0, err
	}
	if err := f.Sync(); err != nil {
		return "", 0, err
	}
	return meta.SHA256, meta.Bytes, nil
}

func (e *Engine) chunkPartPath(recID string, seq int64) string {
	return filepath.Join(e.AudioDir, recID, fmt.Sprintf("chunk-%08d.pcm.part", seq))
}

func (e *Engine) chunkPath(recID string, seq int64) string {
	return filepath.Join(e.AudioDir, recID, fmt.Sprintf("chunk-%08d.pcm", seq))
}

// stitchedPath returns the concatenated recording file path.
func (e *Engine) stitchedPath(recID string) string {
	return filepath.Join(e.AudioDir, recID+".pcm")
}

func (e *Engine) ensureRecording(ctx context.Context, recID string, now int64) error {
	tx, err := e.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := upsertRecordingTx(ctx, tx, recID, "", "", now); err != nil {
		return err
	}
	return tx.Commit()
}

func (e *Engine) handleFinished(ctx context.Context, ev Event, rep *Report) (bool, error) {
	var p RecordingFinishedPayload
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return e.holdEvent(ctx, rep, &ev, fmt.Sprintf("event %d: malformed recording.finished payload: %v", ev.ID, err))
	}
	if err := e.ensureRecording(ctx, ev.RecordingID, time.Now().UnixMilli()); err != nil {
		return false, err
	}
	row, err := loadRecording(ctx, e.DB, ev.RecordingID)
	if err != nil {
		return false, err
	}
	switch row.State {
	case stateIngested:
		// Replayed finish after full ingestion: settle idempotently.
		rep.EventsProcessed++
		return true, e.commitEvent(ctx, &ev, nil)
	case stateTranscriptionPending:
		if e.Transcriber == nil {
			// Still no transcriber configured; keep pending, settle the event.
			rep.EventsProcessed++
			return true, e.commitEvent(ctx, &ev, nil)
		}
		return e.transcribeAndSettle(ctx, &ev, rep, row)
	case stateDeleted, stateFailed:
		// A deleted or failed recording can never complete; settle the event
		// so the stream keeps flowing (the failed state is operator-visible).
		e.logf("cloudsync: recording %s is %s; settling finish without transcription", ev.RecordingID, row.State)
		rep.EventsProcessed++
		return true, e.commitEvent(ctx, &ev, nil)
	}
	// Completeness: every seq present, count matches, sample sum matches.
	chunks, err := listChunks(ctx, e.DB, ev.RecordingID)
	if err != nil {
		return false, err
	}
	if gap := completenessGap(chunks, p.TotalChunks, p.TotalSamples); gap != "" {
		e.logf("cloudsync: recording %s finish incomplete: %s; keeping state downloading", ev.RecordingID, gap)
		rep.Errors++
		return e.holdEvent(ctx, rep, &ev, fmt.Sprintf("recording %s incomplete: %s", ev.RecordingID, gap))
	}
	return e.stitchTranscribeSettle(ctx, &ev, p, rep, row, chunks)
}

// completenessGap returns "" when the local chunk set exactly covers
// 0..totalChunks-1 with the expected sample sum, else a human-readable gap.
func completenessGap(chunks []chunkRow, totalChunks, totalSamples int64) string {
	var sum int64
	seen := make(map[int64]bool, len(chunks))
	for _, c := range chunks {
		if c.Seq < 0 || c.Seq >= totalChunks {
			return fmt.Sprintf("unexpected seq %d for total_chunks %d", c.Seq, totalChunks)
		}
		if seen[c.Seq] {
			return fmt.Sprintf("duplicate seq %d", c.Seq)
		}
		seen[c.Seq] = true
		sum += c.SampleCount
	}
	var missing []int64
	for i := range totalChunks {
		if !seen[i] {
			missing = append(missing, i)
		}
	}
	if len(missing) > 0 {
		return fmt.Sprintf("missing sequences %v", missing)
	}
	if sum != totalSamples {
		return fmt.Sprintf("sample sum %d != total_samples %d", sum, totalSamples)
	}
	return ""
}

// stitchTranscribeSettle stitches the verified chunk files into one PCM file,
// then either parks transcription_pending (no transcriber) or transcribes and
// ingests. Chunk files and rows are dropped only in the settle transaction,
// together with the event marker and cursor advance. A transcription or
// ingest failure holds the event (chunks remain on disk) so the next pass
// retries deterministically.
func (e *Engine) stitchTranscribeSettle(ctx context.Context, ev *Event, p RecordingFinishedPayload, rep *Report, row *syncRecording, chunks []chunkRow) (bool, error) {
	stitched := e.stitchedPath(row.RecordingID)
	sum, err := stitchChunks(chunks, stitched)
	if err != nil {
		return false, err
	}
	if e.Transcriber == nil {
		now := time.Now().UnixMilli()
		pending := stateTranscriptionPending
		stitch := stitched
		tstate := "pending"
		err := e.commitEvent(ctx, ev, func(ctx context.Context, tx *sql.Tx) error {
			if err := updateRecordingTx(ctx, tx, row.RecordingID, &pending, &tstate, nil, &p.TotalChunks, &p.TotalSamples, &stitch, nil, now); err != nil {
				return err
			}
			if err := deleteChunksTx(ctx, tx, row.RecordingID); err != nil {
				return err
			}
			return removeChunkFiles(row.RecordingID, e.AudioDir)
		})
		if err != nil {
			return false, err
		}
		rep.EventsProcessed++
		e.ackBestEffort(ctx, row.RecordingID, "pending")
		return true, nil
	}
	results, err := e.Transcriber.Transcribe(ctx, stitched, MetaBasis{
		DeviceID:          row.DeviceID,
		RecordingID:       row.RecordingID,
		ConversationID:    row.RecordingID,
		Revision:          e.revision(),
		SourceAudioSHA256: sum,
	})
	if err != nil {
		e.logf("cloudsync: recording %s transcription failed: %v; will retry", row.RecordingID, err)
		rep.Errors++
		return false, nil
	}
	if err := e.ingestResults(ctx, row.RecordingID, results); err != nil {
		e.logf("cloudsync: recording %s ingest failed: %v; will retry", row.RecordingID, err)
		rep.Errors++
		return false, nil
	}
	now := time.Now().UnixMilli()
	ingested := stateIngested
	stitch := stitched
	tstate := "ingested"
	err = e.commitEvent(ctx, ev, func(ctx context.Context, tx *sql.Tx) error {
		if err := updateRecordingTx(ctx, tx, row.RecordingID, &ingested, &tstate, nil, &p.TotalChunks, &p.TotalSamples, &stitch, nil, now); err != nil {
			return err
		}
		if err := deleteChunksTx(ctx, tx, row.RecordingID); err != nil {
			return err
		}
		return removeChunkFiles(row.RecordingID, e.AudioDir)
	})
	if err != nil {
		return false, err
	}
	rep.EventsProcessed++
	rep.RecordingsIngested++
	e.ackBestEffort(ctx, row.RecordingID, "ingested")
	return true, nil
}

// transcribeAndSettle drives a recording that is already stitched and parked
// in transcription_pending (a previous pass ran with --asr off).
func (e *Engine) transcribeAndSettle(ctx context.Context, ev *Event, rep *Report, row *syncRecording) (bool, error) {
	if row.StitchedPath == nil {
		return false, fmt.Errorf("cloudsync: recording %s transcription_pending without stitched_path", row.RecordingID)
	}
	sum, err := fileSHA256(*row.StitchedPath)
	if err != nil {
		return false, err
	}
	results, err := e.Transcriber.Transcribe(ctx, *row.StitchedPath, MetaBasis{
		DeviceID:          row.DeviceID,
		RecordingID:       row.RecordingID,
		ConversationID:    row.RecordingID,
		Revision:          e.revision(),
		SourceAudioSHA256: sum,
	})
	if err != nil {
		e.logf("cloudsync: recording %s transcription failed: %v; will retry", row.RecordingID, err)
		rep.Errors++
		return false, nil
	}
	if err := e.ingestResults(ctx, row.RecordingID, results); err != nil {
		e.logf("cloudsync: recording %s ingest failed: %v; will retry", row.RecordingID, err)
		rep.Errors++
		return false, nil
	}
	now := time.Now().UnixMilli()
	ingested := stateIngested
	tstate := "ingested"
	err = e.commitEvent(ctx, ev, func(ctx context.Context, tx *sql.Tx) error {
		return updateRecordingTx(ctx, tx, row.RecordingID, &ingested, &tstate, nil, nil, nil, nil, nil, now)
	})
	if err != nil {
		return false, err
	}
	rep.EventsProcessed++
	rep.RecordingsIngested++
	e.ackBestEffort(ctx, row.RecordingID, "ingested")
	return true, nil
}

// sweepPendingTranscriptions completes recordings parked in
// transcription_pending by an earlier --asr off run. Their finish events are
// already settled (cursor advanced), so this sweep is the only re-drive path;
// re-running with a transcriber is idempotent (same revision replays insert
// nothing new).
func (e *Engine) sweepPendingTranscriptions(ctx context.Context, rep *Report) error {
	if e.Transcriber == nil {
		return nil
	}
	rows, err := e.DB.QueryContext(ctx,
		`SELECT recording_id,device_id,stitched_path FROM sync_recordings WHERE state=? ORDER BY recording_id`, stateTranscriptionPending)
	if err != nil {
		return fmt.Errorf("cloudsync: list transcription_pending: %w", err)
	}
	type pending struct {
		recID, deviceID, path string
	}
	var pendings []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.recID, &p.deviceID, &p.path); err != nil {
			rows.Close()
			return err
		}
		pendings = append(pendings, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, p := range pendings {
		sum, err := fileSHA256(p.path)
		if err != nil {
			e.logf("cloudsync: recording %s pending sweep skipped: %v", p.recID, err)
			rep.Errors++
			continue
		}
		results, err := e.Transcriber.Transcribe(ctx, p.path, MetaBasis{
			DeviceID:          p.deviceID,
			RecordingID:       p.recID,
			ConversationID:    p.recID,
			Revision:          e.revision(),
			SourceAudioSHA256: sum,
		})
		if err != nil {
			e.logf("cloudsync: recording %s transcription failed: %v; stays pending", p.recID, err)
			rep.Errors++
			continue
		}
		if err := e.ingestResults(ctx, p.recID, results); err != nil {
			e.logf("cloudsync: recording %s ingest failed: %v; stays pending", p.recID, err)
			rep.Errors++
			continue
		}
		now := time.Now().UnixMilli()
		ingested := stateIngested
		tstate := "ingested"
		err = e.commitEvent(ctx, nil, func(ctx context.Context, tx *sql.Tx) error {
			return updateRecordingTx(ctx, tx, p.recID, &ingested, &tstate, nil, nil, nil, nil, nil, now)
		})
		if err != nil {
			return err
		}
		rep.RecordingsIngested++
		e.ackBestEffort(ctx, p.recID, "ingested")
	}
	return nil
}

// ingestResults pushes every segment result through the single memory
// ingestion entry point, in segment order.
func (e *Engine) ingestResults(ctx context.Context, recID string, results []asrinput.Result) error {
	if e.Store == nil {
		return errors.New("cloudsync: memory store is required for transcription ingest")
	}
	for i, r := range results {
		report, err := e.Store.IngestTranscript(ctx, r)
		if err != nil {
			return fmt.Errorf("segment %d: %w", i, err)
		}
		e.logf("cloudsync: recording %s segment %d ingested (inserted=%v turns=%d)", recID, i, report.Inserted, report.Turns)
	}
	return nil
}

// removeChunkFiles deletes the per-recording chunk directory (missing files
// are fine — deletes are idempotent).
func removeChunkFiles(recID, audioDir string) error {
	return os.RemoveAll(filepath.Join(audioDir, recID))
}

// stitchChunks concatenates the ordered chunk files into out and returns the
// hex SHA-256 of the stitched bytes. It streams; PCM needs no re-encoding.
func stitchChunks(chunks []chunkRow, out string) (string, error) {
	f, err := os.OpenFile(out, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	counting := &countingWriter{w: f, hash: hash}
	for _, c := range chunks {
		src, err := os.Open(c.FilePath)
		if err != nil {
			f.Close()
			return "", fmt.Errorf("stitch seq %d: %w", c.Seq, err)
		}
		_, err = io.Copy(counting, src)
		src.Close()
		if err != nil {
			f.Close()
			return "", fmt.Errorf("stitch seq %d: %w", c.Seq, err)
		}
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (e *Engine) handleDeleted(ctx context.Context, ev Event, rep *Report) (bool, error) {
	var p RecordingDeletedPayload
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return e.holdEvent(ctx, rep, &ev, fmt.Sprintf("event %d: malformed recording.deleted payload: %v", ev.ID, err))
	}
	// Local audio is removed; derived memory data (turns, candidates,
	// embeddings) is intentionally kept — deletion is an audit boundary.
	_ = os.Remove(e.stitchedPath(ev.RecordingID))
	_ = removeChunkFiles(ev.RecordingID, e.AudioDir)
	now := time.Now().UnixMilli()
	deleted := stateDeleted
	err := e.commitEvent(ctx, &ev, func(ctx context.Context, tx *sql.Tx) error {
		if err := deleteChunksTx(ctx, tx, ev.RecordingID); err != nil {
			return err
		}
		return updateRecordingTx(ctx, tx, ev.RecordingID, &deleted, nil, nil, nil, nil, nil, nil, now)
	})
	if err != nil {
		return false, err
	}
	rep.EventsProcessed++
	return true, nil
}

// recoverFromSnapshot rebuilds local state from GET /v1/sync/recordings after
// a 410 cursor_expired, then jumps the cursor to the current max_event_id
// from one SyncEvents probe.
func (e *Engine) recoverFromSnapshot(ctx context.Context, rep *Report) error {
	e.logf("cloudsync: cursor expired; recovering from snapshot")
	after := ""
	for {
		page, err := e.Client.ListRecordings(ctx, after, 0)
		if err != nil {
			return err
		}
		for _, rec := range page.Recordings {
			if err := e.recoverRecording(ctx, rec, rep); err != nil {
				return err
			}
		}
		if !page.HasMore || page.NextAfter == nil || *page.NextAfter == "" {
			break
		}
		after = *page.NextAfter
	}
	// One probe call yields the current max_event_id; the snapshot has
	// already reconciled everything the trimmed window used to describe.
	probe, err := e.Client.SyncEvents(ctx, 0, 1)
	if err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	tx, err := e.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := advanceCursorTx(ctx, tx, probe.MaxEventID, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sync_state SET last_error=NULL WHERE id=1`); err != nil {
		return err
	}
	return tx.Commit()
}

// recoverRecording reconciles one snapshot recording with local state.
func (e *Engine) recoverRecording(ctx context.Context, rec RecordingDetail, rep *Report) error {
	now := time.Now().UnixMilli()
	if rec.State == "deleted" {
		_ = os.Remove(e.stitchedPath(rec.RecordingID))
		_ = removeChunkFiles(rec.RecordingID, e.AudioDir)
		deleted := stateDeleted
		return e.commitEvent(ctx, nil, func(ctx context.Context, tx *sql.Tx) error {
			if err := deleteChunksTx(ctx, tx, rec.RecordingID); err != nil {
				return err
			}
			return updateRecordingTx(ctx, tx, rec.RecordingID, &deleted, nil, nil, nil, nil, nil, nil, now)
		})
	}
	if err := e.ensureRecording(ctx, rec.RecordingID, now); err != nil {
		return err
	}
	// Download any chunk missing locally, verifying against the manifest.
	for _, ch := range rec.Chunks {
		seen, err := hasChunk(ctx, e.DB, rec.RecordingID, ch.Seq)
		if err != nil {
			return err
		}
		if seen {
			continue
		}
		payload := ChunkCommittedPayload{
			Seq:           ch.Seq,
			SampleOffset:  ch.SampleOffset,
			SampleCount:   ch.SampleCount,
			Bytes:         ch.Bytes,
			ContentSHA256: ch.ContentSHA256,
		}
		if err := e.downloadChunkVerified(ctx, rec.RecordingID, payload); err != nil {
			if _, ok := err.(*checksumMismatchError); ok {
				// Same rule as the event path: fail the recording, keep
				// recovering the rest of the snapshot.
				e.logf("cloudsync: recovery: recording %s chunk %d: %v", rec.RecordingID, ch.Seq, err)
				rep.Errors++
				if err := e.commitEvent(ctx, nil, func(ctx context.Context, tx *sql.Tx) error {
					return failRecordingTx(ctx, tx, rec.RecordingID, time.Now().UnixMilli())
				}); err != nil {
					return err
				}
				return nil
			}
			return err
		}
		if err := e.commitEvent(ctx, nil, func(ctx context.Context, tx *sql.Tx) error {
			return addChunkTx(ctx, tx, rec.RecordingID, chunkRow{
				Seq:         ch.Seq,
				SHA256:      ch.ContentSHA256,
				Bytes:       ch.Bytes,
				SampleCount: ch.SampleCount,
				FilePath:    e.chunkPath(rec.RecordingID, ch.Seq),
			}, time.Now().UnixMilli())
		}); err != nil {
			return err
		}
		rep.ChunksDownloaded++
	}
	if rec.State != "complete" || rec.TotalChunks == nil || rec.TotalSamples == nil {
		return nil
	}
	row, err := loadRecording(ctx, e.DB, rec.RecordingID)
	if err != nil {
		return err
	}
	if row.State == stateIngested || row.State == stateTranscriptionPending {
		return nil
	}
	chunks, err := listChunks(ctx, e.DB, rec.RecordingID)
	if err != nil {
		return err
	}
	if gap := completenessGap(chunks, *rec.TotalChunks, *rec.TotalSamples); gap != "" {
		e.logf("cloudsync: recovery: recording %s incomplete: %s", rec.RecordingID, gap)
		rep.Errors++
		return nil
	}
	p := RecordingFinishedPayload{TotalChunks: *rec.TotalChunks, TotalSamples: *rec.TotalSamples}
	if _, err := e.stitchTranscribeSettle(ctx, nil, p, rep, row, chunks); err != nil {
		return err
	}
	return nil
}

// ackBestEffort reports transcription state to the server, tagging the ack
// with the current sync cursor; failures are logged (the Android client reads
// synced_at/transcription_state via endpoint 6) but never fail the pass.
func (e *Engine) ackBestEffort(ctx context.Context, recID, state string) {
	if e.DisableAck {
		return
	}
	lastEventID, err := cursor(ctx, e.DB)
	if err != nil {
		e.logf("cloudsync: ack %s %s skipped: %v", recID, state, err)
		return
	}
	if err := e.Client.Ack(ctx, recID, lastEventID, state, ""); err != nil {
		e.logf("cloudsync: ack %s %s failed: %v", recID, state, err)
	}
}
