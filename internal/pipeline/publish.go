package pipeline

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"recordingmcp/internal/memory"
	"recordingmcp/internal/process"
)

// autoTopicPrefix and autoNotePrefix namespace the deterministic ids of topics
// and notes derived from memory candidates, so they can never collide with
// human-authored annotation ids.
const (
	autoTopicPrefix = "topic:auto:"
	autoNotePrefix  = "note:auto:"
	// autoBasis marks derived annotations that no human has reviewed yet.
	autoBasis = "自动分析；未经人工复核"
)

// PublishHandler is the publish-stage executor: it merges a conversation's
// validated memory candidates into its derived annotation view and records the
// outcome of the attempt.
type PublishHandler struct {
	DB               *sql.DB
	Store            *memory.Store
	ProcessorVersion string
}

// Execute implements worker.Executor. It reads conversation_id from the job
// payload, loads the validated candidates, merges them into the current
// annotations and applies the result. A conversation with no validated
// candidates succeeds without writing anything; a version conflict is recorded
// as a conflict publication run and returned so the worker can retry it.
func (h *PublishHandler) Execute(ctx context.Context, job process.Job) (string, error) {
	if h.DB == nil {
		return "", errors.New("pipeline: PublishHandler.DB is required")
	}
	if h.Store == nil {
		return "", errors.New("pipeline: PublishHandler.Store is required")
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
	return h.publish(ctx, payload.ConversationID, "")
}

// publish merges and applies the validated candidates of one conversation. An
// empty expectedVersion uses the version currently stored for the conversation;
// a non-empty one is compared against it, which is how a stale or concurrent
// writer is detected as a conflict.
func (h *PublishHandler) publish(ctx context.Context, conversationID, expectedVersion string) (string, error) {
	candidates, err := ListCandidates(ctx, h.DB, conversationID, "validated")
	if err != nil {
		return "", fmt.Errorf("pipeline: list candidates: %w", err)
	}
	if len(candidates) == 0 {
		return "publish:noop", nil
	}
	// Superseded evidence must never re-enter the published view: candidates
	// whose backing attachment revision is no longer active are excluded, and
	// the published version is derived only from the candidates that remain.
	validatedIDs := make([]string, 0, len(candidates))
	for _, c := range candidates {
		validatedIDs = append(validatedIDs, c.ID)
	}
	staleIDs, err := staleArtifactIDs(ctx, h.DB, "candidate", validatedIDs)
	if err != nil {
		return "", fmt.Errorf("pipeline: read candidate staleness: %w", err)
	}
	fresh := make([]Candidate, 0, len(candidates))
	for _, c := range candidates {
		if !staleIDs[c.ID] {
			fresh = append(fresh, c)
		}
	}
	if len(fresh) == 0 {
		return "publish:noop", nil
	}
	candidates = fresh

	current, currentVersion, err := h.Store.CurrentAnnotations(ctx, conversationID)
	if err != nil {
		return "", fmt.Errorf("pipeline: read current annotations: %w", err)
	}
	expected := currentVersion
	if expectedVersion != "" {
		expected = expectedVersion
	}

	merged, candidateIDs := mergeCandidates(current, candidates)
	version := "auto-" + sha256Hex(strings.Join(candidateIDs, "|"), 12)
	if strings.TrimSpace(merged.Title) == "" {
		title, err := conversationTitle(ctx, h.DB, conversationID)
		if err != nil {
			return "", err
		}
		merged.Title = title
	}
	merged.ConversationID = conversationID
	merged.AnnotationVersion = version
	merged.AnnotationBasis = autoBasis

	if err := h.Store.ApplyAnnotations(ctx, conversationID, expected, merged); err != nil {
		status := "failed"
		if strings.Contains(err.Error(), "annotation version conflict") {
			status = "conflict"
		}
		rec := PublicationRun{
			ID:              publicationRunID(conversationID, candidateIDs, status),
			ConversationID:  conversationID,
			ExpectedVersion: expected,
			CandidateIDs:    candidateIDs,
			Status:          status,
			Error:           err.Error(),
		}
		if recErr := InsertPublicationRun(ctx, h.DB, rec, time.Now()); recErr != nil {
			return "", fmt.Errorf("pipeline: publish %s: %w (recording publication run: %v)", conversationID, err, recErr)
		}
		return "", fmt.Errorf("pipeline: publish %s: %w", conversationID, err)
	}

	rec := PublicationRun{
		ID:               publicationRunID(conversationID, candidateIDs, "committed"),
		ConversationID:   conversationID,
		ExpectedVersion:  expected,
		CommittedVersion: version,
		CandidateIDs:     candidateIDs,
		Status:           "committed",
	}
	if err := InsertPublicationRun(ctx, h.DB, rec, time.Now()); err != nil {
		return "", fmt.Errorf("pipeline: record publication run: %w", err)
	}
	if err := enqueueCurate(ctx, h.DB, conversationID, version); err != nil {
		return "", err
	}
	return "publish:" + version, nil
}

// mergeCandidates folds validated candidates into the current annotation view.
// Existing topics, notes and turn topics are preserved; new topics and notes
// are appended in candidate order and deterministic ids that already exist are
// skipped, so replaying a publish is idempotent. It returns the merged view and
// the sorted candidate ids that key the derived annotation version.
//
// TODO(revision-aware topics): topic_conversations membership is historical
// taxonomy here — a superseded revision's topic links survive in the merged
// view. Current-state retrieval is still safe because topic membership is
// evaluated over current turns only (memory.CurrentTurnPredicate); a full fix
// would rebuild topic membership per active revision at publish time.
func mergeCandidates(current memory.Annotations, candidates []Candidate) (memory.Annotations, []string) {
	merged := current
	topicIDs := make(map[string]bool, len(merged.Topics))
	for _, t := range merged.Topics {
		topicIDs[t.ID] = true
	}
	noteIDs := make(map[string]bool, len(merged.Notes))
	for _, n := range merged.Notes {
		noteIDs[n.ID] = true
	}

	ids := make([]string, 0, len(candidates))
	for _, c := range candidates {
		ids = append(ids, c.ID)

		topicID := ""
		if title := strings.TrimSpace(c.TopicTitle); title != "" {
			topicID = autoTopicPrefix + sha256Hex(title, 16)
			if !topicIDs[topicID] {
				merged.Topics = append(merged.Topics, memory.TopicAnnotation{ID: topicID, Title: c.TopicTitle})
				topicIDs[topicID] = true
			}
		}

		noteID := autoNotePrefix + sha256Hex(c.ID, 16)
		if noteIDs[noteID] || strings.TrimSpace(c.Summary) == "" {
			continue
		}
		var topicRefs []string
		if topicID != "" {
			topicRefs = []string{topicID}
		}
		merged.Notes = append(merged.Notes, memory.NoteAnnotation{
			ID:              noteID,
			Kind:            c.Kind,
			TopicIDs:        topicRefs,
			Summary:         c.Summary,
			EvidenceTurnIDs: append([]string(nil), c.EvidenceTurnIDs...),
		})
		noteIDs[noteID] = true
	}
	sort.Strings(ids)
	return merged, ids
}

// conversationTitle reads the display title of a conversation so a first
// automatic publish can satisfy the annotation contract even when the stored
// annotations blob carries no title yet.
func conversationTitle(ctx context.Context, db *sql.DB, conversationID string) (string, error) {
	var title string
	if err := db.QueryRowContext(ctx, `SELECT title FROM conversations WHERE id = ?`, conversationID).Scan(&title); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("pipeline: conversation not found: %s", conversationID)
		}
		return "", fmt.Errorf("pipeline: read conversation title: %w", err)
	}
	return title, nil
}

// publicationRunID derives the deterministic id of a publication attempt from
// the conversation, the candidate set and the outcome. A repeat of the same
// attempt replaces its own row rather than accumulating duplicates, while a
// later attempt with a different outcome keeps its own history row.
func publicationRunID(conversationID string, candidateIDs []string, status string) string {
	parts := []string{conversationID, strings.Join(candidateIDs, ","), status}
	return "pub:" + sha256Hex(strings.Join(parts, "|"), 32)
}

// sha256Hex returns the first n lowercase hex characters of sha256(s).
func sha256Hex(s string, n int) string {
	sum := sha256.Sum256([]byte(s))
	full := hex.EncodeToString(sum[:])
	if n > len(full) {
		n = len(full)
	}
	return full[:n]
}
