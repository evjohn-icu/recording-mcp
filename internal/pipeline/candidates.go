package pipeline

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Candidate is one persisted memory candidate together with the validation
// outcome recorded when it was stored. RunID and JobID may be empty when the
// candidate was not produced by a processing job; every other field mirrors
// the memory_candidates row.
type Candidate struct {
	ID                 string
	ConversationID     string
	RunID              string
	JobID              string
	Kind               string
	Summary            string
	TopicTitle         string
	EvidenceTurnIDs    []string
	InterpretationNote string
	Status             string
	RejectionReason    string
	GeneratorModel     string
	CreatedAt          time.Time
}

// CandidateDraft is a generated memory candidate before validation. It is the
// payload the generator returns and the input to validation.
type CandidateDraft struct {
	Kind               string
	Summary            string
	TopicTitle         string
	EvidenceTurnIDs    []string
	InterpretationNote string
}

// ValidatedDraft is a candidate draft together with its validation outcome.
// Status is either "validated" or "rejected"; RejectionReason is only
// meaningful for rejected drafts.
type ValidatedDraft struct {
	CandidateDraft
	Status          string
	RejectionReason string
}

// PublicationRun records one attempt to merge a conversation's validated
// candidates into its annotations. Status is one of "committed", "conflict" or
// "failed"; CandidateIDs is the set of candidate ids the attempt considered.
type PublicationRun struct {
	ID               string
	ConversationID   string
	ExpectedVersion  string
	CommittedVersion string
	CandidateIDs     []string
	Status           string
	Error            string
}

// candidateID derives the deterministic id of a memory candidate from its
// content identity tuple. Identical (conversation, kind, summary, evidence)
// always yields the same id regardless of the producing run or attempt, which
// is what makes InsertCandidates idempotent across retries and repeated
// windows.
func candidateID(conversationID, kind, summary, evidenceJSON string) string {
	sum := sha256.Sum256([]byte(strings.Join(
		[]string{conversationID, kind, summary, evidenceJSON}, "|")))
	return "cand:" + hex.EncodeToString(sum[:])[:32]
}

// InsertCandidates stores validated and rejected drafts for one conversation
// and returns their deterministic ids in draft order. Rows are inserted with
// INSERT OR IGNORE so re-inserting the same candidate is a no-op; the returned
// id is stable across repeats. runID and jobID are provenance only: a repeat of
// the same content keeps the first producer's values. Every draft is written in
// a single transaction.
func InsertCandidates(ctx context.Context, db *sql.DB, conversationID, runID, jobID, generatorModel string, drafts []ValidatedDraft, now time.Time) ([]string, error) {
	ids := make([]string, 0, len(drafts))

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("pipeline: insert candidates: begin: %w", err)
	}
	defer tx.Rollback()

	for _, d := range drafts {
		evidenceJSON, err := json.Marshal(evidenceOrEmpty(d.EvidenceTurnIDs))
		if err != nil {
			return nil, fmt.Errorf("pipeline: insert candidates: marshal evidence: %w", err)
		}
		id := candidateID(conversationID, d.Kind, d.Summary, string(evidenceJSON))
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO memory_candidates(id, conversation_id, run_id, job_id, kind, summary, topic_title, evidence_turn_ids_json, interpretation_note, status, rejection_reason, generator_model, created_at)
			 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, conversationID, nullableString(runID), nullableString(jobID), d.Kind, d.Summary,
			d.TopicTitle, string(evidenceJSON), d.InterpretationNote, d.Status, d.RejectionReason,
			generatorModel, now.UnixMilli(),
		); err != nil {
			return nil, fmt.Errorf("pipeline: insert candidates: insert %s: %w", id, err)
		}
		states, err := evidenceAttachmentStates(ctx, tx, d.EvidenceTurnIDs)
		if err != nil {
			return nil, fmt.Errorf("pipeline: insert candidates: evidence attachments %s: %w", id, err)
		}
		if err := recordDerivedArtifact(ctx, tx, "candidate", id, conversationID, states, now); err != nil {
			return nil, fmt.Errorf("pipeline: insert candidates: provenance %s: %w", id, err)
		}
		ids = append(ids, id)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("pipeline: insert candidates: commit: %w", err)
	}
	return ids, nil
}

// ListCandidates loads the candidates of one conversation in deterministic
// order (created_at, then id). An empty status returns candidates of every
// status; otherwise only rows with exactly that status are returned.
func ListCandidates(ctx context.Context, db *sql.DB, conversationID, status string) ([]Candidate, error) {
	query := `SELECT id, conversation_id, run_id, job_id, kind, summary, topic_title, evidence_turn_ids_json, interpretation_note, status, rejection_reason, generator_model, created_at
	          FROM memory_candidates WHERE conversation_id = ?`
	args := []any{conversationID}
	if status != "" {
		query += ` AND status = ?`
		args = append(args, status)
	}
	query += ` ORDER BY created_at, id`

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("pipeline: list candidates: %w", err)
	}
	defer rows.Close()

	var out []Candidate
	for rows.Next() {
		var (
			c              Candidate
			runID, jobID   sql.NullString
			evidenceJSON   string
			createdAtMilli int64
		)
		if err := rows.Scan(&c.ID, &c.ConversationID, &runID, &jobID, &c.Kind, &c.Summary,
			&c.TopicTitle, &evidenceJSON, &c.InterpretationNote, &c.Status, &c.RejectionReason,
			&c.GeneratorModel, &createdAtMilli); err != nil {
			return nil, fmt.Errorf("pipeline: list candidates: scan: %w", err)
		}
		c.RunID = runID.String
		c.JobID = jobID.String
		c.CreatedAt = time.UnixMilli(createdAtMilli)
		if err := json.Unmarshal([]byte(evidenceJSON), &c.EvidenceTurnIDs); err != nil {
			return nil, fmt.Errorf("pipeline: list candidates %s: unmarshal evidence: %w", c.ID, err)
		}
		if c.EvidenceTurnIDs == nil {
			c.EvidenceTurnIDs = []string{}
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pipeline: list candidates: read: %w", err)
	}
	return out, nil
}

// InsertPublicationRun records one publication attempt. A repeat of the same
// id replaces the previous row, so a retried attempt can update its outcome.
func InsertPublicationRun(ctx context.Context, db *sql.DB, rec PublicationRun, now time.Time) error {
	candidateIDsJSON, err := json.Marshal(evidenceOrEmpty(rec.CandidateIDs))
	if err != nil {
		return fmt.Errorf("pipeline: insert publication run: marshal candidate ids: %w", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT OR REPLACE INTO publication_runs(id, conversation_id, expected_version, committed_version, candidate_ids_json, status, error, created_at)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.ID, rec.ConversationID, rec.ExpectedVersion, rec.CommittedVersion,
		string(candidateIDsJSON), rec.Status, rec.Error, now.UnixMilli(),
	); err != nil {
		return fmt.Errorf("pipeline: insert publication run %s: %w", rec.ID, err)
	}
	return nil
}

// evidenceOrEmpty normalizes a nil id slice to an empty slice so stored JSON
// is always an array of the expected shape rather than null.
func evidenceOrEmpty(ids []string) []string {
	if ids == nil {
		return []string{}
	}
	return ids
}
