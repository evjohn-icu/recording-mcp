package pipeline

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"recordingmcp/internal/decision"
	"recordingmcp/internal/generate"
	"recordingmcp/internal/process"
)

// maxTurnsPerWindow bounds how many turns one engine evaluation receives.
const maxTurnsPerWindow = 200

// defaultMaxChars bounds the rendered state handed to the engine.
const defaultMaxChars = 8000

// maxCandidateSummaryRunes bounds the summary length of a memory candidate.
const maxCandidateSummaryRunes = 1000

// ExtractHandler is the extract-stage executor: it turns one conversation job
// into recorded decision-engine evaluations. When Generator is set it also
// derives memory candidates from each window's rendered state, validates and
// persists them, and enqueues one idempotent publish job per window.
type ExtractHandler struct {
	DB               *sql.DB
	Engine           decision.Engine
	Endpoint         string
	ProcessorVersion string
	// Generator proposes memory candidates for a rendered window. A nil
	// Generator skips candidate generation entirely.
	Generator generate.Generator
}

// Execute implements worker.Executor. It reads conversation_id from the job
// payload, evaluates every window and records each result. A conversation with
// no turns succeeds with an empty result reference; any load, render, engine,
// generation or persistence failure is returned so the worker can retry it.
//
// With a Generator configured, every window additionally yields validated
// memory candidates and a publish job. The returned reference is
// "decision-run:<runID>+candidates:<validatedCount>", where validatedCount is
// the number of candidates that passed validation across all windows.
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
	validatedCount := 0
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

		if h.Generator == nil {
			continue
		}
		drafts, model, err := h.Generator.Generate(ctx, state, w.TurnIDs)
		if err != nil {
			return "", fmt.Errorf("pipeline: generate candidates: %w", err)
		}
		validated := validateDrafts(drafts, w.TurnIDs)
		if _, err := InsertCandidates(ctx, h.DB, payload.ConversationID, runID, job.ID, model, validated, time.Now()); err != nil {
			return "", fmt.Errorf("pipeline: insert candidates: %w", err)
		}
		validatedCount += countValidated(validated)
		if err := enqueuePublish(ctx, h.DB, payload.ConversationID, runID); err != nil {
			return "", err
		}
	}

	if h.Generator == nil {
		return "decision-run:" + lastRunID, nil
	}
	return "decision-run:" + lastRunID + "+candidates:" + strconv.Itoa(validatedCount), nil
}

// validateDrafts applies the candidate contract to every generated draft:
// the kind must be one of the accepted kinds, the summary non-empty and at
// most maxCandidateSummaryRunes runes, and the evidence a non-empty subset of
// the window's turn ids. A draft that violates any rule is marked rejected
// with a reason; everything else is validated.
func validateDrafts(drafts []generate.Draft, windowTurnIDs []string) []ValidatedDraft {
	allowed := make(map[string]bool, len(windowTurnIDs))
	for _, id := range windowTurnIDs {
		allowed[id] = true
	}
	out := make([]ValidatedDraft, 0, len(drafts))
	for _, d := range drafts {
		vd := ValidatedDraft{CandidateDraft: CandidateDraft{
			Kind:               d.Kind,
			Summary:            d.Summary,
			TopicTitle:         d.TopicTitle,
			EvidenceTurnIDs:    d.EvidenceTurnIDs,
			InterpretationNote: d.InterpretationNote,
		}}
		switch {
		case !generate.ValidKind(d.Kind):
			vd.Status = "rejected"
			vd.RejectionReason = fmt.Sprintf("invalid kind %q", d.Kind)
		case strings.TrimSpace(d.Summary) == "":
			vd.Status = "rejected"
			vd.RejectionReason = "empty summary"
		case utf8.RuneCountInString(d.Summary) > maxCandidateSummaryRunes:
			vd.Status = "rejected"
			vd.RejectionReason = fmt.Sprintf("summary exceeds %d runes", maxCandidateSummaryRunes)
		case len(d.EvidenceTurnIDs) == 0:
			vd.Status = "rejected"
			vd.RejectionReason = "no evidence turn ids"
		default:
			bad := ""
			for _, id := range d.EvidenceTurnIDs {
				if !allowed[id] {
					bad = id
					break
				}
			}
			if bad != "" {
				vd.Status = "rejected"
				vd.RejectionReason = fmt.Sprintf("evidence turn id %q not in window", bad)
			} else {
				vd.Status = "validated"
			}
		}
		out = append(out, vd)
	}
	return out
}

// countValidated reports how many drafts passed validation.
func countValidated(drafts []ValidatedDraft) int {
	n := 0
	for _, d := range drafts {
		if d.Status == "validated" {
			n++
		}
	}
	return n
}

// enqueuePublish schedules the publish stage for one conversation and decision
// run. The idempotency key ties the job to that run so replays of the same
// extract attempt never enqueue a duplicate.
func enqueuePublish(ctx context.Context, db *sql.DB, conversationID, runID string) error {
	payload, err := json.Marshal(struct {
		ConversationID string `json:"conversation_id"`
	}{ConversationID: conversationID})
	if err != nil {
		return fmt.Errorf("pipeline: encode publish payload: %w", err)
	}
	spec := process.Spec{
		Stage:           "publish",
		InputSnapshotID: conversationID,
		IdempotencyKey:  "publish|" + conversationID + "|" + runID,
		PayloadJSON:     string(payload),
		MaxAttempts:     5,
	}
	if _, _, err := process.Enqueue(ctx, db, spec, time.Now()); err != nil {
		return fmt.Errorf("pipeline: enqueue publish: %w", err)
	}
	return nil
}
