package app

import (
	"context"
	"encoding/json"
	"time"

	"recordingmcp/internal/memory"
	"recordingmcp/internal/process"
)

type ProcessingSnapshot struct {
	Status           memory.ProcessingStatus `json:"status"`
	Jobs             []*process.Job          `json:"jobs"`
	Workers          []process.Heartbeat     `json:"workers"`
	GeneratorEnabled bool                    `json:"generator_enabled"`
}

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
	gen := false
	for _, w := range workers {
		if w.Alive && w.GeneratorEnabled {
			gen = true
		}
	}
	return ProcessingSnapshot{Status: status, Jobs: jobs, Workers: workers, GeneratorEnabled: gen}, nil
}

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
