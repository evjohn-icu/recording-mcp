package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"recordingmcp/internal/app"
	"recordingmcp/internal/decision"
	"recordingmcp/internal/memory"
	"recordingmcp/internal/plane"
	"recordingmcp/internal/process"
	"recordingmcp/internal/worker"
)

// WorldlineJobExecutor is scoped to one already-resolved profile. A job cannot
// choose a database/event-plane path; its scope must match this trusted worker.
type WorldlineJobExecutor struct {
	Store      *memory.Store
	Scope      string
	EventsPath string
	Engine     decision.Engine
}

type worldlineStageRouter struct {
	next      worker.Executor
	worldline worker.Executor
}

func routeWorldlineJobs(next worker.Executor, worldline worker.Executor) worker.Executor {
	return &worldlineStageRouter{next: next, worldline: worldline}
}
func (r *worldlineStageRouter) Execute(ctx context.Context, job process.Job) (string, error) {
	if job.Stage == "worldline" {
		return r.worldline.Execute(ctx, job)
	}
	return r.next.Execute(ctx, job)
}

func (e *WorldlineJobExecutor) Execute(ctx context.Context, job process.Job) (string, error) {
	if e == nil || e.Store == nil || strings.TrimSpace(e.Scope) == "" || strings.TrimSpace(e.EventsPath) == "" {
		return "", errors.New("worldline worker requires a scoped store and event plane")
	}
	if job.Stage != "worldline" {
		return "", fmt.Errorf("worldline worker cannot handle stage %q", job.Stage)
	}
	var payload struct {
		Scope     string `json:"scope"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal([]byte(job.PayloadJSON), &payload); err != nil {
		return "", err
	}
	if payload.Scope != e.Scope {
		return "", errors.New("worldline job scope does not match worker profile")
	}
	if strings.TrimSpace(payload.RequestID) == "" {
		return "", errors.New("worldline request_id required")
	}
	events, err := plane.Open(e.EventsPath)
	if err != nil {
		return "", err
	}
	defer events.Close()
	svc := &app.WorldlineService{Time: &app.TimeService{Store: e.Store, Events: events}, DecisionEngine: e.Engine}
	result, err := svc.RunAnalysisJob(ctx, e.Scope, payload.RequestID)
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(result)
	return string(raw), err
}
