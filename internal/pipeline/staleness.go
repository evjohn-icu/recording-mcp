package pipeline

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

// staleReasonSuperseded marks derived artifacts invalidated because a newer
// transcript revision replaced the attachment they were built from.
const staleReasonSuperseded = "transcript_revision_superseded"

// evidenceAttachmentStates returns the distinct source attachments behind the
// given evidence turns and whether each is still active. Turns without an
// attachment row (legacy imports) contribute nothing, so they leave artifacts
// current with zero source rows.
func evidenceAttachmentStates(ctx context.Context, tx *sql.Tx, turnIDs []string) (map[string]bool, error) {
	states := map[string]bool{}
	if len(turnIDs) == 0 {
		return states, nil
	}
	args := make([]any, len(turnIDs))
	for i, id := range turnIDs {
		args[i] = id
	}
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT at.attachment_id, a.active
		FROM attachment_turns at
		JOIN source_attachments a ON a.id=at.attachment_id
		WHERE at.turn_id IN (`+placeholders(len(turnIDs))+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var attachmentID string
		var active int
		if err := rows.Scan(&attachmentID, &active); err != nil {
			return nil, err
		}
		states[attachmentID] = active == 1
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return states, nil
}

// recordDerivedArtifact records the staleness state of one derived artifact
// and its source attachments inside the caller's transaction. A late write
// whose evidence attachment is already superseded lands stale immediately;
// INSERT OR IGNORE keeps the first-recorded state on replay, because later
// supersedes go through markStaleForAttachment.
func recordDerivedArtifact(ctx context.Context, tx *sql.Tx, kind, artifactID, conversationID string, attachments map[string]bool, now time.Time) error {
	stale := 0
	reason := ""
	var staleAt any
	for _, active := range attachments {
		if !active {
			stale, reason, staleAt = 1, staleReasonSuperseded, now.UnixMilli()
			break
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO derived_artifacts(kind, artifact_id, conversation_id, stale, stale_reason, stale_at) VALUES(?,?,?,?,?,?)`, kind, artifactID, conversationID, stale, reason, staleAt); err != nil {
		return err
	}
	for attachmentID := range attachments {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO derived_artifact_sources(kind, artifact_id, attachment_id) VALUES(?,?,?)`, kind, artifactID, attachmentID); err != nil {
			return err
		}
	}
	return nil
}

// staleArtifactIDs returns the subset of the given derived-artifact ids that is
// currently marked stale. Artifacts without a staleness row are treated as
// current, matching the migration backfill for legacy data.
func staleArtifactIDs(ctx context.Context, db *sql.DB, kind string, ids []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(ids)+1)
	args = append(args, kind)
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := db.QueryContext(ctx, `SELECT artifact_id FROM derived_artifacts WHERE kind=? AND stale=1 AND artifact_id IN (`+placeholders(len(ids))+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// placeholders returns "?,?,..." with n placeholders for IN clauses.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
