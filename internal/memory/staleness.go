package memory

import (
	"context"
	"database/sql"
	"time"
)

// staleReasonSuperseded marks derived artifacts invalidated because a newer
// transcript revision replaced the attachment they were built from.
const staleReasonSuperseded = "transcript_revision_superseded"

// markStaleForAttachment marks every currently-current derived artifact that
// depends on attachmentID as stale, inside the caller's transaction, and
// returns the number of rows changed. Artifacts already marked stale keep
// their original reason and timestamp; repeats are no-ops.
func markStaleForAttachment(ctx context.Context, tx *sql.Tx, attachmentID, reason string, now time.Time) (int64, error) {
	res, err := tx.ExecContext(ctx, `UPDATE derived_artifacts SET stale=1, stale_reason=?, stale_at=?
		WHERE stale=0 AND EXISTS(SELECT 1 FROM derived_artifact_sources s
			WHERE s.kind=derived_artifacts.kind AND s.artifact_id=derived_artifacts.artifact_id AND s.attachment_id=?)`,
		reason, now.UnixMilli(), attachmentID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// InvalidateDerivedArtifacts is the explicit, idempotent invalidation entry
// point: it marks every derived artifact sourced from attachmentID as stale
// and returns how many rows changed. Repeating the call over the same
// attachment changes nothing (stale rows are skipped), so it can never spawn
// duplicate work.
func (s *Store) InvalidateDerivedArtifacts(ctx context.Context, attachmentID, reason string) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	n, err := markStaleForAttachment(ctx, tx, attachmentID, reason, time.Now())
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int(n), nil
}
