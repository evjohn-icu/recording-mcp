package app

import (
	"context"
	"encoding/json"
	"time"

	"recordingmcp/internal/memory"
	"recordingmcp/internal/process"
)

// ProcessingPhase is one user-facing step of the ingest pipeline. The UI shows
// these in fixed order instead of the raw job rows, so "waiting for the
// worker", "failed with an action" and "skipped because indexing is off" are
// distinguishable without reading SQL state.
type ProcessingPhase struct {
	Name        string    `json:"name"`
	State       string    `json:"state"`
	JobID       string    `json:"job_id,omitempty"`
	Attempt     int       `json:"attempt,omitempty"`
	UpdatedAt   time.Time `json:"updated_at,omitempty"`
	Error       string    `json:"error,omitempty"`
	ErrorCode   string    `json:"error_code,omitempty"`
	ErrorAction string    `json:"error_action,omitempty"`
}

// ProcessingSnapshot is the read model behind the progress panel.
type ProcessingSnapshot struct {
	Status           memory.ProcessingStatus `json:"status"`
	Jobs             []*process.Job          `json:"jobs"`
	Workers          []process.Heartbeat     `json:"workers"`
	GeneratorEnabled bool                    `json:"generator_enabled"`
	Phases           []ProcessingPhase       `json:"phases"`
	CurrentStage     string                  `json:"current_stage"`
	ASRProvider      string                  `json:"asr_provider"`
	ASRAvailable     bool                    `json:"asr_available"`
	WorkerAlive      bool                    `json:"worker_alive"`
}

// phaseOrder is the fixed pipeline order; "index" is the embed stage.
var phaseOrder = []string{"ingest", "asr", "extract", "publish", "index"}

func phaseForStage(stage string) string {
	if stage == "embed" {
		return "index"
	}
	return stage
}

func phaseState(status string) string {
	switch status {
	case "queued", "retry_wait":
		return "pending"
	case "running":
		return "running"
	case "succeeded":
		return "done"
	case "failed", "cancelled":
		return "failed"
	default:
		return "pending"
	}
}

// ProcessingSnapshot reads the status, jobs and worker capabilities, then
// projects them onto the fixed phase list. The raw jobs stay for compatibility
// and debugging; phases are what the UI renders.
func (s *Service) ProcessingSnapshot(ctx context.Context, conversationID string) (ProcessingSnapshot, error) {
	status, err := s.Store.GetProcessingStatus(ctx, conversationID)
	if err != nil {
		return ProcessingSnapshot{}, err
	}
	jobs, err := process.ListForConversation(ctx, s.Store.DB(), conversationID, 100)
	if err != nil {
		return ProcessingSnapshot{}, err
	}
	if jobs == nil {
		jobs = []*process.Job{}
	}
	now := time.Now()
	workers, err := process.ReadHeartbeats(ctx, s.Store.DB(), now, 30*time.Second)
	if err != nil {
		return ProcessingSnapshot{}, err
	}
	if workers == nil {
		workers = []process.Heartbeat{}
	}
	out := ProcessingSnapshot{
		Status: status, Jobs: jobs, Workers: workers,
		ASRProvider: s.ASRProvider,
	}
	for _, w := range workers {
		if !w.Alive {
			continue
		}
		out.WorkerAlive = true
		if w.GeneratorEnabled {
			out.GeneratorEnabled = true
		}
		if w.ASRAvailable {
			out.ASRAvailable = true
		}
	}

	// The job list is capped, so derive phases from a per-stage lookup. With
	// hundreds of extract/embed rows the newest 100 entries can omit ingest and
	// asr entirely, which would leave the UI stuck on a pending phase.
	stageJobs, err := process.LatestByStage(ctx, s.Store.DB(), conversationID, []string{"ingest", "asr", "extract", "publish", "embed"})
	if err != nil {
		return ProcessingSnapshot{}, err
	}
	latest := map[string]*process.Job{}
	for stage, job := range stageJobs {
		latest[phaseForStage(stage)] = job
	}
	phases := make([]ProcessingPhase, 0, len(phaseOrder))
	failed := false
	for i, name := range phaseOrder {
		phase := ProcessingPhase{Name: name, State: "pending"}
		if job, ok := latest[name]; ok {
			phase.State = phaseState(job.Status)
			phase.JobID = job.ID
			phase.Attempt = job.AttemptCount
			phase.UpdatedAt = job.UpdatedAt
			if phase.State == "failed" {
				if failure, err := process.LastError(ctx, s.Store.DB(), job.ID); err == nil {
					phase.Error = failure.Error
					phase.ErrorCode = failure.Code
					phase.ErrorAction = failure.Action
				}
			}
		} else if failed {
			// Nothing queued after a failure: later stages are skipped, not
			// pending, so the UI does not imply work is still coming.
			phase.State = "skipped"
		} else if i > 0 {
			previous := phases[i-1].State
			switch name {
			case "publish":
				if previous == "done" {
					phase.State = "skipped"
				}
			case "index":
				if previous == "done" || previous == "skipped" {
					phase.State = "skipped"
				}
			}
		}
		if phase.State == "failed" {
			failed = true
		}
		phases = append(phases, phase)
	}
	out.Phases = phases
	out.CurrentStage = "completed"
	for _, phase := range phases {
		if phase.State == "running" || phase.State == "failed" || phase.State == "pending" {
			out.CurrentStage = phase.Name
			break
		}
	}
	return out, nil
}

// RetryProcessing requeues a failed job after checking that its source snapshot
// is still current. Extract jobs snapshot an attachment, later stages snapshot
// the conversation itself.
func (s *Service) RetryProcessing(ctx context.Context, conversationID, jobID string, expectedAttempt int, requestID string) (*process.Job, error) {
	job, err := process.Get(ctx, s.Store.DB(), jobID)
	if err != nil {
		return nil, err
	}
	var payload struct {
		ConversationID string `json:"conversation_id"`
	}
	if err := json.Unmarshal([]byte(job.PayloadJSON), &payload); err != nil {
		return nil, err
	}
	if payload.ConversationID != "" && payload.ConversationID != conversationID {
		return nil, process.ErrStaleSource
	}
	if job.Stage == "extract" {
		var active int
		if err := s.Store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM source_attachments WHERE id=? AND conversation_id=? AND active=1`, job.InputSnapshotID, conversationID).Scan(&active); err != nil {
			return nil, err
		}
		if active != 1 {
			return nil, process.ErrStaleSource
		}
	} else if job.InputSnapshotID != conversationID {
		return nil, process.ErrStaleSource
	}
	var exists int
	if err := s.Store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM conversations WHERE id=? AND recording_id IS NOT NULL`, conversationID).Scan(&exists); err != nil {
		return nil, err
	}
	if exists != 1 {
		return nil, process.ErrStaleSource
	}
	return process.RetryFailedForConversation(ctx, s.Store.DB(), conversationID, jobID, expectedAttempt, requestID, time.Now())
}
