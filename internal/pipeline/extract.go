package pipeline

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"recordingmcp/internal/decision"
	"recordingmcp/internal/process"
)

// maxTurnsPerWindow bounds how many turns one engine evaluation receives.
const maxTurnsPerWindow = 200

// defaultMaxChars bounds the rendered state handed to the engine.
const defaultMaxChars = 8000

// ExtractHandler is the extract-stage executor: it turns one conversation job
// into recorded decision-engine evaluations.
type ExtractHandler struct {
	DB               *sql.DB
	Engine           decision.Engine
	Endpoint         string
	ProcessorVersion string
}

// Execute implements worker.Executor. It reads conversation_id from the job
// payload, evaluates every window and records each result. A conversation with
// no turns succeeds with an empty result reference; any load, render, engine or
// persistence failure is returned so the worker can retry it.
func (h *ExtractHandler) Execute(ctx context.Context, job process.Job) (string, error) {
	if h.DB == nil {
		return "", errors.New("pipeline: ExtractHandler.DB is required")
	}
	if h.Engine == nil {
		return "", errors.New("pipeline: ExtractHandler.Engine is required")
	}
	var payload struct {
		ConversationID string `json:"conversation_id"`
	}
	if err := json.Unmarshal([]byte(job.PayloadJSON), &payload); err != nil {
		return "", fmt.Errorf("pipeline: decode payload: %w", err)
	}
	if payload.ConversationID == "" {
		return "", errors.New("pipeline: payload.conversation_id is required")
	}

	turns, err := LoadConversationTurns(ctx, h.DB, payload.ConversationID)
	if err != nil {
		return "", fmt.Errorf("pipeline: load turns: %w", err)
	}
	if len(turns) == 0 {
		return "", nil
	}

	questions := BuildQuestions()
	questionsJSON, err := json.Marshal(questions)
	if err != nil {
		return "", err
	}

	windows := BuildWindows(turns, maxTurnsPerWindow)
	if len(windows) == 0 {
		return "decision-run:none", nil
	}

	lastRunID := ""
	for _, w := range windows {
		state := RenderState(turns, w, defaultMaxChars)
		result, err := h.Engine.Evaluate(ctx, state, questions)
		if err != nil {
			return "", fmt.Errorf("pipeline: evaluate window: %w", err)
		}
		usageJSON, err := json.Marshal(result.Usage)
		if err != nil {
			return "", err
		}
		weightsVersion := result.WeightsVersion
		if weightsVersion == "" {
			weightsVersion = h.ProcessorVersion
		}
		runID, err := RecordDecisionRun(ctx, h.DB, RunRecord{
			JobID:           job.ID,
			InputSnapshotID: job.InputSnapshotID,
			InputHash:       result.InputHash,
			Model:           result.Model,
			WeightsVersion:  weightsVersion,
			Endpoint:        h.Endpoint,
			QuestionsJSON:   string(questionsJSON),
			UsageJSON:       string(usageJSON),
			Attempt:         job.AttemptCount,
			Answers:         result.Answers,
		}, time.Now())
		if err != nil {
			return "", fmt.Errorf("pipeline: record decision run: %w", err)
		}
		lastRunID = runID
	}
	return "decision-run:" + lastRunID, nil
}
