package audioanalysis

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strings"
	"time"

	"recordingmcp/internal/process"
)

// Store owns the durable audio-analysis state: runs, events, speaker
// embeddings and the per-recording desired head.
type Store struct {
	DB  *sql.DB
	Now func() time.Time
}

// NewStore wraps db.
func NewStore(db *sql.DB) *Store { return &Store{DB: db} }

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// EnqueueInput describes one analysis request.
type EnqueueInput struct {
	RecordingID string
	Capability  string
	// Generation is the caller's request id: it is the only way to force a new
	// run for the same audio.
	Generation string
	Request    Request
	// Dependencies are parent job ids (VAD/ASR/tagging) that must terminate
	// before the worker claims this job.
	Dependencies []string
	Priority     int
	MaxAttempts  int
}

// EnqueueOutput is the idempotent enqueue result.
type EnqueueOutput struct {
	JobID    string `json:"job_id"`
	RunID    string `json:"run_id"`
	Inserted bool   `json:"inserted"`
}

// jobPayload is the canonical queue payload; a repeat with different bytes is
// an audio_request_conflict.
type jobPayload struct {
	Request    Request `json:"request"`
	Generation string  `json:"generation"`
}

// Enqueue validates the request and enqueues it atomically with its running
// run row and desired head. An identical generation is idempotent; a differing
// canonical payload is rejected with audio_request_conflict.
func (s *Store) Enqueue(ctx context.Context, in EnqueueInput) (EnqueueOutput, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return EnqueueOutput{}, err
	}
	defer func() { _ = tx.Rollback() }()
	out, err := s.EnqueueTx(ctx, tx, in)
	if err != nil {
		return EnqueueOutput{}, err
	}
	if err := tx.Commit(); err != nil {
		return EnqueueOutput{}, err
	}
	return out, nil
}

// EnqueueTx is Enqueue inside the caller's transaction, so the ingest stage can
// create the audio run and its job atomically with the ASR job.
func (s *Store) EnqueueTx(ctx context.Context, tx *sql.Tx, in EnqueueInput) (EnqueueOutput, error) {
	if strings.TrimSpace(in.RecordingID) == "" {
		return EnqueueOutput{}, NewError(CodeInputInvalid, false, "recording_id is required")
	}
	if !IsCapability(in.Capability) {
		return EnqueueOutput{}, NewError(CodeInputInvalid, false, "unknown capability")
	}
	if strings.TrimSpace(in.Generation) == "" {
		return EnqueueOutput{}, NewError(CodeInputInvalid, false, "generation (request id) is required")
	}
	req := in.Request
	req.RecordingID = in.RecordingID
	req.Capability = in.Capability
	req.SchemaVersion = ContractSchema
	if err := req.Validate(); err != nil {
		return EnqueueOutput{}, err
	}
	computed := ComputeRunID(req, in.Generation)
	if req.RunID == "" {
		req.RunID = computed
	} else if req.RunID != computed {
		return EnqueueOutput{}, NewError(CodeInputInvalid, false, "run_id does not match the request identity")
	}
	payloadBytes, err := json.Marshal(jobPayload{Request: req, Generation: in.Generation})
	if err != nil {
		return EnqueueOutput{}, err
	}
	payload := string(payloadBytes)
	idempotency := "audio|" + in.RecordingID + "|" + in.Capability + "|" + in.Generation
	stage := "audio_" + in.Capability

	now := s.now()
	var existingID, existingPayload, existingStatus string
	err = tx.QueryRowContext(ctx,
		`SELECT id,payload_json,status FROM processing_jobs WHERE idempotency_key=?`, idempotency).
		Scan(&existingID, &existingPayload, &existingStatus)
	switch {
	case err == nil:
		if existingPayload != payload {
			return EnqueueOutput{}, NewError(CodeRequestConflict, false, "the same generation was enqueued with a different payload")
		}
		runID, runErr := s.runIDForJob(ctx, tx, existingID)
		if runErr != nil {
			return EnqueueOutput{}, runErr
		}
		return EnqueueOutput{JobID: existingID, RunID: runID, Inserted: false}, nil
	case !errors.Is(err, sql.ErrNoRows):
		return EnqueueOutput{}, err
	}

	jobID, inserted, err := process.EnqueueTx(ctx, tx, process.Spec{
		Stage:           stage,
		InputSnapshotID: in.RecordingID,
		IdempotencyKey:  idempotency,
		PayloadJSON:     payload,
		Priority:        in.Priority,
		MaxAttempts:     in.MaxAttempts,
		Dependencies:    in.Dependencies,
	}, now)
	if err != nil {
		return EnqueueOutput{}, err
	}
	if !inserted {
		// A different enqueue path won the race; its payload must already
		// match because the unique key is the same.
		var stored string
		if err := tx.QueryRowContext(ctx, `SELECT payload_json FROM processing_jobs WHERE id=?`, jobID).Scan(&stored); err != nil {
			return EnqueueOutput{}, err
		}
		if stored != payload {
			return EnqueueOutput{}, NewError(CodeRequestConflict, false, "the same generation was enqueued with a different payload")
		}
		runID, runErr := s.runIDForJob(ctx, tx, jobID)
		if runErr != nil {
			return EnqueueOutput{}, runErr
		}
		return EnqueueOutput{JobID: jobID, RunID: runID, Inserted: false}, nil
	}

	attachments := req.SourceAttachmentIDs
	if attachments == nil {
		attachments = []string{}
	}
	attachmentsJSON, err := json.Marshal(attachments)
	if err != nil {
		return EnqueueOutput{}, err
	}
	nowMS := now.UnixMilli()
	var revision any
	if req.TranscriptRevision != nil {
		revision = *req.TranscriptRevision
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO audio_analysis_runs
		 (id,job_id,recording_id,source_audio_sha256,input_sha256,transcript_revision,source_attachment_ids_json,
		  source_snapshot_sha256,capability,start_ms,end_ms,model_id,model_revision,config_sha256,generation,status,active,result_json,created_at,updated_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,'running',0,'{}',?,?)`,
		req.RunID, jobID, req.RecordingID, req.SourceAudioSHA256, req.InputSHA256, revision, string(attachmentsJSON),
		req.SourceSnapshotSHA256, req.Capability, req.StartMS, req.EndMS, req.ModelID, req.ModelRevision,
		req.ConfigSHA256, in.Generation, nowMS, nowMS); err != nil {
		return EnqueueOutput{}, err
	}
	// A new generation supersedes the previous active result immediately, while
	// the head blocks any older worker from publishing late.
	if _, err := tx.ExecContext(ctx,
		`UPDATE audio_analysis_runs SET active=0,status='invalidated',updated_at=?
		 WHERE recording_id=? AND capability=? AND id<>? AND status IN ('succeeded','failed','running')`,
		nowMS, req.RecordingID, req.Capability, req.RunID); err != nil {
		return EnqueueOutput{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO audio_analysis_heads(recording_id,capability,desired_job_id,current_run_id)
		 VALUES(?,?,?,NULL)
		 ON CONFLICT(recording_id,capability) DO UPDATE SET desired_job_id=excluded.desired_job_id, current_run_id=NULL`,
		req.RecordingID, req.Capability, jobID); err != nil {
		return EnqueueOutput{}, err
	}
	return EnqueueOutput{JobID: jobID, RunID: req.RunID, Inserted: true}, nil
}

// runIDForJob reads the run identity of an existing job.
func (s *Store) runIDForJob(ctx context.Context, tx *sql.Tx, jobID string) (string, error) {
	var runID string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM audio_analysis_runs WHERE job_id=?`, jobID).Scan(&runID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", NewError(CodeRequestConflict, false, "job exists without an audio run")
		}
		return "", err
	}
	return runID, nil
}

// CommitInput is one terminal run submission.
type CommitInput struct {
	JobID      string
	LeaseToken string
	Result     Result
	// Embeddings maps event id to its speaker vector; it is stored separately
	// and never serialised into result_json.
	Embeddings map[string][]float32
	Now        time.Time
}

// Commit finalises one run atomically: it checks the desired head, the live
// lease, the forget barrier and the source attachments, writes events and
// embeddings, marks the run current and completes the job in the same
// transaction.
func (s *Store) Commit(ctx context.Context, in CommitInput) (string, error) {
	now := in.Now
	if now.IsZero() {
		now = s.now()
	}
	nowMS := now.UnixMilli()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()

	var (
		runID, recordingID, capability, generation, snapshotHash, attachmentsJSON string
		status                                                                    string
		startMS, endMS                                                            int64
	)
	err = tx.QueryRowContext(ctx,
		`SELECT id,recording_id,capability,generation,status,source_snapshot_sha256,source_attachment_ids_json,start_ms,end_ms
		 FROM audio_analysis_runs WHERE job_id=?`, in.JobID).
		Scan(&runID, &recordingID, &capability, &generation, &status, &snapshotHash, &attachmentsJSON, &startMS, &endMS)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", NewError(CodeLeaseLost, false, "run does not exist")
		}
		return "", err
	}
	if status != StatusRunning {
		return "", NewError(CodeLeaseLost, false, "run is already finalised")
	}
	if in.Result.RunID != runID || in.Result.RecordingID != recordingID || in.Result.Capability != capability {
		return "", NewError(CodeResultInvalid, false, "result identity does not match the run")
	}
	if in.Result.SourceSnapshotSHA256 != snapshotHash {
		return "", NewError(CodeResultInvalid, false, "result snapshot does not match the run")
	}
	var resultAttachments []string
	if err := json.Unmarshal([]byte(attachmentsJSON), &resultAttachments); err != nil {
		return "", err
	}
	if strings.Join(in.Result.SourceAttachmentIDs, "\x00") != strings.Join(resultAttachments, "\x00") {
		return "", NewError(CodeResultInvalid, false, "result attachments do not match the run")
	}
	var payloadJSON string
	if err := tx.QueryRowContext(ctx, `SELECT payload_json FROM processing_jobs WHERE id=?`, in.JobID).Scan(&payloadJSON); err != nil {
		return "", err
	}
	var payload jobPayload
	if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
		return "", err
	}
	if payload.Request.RunID != runID || payload.Generation != generation {
		return "", NewError(CodeResultInvalid, false, "queued payload does not match the run")
	}
	if err := in.Result.ValidateResult(payload.Request, endMS-startMS); err != nil {
		return "", err
	}
	if err := commitGuards(ctx, tx, in.JobID, in.LeaseToken, recordingID, capability, now); err != nil {
		return "", err
	}
	if err := verifySourceSnapshot(ctx, tx, recordingID, in.Result.SourceAttachmentIDs, snapshotHash); err != nil {
		return "", err
	}
	events := append([]Event(nil), in.Result.Events...)
	sort.SliceStable(events, func(i, j int) bool {
		if events[i].StartMS != events[j].StartMS {
			return events[i].StartMS < events[j].StartMS
		}
		if events[i].EndMS != events[j].EndMS {
			return events[i].EndMS < events[j].EndMS
		}
		if events[i].EventType != events[j].EventType {
			return events[i].EventType < events[j].EventType
		}
		return events[i].ID < events[j].ID
	})
	for _, event := range events {
		payload := event.Payload
		if payload == nil {
			payload = map[string]any{}
		}
		payloadJSON, err := json.Marshal(payload)
		if err != nil {
			return "", err
		}
		var confidence any
		if event.Confidence != nil {
			confidence = *event.Confidence
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO audio_analysis_events(id,run_id,event_type,start_ms,end_ms,confidence,payload_json)
			 VALUES(?,?,?,?,?,?,?)`,
			event.ID, runID, event.EventType, event.StartMS, event.EndMS, confidence, string(payloadJSON)); err != nil {
			return "", err
		}
	}
	for eventID, vector := range in.Embeddings {
		if err := validateEmbedding(vector); err != nil {
			return "", err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO audio_speaker_embeddings(event_id,dims,vector) VALUES(?,?,?)`,
			eventID, len(vector), encodeVector(vector)); err != nil {
			return "", err
		}
	}
	resultJSON, err := json.Marshal(in.Result)
	if err != nil {
		return "", err
	}
	runStatus := StatusFailed
	active := 0
	errorCode := any(nil)
	currentRun := any(nil)
	if in.Result.Status == StatusSucceeded {
		runStatus = StatusSucceeded
		active = 1
		currentRun = runID
	} else if in.Result.Error != nil {
		errorCode = in.Result.Error.Code
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE audio_analysis_runs SET status=?,active=?,result_json=?,error_code=?,updated_at=? WHERE id=?`,
		runStatus, active, string(resultJSON), errorCode, nowMS, runID); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE audio_analysis_heads SET current_run_id=? WHERE recording_id=? AND capability=? AND desired_job_id=?`,
		currentRun, recordingID, capability, in.JobID); err != nil {
		return "", err
	}
	if err := process.CompleteTx(ctx, tx, in.JobID, in.LeaseToken, "audio:"+runID, now); err != nil {
		if errors.Is(err, process.ErrLeaseLost) {
			return "", NewError(CodeLeaseLost, false, "lease lost before commit")
		}
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return runID, nil
}

// commitGuards enforces the desired head, live lease, retirement and forget
// barrier before any result bytes are written.
func commitGuards(ctx context.Context, tx *sql.Tx, jobID, token, recordingID, capability string, now time.Time) error {
	var desired string
	err := tx.QueryRowContext(ctx,
		`SELECT desired_job_id FROM audio_analysis_heads WHERE recording_id=? AND capability=?`,
		recordingID, capability).Scan(&desired)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return NewError(CodeLeaseLost, false, "no desired head for this run")
		}
		return err
	}
	if desired != jobID {
		return NewError(CodeLeaseLost, false, "a newer generation is desired")
	}
	var status string
	var leaseToken sql.NullString
	var expires sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT status,lease_token,lease_expires_at FROM processing_jobs WHERE id=?`, jobID).
		Scan(&status, &leaseToken, &expires); err != nil {
		return err
	}
	if status != "running" || !leaseToken.Valid || leaseToken.String != token || !expires.Valid || expires.Int64 <= now.UnixMilli() {
		return NewError(CodeLeaseLost, false, "lease is not live")
	}
	var retired int
	if err := tx.QueryRowContext(ctx, `SELECT retired FROM profile_control WHERE id=1`).Scan(&retired); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if retired == 1 {
		return NewError(CodeBarrierBlocked, false, "profile is retired")
	}
	var tombstones int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM forget_tombstones WHERE recording_id=?`, recordingID).Scan(&tombstones); err != nil {
		return err
	}
	if tombstones > 0 {
		return NewError(CodeBarrierBlocked, false, "recording has a permanent-forget tombstone")
	}
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM recordings WHERE id=?`, recordingID).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return NewError(CodeBarrierBlocked, false, "recording no longer exists")
	}
	return nil
}

// verifySourceSnapshot re-checks that every pinned attachment is still active
// with the pinned revision. Any supersede invalidates the whole mapping run.
func verifySourceSnapshot(ctx context.Context, tx *sql.Tx, recordingID string, attachmentIDs []string, snapshotHash string) error {
	current, err := currentAttachmentRefs(ctx, tx, recordingID)
	if err != nil {
		return err
	}
	if len(attachmentIDs) > 0 {
		pinned := make([]AttachmentRef, 0, len(attachmentIDs))
		byID := map[string]string{}
		for _, ref := range current {
			byID[ref.ID] = ref.Revision
		}
		rows, err := tx.QueryContext(ctx,
			`SELECT id,transcript_revision,active FROM source_attachments WHERE id IN (`+placeholders(len(attachmentIDs))+`)`,
			stringsToAny(attachmentIDs)...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id, revision string
			var active int
			if err := rows.Scan(&id, &revision, &active); err != nil {
				rows.Close()
				return err
			}
			if active != 1 {
				rows.Close()
				return NewError(CodeSourceSuperseded, false, "a pinned attachment is no longer active")
			}
			pinned = append(pinned, AttachmentRef{ID: id, Revision: revision})
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if len(pinned) != len(attachmentIDs) {
			return NewError(CodeSourceSuperseded, false, "a pinned attachment is missing")
		}
		computed, err := SnapshotHash(pinned)
		if err != nil {
			return err
		}
		if computed != snapshotHash {
			return NewError(CodeSourceSuperseded, false, "the pinned attachment snapshot changed")
		}
	}
	return nil
}

// Fail records a terminal or retryable worker failure. Retryable failures keep
// the running row so the next attempt reuses it; fatal failures mark the run
// failed and inactive.
func (s *Store) Fail(ctx context.Context, jobID, token, code string, retryable bool, now time.Time) error {
	if now.IsZero() {
		now = s.now()
	}
	nowMS := now.UnixMilli()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if !retryable {
		if _, err := tx.ExecContext(ctx,
			`UPDATE audio_analysis_runs SET status='failed',active=0,error_code=?,updated_at=? WHERE job_id=? AND status='running'`,
			code, nowMS, jobID); err != nil {
			return err
		}
	}
	errorJSON, _ := json.Marshal(map[string]any{"code": code, "retryable": retryable})
	if err := process.FailTx(ctx, tx, jobID, token, string(errorJSON), retryable, now); err != nil {
		if errors.Is(err, process.ErrLeaseLost) {
			return NewError(CodeLeaseLost, false, "lease lost before failure")
		}
		return err
	}
	return tx.Commit()
}

// FailUnclaimed terminates claimable audio jobs of one stage with a safe error
// code. It is used at worker startup when a configured stage is disabled or its
// model became unavailable, so the jobs do not wait forever and their ASR
// dependents can proceed.
func (s *Store) FailUnclaimed(ctx context.Context, stage, code string, now time.Time) error {
	if now.IsZero() {
		now = s.now()
	}
	nowMS := now.UnixMilli()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	jobIDs, err := idsForStage(ctx, tx, stage, nowMS)
	if err != nil {
		return err
	}
	if len(jobIDs) == 0 {
		return tx.Commit()
	}
	errorJSON, _ := json.Marshal(map[string]any{"code": code, "retryable": false})
	for _, jobID := range jobIDs {
		if _, err := tx.ExecContext(ctx,
			`UPDATE audio_analysis_runs SET status='failed',active=0,error_code=?,updated_at=? WHERE job_id=? AND status='running'`,
			code, nowMS, jobID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE processing_jobs SET status='failed',lease_token=NULL,lease_expires_at=NULL,worker_id=NULL,updated_at=?,available_at=? WHERE id=?`,
			nowMS, nowMS, jobID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE processing_attempts SET outcome='fatal_error',error_json=?,finished_at=? WHERE job_id=? AND outcome='running'`,
			string(errorJSON), nowMS, jobID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// idsForStage lists jobs of one stage that are claimable or whose lease already
// expired.
func idsForStage(ctx context.Context, tx *sql.Tx, stage string, nowMS int64) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM processing_jobs
		 WHERE stage=? AND (status IN ('queued','retry_wait') OR (status='running' AND lease_expires_at IS NOT NULL AND lease_expires_at<=?))
		 ORDER BY id`, stage, nowMS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// RunStatus returns the persisted status of the run attached to jobID.
func (s *Store) RunStatus(ctx context.Context, jobID string) (string, error) {
	var status string
	if err := s.DB.QueryRowContext(ctx, `SELECT status FROM audio_analysis_runs WHERE job_id=?`, jobID).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", NewError(CodeLeaseLost, false, "run does not exist")
		}
		return "", err
	}
	return status, nil
}

// RunView is the safe read projection of one run. It never contains vectors.
type RunView struct {
	RunID                string      `json:"run_id"`
	RecordingID          string      `json:"recording_id"`
	Capability           string      `json:"capability"`
	ModelID              string      `json:"model_id"`
	ModelRevision        string      `json:"model_revision"`
	Status               string      `json:"status"`
	Generation           string      `json:"generation"`
	Active               bool        `json:"active"`
	Current              bool        `json:"current"`
	ErrorCode            string      `json:"error_code,omitempty"`
	StartMS              int64       `json:"start_ms"`
	EndMS                int64       `json:"end_ms"`
	SourceSnapshotSHA256 string      `json:"source_snapshot_sha256,omitempty"`
	CreatedAt            int64       `json:"created_at"`
	UpdatedAt            int64       `json:"updated_at"`
	Events               []EventView `json:"events"`
}

// EventView is one event's public metadata.
type EventView struct {
	ID          string `json:"id"`
	RunID       string `json:"run_id"`
	RecordingID string `json:"recording_id"`
	EventType   string `json:"event_type"`
	// StartMS/EndMS are in the analysed window clock; RunStartMS converts them
	// to the recording clock for playback.
	StartMS    int64          `json:"start_ms"`
	EndMS      int64          `json:"end_ms"`
	RunStartMS int64          `json:"run_start_ms,omitempty"`
	Confidence *float64       `json:"confidence"`
	Payload    map[string]any `json:"payload"`
}

// CurrentRuns returns the current, active result of every capability for one
// recording. A run whose pinned snapshot no longer matches the current
// attachments is not current and is omitted.
func (s *Store) CurrentRuns(ctx context.Context, recordingID string) ([]RunView, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT r.id,r.recording_id,r.capability,r.model_id,r.model_revision,r.status,r.generation,r.error_code,
		        r.start_ms,r.end_ms,r.source_snapshot_sha256,r.created_at,r.updated_at
		 FROM audio_analysis_runs r
		 JOIN audio_analysis_heads h ON h.recording_id=r.recording_id AND h.capability=r.capability AND h.current_run_id=r.id
		 WHERE r.recording_id=? AND r.active=1 AND r.status='succeeded'
		 ORDER BY r.capability`, recordingID)
	if err != nil {
		return nil, err
	}
	views := []RunView{}
	for rows.Next() {
		var view RunView
		var errorCode sql.NullString
		if err := rows.Scan(&view.RunID, &view.RecordingID, &view.Capability, &view.ModelID, &view.ModelRevision,
			&view.Status, &view.Generation, &errorCode, &view.StartMS, &view.EndMS, &view.SourceSnapshotSHA256,
			&view.CreatedAt, &view.UpdatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		view.ErrorCode = errorCode.String
		view.Active = true
		views = append(views, view)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	filtered := make([]RunView, 0, len(views))
	for _, view := range views {
		current, err := s.runSourceCurrent(ctx, view)
		if err != nil {
			return nil, err
		}
		if !current {
			continue
		}
		view.Current = true
		events, err := s.eventsForRun(ctx, view.RunID)
		if err != nil {
			return nil, err
		}
		view.Events = events
		filtered = append(filtered, view)
	}
	return filtered, nil
}

// runSourceCurrent reports whether the run's pinned snapshot still matches the
// current attachments and the recording has no active forget barrier.
func (s *Store) runSourceCurrent(ctx context.Context, view RunView) (bool, error) {
	var tombstones int
	if err := s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM forget_tombstones WHERE recording_id=?`, view.RecordingID).Scan(&tombstones); err != nil {
		return false, err
	}
	if tombstones > 0 {
		return false, nil
	}
	if view.Capability != CapabilitySpeaker || view.SourceSnapshotSHA256 == "" {
		return true, nil
	}
	var attachmentsJSON string
	if err := s.DB.QueryRowContext(ctx, `SELECT source_attachment_ids_json FROM audio_analysis_runs WHERE id=?`, view.RunID).Scan(&attachmentsJSON); err != nil {
		return false, err
	}
	var ids []string
	if err := json.Unmarshal([]byte(attachmentsJSON), &ids); err != nil {
		return false, err
	}
	if len(ids) == 0 {
		return true, nil
	}
	refs := []AttachmentRef{}
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id,transcript_revision FROM source_attachments WHERE id IN (`+placeholders(len(ids))+`) AND active=1`,
		stringsToAny(ids)...)
	if err != nil {
		return false, err
	}
	for rows.Next() {
		var id, revision string
		if err := rows.Scan(&id, &revision); err != nil {
			rows.Close()
			return false, err
		}
		refs = append(refs, AttachmentRef{ID: id, Revision: revision})
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	if len(refs) != len(ids) {
		return false, nil
	}
	hash, err := SnapshotHash(refs)
	if err != nil {
		return false, err
	}
	return hash == view.SourceSnapshotSHA256, nil
}

// eventsForRun loads one run's events in stable order.
func (s *Store) eventsForRun(ctx context.Context, runID string) ([]EventView, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id,run_id,event_type,start_ms,end_ms,confidence,payload_json
		 FROM audio_analysis_events WHERE run_id=? ORDER BY start_ms,end_ms,event_type,id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []EventView{}
	for rows.Next() {
		var view EventView
		var confidence sql.NullFloat64
		var payload string
		if err := rows.Scan(&view.ID, &view.RunID, &view.EventType, &view.StartMS, &view.EndMS, &confidence, &payload); err != nil {
			return nil, err
		}
		if confidence.Valid {
			value := confidence.Float64
			view.Confidence = &value
		}
		view.Payload = map[string]any{}
		_ = json.Unmarshal([]byte(payload), &view.Payload)
		out = append(out, view)
	}
	return out, rows.Err()
}

// EventForAudio resolves one readable event, enforcing current run, active
// source and the forget barrier.
func (s *Store) EventForAudio(ctx context.Context, eventID string) (EventView, error) {
	var view EventView
	var confidence sql.NullFloat64
	var payload string
	err := s.DB.QueryRowContext(ctx,
		`SELECT e.id,e.run_id,e.event_type,e.start_ms,e.end_ms,e.confidence,e.payload_json,r.recording_id,r.start_ms
		 FROM audio_analysis_events e
		 JOIN audio_analysis_runs r ON r.id=e.run_id
		 JOIN audio_analysis_heads h ON h.recording_id=r.recording_id AND h.capability=r.capability AND h.current_run_id=r.id
		 WHERE e.id=? AND r.active=1 AND r.status='succeeded'`, eventID).
		Scan(&view.ID, &view.RunID, &view.EventType, &view.StartMS, &view.EndMS, &confidence, &payload,
			&view.RecordingID, &view.RunStartMS)
	if err != nil {
		return EventView{}, err
	}
	if confidence.Valid {
		value := confidence.Float64
		view.Confidence = &value
	}
	view.Payload = map[string]any{}
	_ = json.Unmarshal([]byte(payload), &view.Payload)
	var tombstones int
	if err := s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM forget_tombstones WHERE recording_id=?`, view.RecordingID).Scan(&tombstones); err != nil {
		return EventView{}, err
	}
	if tombstones > 0 {
		return EventView{}, sql.ErrNoRows
	}
	return view, nil
}

// currentAttachmentRefs lists the active attachment ids and revisions of a
// recording.
func currentAttachmentRefs(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, recordingID string) ([]AttachmentRef, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT id,transcript_revision FROM source_attachments WHERE recording_id=? AND active=1 ORDER BY id`, recordingID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	refs := []AttachmentRef{}
	for rows.Next() {
		var ref AttachmentRef
		if err := rows.Scan(&ref.ID, &ref.Revision); err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	return refs, rows.Err()
}

// validateEmbedding enforces a finite, non-zero vector.
func validateEmbedding(vector []float32) error {
	if len(vector) == 0 {
		return NewError(CodeResultInvalid, false, "speaker embedding is empty")
	}
	norm := 0.0
	for _, value := range vector {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return NewError(CodeResultInvalid, false, "speaker embedding is not finite")
		}
		norm += float64(value) * float64(value)
	}
	if norm == 0 {
		return NewError(CodeResultInvalid, false, "speaker embedding has zero norm")
	}
	return nil
}

// encodeVector serialises float32 values little endian.
func encodeVector(vector []float32) []byte {
	out := make([]byte, 4*len(vector))
	for i, value := range vector {
		binary.LittleEndian.PutUint32(out[4*i:], math.Float32bits(value))
	}
	return out
}

func placeholders(n int) string {
	if n <= 0 {
		return "NULL"
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func stringsToAny(values []string) []any {
	out := make([]any, len(values))
	for i, value := range values {
		out[i] = value
	}
	return out
}
