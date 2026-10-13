package pipeline

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"recordingmcp/internal/asrinput"
	"recordingmcp/internal/decision"
	"recordingmcp/internal/generate"
	"recordingmcp/internal/memory"
	"recordingmcp/internal/process"
	"recordingmcp/internal/semantic"
	"recordingmcp/internal/worker"
)

// e2eResult builds one final ASR segment for the end-to-end revision test.
func e2eResult(segment, revision, turnID, text string, offset int64) asrinput.Result {
	start, end := int64(10), int64(900)
	globalStart, globalEnd := offset+start, offset+end
	sp := "spk:device-1:1"
	return asrinput.Result{
		ContractVersion: asrinput.ContractVersion,
		Source: asrinput.Source{
			Provider: "provider-x", ProviderRequestID: "req-" + segment + "-" + revision,
			DeviceID: "device-1", RecordingID: "rec-e2e", ConversationID: "meeting-e2e",
			SegmentID: segment, SegmentOffsetMS: offset, SegmentDurationMS: 1000,
			TranscriptRevision: revision, CompletionStatus: "final",
		},
		Turns: []asrinput.Turn{{
			ID: turnID, OriginalIndex: 1, Text: text,
			LocalStartMS: &start, LocalEndMS: &end,
			GlobalStartMS: &globalStart, GlobalEndMS: &globalEnd,
			SpeakerScopedID: &sp,
		}},
	}
}

// e2eClaim claims the oldest runnable job of one stage.
func e2eClaim(t *testing.T, db *sql.DB, stage string) process.Job {
	t.Helper()
	job, err := process.Claim(context.Background(), db, process.ClaimOptions{
		WorkerID: "e2e-worker", Stages: []string{stage}, Lease: time.Minute, Now: time.Now(),
	})
	if err != nil {
		t.Fatalf("claim %s: %v", stage, err)
	}
	if job == nil {
		t.Fatalf("no runnable %s job", stage)
	}
	return *job
}

func e2eSearchHit(page memory.SearchPage, kind, id string) (memory.SearchHit, bool) {
	for _, h := range page.Hits {
		if h.Kind == kind && h.ID == id {
			return h, true
		}
	}
	return memory.SearchHit{}, false
}

// TestEndToEndSupersedeReprocess proves the full sequence: ingest v1 -> extract
// -> publish -> search finds the derived note; ingest v2 superseding v1 marks
// the old note stale and queues re-analysis for the new attachment; re-extract
// and re-publish make the new note current while the old note stays auditable
// through include_stale, get_evidence_context and get_annotation_history.
func TestEndToEndSupersedeReprocess(t *testing.T) {
	ctx := context.Background()
	store, err := memory.Open(filepath.Join(t.TempDir(), "e2e.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	db := store.DB()

	const v1Text = "预算批准了"
	const v2Text = "预算下周三批准"

	// --- v1: ingest -> extract -> publish ---
	v1 := e2eResult("seg-1", "rev-1", "utt-1", v1Text, 0)
	report, err := store.IngestTranscript(ctx, v1)
	if err != nil {
		t.Fatalf("ingest v1: %v", err)
	}
	conv := report.ConversationID

	extract := &ExtractHandler{DB: db, Engine: decision.Synthetic{}, Endpoint: "http://test/v1", ProcessorVersion: "extract-v1", Generator: generate.Synthetic{}}
	if _, err := extract.Execute(ctx, e2eClaim(t, db, "extract")); err != nil {
		t.Fatalf("extract v1: %v", err)
	}
	publish := &PublishHandler{DB: db, Store: store, ProcessorVersion: "extract-v1"}
	firstRef, err := publish.Execute(ctx, e2eClaim(t, db, "publish"))
	if err != nil {
		t.Fatalf("publish v1: %v", err)
	}
	if !strings.HasPrefix(firstRef, "publish:auto-") {
		t.Fatalf("first publish ref = %q, want committed auto version", firstRef)
	}

	page, err := store.SearchMemory(ctx, memory.MemorySearch{Text: "合成候选", Scope: "both"})
	if err != nil {
		t.Fatalf("search v1: %v", err)
	}
	var oldNote memory.SearchHit
	found := false
	for _, h := range page.Hits {
		if h.Kind == "note" {
			oldNote, found = h, true
			break
		}
	}
	if !found {
		t.Fatalf("no derived note visible after first publish; hits=%+v", page.Hits)
	}
	if len(oldNote.EvidenceTurnIDs) == 0 {
		t.Fatalf("note %s has no evidence turns", oldNote.ID)
	}
	v1TurnID := oldNote.EvidenceTurnIDs[0]

	// --- v2 supersedes rev-1 ---
	v2 := e2eResult("seg-1", "rev-2", "utt-1", v2Text, 0)
	v2.Source.SupersedesRevision = "rev-1"
	if _, err := store.IngestTranscript(ctx, v2); err != nil {
		t.Fatalf("ingest v2 supersede: %v", err)
	}
	var stale int
	if err := db.QueryRow(`SELECT stale FROM derived_artifacts WHERE kind='note' AND artifact_id=?`, oldNote.ID).Scan(&stale); err != nil {
		t.Fatalf("old note staleness: %v", err)
	}
	if stale != 1 {
		t.Fatalf("old note %s not marked stale after supersede", oldNote.ID)
	}
	var newAttachmentID string
	if err := db.QueryRow(`SELECT id FROM source_attachments WHERE segment_id='seg-1' AND transcript_revision='rev-2'`).Scan(&newAttachmentID); err != nil {
		t.Fatalf("new attachment id: %v", err)
	}
	var queued int
	if err := db.QueryRow(`SELECT COUNT(*) FROM processing_jobs WHERE stage='extract' AND input_snapshot_id=? AND status='queued'`, newAttachmentID).Scan(&queued); err != nil {
		t.Fatalf("queued extract job: %v", err)
	}
	if queued != 1 {
		t.Fatalf("queued extract jobs for new attachment = %d, want 1", queued)
	}
	// The superseded attachment row, its turns and its links survive as audit.
	var auditRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM attachment_turns at JOIN source_attachments a ON a.id=at.attachment_id WHERE a.segment_id='seg-1' AND a.transcript_revision='rev-1' AND a.active=0`).Scan(&auditRows); err != nil {
		t.Fatalf("audit rows: %v", err)
	}
	if auditRows != 1 {
		t.Fatalf("v1 audit rows = %d, want 1", auditRows)
	}

	// --- re-extract the new revision, then re-publish ---
	job := e2eClaim(t, db, "extract")
	if job.InputSnapshotID != newAttachmentID {
		t.Fatalf("re-extract claimed snapshot %s, want %s", job.InputSnapshotID, newAttachmentID)
	}
	if _, err := extract.Execute(ctx, job); err != nil {
		t.Fatalf("extract v2: %v", err)
	}
	secondRef, err := publish.Execute(ctx, e2eClaim(t, db, "publish"))
	if err != nil {
		t.Fatalf("publish v2: %v", err)
	}
	if !strings.HasPrefix(secondRef, "publish:auto-") || secondRef == firstRef {
		t.Fatalf("second publish ref = %q, want a new committed version", secondRef)
	}

	// Current search: the old note is gone, only v2-derived memory remains.
	page, err = store.SearchMemory(ctx, memory.MemorySearch{Text: "合成候选", Scope: "both"})
	if err != nil {
		t.Fatalf("search v2: %v", err)
	}
	if _, ok := e2eSearchHit(page, "note", oldNote.ID); ok {
		t.Fatalf("stale note %s still returned as current", oldNote.ID)
	}
	currentNotes := 0
	for _, h := range page.Hits {
		if h.Kind == "note" {
			currentNotes++
			if h.Stale {
				t.Fatalf("current search returned stale note %s", h.ID)
			}
		}
	}
	if currentNotes == 0 {
		t.Fatalf("no current note after re-publish; hits=%+v", page.Hits)
	}

	// Explicit audit: include_stale surfaces the old note and marks it.
	stalePage, err := store.SearchMemory(ctx, memory.MemorySearch{Text: "合成候选", Scope: "both", IncludeStale: true})
	if err != nil {
		t.Fatalf("search include_stale: %v", err)
	}
	audited, ok := e2eSearchHit(stalePage, "note", oldNote.ID)
	if !ok || !audited.Stale {
		t.Fatalf("include_stale did not surface stale note %s (ok=%v stale=%v)", oldNote.ID, ok, audited.Stale)
	}

	// get_evidence_context on a superseded turn is still an explicit audit path.
	ev, err := store.GetEvidenceContext(ctx, v1TurnID, 1, 4000)
	if err != nil {
		t.Fatalf("evidence context v1 turn: %v", err)
	}
	if !ev.Stale {
		t.Fatalf("v1 turn %s not reported stale", v1TurnID)
	}
	foundV1 := false
	for _, turn := range ev.Turns {
		if turn.ID == v1TurnID && strings.Contains(turn.Text, "预算批准了") {
			foundV1 = true
		}
	}
	if !foundV1 {
		t.Fatalf("v1 evidence window lost the superseded turn text: %+v", ev.Turns)
	}

	// The first annotation version remains in history.
	history, err := store.GetAnnotationHistory(ctx, conv)
	if err != nil {
		t.Fatalf("annotation history: %v", err)
	}
	firstVersion := strings.TrimPrefix(firstRef, "publish:")
	archived := false
	for _, snap := range history {
		if snap.Version == firstVersion && !snap.Current {
			archived = true
		}
	}
	if !archived {
		t.Fatalf("first annotation version %s not archived; history=%+v", firstVersion, history)
	}

	// --- embed round: the supersede cycle must re-index on its own ---
	// The publish-time enqueueEmbed jobs are still queued, so the index status
	// must honestly report pending; after the embed stage drains it must be
	// ready again with no manual bin/context-embed run.
	status, err := store.GetProcessingStatus(ctx, conv)
	if err != nil {
		t.Fatalf("processing status: %v", err)
	}
	if status.IndexingStatus != "pending" {
		t.Fatalf("indexing_status = %q while embed jobs are queued, want pending", status.IndexingStatus)
	}
	embed := &EmbedHandler{DB: db, Provider: semantic.NewHashProvider(semantic.DefaultHashDims)}
	// Drain the queued embed jobs through the real worker path so jobs are
	// claimed AND completed, exactly as bin/context-worker would.
	embedDone := false
	for range 10 {
		more, err := worker.RunOnce(ctx, worker.Config{
			DB: db, WorkerID: "e2e-worker", Stages: []string{"embed"},
			Lease: time.Minute, ProcessorVersion: "embed-v1",
			Executor: embedExecutor{h: embed},
		})
		if err != nil {
			t.Fatalf("embed worker run: %v", err)
		}
		if !more {
			embedDone = true
			break
		}
	}
	if !embedDone {
		t.Fatal("embed worker did not drain the queue")
	}
	status, err = store.GetProcessingStatus(ctx, conv)
	if err != nil {
		t.Fatalf("processing status after embed: %v", err)
	}
	if status.IndexingStatus != "ready" {
		t.Fatalf("indexing_status = %q after the supersede round, want ready", status.IndexingStatus)
	}
}

// embedExecutor adapts EmbedHandler to the worker.Executor interface for the
// supersede re-index regression.
type embedExecutor struct{ h *EmbedHandler }

func (e embedExecutor) Execute(ctx context.Context, job process.Job) (string, error) {
	return e.h.Execute(ctx, job)
}
