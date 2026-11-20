package migrations

import (
	"context"
	"testing"
)

// TestAudioAnalysisUpgradePreservesSources reconstructs a v22 database with
// real source rows, applies v23 twice, and proves the old data survived while
// the new tables exist.
func TestAudioAnalysisUpgradePreservesSources(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if err := Apply(ctx, db); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`DROP TABLE audio_speaker_embeddings`,
		`DROP TABLE audio_analysis_events`,
		`DROP TABLE audio_analysis_heads`,
		`DROP TABLE audio_analysis_runs`,
		`DROP TABLE processing_job_dependencies`,
		`DELETE FROM schema_migrations WHERE version>=23`,
		`INSERT INTO conversations(id,title,summary,annotation_version,annotation_basis,recording_id,source_sha256,transcript_version,duration_ms,transcript_json,annotations_json) VALUES('conv:upgrade','keep','','v1','','rec:upgrade','sha','tv',1000,'{}','{}')`,
		`INSERT INTO recordings(id,conversation_id,source_file,source_sha256,duration_ms,recording_started_at,absolute_time_known) VALUES('rec:upgrade','conv:upgrade','file','sha',1000,NULL,0)`,
		`INSERT INTO processing_jobs(id,stage,input_snapshot_id,idempotency_key,status,priority,payload_json,max_attempts,attempt_count,available_at,created_at,updated_at) VALUES('job:upgrade','escalate','rec:upgrade','upgrade-key','queued',0,'{}',3,0,1,1,1)`,
	} {
		if _, err := db.ExecContext(ctx, query); err != nil {
			t.Fatalf("prepare v22: %v", err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := Apply(ctx, db); err != nil {
			t.Fatalf("apply v23: %v", err)
		}
	}
	var title string
	if err := db.QueryRowContext(ctx, `SELECT title FROM conversations WHERE id='conv:upgrade'`).Scan(&title); err != nil || title != "keep" {
		t.Fatalf("existing source changed: %q %v", title, err)
	}
	var jobStatus string
	if err := db.QueryRowContext(ctx, `SELECT status FROM processing_jobs WHERE id='job:upgrade'`).Scan(&jobStatus); err != nil || jobStatus != "queued" {
		t.Fatalf("queued job changed: %q %v", jobStatus, err)
	}
	names := tableNames(t, db)
	for _, table := range []string{"audio_analysis_runs", "audio_analysis_events", "audio_speaker_embeddings", "processing_job_dependencies", "audio_analysis_heads"} {
		if !names[table] {
			t.Fatalf("missing v23 table %s", table)
		}
	}
	if err := Apply(ctx, db); err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	version, err := Version(ctx, db)
	if err != nil || version < 23 {
		t.Fatalf("schema version = %d, %v", version, err)
	}
}
