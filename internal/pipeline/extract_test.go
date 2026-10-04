package pipeline

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"recordingmcp/internal/decision"
	"recordingmcp/internal/migrations"
	"recordingmcp/internal/process"
)

// openTestDB creates a migrated, single-connection SQLite database in a temp dir.
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pipeline.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000"); err != nil {
		t.Fatalf("configure db: %v", err)
	}
	if err := migrations.Apply(context.Background(), db); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// insertTurn writes one conversation turn with a minimal valid turn_json blob.
func insertTurn(t *testing.T, db *sql.DB, conversationID, recordingID, id string, segmentIndex int, globalStart *int64, originalIndex int, speaker *string) {
	t.Helper()
	turnJSON := `{"id":"` + id + `","original_index":` + itoa(originalIndex) + `}`
	if _, err := db.Exec(`INSERT INTO turns(id,conversation_id,recording_id,segment_index,global_start_ms,global_end_ms,local_start_ms,local_end_ms,speaker_scoped_id,speaker_entity_id,content,definite,source_sha256,transcript_version,original_turn_id,turn_json) VALUES(?,?,?,?,?,?,NULL,NULL,?,NULL,?,NULL,'sha','v1',?,?)`,
		id, conversationID, recordingID, segmentIndex, globalStart, nil, speaker, "text of "+id, id+"/orig", turnJSON); err != nil {
		t.Fatalf("insert turn %s: %v", id, err)
	}
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// seedConversation inserts one conversation, one recording and its turns.
func seedConversation(t *testing.T, db *sql.DB, conversationID string, turns []struct {
	id      string
	seg     int
	start   *int64
	orig    int
	speaker *string
}) {
	t.Helper()
	recordingID := "rec-" + conversationID
	if _, err := db.Exec(`INSERT INTO conversations(id,title,summary,annotation_version,annotation_basis,recording_id,source_sha256,transcript_version,duration_ms,transcript_json,annotations_json) VALUES(?,?,'','v1','basis',?,'sha','v1',1000,'{}','{}')`, conversationID, "title", recordingID); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO recordings(id,conversation_id,source_file,source_sha256,duration_ms,recording_started_at,absolute_time_known) VALUES(?,?,'file','sha',1000,NULL,0)`, recordingID, conversationID); err != nil {
		t.Fatalf("insert recording: %v", err)
	}
	for _, tr := range turns {
		insertTurn(t, db, conversationID, recordingID, tr.id, tr.seg, tr.start, tr.orig, tr.speaker)
	}
}

func enqueueExtract(t *testing.T, db *sql.DB, conversationID string) process.Job {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	spec := process.Spec{
		Stage:           "extract",
		InputSnapshotID: "snap-1",
		IdempotencyKey:  "extract:" + conversationID,
		PayloadJSON:     `{"conversation_id":"` + conversationID + `"}`,
	}
	if _, _, err := process.EnqueueTx(context.Background(), tx, spec, time.Now()); err != nil {
		tx.Rollback()
		t.Fatalf("enqueue: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	job, err := process.Claim(context.Background(), db, process.ClaimOptions{
		WorkerID: "test-worker", Stages: []string{"extract"}, Lease: time.Minute, Now: time.Now(),
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	return *job
}

func TestExtractHandlerRecordsDecisionRun(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	start := int64(100)
	speaker := "speaker-1"
	seedConversation(t, db, "conv-1", []struct {
		id      string
		seg     int
		start   *int64
		orig    int
		speaker *string
	}{
		{id: "t1", seg: 0, start: &start, orig: 1, speaker: &speaker},
		{id: "t2", seg: 1, start: nil, orig: 2, speaker: nil},
		{id: "t3", seg: 2, start: nil, orig: 3, speaker: &speaker},
	})
	job := enqueueExtract(t, db, "conv-1")

	handler := &ExtractHandler{DB: db, Engine: decision.Synthetic{}, Endpoint: "http://test/v1", ProcessorVersion: "extract-v1"}
	resultRef, err := handler.Execute(ctx, job)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.HasPrefix(resultRef, "decision-run:run:") {
		t.Fatalf("result ref %q does not carry the decision-run prefix", resultRef)
	}

	var runs int
	if err := db.QueryRow(`SELECT COUNT(*) FROM decision_runs`).Scan(&runs); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runs != 1 {
		t.Fatalf("decision_runs = %d, want 1", runs)
	}
	runID := strings.TrimPrefix(resultRef, "decision-run:")
	var answers int
	if err := db.QueryRow(`SELECT COUNT(*) FROM decision_answers WHERE run_id=?`, runID).Scan(&answers); err != nil {
		t.Fatalf("count answers: %v", err)
	}
	if answers != len(BuildQuestions()) {
		t.Fatalf("decision_answers = %d, want %d", answers, len(BuildQuestions()))
	}

	rec, err := GetDecisionRun(ctx, db, runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if rec.JobID != job.ID {
		t.Fatalf("run job id = %q, want %q", rec.JobID, job.ID)
	}
	if rec.Attempt != job.AttemptCount {
		t.Fatalf("run attempt = %d, want %d", rec.Attempt, job.AttemptCount)
	}
	if rec.InputSnapshotID != "snap-1" {
		t.Fatalf("run snapshot = %q, want snap-1", rec.InputSnapshotID)
	}
	if rec.Endpoint != "http://test/v1" {
		t.Fatalf("run endpoint = %q", rec.Endpoint)
	}
	if len(rec.Answers) != len(BuildQuestions()) {
		t.Fatalf("run answers = %d", len(rec.Answers))
	}
	for _, q := range BuildQuestions() {
		if _, ok := rec.Answers[q.ID]; !ok {
			t.Fatalf("missing answer for %s", q.ID)
		}
	}

	// Re-running the same attempt is idempotent: same run id, no duplicate rows.
	again, err := handler.Execute(ctx, job)
	if err != nil {
		t.Fatalf("re-execute: %v", err)
	}
	if again != resultRef {
		t.Fatalf("re-execute ref = %q, want %q", again, resultRef)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM decision_runs`).Scan(&runs); err != nil {
		t.Fatalf("recount runs: %v", err)
	}
	if runs != 1 {
		t.Fatalf("decision_runs after replay = %d, want 1", runs)
	}
}

func TestExtractHandlerNoTurnsSucceeds(t *testing.T) {
	db := openTestDB(t)
	seedConversation(t, db, "conv-empty", nil)
	job := enqueueExtract(t, db, "conv-empty")

	handler := &ExtractHandler{DB: db, Engine: decision.Synthetic{}}
	ref, err := handler.Execute(context.Background(), job)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if ref != "" {
		t.Fatalf("ref = %q, want empty", ref)
	}
	var runs int
	if err := db.QueryRow(`SELECT COUNT(*) FROM decision_runs`).Scan(&runs); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runs != 0 {
		t.Fatalf("decision_runs = %d, want 0", runs)
	}
}

type failingEngine struct{ err error }

func (f failingEngine) Evaluate(context.Context, string, []decision.Question) (decision.Result, error) {
	return decision.Result{}, f.err
}

func TestExtractHandlerEngineError(t *testing.T) {
	db := openTestDB(t)
	start := int64(0)
	seedConversation(t, db, "conv-err", []struct {
		id      string
		seg     int
		start   *int64
		orig    int
		speaker *string
	}{{id: "t1", seg: 0, start: &start, orig: 1, speaker: nil}})
	job := enqueueExtract(t, db, "conv-err")

	want := errors.New("engine exploded")
	handler := &ExtractHandler{DB: db, Engine: failingEngine{err: want}}
	if _, err := handler.Execute(context.Background(), job); !errors.Is(err, want) {
		t.Fatalf("execute error = %v, want %v", err, want)
	}
	var runs int
	if err := db.QueryRow(`SELECT COUNT(*) FROM decision_runs`).Scan(&runs); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runs != 0 {
		t.Fatalf("decision_runs = %d, want 0", runs)
	}
}

func TestExtractHandlerMissingConversationID(t *testing.T) {
	db := openTestDB(t)
	handler := &ExtractHandler{DB: db, Engine: decision.Synthetic{}}
	if _, err := handler.Execute(context.Background(), process.Job{ID: "job:x", PayloadJSON: `{}`}); err == nil {
		t.Fatal("expected error for missing conversation_id")
	}
	if _, err := handler.Execute(context.Background(), process.Job{ID: "job:x", PayloadJSON: `not json`}); err == nil {
		t.Fatal("expected error for malformed payload")
	}
}

func TestBuildWindowsChunks(t *testing.T) {
	turns := make([]Turn, 5)
	for i := range turns {
		turns[i] = Turn{ID: "t" + itoa(i)}
	}
	if w := BuildWindows(turns, 200); len(w) != 1 || len(w[0].TurnIDs) != 5 {
		t.Fatalf("single window = %+v", w)
	}
	w := BuildWindows(turns, 2)
	if len(w) != 3 {
		t.Fatalf("windows = %d, want 3", len(w))
	}
	if len(w[0].TurnIDs) != 2 || len(w[1].TurnIDs) != 2 || len(w[2].TurnIDs) != 1 {
		t.Fatalf("chunk sizes wrong: %+v", w)
	}
	if w[2].TurnIDs[0] != "t4" {
		t.Fatalf("last window = %+v", w[2])
	}
	if got := BuildWindows(nil, 10); got != nil {
		t.Fatalf("empty turns = %+v, want nil", got)
	}
}

func TestRenderStateFormatAndTruncation(t *testing.T) {
	speaker := "spk"
	turns := []Turn{
		{ID: "t1", Text: "hello", SpeakerScopedID: &speaker},
		{ID: "t2", Text: "world"},
	}
	w := Window{TurnIDs: []string{"t1", "t2"}}
	got := RenderState(turns, w, 0)
	want := "[t1] spk: hello\n[t2] 未知: world"
	if got != want {
		t.Fatalf("render = %q, want %q", got, want)
	}
	truncated := RenderState(turns, w, 5)
	if !strings.HasSuffix(truncated, "…(truncated)") {
		t.Fatalf("truncated = %q", truncated)
	}
	if strings.HasPrefix(truncated, want) {
		t.Fatalf("truncated output should be shorter: %q", truncated)
	}
}

func TestBuildQuestionsShape(t *testing.T) {
	questions := BuildQuestions()
	if len(questions) != 5 {
		t.Fatalf("questions = %d, want 5", len(questions))
	}
	seen := map[string]bool{}
	for _, q := range questions {
		if q.Type != decision.TypeNoul {
			t.Fatalf("question %s type = %s", q.ID, q.Type)
		}
		if strings.TrimSpace(q.Instructions) == "" {
			t.Fatalf("question %s has empty instructions", q.ID)
		}
		if seen[q.ID] {
			t.Fatalf("duplicate question id %s", q.ID)
		}
		seen[q.ID] = true
	}
}

func TestLoadConversationTurnsOrdering(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	start := int64(500)
	seedConversation(t, db, "conv-order", []struct {
		id      string
		seg     int
		start   *int64
		orig    int
		speaker *string
	}{
		{id: "b", seg: 5, start: nil, orig: 2},
		{id: "a", seg: 3, start: &start, orig: 3},
		{id: "c", seg: 4, start: nil, orig: 1},
	})
	turns, err := LoadConversationTurns(ctx, db, "conv-order")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(turns) != 3 {
		t.Fatalf("turns = %d", len(turns))
	}
	// Timed turn first, then untimed by segment index.
	if turns[0].ID != "a" || turns[1].ID != "c" || turns[2].ID != "b" {
		t.Fatalf("order = %s,%s,%s", turns[0].ID, turns[1].ID, turns[2].ID)
	}
	if turns[0].OriginalIndex != 3 || turns[1].OriginalIndex != 1 {
		t.Fatalf("original indexes = %d,%d", turns[0].OriginalIndex, turns[1].OriginalIndex)
	}
}
