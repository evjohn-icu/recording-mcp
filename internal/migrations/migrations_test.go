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
	if got := mustVersion(t, ctx, db); got != 6 {
		t.Fatalf("Version() = %d, want 6", got)
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
	for _, want := range []string{"turns_conv_order", "notes_conv", "source_attachments_conversation", "processing_jobs_claim", "memory_candidates_conv", "memory_links_from", "memory_links_to"} {
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
	if got := mustVersion(t, ctx, db); got != 6 {
		t.Fatalf("Version() = %d, want 6 after repeated Apply", got)
	}
	var applied int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	if applied != 6 {
		t.Fatalf("schema_migrations rows = %d, want 6", applied)
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
	if got := mustVersion(t, ctx, db); got != 6 {
		t.Fatalf("Version() = %d, want 6", got)
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
	if _, err := db.Exec(`DELETE FROM schema_migrations WHERE version IN (3,4,5,6)`); err != nil {
		t.Fatal(err)
	}
	// Re-running Apply must re-create the (dropped) v3..v6 objects only.
	for _, stmt := range []string{
		`DROP TABLE decision_answers`,
		`DROP TABLE decision_runs`,
		`DROP TABLE memory_candidates`,
		`DROP TABLE publication_runs`,
		`DROP TABLE analysis_coverage`,
		`DROP TABLE memory_links`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if err := Apply(ctx, db); err != nil {
		t.Fatalf("re-Apply after partial rollback: %v", err)
	}
	if got := mustVersion(t, ctx, db); got != 6 {
		t.Fatalf("Version() = %d, want 6", got)
	}
	names := tableNames(t, db)
	for _, want := range []string{"decision_runs", "decision_answers", "memory_candidates", "publication_runs", "analysis_coverage", "memory_links"} {
		if !names[want] {
			t.Fatalf("table %q not re-applied: %v", want, names)
		}
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
