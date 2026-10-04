package pipeline

import (
	"context"
	"database/sql"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"recordingmcp/internal/memory"
	"recordingmcp/internal/migrations"
	"recordingmcp/internal/process"
)

// openTestDBAndStore opens the same SQLite file through a raw handle (used for
// candidates, jobs and publication runs) and a memory.Store (used for the
// annotation view), mirroring how cmd/context-worker wires the publish stage.
func openTestDBAndStore(t *testing.T) (*sql.DB, *memory.Store) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "publish.db")
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

	store, err := memory.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return db, store
}

// seedTwoTurns inserts a conversation with two turns and returns its id.
func seedTwoTurns(t *testing.T, db *sql.DB, conversationID string) {
	t.Helper()
	zero, oneThousand := int64(0), int64(1000)
	seedConversation(t, db, conversationID, []struct {
		id      string
		seg     int
		start   *int64
		orig    int
		speaker *string
	}{
		{id: "t1", seg: 0, start: &zero, orig: 1, speaker: nil},
		{id: "t2", seg: 0, start: &oneThousand, orig: 2, speaker: nil},
	})
}

// insertValidated stores two validated candidates citing real turns.
func insertValidated(t *testing.T, db *sql.DB, conversationID, runID string) []string {
	t.Helper()
	ids, err := InsertCandidates(context.Background(), db, conversationID, runID, "job-"+runID, "synthetic", []ValidatedDraft{
		{
			CandidateDraft: CandidateDraft{
				Kind: "observation", Summary: "讨论排期", TopicTitle: "排期",
				EvidenceTurnIDs: []string{"t1"}, InterpretationNote: "note",
			},
			Status: "validated",
		},
		{
			CandidateDraft: CandidateDraft{
				Kind: "request", Summary: "需要确认预算", TopicTitle: "预算",
				EvidenceTurnIDs: []string{"t2"},
			},
			Status: "validated",
		},
	}, time.Now())
	if err != nil {
		t.Fatalf("insert candidates: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("insert candidates: got %d ids", len(ids))
	}
	return ids
}

// claimPublishJob enqueues and claims one publish job for a conversation.
func claimPublishJob(t *testing.T, db *sql.DB, conversationID, runID string) process.Job {
	t.Helper()
	spec := process.Spec{
		Stage:           "publish",
		InputSnapshotID: conversationID,
		IdempotencyKey:  "publish|" + conversationID + "|" + runID,
		PayloadJSON:     `{"conversation_id":"` + conversationID + `"}`,
		MaxAttempts:     5,
	}
	if _, _, err := process.Enqueue(context.Background(), db, spec, time.Now()); err != nil {
		t.Fatalf("enqueue publish: %v", err)
	}
	job, err := process.Claim(context.Background(), db, process.ClaimOptions{
		WorkerID: "publish-worker", Stages: []string{"publish"}, Lease: time.Minute, Now: time.Now(),
	})
	if err != nil {
		t.Fatalf("claim publish: %v", err)
	}
	return *job
}

func TestPublishHandlerCommitsAndReplays(t *testing.T) {
	ctx := context.Background()
	db, store := openTestDBAndStore(t)
	conv := "conv-publish-1"
	seedTwoTurns(t, db, conv)
	candidateIDs := insertValidated(t, db, conv, "run-1")
	job := claimPublishJob(t, db, conv, "run-1")

	h := &PublishHandler{DB: db, Store: store, ProcessorVersion: "publish-v1"}
	result, err := h.Execute(ctx, job)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if !strings.HasPrefix(result, "publish:auto-") {
		t.Fatalf("unexpected result ref %q", result)
	}

	a, version, err := store.CurrentAnnotations(ctx, conv)
	if err != nil {
		t.Fatalf("current annotations: %v", err)
	}
	if version != strings.TrimPrefix(result, "publish:") {
		t.Fatalf("stored version %q does not match result %q", version, result)
	}
	if a.AnnotationVersion != version || a.AnnotationBasis != autoBasis {
		t.Fatalf("annotation metadata not applied: %+v", a)
	}
	if a.Title == "" {
		t.Fatalf("publish did not fill a title")
	}
	if len(a.Topics) != 2 || len(a.Notes) != 2 {
		t.Fatalf("want 2 topics and 2 notes, got %d/%d", len(a.Topics), len(a.Notes))
	}
	for _, topic := range a.Topics {
		if !strings.HasPrefix(topic.ID, autoTopicPrefix) || strings.TrimSpace(topic.Title) == "" {
			t.Fatalf("unexpected auto topic %+v", topic)
		}
	}
	for _, note := range a.Notes {
		if !strings.HasPrefix(note.ID, autoNotePrefix) {
			t.Fatalf("unexpected auto note id %q", note.ID)
		}
		if len(note.EvidenceTurnIDs) != 1 || (note.EvidenceTurnIDs[0] != "t1" && note.EvidenceTurnIDs[0] != "t2") {
			t.Fatalf("note evidence must be a real turn id, got %v", note.EvidenceTurnIDs)
		}
	}

	// The deterministic version keys off the sorted candidate ids.
	sortedIDs := append([]string(nil), candidateIDs...)
	sort.Strings(sortedIDs)
	expectedVersion := "auto-" + sha256Hex(strings.Join(sortedIDs, "|"), 12)
	if version != expectedVersion {
		t.Fatalf("version %q != %q", version, expectedVersion)
	}

	// Replaying the same job is idempotent: same version, single committed run.
	replay, err := h.Execute(ctx, job)
	if err != nil {
		t.Fatalf("replay publish: %v", err)
	}
	if replay != result {
		t.Fatalf("replay result %q != %q", replay, result)
	}
	var committed int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM publication_runs WHERE conversation_id=? AND status='committed'`, conv).Scan(&committed); err != nil {
		t.Fatalf("count committed runs: %v", err)
	}
	if committed != 1 {
		t.Fatalf("want 1 committed run, got %d", committed)
	}
	var noteCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notes WHERE conversation_id=?`, conv).Scan(&noteCount); err != nil {
		t.Fatalf("count notes: %v", err)
	}
	if noteCount != 2 {
		t.Fatalf("replay duplicated notes: got %d", noteCount)
	}
}

func TestPublishHandlerNoCandidatesIsNoop(t *testing.T) {
	ctx := context.Background()
	db, store := openTestDBAndStore(t)
	conv := "conv-publish-noop"
	seedTwoTurns(t, db, conv)
	job := claimPublishJob(t, db, conv, "run-empty")

	h := &PublishHandler{DB: db, Store: store}
	result, err := h.Execute(ctx, job)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if result != "publish:noop" {
		t.Fatalf("want publish:noop, got %q", result)
	}
	var runs int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM publication_runs WHERE conversation_id=?`, conv).Scan(&runs); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runs != 0 {
		t.Fatalf("noop publish must not record a run, got %d", runs)
	}
}

func TestPublishHandlerRecordsConflict(t *testing.T) {
	ctx := context.Background()
	db, store := openTestDBAndStore(t)
	conv := "conv-publish-conflict"
	seedTwoTurns(t, db, conv)
	insertValidated(t, db, conv, "run-1")
	job := claimPublishJob(t, db, conv, "run-1")

	h := &PublishHandler{DB: db, Store: store}
	result, err := h.Execute(ctx, job)
	if err != nil {
		t.Fatalf("first publish: %v", err)
	}
	staleVersion := strings.TrimPrefix(result, "publish:")

	// A concurrent human edit moves the version forward after we captured it.
	if _, err := db.ExecContext(ctx, `UPDATE conversations SET annotation_version='manual-edit' WHERE id=?`, conv); err != nil {
		t.Fatalf("bump version: %v", err)
	}
	if _, err := h.publish(ctx, conv, staleVersion); err == nil {
		t.Fatalf("expected a version conflict")
	} else if !strings.Contains(err.Error(), "annotation version conflict") {
		t.Fatalf("unexpected conflict error: %v", err)
	}

	var recorded string
	if err := db.QueryRowContext(ctx, `SELECT error FROM publication_runs WHERE conversation_id=? AND status='conflict'`, conv).Scan(&recorded); err != nil {
		t.Fatalf("read conflict run: %v", err)
	}
	if !strings.Contains(recorded, "annotation version conflict") {
		t.Fatalf("conflict run did not capture the error: %q", recorded)
	}
}

func TestPublishHandlerPreservesExistingNotes(t *testing.T) {
	ctx := context.Background()
	db, store := openTestDBAndStore(t)
	conv := "conv-publish-preserve"
	seedTwoTurns(t, db, conv)

	manual := memory.Annotations{
		ConversationID:    conv,
		Title:             "手工标题",
		AnnotationVersion: "manual-1",
		AnnotationBasis:   "人工复核",
		Topics:            []memory.TopicAnnotation{{ID: "topic:manual:1", Title: "手工主题"}},
		Notes: []memory.NoteAnnotation{{
			ID: "note:manual:1", Kind: "observation", TopicIDs: []string{"topic:manual:1"},
			Summary: "人工笔记", EvidenceTurnIDs: []string{"t1"},
		}},
	}
	if err := store.ApplyAnnotations(ctx, conv, "v1", manual); err != nil {
		t.Fatalf("seed manual annotations: %v", err)
	}

	insertValidated(t, db, conv, "run-1")
	job := claimPublishJob(t, db, conv, "run-1")
	h := &PublishHandler{DB: db, Store: store}
	if _, err := h.Execute(ctx, job); err != nil {
		t.Fatalf("publish: %v", err)
	}

	a, _, err := store.CurrentAnnotations(ctx, conv)
	if err != nil {
		t.Fatalf("current annotations: %v", err)
	}
	if len(a.Notes) != 3 || len(a.Topics) != 3 {
		t.Fatalf("want 3 notes/topics (1 manual + 2 auto), got %d/%d", len(a.Notes), len(a.Topics))
	}
	found := false
	for _, note := range a.Notes {
		if note.ID == "note:manual:1" {
			found = true
			if note.Summary != "人工笔记" || len(note.EvidenceTurnIDs) != 1 || note.EvidenceTurnIDs[0] != "t1" {
				t.Fatalf("manual note was altered: %+v", note)
			}
		}
	}
	if !found {
		t.Fatalf("manual note was dropped: %+v", a.Notes)
	}
}

func TestPublishHandlerRequiresStoreAndConversation(t *testing.T) {
	ctx := context.Background()
	db, store := openTestDBAndStore(t)
	if _, err := (&PublishHandler{Store: store}).Execute(ctx, process.Job{PayloadJSON: `{}`}); err == nil {
		t.Fatalf("expected DB requirement error")
	}
	if _, err := (&PublishHandler{DB: db}).Execute(ctx, process.Job{PayloadJSON: `{}`}); err == nil {
		t.Fatalf("expected Store requirement error")
	}
	if _, err := (&PublishHandler{DB: db, Store: store}).Execute(ctx, process.Job{PayloadJSON: `{"conversation_id":""}`}); err == nil {
		t.Fatalf("expected conversation_id requirement error")
	}
}
