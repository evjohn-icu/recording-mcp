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

// analysisCoverageDDL adds the per-turn analysis-coverage ledger: one row per
// (conversation, turn, processor version) records that the extract stage has
// already evaluated that turn, so per-segment jobs never re-analyse turns a
// previous run already covered. The primary key makes recording idempotent.
const analysisCoverageDDL = `CREATE TABLE IF NOT EXISTS analysis_coverage(conversation_id TEXT NOT NULL REFERENCES conversations(id), turn_id TEXT NOT NULL REFERENCES turns(id), processor_version TEXT NOT NULL, run_id TEXT NOT NULL DEFAULT '', covered_at INTEGER NOT NULL, PRIMARY KEY(conversation_id, turn_id, processor_version));`

// memoryLinksDDL adds the memory-link staging table: candidate associations
// between two memory records proposed by an agent. Links are recorded as
// suggestions only (status defaults to 'proposed'); nothing is merged
// automatically. Evidence turn ids and the proposing model are kept so a
// human can review the proposal before confirming or rejecting it.
const memoryLinksDDL = `CREATE TABLE IF NOT EXISTS memory_links(id TEXT PRIMARY KEY, from_kind TEXT NOT NULL CHECK(from_kind IN ('topic','conversation','turn','note')), from_id TEXT NOT NULL, to_kind TEXT NOT NULL CHECK(to_kind IN ('topic','conversation','turn','note')), to_id TEXT NOT NULL, relation TEXT NOT NULL, reason TEXT NOT NULL DEFAULT '', evidence_turn_ids_json TEXT NOT NULL DEFAULT '[]', confidence REAL NOT NULL DEFAULT 0, status TEXT NOT NULL DEFAULT 'proposed' CHECK(status IN ('proposed','confirmed','rejected')), model TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS memory_links_from ON memory_links(from_kind, from_id);
CREATE INDEX IF NOT EXISTS memory_links_to ON memory_links(to_kind, to_id);`

// annotationHistoryDDL promotes the lazily created annotation_history table to
// a tracked migration so every database (including ones created by an older
// build that created it on demand) ends up with the same schema. CREATE TABLE
// IF NOT EXISTS keeps this a no-op where the table already exists.
const annotationHistoryDDL = `CREATE TABLE IF NOT EXISTS annotation_history(conversation_id TEXT NOT NULL REFERENCES conversations(id), annotation_version TEXT NOT NULL, annotation_basis TEXT NOT NULL, summary TEXT NOT NULL, annotations_json TEXT NOT NULL, PRIMARY KEY(conversation_id,annotation_version));`

// derivedArtifactsDDL adds per-artifact staleness state plus the source
// attachment links needed to propagate a transcript revision supersede to
// every derived artifact (note or candidate) that was built from the
// superseded attachment. The backfill derives the same state for rows that
// predate the migration; artifacts whose evidence has no attachment rows
// (legacy store.Import data) stay current with zero source rows.
const derivedArtifactsDDL = `CREATE TABLE IF NOT EXISTS derived_artifacts(kind TEXT NOT NULL, artifact_id TEXT NOT NULL, conversation_id TEXT NOT NULL, stale INTEGER NOT NULL DEFAULT 0, stale_reason TEXT NOT NULL DEFAULT '', stale_at INTEGER NULL, PRIMARY KEY(kind, artifact_id));
CREATE INDEX IF NOT EXISTS derived_artifact_conv ON derived_artifacts(conversation_id, kind);
CREATE TABLE IF NOT EXISTS derived_artifact_sources(kind TEXT NOT NULL, artifact_id TEXT NOT NULL, attachment_id TEXT NOT NULL REFERENCES source_attachments(id), PRIMARY KEY(kind, artifact_id, attachment_id));
CREATE INDEX IF NOT EXISTS derived_artifact_sources_attachment ON derived_artifact_sources(attachment_id);
INSERT OR IGNORE INTO derived_artifacts(kind, artifact_id, conversation_id, stale, stale_reason, stale_at) SELECT 'note', n.id, n.conversation_id, COALESCE(MAX(CASE WHEN a.active=0 THEN 1 ELSE 0 END),0), '', NULL FROM notes n LEFT JOIN note_evidence ne ON ne.note_id=n.id LEFT JOIN attachment_turns at ON at.turn_id=ne.turn_id LEFT JOIN source_attachments a ON a.id=at.attachment_id GROUP BY n.id, n.conversation_id;
INSERT OR IGNORE INTO derived_artifact_sources(kind, artifact_id, attachment_id) SELECT DISTINCT 'note', n.id, a.id FROM notes n JOIN note_evidence ne ON ne.note_id=n.id JOIN attachment_turns at ON at.turn_id=ne.turn_id JOIN source_attachments a ON a.id=at.attachment_id;
INSERT OR IGNORE INTO derived_artifacts(kind, artifact_id, conversation_id, stale, stale_reason, stale_at) SELECT 'candidate', c.id, c.conversation_id, COALESCE(MAX(CASE WHEN a.active=0 THEN 1 ELSE 0 END),0), '', NULL FROM memory_candidates c LEFT JOIN json_each(c.evidence_turn_ids_json) je LEFT JOIN attachment_turns at ON at.turn_id=je.value LEFT JOIN source_attachments a ON a.id=at.attachment_id GROUP BY c.id, c.conversation_id;
INSERT OR IGNORE INTO derived_artifact_sources(kind, artifact_id, attachment_id) SELECT DISTINCT 'candidate', c.id, a.id FROM memory_candidates c JOIN json_each(c.evidence_turn_ids_json) je JOIN attachment_turns at ON at.turn_id=je.value JOIN source_attachments a ON a.id=at.attachment_id;`

// speakerBindingDDL adds the speaker-identity curation tables: one
// speaker_bindings row records the reviewed identity of a speaker label inside
// one recording (a label is only meaningful within its recording), and one
// append-only speaker_binding_events row records every proposal and review
// action against a binding. The UNIQUE key on (recording_id,
// speaker_scoped_id, version) leaves room for versioned bindings of the same
// label; version is bumped when a proposal is confirmed.
const speakerBindingDDL = `CREATE TABLE IF NOT EXISTS speaker_bindings(id TEXT PRIMARY KEY, recording_id TEXT NOT NULL REFERENCES recordings(id), transcript_revision TEXT NOT NULL DEFAULT '', speaker_scoped_id TEXT NOT NULL, person_id TEXT NULL, status TEXT NOT NULL CHECK(status IN ('proposed','confirmed','rejected','revoked')), evidence_json TEXT NOT NULL DEFAULT '[]', reviewer TEXT NOT NULL DEFAULT '', version INTEGER NOT NULL DEFAULT 1, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, UNIQUE(recording_id, speaker_scoped_id, version));
CREATE TABLE IF NOT EXISTS speaker_binding_events(id TEXT PRIMARY KEY, binding_id TEXT NOT NULL REFERENCES speaker_bindings(id), action TEXT NOT NULL CHECK(action IN ('propose','confirm','reject','revoke')), actor TEXT NOT NULL DEFAULT '', note TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL);`

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
	{version: 5, name: "analysis-coverage", statements: statements(analysisCoverageDDL)},
	{version: 6, name: "memory-links", statements: statements(memoryLinksDDL)},
	{version: 7, name: "annotation-history", statements: statements(annotationHistoryDDL)},
	{version: 8, name: "derived-artifact-staleness", statements: statements(derivedArtifactsDDL)},
	{version: 9, name: "speaker-binding", statements: statements(speakerBindingDDL)},
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
