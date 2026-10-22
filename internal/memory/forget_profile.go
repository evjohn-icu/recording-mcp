package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// BeginProfileRetirement commits the global write fence before any external
// audio/plane/index cleanup. Every retry uses the same tombstone ID.
func (s *Store) BeginProfileRetirement(ctx context.Context, tombstoneID string) error {
	if tombstoneID == "" {
		return errors.New("profile retirement tombstone ID required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO profile_control(id,retired,tombstone_id,created_at) VALUES(1,1,?,?)`, tombstoneID, time.Now().UnixMilli()); err != nil {
		return err
	}
	var retired int
	var existing string
	if err = tx.QueryRowContext(ctx, `SELECT retired,tombstone_id FROM profile_control WHERE id=1`).Scan(&retired, &existing); err != nil {
		return err
	}
	if retired != 1 {
		return errors.New("profile retirement fence was not stored")
	}
	if existing != tombstoneID {
		return fmt.Errorf("profile already retired under operation %s", existing)
	}
	return tx.Commit()
}

func (s *Store) ProfileRetirement(ctx context.Context) (string, bool, error) {
	var id string
	var retired int
	err := s.db.QueryRowContext(ctx, `SELECT tombstone_id,retired FROM profile_control WHERE id=1`).Scan(&id, &retired)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return id, retired == 1, err
}

// PurgeRetiredProfile removes every canonical SQLite content row while
// retaining only the minimal profile_control retirement marker. It is safe to
// retry after a crash; the purge runs in one transaction.
func (s *Store) PurgeRetiredProfile(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var retired int
	if err = tx.QueryRowContext(ctx, `SELECT retired FROM profile_control WHERE id=1`).Scan(&retired); err != nil {
		return err
	}
	if retired != 1 {
		return errors.New("profile retirement fence is not active")
	}
	tables := []string{
		"decision_answers", "processing_attempts", "note_evidence", "turn_topics", "turn_classes", "analysis_coverage", "attachment_turns", "derived_artifact_sources", "speaker_binding_events",
		"memory_embeddings", "memory_links", "derived_artifacts", "decision_runs", "publication_runs", "memory_candidates", "notes", "speaker_bindings", "turns", "source_attachments", "processing_jobs", "recordings", "topic_conversations", "topics", "conversations", "sync_chunks", "sync_events", "sync_recordings", "sync_state", "ai_note_suppressions", "suppression_cleanup_operations", "annotation_history", "forget_files", "forget_provider_fences", "forget_tombstones",
	}
	for _, table := range tables {
		if _, err = tx.ExecContext(ctx, "DELETE FROM "+table); err != nil {
			return fmt.Errorf("purge profile table %s: %w", table, err)
		}
	}
	return tx.Commit()
}
