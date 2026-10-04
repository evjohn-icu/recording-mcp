package pipeline

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"recordingmcp/internal/migrations"
)

// newCandidateDB creates a migrated SQLite database in a temp dir for the
// candidate-store tests.
func newCandidateDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "candidates.db")
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

// seedCandidateConversation inserts the minimal conversation row candidates
// reference by foreign key.
func seedCandidateConversation(t *testing.T, db *sql.DB, conversationID string) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO conversations(id,title,summary,annotation_version,annotation_basis,recording_id,source_sha256,transcript_version,duration_ms,transcript_json,annotations_json)
		 VALUES(?,?,'','v1','basis',?,'sha','v1',1000,'{}','{}')`,
		conversationID, "title", "rec-"+conversationID); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
}

func sampleValidatedDraft() ValidatedDraft {
	return ValidatedDraft{
		CandidateDraft: CandidateDraft{
			Kind:               "observation",
			Summary:            "用户偏好每天早会",
			TopicTitle:         "工作节奏",
			EvidenceTurnIDs:    []string{"turn-1", "turn-2"},
			InterpretationNote: "从两次提及推断",
		},
		Status: "validated",
	}
}

func sampleRejectedDraft() ValidatedDraft {
	return ValidatedDraft{
		CandidateDraft: CandidateDraft{
			Kind:            "suggestion",
			Summary:         "",
			EvidenceTurnIDs: []string{"turn-3"},
		},
		Status:          "rejected",
		RejectionReason: "summary is empty",
	}
}

func TestInsertAndListCandidatesFilters(t *testing.T) {
	db := newCandidateDB(t)
	ctx := context.Background()
	seedCandidateConversation(t, db, "conv-1")
	now := time.UnixMilli(1_700_000_000_000)

	ids, err := InsertCandidates(ctx, db, "conv-1", "run-1", "job-1", "synthetic",
		[]ValidatedDraft{sampleValidatedDraft(), sampleRejectedDraft()}, now)
	if err != nil {
		t.Fatalf("InsertCandidates: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("ids = %d, want 2", len(ids))
	}
	for _, id := range ids {
		if len(id) != len("cand:")+32 || id[:5] != "cand:" {
			t.Errorf("candidate id %q does not match cand:<32 hex>", id)
		}
	}

	all, err := ListCandidates(ctx, db, "conv-1", "")
	if err != nil {
		t.Fatalf("ListCandidates(all): %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("all candidates = %d, want 2", len(all))
	}

	validated, err := ListCandidates(ctx, db, "conv-1", "validated")
	if err != nil {
		t.Fatalf("ListCandidates(validated): %v", err)
	}
	if len(validated) != 1 || validated[0].Status != "validated" {
		t.Fatalf("validated = %+v, want exactly one validated candidate", validated)
	}
	if validated[0].Summary != "用户偏好每天早会" || validated[0].TopicTitle != "工作节奏" {
		t.Errorf("validated fields = %+v", validated[0])
	}
	if validated[0].RejectionReason != "" {
		t.Errorf("validated RejectionReason = %q, want empty", validated[0].RejectionReason)
	}

	rejected, err := ListCandidates(ctx, db, "conv-1", "rejected")
	if err != nil {
		t.Fatalf("ListCandidates(rejected): %v", err)
	}
	if len(rejected) != 1 || rejected[0].Status != "rejected" {
		t.Fatalf("rejected = %+v, want exactly one rejected candidate", rejected)
	}
	if rejected[0].RejectionReason != "summary is empty" {
		t.Errorf("rejected RejectionReason = %q", rejected[0].RejectionReason)
	}

	// A different conversation must not leak candidates.
	seedCandidateConversation(t, db, "conv-2")
	other, err := ListCandidates(ctx, db, "conv-2", "")
	if err != nil {
		t.Fatalf("ListCandidates(conv-2): %v", err)
	}
	if len(other) != 0 {
		t.Fatalf("conv-2 candidates = %d, want 0", len(other))
	}
}

func TestInsertCandidatesIdempotent(t *testing.T) {
	db := newCandidateDB(t)
	ctx := context.Background()
	seedCandidateConversation(t, db, "conv-1")
	now := time.UnixMilli(1_700_000_000_000)

	first, err := InsertCandidates(ctx, db, "conv-1", "run-1", "job-1", "synthetic",
		[]ValidatedDraft{sampleValidatedDraft()}, now)
	if err != nil {
		t.Fatalf("first InsertCandidates: %v", err)
	}
	second, err := InsertCandidates(ctx, db, "conv-1", "run-1", "job-1", "synthetic",
		[]ValidatedDraft{sampleValidatedDraft()}, now.Add(time.Second))
	if err != nil {
		t.Fatalf("second InsertCandidates: %v", err)
	}
	if len(first) != 1 || len(second) != 1 || first[0] != second[0] {
		t.Fatalf("ids first=%v second=%v, want identical single id", first, second)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM memory_candidates WHERE conversation_id='conv-1'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("candidate rows = %d, want 1 after repeat insert", count)
	}

	// A different summary must produce a different deterministic id.
	other := sampleValidatedDraft()
	other.Summary = "另一条候选"
	ids, err := InsertCandidates(ctx, db, "conv-1", "run-1", "job-1", "synthetic",
		[]ValidatedDraft{other}, now)
	if err != nil {
		t.Fatalf("InsertCandidates(other): %v", err)
	}
	if ids[0] == first[0] {
		t.Fatalf("different summary produced same id %q", ids[0])
	}
}

func TestCandidateFieldsRoundTrip(t *testing.T) {
	db := newCandidateDB(t)
	ctx := context.Background()
	seedCandidateConversation(t, db, "conv-1")
	now := time.UnixMilli(1_700_000_123_456)

	draft := sampleValidatedDraft()
	draft.InterpretationNote = "note text"
	if _, err := InsertCandidates(ctx, db, "conv-1", "run-9", "job-9", "glm-5.3-flash",
		[]ValidatedDraft{draft}, now); err != nil {
		t.Fatalf("InsertCandidates: %v", err)
	}

	got, err := ListCandidates(ctx, db, "conv-1", "validated")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1", len(got))
	}
	c := got[0]
	if c.ConversationID != "conv-1" || c.RunID != "run-9" || c.JobID != "job-9" {
		t.Errorf("identity fields = %+v", c)
	}
	if c.Kind != "observation" || c.Summary != draft.Summary || c.TopicTitle != "工作节奏" {
		t.Errorf("content fields = %+v", c)
	}
	if c.InterpretationNote != "note text" || c.GeneratorModel != "glm-5.3-flash" {
		t.Errorf("note/model fields = %+v", c)
	}
	if len(c.EvidenceTurnIDs) != 2 || c.EvidenceTurnIDs[0] != "turn-1" || c.EvidenceTurnIDs[1] != "turn-2" {
		t.Errorf("EvidenceTurnIDs = %v", c.EvidenceTurnIDs)
	}
	if c.CreatedAt.UnixMilli() != now.UnixMilli() {
		t.Errorf("CreatedAt = %v, want %v", c.CreatedAt, now)
	}
}

func TestInsertCandidatesEmptyEvidenceIsArray(t *testing.T) {
	db := newCandidateDB(t)
	ctx := context.Background()
	seedCandidateConversation(t, db, "conv-1")

	d := sampleValidatedDraft()
	d.EvidenceTurnIDs = nil
	if _, err := InsertCandidates(ctx, db, "conv-1", "run-1", "", "",
		[]ValidatedDraft{d}, time.UnixMilli(1)); err != nil {
		t.Fatalf("InsertCandidates: %v", err)
	}
	var raw string
	if err := db.QueryRow(`SELECT evidence_turn_ids_json FROM memory_candidates WHERE conversation_id='conv-1'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw != "[]" {
		t.Fatalf("evidence_turn_ids_json = %q, want []", raw)
	}
}

func TestInsertPublicationRunReadBack(t *testing.T) {
	db := newCandidateDB(t)
	ctx := context.Background()
	seedCandidateConversation(t, db, "conv-1")
	now := time.UnixMilli(1_700_000_000_000)

	rec := PublicationRun{
		ID:               "pub-1",
		ConversationID:   "conv-1",
		ExpectedVersion:  "ann-1",
		CommittedVersion: "auto-abcdef012345",
		CandidateIDs:     []string{"cand:a", "cand:b"},
		Status:           "committed",
	}
	if err := InsertPublicationRun(ctx, db, rec, now); err != nil {
		t.Fatalf("InsertPublicationRun: %v", err)
	}

	var (
		convID, expected, committed, candidateJSON, status, errText string
		createdAt                                                   int64
	)
	if err := db.QueryRow(
		`SELECT conversation_id, expected_version, committed_version, candidate_ids_json, status, error, created_at FROM publication_runs WHERE id='pub-1'`,
	).Scan(&convID, &expected, &committed, &candidateJSON, &status, &errText, &createdAt); err != nil {
		t.Fatal(err)
	}
	if convID != "conv-1" || expected != "ann-1" || committed != "auto-abcdef012345" || status != "committed" || errText != "" {
		t.Fatalf("publication row = %q %q %q %q %q", convID, expected, committed, status, errText)
	}
	var ids []string
	if err := json.Unmarshal([]byte(candidateJSON), &ids); err != nil {
		t.Fatalf("unmarshal candidate ids: %v", err)
	}
	if len(ids) != 2 || ids[0] != "cand:a" || ids[1] != "cand:b" {
		t.Fatalf("candidate ids = %v", ids)
	}
	if createdAt != now.UnixMilli() {
		t.Fatalf("created_at = %d, want %d", createdAt, now.UnixMilli())
	}

	// Re-inserting the same id with a new outcome replaces the row.
	rec.Status = "conflict"
	rec.CommittedVersion = ""
	rec.Error = "annotation version conflict"
	if err := InsertPublicationRun(ctx, db, rec, now.Add(time.Second)); err != nil {
		t.Fatalf("second InsertPublicationRun: %v", err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM publication_runs`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("publication_runs rows = %d, want 1", count)
	}
	if err := db.QueryRow(`SELECT status, error FROM publication_runs WHERE id='pub-1'`).Scan(&status, &errText); err != nil {
		t.Fatal(err)
	}
	if status != "conflict" || errText != "annotation version conflict" {
		t.Fatalf("updated row = %q %q", status, errText)
	}
}

func TestInsertPublicationRunEmptyCandidatesIsArray(t *testing.T) {
	db := newCandidateDB(t)
	ctx := context.Background()
	seedCandidateConversation(t, db, "conv-1")

	rec := PublicationRun{ID: "pub-2", ConversationID: "conv-1", ExpectedVersion: "v", Status: "failed"}
	if err := InsertPublicationRun(ctx, db, rec, time.UnixMilli(1)); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := db.QueryRow(`SELECT candidate_ids_json FROM publication_runs WHERE id='pub-2'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw != "[]" {
		t.Fatalf("candidate_ids_json = %q, want []", raw)
	}
}
