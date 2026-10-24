package migrations

import (
	"context"
	"testing"
)

func TestProcessingRuntimeUpgradePreservesExistingSource(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if err := Apply(ctx, db); err != nil {
		t.Fatal(err)
	}
	// Reconstruct the v17 operational schema without touching source tables.
	for _, q := range []string{
		`DROP TABLE processing_retry_requests`,
		`DROP TABLE worker_heartbeats`,
		`DELETE FROM schema_migrations WHERE version>=18`,
		`INSERT INTO conversations(id,title,summary,annotation_version,annotation_basis,recording_id,source_sha256,transcript_version,duration_ms,transcript_json,annotations_json) VALUES('migration-source','keep','','','','rec','hash','v1',0,'{}','{}')`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := Apply(ctx, db); err != nil {
			t.Fatal(err)
		}
	}
	var title string
	if err := db.QueryRowContext(ctx, `SELECT title FROM conversations WHERE id='migration-source'`).Scan(&title); err != nil || title != "keep" {
		t.Fatalf("existing source changed: %q %v", title, err)
	}
	for _, table := range []string{"worker_heartbeats", "processing_retry_requests"} {
		if !tableNames(t, db)[table] {
			t.Fatalf("missing runtime table %s", table)
		}
	}
}
