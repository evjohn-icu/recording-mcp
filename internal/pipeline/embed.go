package pipeline

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"recordingmcp/internal/process"
	"recordingmcp/internal/semantic"
)

// EmbedHandler is the embed-stage executor: it refreshes the semantic index
// for one conversation. It reuses semantic.Indexer, so indexing stays
// content-hash idempotent and stale artifacts are excluded. A nil Provider
// succeeds with a "embed:skipped:no-provider" reference: indexing is disabled
// by configuration, not a failure.
type EmbedHandler struct {
	DB       *sql.DB
	Provider semantic.Provider
}

// Execute implements worker.Executor. It reads conversation_id from the job
// payload and re-indexes that conversation's notes and turns. The returned
// reference is "embed:<provider>/<model>+notes:<n>+turns:<n>+skipped:<n>+pruned:<n>".
func (h *EmbedHandler) Execute(ctx context.Context, job process.Job) (string, error) {
	if h.DB == nil {
		return "", errors.New("pipeline: EmbedHandler.DB is required")
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
	if h.Provider == nil {
		return "embed:skipped:no-provider", nil
	}
	ix := &semantic.Indexer{DB: h.DB, Provider: h.Provider}
	rep, err := ix.IndexConversation(ctx, payload.ConversationID)
	if err != nil {
		return "", fmt.Errorf("pipeline: embed index conversation: %w", err)
	}
	pruned, err := ix.PruneConversation(ctx, payload.ConversationID)
	if err != nil {
		return "", fmt.Errorf("pipeline: embed prune conversation: %w", err)
	}
	return fmt.Sprintf("embed:%s/%s+notes:%d+turns:%d+skipped:%d+pruned:%d", rep.ProviderID, rep.Model, rep.Notes, rep.Turns, rep.Skipped, pruned), nil
}

// enqueueEmbed schedules the embed stage for one conversation. The caller
// derives the idempotency key from what triggered the refresh (ingested
// attachment or published annotation version), so replays enqueue no new job.
func enqueueEmbed(ctx context.Context, db *sql.DB, conversationID, idempotencyKey string) error {
	payload, err := json.Marshal(struct {
		ConversationID string `json:"conversation_id"`
	}{ConversationID: conversationID})
	if err != nil {
		return fmt.Errorf("pipeline: encode embed payload: %w", err)
	}
	spec := process.Spec{
		Stage:           "embed",
		InputSnapshotID: conversationID,
		IdempotencyKey:  idempotencyKey,
		PayloadJSON:     string(payload),
		MaxAttempts:     3,
	}
	if _, _, err := process.Enqueue(ctx, db, spec, time.Now()); err != nil {
		return fmt.Errorf("pipeline: enqueue embed: %w", err)
	}
	return nil
}
