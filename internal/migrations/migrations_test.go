package migrations

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA foreign_keys=ON`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func tableNames(t *testing.T, db *sql.DB) map[string]bool {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	names := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return names
}

func mustVersion(t *testing.T, ctx context.Context, db *sql.DB) int {
	t.Helper()
	v, err := Version(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestVersionEmptyDatabase(t *testing.T) {
	db := testDB(t)
	if got := mustVersion(t, context.Background(), db); got != 0 {
		t.Fatalf("Version() = %d, want 0 on empty database", got)
	}
}

func TestApplyFreshDatabase(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if err := Apply(ctx, db); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := mustVersion(t, ctx, db); got != 13 {
		t.Fatalf("Version() = %d, want 13", got)
	}
	names := tableNames(t, db)
	for _, want := range []string{
		"schema_migrations", "conversations", "recordings", "topics", "topic_conversations",
		"turns", "turn_topics", "notes", "note_evidence",
		"source_attachments", "attachment_turns",
		"processing_jobs", "processing_attempts",
		"decision_runs", "decision_answers",
		"memory_candidates", "publication_runs",
		"analysis_coverage",
		"memory_links",
		"annotation_history",
		"derived_artifacts", "derived_artifact_sources",
		"speaker_bindings", "speaker_binding_events",
		"memory_embeddings",
		"sync_state", "sync_events", "sync_recordings", "sync_chunks",
	} {
		if !names[want] {
			t.Errorf("missing table %q", want)
		}
	}
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='index'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	indexes := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		indexes[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"turns_conv_order", "notes_conv", "source_attachments_conversation", "processing_jobs_claim", "memory_candidates_conv", "memory_links_from", "memory_links_to", "derived_artifact_conv", "derived_artifact_sources_attachment", "source_attachments_active_head", "memory_embeddings_conv"} {
		if !indexes[want] {
			t.Errorf("missing index %q", want)
		}
	}
}

func TestApplyIdempotent(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if err := Apply(ctx, db); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	if err := Apply(ctx, db); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if got := mustVersion(t, ctx, db); got != 13 {
		t.Fatalf("Version() = %d, want 13 after repeated Apply", got)
	}
	var applied int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	if applied != 13 {
		t.Fatalf("schema_migrations rows = %d, want 13", applied)
	}
}

func TestApplyOnLegacyDatabasePreservesData(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	// Simulate a pre-migration database created by the old memory.Open: inline
	// v1 DDL and some data, but no schema_migrations table.
	for _, stmt := range statements(baseSchemaDDL) {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("legacy DDL: %v: %v", stmt, err)
		}
	}
	if _, err := db.Exec(`INSERT INTO conversations(id,title,summary,annotation_version,annotation_basis,recording_id,source_sha256,transcript_version,duration_ms,transcript_json,annotations_json) VALUES('conv-1','t','s','1','inference','rec-1','hash-1','v1',1000,'{}','{}')`); err != nil {
		t.Fatal(err)
	}
	if err := Apply(ctx, db); err != nil {
		t.Fatalf("Apply on legacy database: %v", err)
	}
	if got := mustVersion(t, ctx, db); got != 13 {
		t.Fatalf("Version() = %d, want 13", got)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM conversations WHERE id='conv-1'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("legacy conversation row missing, count = %d", count)
	}
	// legacy row count means v1 statements were no-ops on existing tables.
	var total int
	if err := db.QueryRow(`SELECT COUNT(*) FROM conversations`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Fatalf("got %d conversations, want exactly the legacy row", total)
	}
}

func TestApplyOnSingleVersionDatabaseOnlyAppliesLater(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if err := Apply(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM schema_migrations WHERE version IN (3,4,5,6,7,8,9,10,11,12,13)`); err != nil {
		t.Fatal(err)
	}
	// Re-running Apply must re-create the (dropped) v3..v12 objects only.
	// annotation_history is deliberately kept: v7 must be a no-op on a
	// database that already has the table (it was created lazily by older
	// builds of memory.Store before v7 existed).
	for _, stmt := range []string{
		`DROP TABLE decision_answers`,
		`DROP TABLE decision_runs`,
		`DROP TABLE memory_candidates`,
		`DROP TABLE publication_runs`,
		`DROP TABLE analysis_coverage`,
		`DROP TABLE memory_links`,
		`DROP TABLE derived_artifact_sources`,
		`DROP TABLE derived_artifacts`,
		`DROP TABLE speaker_binding_events`,
		`DROP TABLE speaker_bindings`,
		`DROP TABLE turn_classes`,
		`DROP INDEX source_attachments_active_head`,
		`DROP TABLE sync_state`,
		`DROP TABLE sync_events`,
		`DROP TABLE sync_recordings`,
		`DROP TABLE sync_chunks`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if err := Apply(ctx, db); err != nil {
		t.Fatalf("re-Apply after partial rollback: %v", err)
	}
	if got := mustVersion(t, ctx, db); got != 13 {
		t.Fatalf("Version() = %d, want 13", got)
	}
	names := tableNames(t, db)
	for _, want := range []string{"decision_runs", "decision_answers", "memory_candidates", "publication_runs", "analysis_coverage", "memory_links", "annotation_history", "derived_artifacts", "derived_artifact_sources", "speaker_bindings", "speaker_binding_events", "turn_classes", "sync_state", "sync_events", "sync_recordings", "sync_chunks"} {
		if !names[want] {
			t.Fatalf("table %q not re-applied: %v", want, names)
		}
	}
}

func TestApplySucceedsWithPreexistingLazyAnnotationHistory(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	// Older builds created annotation_history on demand, outside the migration
	// ledger. v7 must adopt such a database without failing.
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS annotation_history (
		conversation_id TEXT NOT NULL REFERENCES conversations(id), annotation_version TEXT NOT NULL,
		annotation_basis TEXT NOT NULL, summary TEXT NOT NULL, annotations_json TEXT NOT NULL,
		PRIMARY KEY(conversation_id,annotation_version))`); err != nil {
		t.Fatal(err)
	}
	if err := Apply(ctx, db); err != nil {
		t.Fatalf("Apply with preexisting annotation_history: %v", err)
	}
	if got := mustVersion(t, ctx, db); got != 13 {
		t.Fatalf("Version() = %d, want 13", got)
	}
	// annotation_history references conversations, so seed the parent row the
	// adopted table points at; without it the foreign key rejects the insert
	// regardless of which migration created the table.
	if _, err := db.Exec(`INSERT INTO conversations(id,title,summary,annotation_version,annotation_basis,recording_id,source_sha256,transcript_version,duration_ms,transcript_json,annotations_json) VALUES('conv-x','t','','v1','','rec-x','sha','v1',1000,'{}','{}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO annotation_history VALUES('conv-x','v1','basis','summary','{}')`); err != nil {
		t.Fatalf("adopted annotation_history is not usable: %v", err)
	}
}

func TestVersionOnFileWithoutMigrations(t *testing.T) {
	// Version on a database file that exists on disk but has no tables yet.
	path := filepath.Join(t.TempDir(), "empty.db")
	if _, err := os.Stat(path); err == nil {
		t.Fatal("expected fresh path")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("SELECT 1"); err != nil {
		t.Fatal(err)
	}
	if got, err := Version(context.Background(), db); err != nil || got != 0 {
		t.Fatalf("Version() = %d, %v; want 0, nil", got, err)
	}
}

// TestDerivedArtifactBackfillMarksSupersededEvidence proves that applying the
// derived-artifact migration over a database that already carries notes,
// candidates, attachments and turns derives the same staleness state the
// write paths would have recorded live: artifacts whose only evidence sits on
// a superseded attachment (active=0) land stale, artifacts on the active
// revision stay current, and both get their source-attachment links.
func TestDerivedArtifactBackfillMarksSupersededEvidence(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if err := Apply(ctx, db); err != nil {
		t.Fatal(err)
	}
	// Roll back only the derived-artifact migration so the backfill runs over
	// pre-existing rows, like a database upgraded from an older build. v11
	// through v13 are included so the ledger maximum drops below v8;
	// re-running them is a no-op.
	if _, err := db.Exec(`DELETE FROM schema_migrations WHERE version IN (8,9,10,11,12,13)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE derived_artifact_sources`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE derived_artifacts`); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`INSERT INTO conversations(id,title,summary,annotation_version,annotation_basis,recording_id,source_sha256,transcript_version,duration_ms,transcript_json,annotations_json) VALUES('conv-1','t','s','v1','basis','rec-1','sha','v1',1000,'{}','{}')`,
		`INSERT INTO recordings(id,conversation_id,source_file,source_sha256,duration_ms,recording_started_at,absolute_time_known) VALUES('rec-1','conv-1','f','sha',1000,NULL,0)`,
		`INSERT INTO turns(id,conversation_id,recording_id,segment_index,global_start_ms,global_end_ms,local_start_ms,local_end_ms,speaker_scoped_id,speaker_entity_id,content,definite,source_sha256,transcript_version,original_turn_id,turn_json) VALUES('turn-1','conv-1','rec-1',1,NULL,NULL,NULL,NULL,NULL,NULL,'text one',NULL,'sha','v1','orig-1','{}')`,
		`INSERT INTO turns(id,conversation_id,recording_id,segment_index,global_start_ms,global_end_ms,local_start_ms,local_end_ms,speaker_scoped_id,speaker_entity_id,content,definite,source_sha256,transcript_version,original_turn_id,turn_json) VALUES('turn-2','conv-1','rec-1',2,NULL,NULL,NULL,NULL,NULL,NULL,'text two',NULL,'sha','v1','orig-2','{}')`,
		`INSERT INTO source_attachments(id,conversation_id,recording_id,segment_index,segment_id,transcript_revision,source_json,result_json,content_sha256,active,supersedes_revision) VALUES('att-1','conv-1','rec-1',1,'seg-1','rev-1','{}','{}','sha1',0,NULL)`,
		`INSERT INTO source_attachments(id,conversation_id,recording_id,segment_index,segment_id,transcript_revision,source_json,result_json,content_sha256,active,supersedes_revision) VALUES('att-2','conv-1','rec-1',2,'seg-1','rev-2','{}','{}','sha2',1,'rev-1')`,
		`INSERT INTO attachment_turns(attachment_id,turn_id) VALUES('att-1','turn-1')`,
		`INSERT INTO attachment_turns(attachment_id,turn_id) VALUES('att-2','turn-2')`,
		`INSERT INTO notes(id,conversation_id,kind,summary,topic_ids_json,claim_mode,interpretation_note,annotation_version) VALUES('note-1','conv-1','observation','stale note','[]','inference','n','v1')`,
		`INSERT INTO note_evidence(note_id,turn_id,ordinal) VALUES('note-1','turn-1',0)`,
		`INSERT INTO memory_candidates(id,conversation_id,kind,summary,evidence_turn_ids_json,status,created_at) VALUES('cand-1','conv-1','observation','current candidate','["turn-2"]','validated',1)`,
		`INSERT INTO memory_candidates(id,conversation_id,kind,summary,evidence_turn_ids_json,status,created_at) VALUES('cand-2','conv-1','observation','superseded candidate','["turn-1"]','validated',2)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}
	if err := Apply(ctx, db); err != nil {
		t.Fatalf("re-Apply for backfill: %v", err)
	}
	if got := mustVersion(t, ctx, db); got != 13 {
		t.Fatalf("Version() = %d, want 13", got)
	}
	for _, want := range []struct {
		kind, id string
		stale    int
	}{
		{"note", "note-1", 1},
		{"candidate", "cand-1", 0},
		{"candidate", "cand-2", 1},
	} {
		var stale int
		if err := db.QueryRow(`SELECT stale FROM derived_artifacts WHERE kind=? AND artifact_id=?`, want.kind, want.id).Scan(&stale); err != nil {
			t.Fatalf("derived artifact %s/%s: %v", want.kind, want.id, err)
		}
		if stale != want.stale {
			t.Errorf("%s/%s stale = %d, want %d", want.kind, want.id, stale, want.stale)
		}
	}
	for _, want := range []struct {
		kind, id, attachment string
	}{
		{"note", "note-1", "att-1"},
		{"candidate", "cand-1", "att-2"},
		{"candidate", "cand-2", "att-1"},
	} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM derived_artifact_sources WHERE kind=? AND artifact_id=? AND attachment_id=?`, want.kind, want.id, want.attachment).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("missing source link %s/%s -> %s", want.kind, want.id, want.attachment)
		}
	}
}

// TestActiveHeadEnforcedByDatabase proves the single-active-revision invariant
// is a database guarantee: a second active head for the same recording and
// segment is rejected by the partial unique index, while the sanctioned
// supersede transition (deactivate old, then activate new) still works.
func TestActiveHeadEnforcedByDatabase(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if err := Apply(ctx, db); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`INSERT INTO conversations(id,title,summary,annotation_version,annotation_basis,recording_id,source_sha256,transcript_version,duration_ms,transcript_json,annotations_json) VALUES('conv-1','t','s','v1','basis','rec-1','sha','v1',1000,'{}','{}')`,
		`INSERT INTO recordings(id,conversation_id,source_file,source_sha256,duration_ms,recording_started_at,absolute_time_known) VALUES('rec-1','conv-1','f','sha',1000,NULL,0)`,
		`INSERT INTO source_attachments(id,conversation_id,recording_id,segment_index,segment_id,transcript_revision,source_json,result_json,content_sha256,active,supersedes_revision) VALUES('att-1','conv-1','rec-1',1,'seg-1','rev-1','{}','{}','sha1',1,NULL)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}
	// A second active head for the same (recording, segment) must fail.
	if _, err := db.Exec(`INSERT INTO source_attachments(id,conversation_id,recording_id,segment_index,segment_id,transcript_revision,source_json,result_json,content_sha256,active,supersedes_revision) VALUES('att-2','conv-1','rec-1',2,'seg-1','rev-2','{}','{}','sha2',1,'rev-1')`); err == nil {
		t.Fatal("database accepted two active revisions for one recording+segment")
	}
	// The sanctioned supersede transition keeps exactly one active head.
	if _, err := db.Exec(`UPDATE source_attachments SET active=0 WHERE id='att-1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO source_attachments(id,conversation_id,recording_id,segment_index,segment_id,transcript_revision,source_json,result_json,content_sha256,active,supersedes_revision) VALUES('att-2','conv-1','rec-1',2,'seg-1','rev-2','{}','{}','sha2',1,'rev-1')`); err != nil {
		t.Fatalf("supersede transition rejected: %v", err)
	}
	// Two heads for different segments are independent and allowed.
	if _, err := db.Exec(`INSERT INTO source_attachments(id,conversation_id,recording_id,segment_index,segment_id,transcript_revision,source_json,result_json,content_sha256,active,supersedes_revision) VALUES('att-3','conv-1','rec-1',3,'seg-2','rev-1','{}','{}','sha3',1,NULL)`); err != nil {
		t.Fatalf("second segment head rejected: %v", err)
	}
	var active int
	if err := db.QueryRow(`SELECT COUNT(*) FROM source_attachments WHERE active=1 AND segment_id='seg-1'`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 1 {
		t.Fatalf("active heads for seg-1 = %d, want 1", active)
	}
}
