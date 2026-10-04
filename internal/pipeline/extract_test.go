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
	"recordingmcp/internal/generate"
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

// stubGenerator returns a fixed set of drafts, letting tests exercise the
// validation rules without depending on the synthetic generator's hash.
type stubGenerator struct {
	drafts []generate.Draft
	model  string
	err    error
}

func (s stubGenerator) Generate(context.Context, string, []string) ([]generate.Draft, string, error) {
	if s.err != nil {
		return nil, "", s.err
	}
	return s.drafts, s.model, nil
}

// countPublishJobs counts the publish jobs enqueued for one conversation.
func countPublishJobs(t *testing.T, db *sql.DB, conversationID string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM processing_jobs WHERE stage='publish' AND idempotency_key LIKE ?`,
		"publish|"+conversationID+"|%").Scan(&n); err != nil {
		t.Fatalf("count publish jobs: %v", err)
	}
	return n
}

func TestExtractHandlerGeneratesAndPublishesCandidates(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	start := int64(100)
	speaker := "speaker-1"
	seedConversation(t, db, "conv-gen", []struct {
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
	job := enqueueExtract(t, db, "conv-gen")

	// Derive the exact drafts generate.Synthetic will propose for this window.
	turns, err := LoadConversationTurns(ctx, db, "conv-gen")
	if err != nil {
		t.Fatalf("load turns: %v", err)
	}
	windows := BuildWindows(turns, maxTurnsPerWindow)
	if len(windows) != 1 {
		t.Fatalf("windows = %d, want 1", len(windows))
	}
	state := RenderState(turns, windows[0], defaultMaxChars)
	expected, model, err := generate.Synthetic{}.Generate(ctx, state, windows[0].TurnIDs)
	if err != nil {
		t.Fatalf("synthetic generate: %v", err)
	}
	if len(expected) == 0 {
		t.Fatal("synthetic generator produced no drafts; test seed is not meaningful")
	}

	handler := &ExtractHandler{DB: db, Engine: decision.Synthetic{}, Endpoint: "http://test/v1", ProcessorVersion: "extract-v1", Generator: generate.Synthetic{}}
	resultRef, err := handler.Execute(ctx, job)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	const marker = "+candidates:"
	runID, countStr, ok := strings.Cut(strings.TrimPrefix(resultRef, "decision-run:"), marker)
	if !ok {
		t.Fatalf("result ref %q missing candidates marker", resultRef)
	}
	if !strings.HasPrefix(runID, "run:") {
		t.Fatalf("run id %q lacks run: prefix", runID)
	}
	if countStr != itoa(len(expected)) {
		t.Fatalf("validated count = %s, want %d", countStr, len(expected))
	}

	all, err := ListCandidates(ctx, db, "conv-gen", "")
	if err != nil {
		t.Fatalf("list candidates: %v", err)
	}
	if len(all) != len(expected) {
		t.Fatalf("candidates = %d, want %d", len(all), len(expected))
	}
	windowIDs := make(map[string]bool, len(windows[0].TurnIDs))
	for _, id := range windows[0].TurnIDs {
		windowIDs[id] = true
	}
	for _, c := range all {
		if c.Status != "validated" {
			t.Fatalf("candidate %s status = %q, want validated", c.ID, c.Status)
		}
		if !generate.ValidKind(c.Kind) {
			t.Fatalf("candidate %s kind = %q", c.ID, c.Kind)
		}
		if c.Summary == "" || c.RunID != runID || c.JobID != job.ID {
			t.Fatalf("candidate %s fields wrong: summary=%q run=%q job=%q", c.ID, c.Summary, c.RunID, c.JobID)
		}
		if c.GeneratorModel != model {
			t.Fatalf("candidate %s model = %q, want %q", c.ID, c.GeneratorModel, model)
		}
		if len(c.EvidenceTurnIDs) == 0 {
			t.Fatalf("candidate %s has no evidence", c.ID)
		}
		for _, id := range c.EvidenceTurnIDs {
			if !windowIDs[id] {
				t.Fatalf("candidate %s evidence %q outside window", c.ID, id)
			}
		}
	}

	if n := countPublishJobs(t, db, "conv-gen"); n != 1 {
		t.Fatalf("publish jobs = %d, want 1", n)
	}
	var payload string
	if err := db.QueryRow(`SELECT payload_json FROM processing_jobs WHERE stage='publish' AND idempotency_key LIKE ?`,
		"publish|conv-gen|%").Scan(&payload); err != nil {
		t.Fatalf("read publish payload: %v", err)
	}
	if !strings.Contains(payload, `"conversation_id":"conv-gen"`) {
		t.Fatalf("publish payload = %q", payload)
	}

	// Replaying the same attempt is idempotent: same reference, no duplicate
	// candidates and no duplicate publish job.
	again, err := handler.Execute(ctx, job)
	if err != nil {
		t.Fatalf("re-execute: %v", err)
	}
	if again != resultRef {
		t.Fatalf("re-execute ref = %q, want %q", again, resultRef)
	}
	if got, err := ListCandidates(ctx, db, "conv-gen", ""); err != nil || len(got) != len(expected) {
		t.Fatalf("candidates after replay = %d (err %v), want %d", len(got), err, len(expected))
	}
	if n := countPublishJobs(t, db, "conv-gen"); n != 1 {
		t.Fatalf("publish jobs after replay = %d, want 1", n)
	}
}

func TestExtractHandlerRejectsInvalidCandidates(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	start := int64(10)
	seedConversation(t, db, "conv-rej", []struct {
		id      string
		seg     int
		start   *int64
		orig    int
		speaker *string
	}{
		{id: "t1", seg: 0, start: &start, orig: 1, speaker: nil},
		{id: "t2", seg: 1, start: nil, orig: 2, speaker: nil},
	})
	job := enqueueExtract(t, db, "conv-rej")

	gen := stubGenerator{model: "stub-v1", drafts: []generate.Draft{
		{Kind: "observation", Summary: "valid", EvidenceTurnIDs: []string{"t1"}},
		{Kind: "bogus", Summary: "bad kind", EvidenceTurnIDs: []string{"t1"}},
		{Kind: "request", Summary: "   ", EvidenceTurnIDs: []string{"t1"}},
		{Kind: "intention", Summary: "outside window", EvidenceTurnIDs: []string{"t9"}},
	}}
	handler := &ExtractHandler{DB: db, Engine: decision.Synthetic{}, Generator: gen}
	resultRef, err := handler.Execute(ctx, job)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.HasSuffix(resultRef, "+candidates:1") {
		t.Fatalf("result ref = %q, want one validated candidate", resultRef)
	}

	validated, err := ListCandidates(ctx, db, "conv-rej", "validated")
	if err != nil {
		t.Fatalf("list validated: %v", err)
	}
	if len(validated) != 1 {
		t.Fatalf("validated = %d, want 1", len(validated))
	}
	rejected, err := ListCandidates(ctx, db, "conv-rej", "rejected")
	if err != nil {
		t.Fatalf("list rejected: %v", err)
	}
	if len(rejected) != 3 {
		t.Fatalf("rejected = %d, want 3", len(rejected))
	}
	for _, c := range rejected {
		if c.RejectionReason == "" {
			t.Fatalf("rejected candidate %s has no reason", c.ID)
		}
		if c.GeneratorModel != "stub-v1" {
			t.Fatalf("rejected candidate %s model = %q", c.ID, c.GeneratorModel)
		}
	}

	if n := countPublishJobs(t, db, "conv-rej"); n != 1 {
		t.Fatalf("publish jobs = %d, want 1", n)
	}
}

// enqueueExtractWithPayload enqueues and claims one extract job with an
// explicit payload and idempotency key.
func enqueueExtractWithPayload(t *testing.T, db *sql.DB, payloadJSON, key string) process.Job {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	spec := process.Spec{
		Stage:           "extract",
		InputSnapshotID: "snap-1",
		IdempotencyKey:  key,
		PayloadJSON:     payloadJSON,
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

// coverageRows maps each covered turn id to the run id that covered it.
func coverageRows(t *testing.T, db *sql.DB, conversationID, processorVersion string) map[string]string {
	t.Helper()
	rows, err := db.Query(`SELECT turn_id,run_id FROM analysis_coverage WHERE conversation_id=? AND processor_version=?`, conversationID, processorVersion)
	if err != nil {
		t.Fatalf("query coverage: %v", err)
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var turnID, runID string
		if err := rows.Scan(&turnID, &runID); err != nil {
			t.Fatalf("scan coverage: %v", err)
		}
		out[turnID] = runID
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("coverage rows: %v", err)
	}
	return out
}

// captureGenerator records the turn ids of every window it is asked to process.
type captureGenerator struct {
	model string
	seen  [][]string
}

func (c *captureGenerator) Generate(_ context.Context, _ string, turnIDs []string) ([]generate.Draft, string, error) {
	c.seen = append(c.seen, append([]string(nil), turnIDs...))
	return nil, c.model, nil
}

func TestExtractHandlerScopesToTurnIDs(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	start := int64(100)
	seedConversation(t, db, "conv-scope", []struct {
		id      string
		seg     int
		start   *int64
		orig    int
		speaker *string
	}{
		{id: "t1", seg: 0, start: &start, orig: 1},
		{id: "t2", seg: 1, start: nil, orig: 2},
		{id: "t3", seg: 2, start: nil, orig: 3},
	})
	// turn_ids are deliberately scrambled to prove canonical ordering wins.
	job := enqueueExtractWithPayload(t, db,
		`{"conversation_id":"conv-scope","segment_id":"seg-2","turn_ids":["t3","t2"]}`,
		"extract:conv-scope:seg-2")

	gen := &captureGenerator{model: "cap-v1"}
	handler := &ExtractHandler{DB: db, Engine: decision.Synthetic{}, ProcessorVersion: "extract-v1", Generator: gen}
	ref, err := handler.Execute(ctx, job)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	runID, _, _ := strings.Cut(strings.TrimPrefix(ref, "decision-run:"), "+candidates:")

	// The engine only saw the scoped turns, in canonical order.
	if len(gen.seen) != 1 {
		t.Fatalf("windows seen = %d, want 1", len(gen.seen))
	}
	if got := strings.Join(gen.seen[0], ","); got != "t2,t3" {
		t.Fatalf("window turn ids = %q, want t2,t3", got)
	}

	covered := coverageRows(t, db, "conv-scope", "extract-v1")
	if len(covered) != 2 {
		t.Fatalf("coverage rows = %d (%v), want 2", len(covered), covered)
	}
	for _, id := range []string{"t2", "t3"} {
		if got := covered[id]; got != runID {
			t.Fatalf("coverage[%s] run = %q, want %q", id, got, runID)
		}
	}
	if _, ok := covered["t1"]; ok {
		t.Fatalf("t1 must not be covered: %v", covered)
	}
}

func TestExtractHandlerFallsBackToWholeConversation(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	start := int64(100)
	seedConversation(t, db, "conv-fallback", []struct {
		id      string
		seg     int
		start   *int64
		orig    int
		speaker *string
	}{
		{id: "t1", seg: 0, start: &start, orig: 1},
		{id: "t2", seg: 1, start: nil, orig: 2},
		{id: "t3", seg: 2, start: nil, orig: 3},
	})
	job := enqueueExtract(t, db, "conv-fallback")

	gen := &captureGenerator{model: "cap-v1"}
	handler := &ExtractHandler{DB: db, Engine: decision.Synthetic{}, ProcessorVersion: "extract-v1", Generator: gen}
	if _, err := handler.Execute(ctx, job); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(gen.seen) != 1 || strings.Join(gen.seen[0], ",") != "t1,t2,t3" {
		t.Fatalf("window = %v, want one window with t1,t2,t3", gen.seen)
	}
	covered := coverageRows(t, db, "conv-fallback", "extract-v1")
	if len(covered) != 3 {
		t.Fatalf("coverage rows = %d (%v), want 3", len(covered), covered)
	}
	for _, id := range []string{"t1", "t2", "t3"} {
		if _, ok := covered[id]; !ok {
			t.Fatalf("missing coverage for %s: %v", id, covered)
		}
	}
}

func TestExtractHandlerCoverageIdempotent(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	start := int64(100)
	seedConversation(t, db, "conv-idem", []struct {
		id      string
		seg     int
		start   *int64
		orig    int
		speaker *string
	}{
		{id: "t1", seg: 0, start: &start, orig: 1},
		{id: "t2", seg: 1, start: nil, orig: 2},
	})
	job := enqueueExtractWithPayload(t, db,
		`{"conversation_id":"conv-idem","turn_ids":["t1","t2"]}`,
		"extract:conv-idem")

	handler := &ExtractHandler{DB: db, Engine: decision.Synthetic{}, ProcessorVersion: "extract-v1"}
	if _, err := handler.Execute(ctx, job); err != nil {
		t.Fatalf("execute: %v", err)
	}
	first := coverageRows(t, db, "conv-idem", "extract-v1")
	if len(first) != 2 {
		t.Fatalf("coverage rows = %d (%v), want 2", len(first), first)
	}

	// Replaying the same attempt must not duplicate coverage rows.
	if _, err := handler.Execute(ctx, job); err != nil {
		t.Fatalf("re-execute: %v", err)
	}
	second := coverageRows(t, db, "conv-idem", "extract-v1")
	if len(second) != 2 {
		t.Fatalf("coverage rows after replay = %d (%v), want 2", len(second), second)
	}
	for id, runID := range first {
		if second[id] != runID {
			t.Fatalf("coverage[%s] run changed on replay: %q -> %q", id, runID, second[id])
		}
	}
	var runs int
	if err := db.QueryRow(`SELECT COUNT(*) FROM decision_runs`).Scan(&runs); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runs != 1 {
		t.Fatalf("decision_runs after replay = %d, want 1", runs)
	}
}

func TestExtractHandlerRetryIsIdempotent(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	start := int64(100)
	seedConversation(t, db, "conv-retry", []struct {
		id      string
		seg     int
		start   *int64
		orig    int
		speaker *string
	}{
		{id: "t1", seg: 0, start: &start, orig: 1},
		{id: "t2", seg: 1, start: nil, orig: 2},
	})
	job := enqueueExtractWithPayload(t, db,
		`{"conversation_id":"conv-retry","turn_ids":["t1","t2"]}`,
		"extract:conv-retry")

	gen := stubGenerator{model: "stub-v1", drafts: []generate.Draft{
		{Kind: "observation", Summary: "stable", EvidenceTurnIDs: []string{"t1"}},
	}}
	handler := &ExtractHandler{DB: db, Engine: decision.Synthetic{}, ProcessorVersion: "extract-v1", Generator: gen}
	if _, err := handler.Execute(ctx, job); err != nil {
		t.Fatalf("first execute: %v", err)
	}

	// Retry the same task as a new attempt: the run id changes, the content
	// does not. Candidates, publish jobs and coverage must not be duplicated.
	retry := job
	retry.AttemptCount = job.AttemptCount + 1
	if _, err := handler.Execute(ctx, retry); err != nil {
		t.Fatalf("retry execute: %v", err)
	}

	cands, err := ListCandidates(ctx, db, "conv-retry", "")
	if err != nil {
		t.Fatalf("list candidates: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("candidates after retry = %d, want 1", len(cands))
	}
	if n := countPublishJobs(t, db, "conv-retry"); n != 1 {
		t.Fatalf("publish jobs after retry = %d, want 1", n)
	}
	covered := coverageRows(t, db, "conv-retry", "extract-v1")
	if len(covered) != 2 {
		t.Fatalf("coverage rows after retry = %d (%v), want 2", len(covered), covered)
	}
}

func TestExtractHandlerGeneratorErrorSkipsCoverage(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	start := int64(10)
	seedConversation(t, db, "conv-gen-err", []struct {
		id      string
		seg     int
		start   *int64
		orig    int
		speaker *string
	}{
		{id: "t1", seg: 0, start: &start, orig: 1},
		{id: "t2", seg: 1, start: nil, orig: 2},
	})
	job := enqueueExtractWithPayload(t, db,
		`{"conversation_id":"conv-gen-err","turn_ids":["t1","t2"]}`,
		"extract:conv-gen-err")

	want := errors.New("generator exploded")
	handler := &ExtractHandler{DB: db, Engine: decision.Synthetic{}, ProcessorVersion: "extract-v1", Generator: stubGenerator{err: want}}
	if _, err := handler.Execute(ctx, job); !errors.Is(err, want) {
		t.Fatalf("execute error = %v, want %v", err, want)
	}

	covered := coverageRows(t, db, "conv-gen-err", "extract-v1")
	if len(covered) != 0 {
		t.Fatalf("coverage rows = %d (%v), want 0 when generation fails", len(covered), covered)
	}
	var cands int
	if err := db.QueryRow(`SELECT COUNT(*) FROM memory_candidates WHERE conversation_id='conv-gen-err'`).Scan(&cands); err != nil {
		t.Fatalf("count candidates: %v", err)
	}
	if cands != 0 {
		t.Fatalf("candidates = %d, want 0", cands)
	}
	if n := countPublishJobs(t, db, "conv-gen-err"); n != 0 {
		t.Fatalf("publish jobs = %d, want 0 when generation fails", n)
	}
	// The decision run is still recorded before generation runs.
	var runs int
	if err := db.QueryRow(`SELECT COUNT(*) FROM decision_runs`).Scan(&runs); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runs != 1 {
		t.Fatalf("decision_runs = %d, want 1", runs)
	}
}

func TestExtractHandlerNilGeneratorRecordsCoverage(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	start := int64(100)
	seedConversation(t, db, "conv-nogen", []struct {
		id      string
		seg     int
		start   *int64
		orig    int
		speaker *string
	}{
		{id: "t1", seg: 0, start: &start, orig: 1},
		{id: "t2", seg: 1, start: nil, orig: 2},
	})
	job := enqueueExtractWithPayload(t, db,
		`{"conversation_id":"conv-nogen","turn_ids":["t1","t2"]}`,
		"extract:conv-nogen")

	handler := &ExtractHandler{DB: db, Engine: decision.Synthetic{}, ProcessorVersion: "extract-v1"}
	ref, err := handler.Execute(ctx, job)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if strings.Contains(ref, "+candidates:") {
		t.Fatalf("ref = %q, want no candidates marker", ref)
	}
	covered := coverageRows(t, db, "conv-nogen", "extract-v1")
	if len(covered) != 2 {
		t.Fatalf("coverage rows = %d (%v), want 2 with a nil generator", len(covered), covered)
	}
	if n := countPublishJobs(t, db, "conv-nogen"); n != 0 {
		t.Fatalf("publish jobs = %d, want 0 with a nil generator", n)
	}
}

func TestLoadTurnsByIDsOrderingAndFilter(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	start := int64(500)
	seedConversation(t, db, "conv-byid", []struct {
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
	seedConversation(t, db, "conv-other", []struct {
		id      string
		seg     int
		start   *int64
		orig    int
		speaker *string
	}{{id: "x", seg: 0, start: nil, orig: 1}})

	all, err := LoadConversationTurns(ctx, db, "conv-byid")
	if err != nil {
		t.Fatalf("load all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("all = %d, want 3", len(all))
	}

	got, err := LoadTurnsByIDs(ctx, db, "conv-byid", []string{"b", "c", "a"})
	if err != nil {
		t.Fatalf("load by ids: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("by ids = %d, want 3", len(got))
	}
	for i := range all {
		if got[i].ID != all[i].ID {
			t.Fatalf("order[%d] = %s, want %s", i, got[i].ID, all[i].ID)
		}
	}

	partial, err := LoadTurnsByIDs(ctx, db, "conv-byid", []string{"a", "missing"})
	if err != nil {
		t.Fatalf("partial: %v", err)
	}
	if len(partial) != 1 || partial[0].ID != "a" {
		t.Fatalf("partial = %+v, want just a", partial)
	}

	empty, err := LoadTurnsByIDs(ctx, db, "conv-byid", nil)
	if err != nil {
		t.Fatalf("empty: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("empty = %+v, want none", empty)
	}

	foreign, err := LoadTurnsByIDs(ctx, db, "conv-byid", []string{"x"})
	if err != nil {
		t.Fatalf("foreign: %v", err)
	}
	if len(foreign) != 0 {
		t.Fatalf("foreign ids leaked: %+v", foreign)
	}
}
