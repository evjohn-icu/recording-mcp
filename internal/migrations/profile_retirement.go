package migrations

import "fmt"

// profileRetirementStatements adds a durable local-retirement fence. Deletes
// remain available to the resumable purge; every source, job, sync, and
// derived-data insert/update is rejected after retired becomes true.
var profileRetirementStatements = func() []string {
	out := []string{`CREATE TABLE IF NOT EXISTS profile_control(id INTEGER PRIMARY KEY CHECK(id=1), retired INTEGER NOT NULL DEFAULT 0 CHECK(retired IN (0,1)), tombstone_id TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL DEFAULT 0);`}
	tables := []string{"conversations", "recordings", "topics", "topic_conversations", "turns", "turn_topics", "notes", "note_evidence", "source_attachments", "attachment_turns", "processing_jobs", "processing_attempts", "decision_runs", "decision_answers", "memory_candidates", "publication_runs", "analysis_coverage", "memory_links", "annotation_history", "derived_artifacts", "derived_artifact_sources", "speaker_bindings", "speaker_binding_events", "turn_classes", "memory_embeddings", "sync_state", "sync_events", "sync_recordings", "sync_chunks", "ai_note_suppressions"}
	for _, table := range tables {
		for _, op := range []string{"INSERT", "UPDATE"} {
			name := fmt.Sprintf("profile_retired_%s_%s", table, op)
			out = append(out, fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS %s BEFORE %s ON %s WHEN EXISTS(SELECT 1 FROM profile_control WHERE id=1 AND retired=1) BEGIN SELECT RAISE(ABORT,'profile is permanently retired'); END;`, name, op, table))
		}
	}
	return out
}()
