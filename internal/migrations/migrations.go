// Package migrations applies ordered schema migrations to the recordingmcp
// SQLite database. Each migration runs inside a transaction and is recorded
// in schema_migrations, so Apply is idempotent and safe to call on every open.
package migrations

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// baseSchemaDDL is the original inline DDL from memory.Open, moved verbatim.
const baseSchemaDDL = `CREATE TABLE IF NOT EXISTS conversations(id TEXT PRIMARY KEY,title TEXT NOT NULL,summary TEXT NOT NULL,annotation_version TEXT NOT NULL,annotation_basis TEXT NOT NULL,recording_id TEXT NOT NULL,source_sha256 TEXT NOT NULL,transcript_version TEXT NOT NULL,duration_ms INTEGER NOT NULL,transcript_json TEXT NOT NULL,annotations_json TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS recordings(id TEXT PRIMARY KEY,conversation_id TEXT NOT NULL REFERENCES conversations(id),source_file TEXT NOT NULL,source_sha256 TEXT NOT NULL,duration_ms INTEGER NOT NULL,recording_started_at TEXT NULL,absolute_time_known INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS topics(id TEXT PRIMARY KEY,title TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS topic_conversations(topic_id TEXT NOT NULL REFERENCES topics(id),conversation_id TEXT NOT NULL REFERENCES conversations(id),PRIMARY KEY(topic_id,conversation_id));
CREATE TABLE IF NOT EXISTS turns(id TEXT PRIMARY KEY,conversation_id TEXT NOT NULL REFERENCES conversations(id),recording_id TEXT NOT NULL REFERENCES recordings(id),segment_index INTEGER NOT NULL,global_start_ms INTEGER NULL,global_end_ms INTEGER NULL,local_start_ms INTEGER NULL,local_end_ms INTEGER NULL,speaker_scoped_id TEXT NULL,speaker_entity_id TEXT NULL CHECK(speaker_entity_id IS NULL),content TEXT NOT NULL,definite INTEGER NULL,source_sha256 TEXT NOT NULL,transcript_version TEXT NOT NULL,original_turn_id TEXT NOT NULL,turn_json TEXT NOT NULL,UNIQUE(recording_id,original_turn_id));
CREATE TABLE IF NOT EXISTS turn_topics(turn_id TEXT NOT NULL REFERENCES turns(id),topic_id TEXT NOT NULL REFERENCES topics(id),PRIMARY KEY(turn_id,topic_id));
CREATE TABLE IF NOT EXISTS notes(id TEXT PRIMARY KEY,conversation_id TEXT NOT NULL REFERENCES conversations(id),kind TEXT NOT NULL CHECK(kind IN ('observation','suggestion','request','constraint','intention')),summary TEXT NOT NULL,topic_ids_json TEXT NOT NULL,claim_mode TEXT NOT NULL CHECK(claim_mode='inference'),interpretation_note TEXT NOT NULL,annotation_version TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS note_evidence(note_id TEXT NOT NULL REFERENCES notes(id),turn_id TEXT NOT NULL REFERENCES turns(id),ordinal INTEGER NOT NULL,PRIMARY KEY(note_id,turn_id));
CREATE INDEX IF NOT EXISTS turns_conv_order ON turns(conversation_id,global_start_ms,id);
CREATE INDEX IF NOT EXISTS notes_conv ON notes(conversation_id,id);
CREATE TABLE IF NOT EXISTS source_attachments(
 id TEXT PRIMARY KEY, conversation_id TEXT NOT NULL REFERENCES conversations(id), recording_id TEXT NOT NULL REFERENCES recordings(id), segment_index INTEGER NOT NULL,
 segment_id TEXT NOT NULL, transcript_revision TEXT NOT NULL, source_json TEXT NOT NULL, result_json TEXT NOT NULL, content_sha256 TEXT NOT NULL,
 active INTEGER NOT NULL DEFAULT 1, supersedes_revision TEXT NULL, UNIQUE(recording_id,segment_id,transcript_revision));
CREATE INDEX IF NOT EXISTS source_attachments_conversation ON source_attachments(conversation_id,recording_id);
CREATE TABLE IF NOT EXISTS attachment_turns(attachment_id TEXT NOT NULL REFERENCES source_attachments(id),turn_id TEXT NOT NULL REFERENCES turns(id),PRIMARY KEY(attachment_id,turn_id));`

// processingQueueDDL adds the processing job queue and its attempt log.
const processingQueueDDL = `CREATE TABLE IF NOT EXISTS processing_jobs(id TEXT PRIMARY KEY, stage TEXT NOT NULL, input_snapshot_id TEXT NOT NULL, idempotency_key TEXT NOT NULL UNIQUE, status TEXT NOT NULL CHECK(status IN ('queued','running','succeeded','retry_wait','failed','cancelled')), priority INTEGER NOT NULL DEFAULT 0, payload_json TEXT NOT NULL DEFAULT '{}', max_attempts INTEGER NOT NULL DEFAULT 3, attempt_count INTEGER NOT NULL DEFAULT 0, available_at INTEGER NOT NULL, lease_token TEXT NULL, lease_expires_at INTEGER NULL, worker_id TEXT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS processing_attempts(id TEXT PRIMARY KEY, job_id TEXT NOT NULL REFERENCES processing_jobs(id), attempt INTEGER NOT NULL, worker_id TEXT NOT NULL, lease_token TEXT NOT NULL, processor_version TEXT NOT NULL, started_at INTEGER NOT NULL, finished_at INTEGER NULL, outcome TEXT NOT NULL CHECK(outcome IN ('running','succeeded','retryable_error','fatal_error','lease_lost','cancelled')), error_json TEXT NULL, result_ref TEXT NULL, usage_json TEXT NULL, UNIQUE(job_id,attempt));
CREATE INDEX IF NOT EXISTS processing_jobs_claim ON processing_jobs(status, priority DESC, available_at);`

// decisionLogDDL adds the decision-run log: one row per (job, attempt, input)
// evaluation plus one row per answered question. It is append-only; reruns
// with the same identity keep the first run row and replace its answers.
const decisionLogDDL = `CREATE TABLE IF NOT EXISTS decision_runs(id TEXT PRIMARY KEY, job_id TEXT NULL, attempt INTEGER NOT NULL DEFAULT 0, input_snapshot_id TEXT NOT NULL, input_hash TEXT NOT NULL, model TEXT NOT NULL, weights_version TEXT NOT NULL DEFAULT '', endpoint TEXT NOT NULL DEFAULT '', questions_json TEXT NOT NULL, usage_json TEXT NOT NULL DEFAULT '{}', created_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS decision_answers(run_id TEXT NOT NULL REFERENCES decision_runs(id), question_id TEXT NOT NULL, question_type TEXT NOT NULL, answer_json TEXT NOT NULL, distribution_json TEXT NOT NULL, PRIMARY KEY(run_id, question_id));`

// memoryCandidatesDDL adds the memory-candidate staging table and the
// publication-run log. Candidates are generated by the extract stage and
// validated before they become eligible for publication; publication runs
// record the outcome of merging validated candidates into a conversation's
// annotations.
const memoryCandidatesDDL = `CREATE TABLE IF NOT EXISTS memory_candidates(id TEXT PRIMARY KEY, conversation_id TEXT NOT NULL REFERENCES conversations(id), run_id TEXT NULL, job_id TEXT NULL, kind TEXT NOT NULL, summary TEXT NOT NULL, topic_title TEXT NOT NULL DEFAULT '', evidence_turn_ids_json TEXT NOT NULL, interpretation_note TEXT NOT NULL DEFAULT '', status TEXT NOT NULL CHECK(status IN ('validated','rejected')), rejection_reason TEXT NOT NULL DEFAULT '', generator_model TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS memory_candidates_conv ON memory_candidates(conversation_id, status);
CREATE TABLE IF NOT EXISTS publication_runs(id TEXT PRIMARY KEY, conversation_id TEXT NOT NULL REFERENCES conversations(id), expected_version TEXT NOT NULL, committed_version TEXT NOT NULL DEFAULT '', candidate_ids_json TEXT NOT NULL DEFAULT '[]', status TEXT NOT NULL CHECK(status IN ('committed','conflict','failed')), error TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL);`

type migration struct {
	version    int
	name       string
	statements []string
}

// statement splits a multi-statement DDL script into individual statements.
// The DDL contains no string literals with semicolons, so splitting on ';'
// is safe.
func statements(ddl string) []string {
	parts := strings.Split(ddl, ";")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p+";")
		}
	}
	return out
}

var migrations = []migration{
	{version: 1, name: "base-schema", statements: statements(baseSchemaDDL)},
	{version: 2, name: "processing-queue", statements: statements(processingQueueDDL)},
	{version: 3, name: "decision-log", statements: statements(decisionLogDDL)},
	{version: 4, name: "memory-candidates", statements: statements(memoryCandidatesDDL)},
}

const schemaMigrationsDDL = `CREATE TABLE IF NOT EXISTS schema_migrations(version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at INTEGER NOT NULL);`

// Version returns the highest applied migration version, or 0 when the
// database has never been migrated (schema_migrations absent or empty).
func Version(ctx context.Context, db *sql.DB) (int, error) {
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='schema_migrations'`,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("probe schema_migrations: %w", err)
	}
	if n == 0 {
		return 0, nil
	}
	var v int
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&v); err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	return v, nil
}

// Apply brings the database up to the latest known migration, applying only
// the migrations newer than the currently recorded version. Calling it on an
// up-to-date or empty database is a no-op; calling it after each migration
// commits makes repeated calls idempotent.
func Apply(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, schemaMigrationsDDL); err != nil {
		return fmt.Errorf("ensure schema_migrations: %w", err)
	}
	current, err := Version(ctx, db)
	if err != nil {
		return err
	}
	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		if err := applyOne(ctx, db, m); err != nil {
			return err
		}
		current = m.version
	}
	return nil
}

func applyOne(ctx context.Context, db *sql.DB, m migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("migration %d %q: begin: %w", m.version, m.name, err)
	}
	defer tx.Rollback()
	for _, stmt := range m.statements {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migration %d %q: exec: %w", m.version, m.name, err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations(version, name, applied_at) VALUES(?, ?, ?)`,
		m.version, m.name, time.Now().UnixMilli(),
	); err != nil {
		return fmt.Errorf("migration %d %q: record: %w", m.version, m.name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migration %d %q: commit: %w", m.version, m.name, err)
	}
	return nil
}
