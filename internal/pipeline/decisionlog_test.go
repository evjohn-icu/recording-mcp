package pipeline

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"recordingmcp/internal/decision"
	"recordingmcp/internal/migrations"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`PRAGMA foreign_keys=ON`); err != nil {
		t.Fatal(err)
	}
	if err := migrations.Apply(context.Background(), db); err != nil {
		t.Fatalf("Apply migrations: %v", err)
	}
	return db
}

func sampleRecord(inputHash string) RunRecord {
	return RunRecord{
		JobID:           "job-1",
		InputSnapshotID: "snap-1",
		InputHash:       inputHash,
		Model:           "jev-style-2b-v2",
		WeightsVersion:  "w-42",
		Endpoint:        "http://127.0.0.1:8765/v1/systemone",
		QuestionsJSON:   `[{"id":"q_suggestion","type":"noul"}]`,
		UsageJSON:       `{"latency_ms":12,"input_tokens":3,"output_tokens":4}`,
		Attempt:         1,
		Answers: map[string]decision.Answer{
			"q_suggestion": {
				Type:         decision.TypeNoul,
				Noul:         0.8,
				Confidence:   0.8,
				Distribution: map[string]float64{"true": 0.8, "false": 0.2},
			},
		},
	}
}

func TestRecordAndGetDecisionRun(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	rec := sampleRecord("hash-a")
	now := time.UnixMilli(1_700_000_000_000)

	runID, err := RecordDecisionRun(ctx, db, rec, now)
	if err != nil {
		t.Fatalf("RecordDecisionRun: %v", err)
	}
	if want := decisionRunID(rec.JobID, rec.Attempt, rec.InputHash); runID != want {
		t.Fatalf("runID = %q, want %q", runID, want)
	}

	got, err := GetDecisionRun(ctx, db, runID)
	if err != nil {
		t.Fatalf("GetDecisionRun: %v", err)
	}
	if got.JobID != rec.JobID || got.InputSnapshotID != rec.InputSnapshotID ||
		got.InputHash != rec.InputHash || got.Model != rec.Model ||
		got.WeightsVersion != rec.WeightsVersion || got.Endpoint != rec.Endpoint ||
		got.QuestionsJSON != rec.QuestionsJSON || got.UsageJSON != rec.UsageJSON ||
		got.Attempt != rec.Attempt {
		t.Fatalf("round-tripped record mismatch:\n got  %+v\n want %+v", got, rec)
	}
	if !reflect.DeepEqual(got.Answers, rec.Answers) {
		t.Fatalf("answers mismatch:\n got  %#v\n want %#v", got.Answers, rec.Answers)
	}
}

func TestRecordDecisionRunIdempotent(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	now := time.UnixMilli(1_700_000_000_000)

	first := sampleRecord("hash-a")
	runID1, err := RecordDecisionRun(ctx, db, first, now)
	if err != nil {
		t.Fatalf("first RecordDecisionRun: %v", err)
	}

	second := sampleRecord("hash-a")
	second.Answers["q_suggestion"] = decision.Answer{
		Type:         decision.TypeNoul,
		Noul:         0.1,
		Confidence:   0.9,
		Distribution: map[string]float64{"true": 0.1, "false": 0.9},
	}
	runID2, err := RecordDecisionRun(ctx, db, second, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("second RecordDecisionRun: %v", err)
	}
	if runID1 != runID2 {
		t.Fatalf("runID changed on identical identity: %q != %q", runID1, runID2)
	}

	var runs int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM decision_runs WHERE id = ?`, runID1).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if runs != 1 {
		t.Fatalf("decision_runs rows = %d, want 1", runs)
	}

	got, err := GetDecisionRun(ctx, db, runID1)
	if err != nil {
		t.Fatalf("GetDecisionRun: %v", err)
	}
	if !reflect.DeepEqual(got.Answers, second.Answers) {
		t.Fatalf("answers not replaced:\n got  %#v\n want %#v", got.Answers, second.Answers)
	}
}

func TestRecordDecisionRunDifferentInputHash(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	now := time.UnixMilli(1_700_000_000_000)

	runID1, err := RecordDecisionRun(ctx, db, sampleRecord("hash-a"), now)
	if err != nil {
		t.Fatalf("first RecordDecisionRun: %v", err)
	}
	runID2, err := RecordDecisionRun(ctx, db, sampleRecord("hash-b"), now)
	if err != nil {
		t.Fatalf("second RecordDecisionRun: %v", err)
	}
	if runID1 == runID2 {
		t.Fatalf("different input hashes produced the same runID %q", runID1)
	}
}

func TestGetDecisionRunNotFound(t *testing.T) {
	db := testDB(t)
	_, err := GetDecisionRun(context.Background(), db, "run:does-not-exist")
	if !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("GetDecisionRun error = %v, want ErrRunNotFound", err)
	}
}
