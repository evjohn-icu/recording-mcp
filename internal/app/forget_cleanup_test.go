package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"recordingmcp/internal/memory"
	"recordingmcp/internal/plane"
)

func TestForgetTurnCompletesAudioCleanupAndPreservesNeighbors(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	path := filepath.Join(f.audioDir, "rec-provider-1.pcm")
	original := writeAudioFile(t, path, 30000)
	f.insertSync(t, "ingested", path)
	if _, err := f.store.DB().Exec(`UPDATE sync_recordings SET total_samples=480000 WHERE recording_id=?`, testProviderRecording); err != nil {
		t.Fatal(err)
	}
	id := f.turnRowID(t, "seg-a", "t-1")
	req := memory.ForgetRequest{Scope: "turn", TurnID: id}
	p, err := f.svc.PreviewForget(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if p.RequiresBroadenedAudio {
		t.Fatalf("valid stitched PCM should allow interval cleanup: %+v", p)
	}
	req.ExpectedState = p.ExpectedState
	result, err := f.svc.CommitForget(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if result.CleanupPending || result.Status != "complete" {
		t.Fatalf("cleanup did not complete: %+v", result)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	from, to := 1000*pcmBytesPerMS, 2000*pcmBytesPerMS
	if !bytes.Equal(got[:from], original[:from]) || !bytes.Equal(got[to:], original[to:]) || !bytes.Equal(got[from:to], make([]byte, to-from)) {
		t.Fatal("forgotten interval or adjacent audio incorrect")
	}
	if _, err := f.svc.AudioClip(ctx, id); err == nil {
		t.Fatal("forgotten turn still playable")
	}
	neighborID := f.turnRowID(t, "seg-a", "t-2")
	neighbor, err := f.svc.AudioClip(ctx, neighborID)
	if err != nil {
		t.Fatalf("neighboring turn should remain playable after cleanup: %v", err)
	}
	neighborPCM, err := io.ReadAll(io.LimitReader(neighbor.PCM, neighbor.Size))
	if closeErr := neighbor.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	neighborFrom, neighborTo := 2000*pcmBytesPerMS, 3000*pcmBytesPerMS
	if !bytes.Equal(neighborPCM, original[neighborFrom:neighborTo]) {
		t.Fatal("neighboring turn did not play its unchanged PCM after cleanup")
	}
	if _, err := f.svc.RetryForget(ctx, result.TombstoneID); err != nil {
		t.Fatalf("cleanup retry: %v", err)
	}
}

func TestForgetCleanupFailureStaysPending(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "outside.pcm")
	writeAudioFile(t, path, 30000)
	f.insertSync(t, "ingested", path)
	req := memory.ForgetRequest{Scope: "recording", RecordingID: f.recordingID}
	p, err := f.svc.PreviewForget(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	req.ExpectedState = p.ExpectedState
	result, err := f.svc.CommitForget(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if !result.CleanupPending || result.Status == "complete" {
		t.Fatal("unsafe external path claimed complete")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("external file touched")
	}
	pending, err := f.svc.PendingForgets(ctx)
	if err != nil || len(pending) != 1 || pending[0].ID != result.TombstoneID || pending[0].Status != "pending_external_cleanup" {
		t.Fatalf("pending forget was not discoverable: %+v err=%v", pending, err)
	}
	encoded, err := json.Marshal(pending)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), path) || strings.Contains(string(encoded), "outside.pcm") {
		t.Fatalf("pending forget listing leaked an audio path: %s", encoded)
	}
	status, err := f.store.ForgetStatus(ctx, result.TombstoneID)
	if err != nil || status.Status != "pending_external_cleanup" {
		t.Fatalf("read-only listing ran cleanup: %+v err=%v", status, err)
	}
}

func TestSuppressCleanupFailureCanRetryAfterRestartAndKeepsUnrelatedFacts(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	planePath := filepath.Join(t.TempDir(), "events.db")
	events, err := plane.Open(planePath)
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	if _, err := events.Ingest(ctx, []plane.Event{{ID: "unrelated", Type: "fact.observed", Timestamp: time.Now(), ObservedAt: time.Now(), Source: "human", Content: "keep this fact", Confidence: .9, ConversationID: f.conversationID, StructuredData: map[string]any{"scope": "profile-a"}}}); err != nil {
		t.Fatal(err)
	}
	f.svc.ForgetPlane = events
	indexPath := filepath.Join(t.TempDir(), "index-is-a-file")
	if err := os.WriteFile(indexPath, []byte("force cleanup failure"), 0600); err != nil {
		t.Fatal(err)
	}
	f.svc.ManagedIndexDir = indexPath
	rowID := memory.NoteRowID(f.conversationID, "note:one")
	first, err := f.svc.SuppressNote(ctx, f.conversationID, rowID, "v1")
	if err != nil {
		t.Fatalf("suppress: %v", err)
	}
	if first.OperationID == "" || !first.CleanupPending || first.Status != "pending_external_cleanup" {
		t.Fatalf("failed cleanup was not returned as durable pending operation: %+v", first)
	}
	if _, err := f.svc.GetWebItem(ctx, rowID); err == nil {
		t.Fatal("suppressed note remains visible")
	}
	rows, err := events.History(ctx, "profile-a")
	if err != nil || len(rows) != 1 || rows[0].ID != "unrelated" {
		t.Fatalf("note suppression should preserve unlinked same-conversation facts: %+v err=%v", rows, err)
	}

	// Reopen the canonical store to prove the pending cleanup is durable across
	// process restart, then make the managed path valid and retry by operation ID.
	dbPath := filepath.Join(filepath.Dir(f.audioDir), "memory.db")
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := memory.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	f.store = reopened
	f.svc.Store = reopened
	status, err := f.svc.SuppressionCleanupStatus(ctx, first.OperationID)
	if err != nil || status.Status != "pending_external_cleanup" {
		t.Fatalf("pending status missing: %+v err=%v", status, err)
	}
	pending, err := f.svc.PendingSuppressionCleanups(ctx)
	if err != nil || len(pending) != 1 || pending[0].OperationID != first.OperationID {
		t.Fatalf("pending cleanup was not discoverable after restart: %+v err=%v", pending, err)
	}
	if err := os.Remove(indexPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(indexPath, 0700); err != nil {
		t.Fatal(err)
	}
	second, err := f.svc.RetrySuppressionCleanup(ctx, first.OperationID)
	if err != nil || second.CleanupPending || second.Status != "complete" {
		t.Fatalf("retry did not complete cleanup: %+v err=%v", second, err)
	}
	// A repeated suppression request without the now-stale annotation version
	// resolves to the same operation and remains safe to retry.
	repeated, err := f.svc.SuppressNote(ctx, f.conversationID, rowID, "")
	if err != nil || repeated.OperationID != first.OperationID || repeated.Status != "complete" {
		t.Fatalf("suppression retry did not resolve existing operation: %+v err=%v", repeated, err)
	}
}

func TestForgetTurnPreservesIndependentTemporalFacts(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	events, err := plane.Open(t.TempDir() + "/events.db")
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	f.svc.ForgetPlane = events
	ts := &TimeService{Store: f.store, Events: events}
	for i, turn := range []string{"t-1", "t-2"} {
		e := plane.Event{ID: turn, Type: "fact.observed", Timestamp: time.Now(), Source: "local-web", Content: turn, StructuredData: map[string]any{"dimension": "topic"}}
		if _, err := ts.ObserveFact(ctx, "local", FactRequest{Event: e, Evidence: []EvidenceReference{{TurnID: f.turnRowID(t, "seg-a", turn), Revision: "rev-1"}}}); err != nil {
			t.Fatalf("fact %d: %v", i, err)
		}
	}
	req := memory.ForgetRequest{Scope: "turn", TurnID: f.turnRowID(t, "seg-a", "t-1")}
	preview, err := f.svc.PreviewForget(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	req.ExpectedState = preview.ExpectedState
	result, err := f.svc.CommitForget(ctx, req)
	if err != nil || result.CleanupPending {
		t.Fatalf("forget: %+v %v", result, err)
	}
	rows, err := events.Search(ctx, plane.Query{})
	if err != nil {
		t.Fatal(err)
	}
	preserved := false
	for _, e := range rows {
		if e.ID == "t-1" && e.Content != "[purged]" {
			t.Fatalf("forgotten fact remains: %+v", e)
		}
		if e.ID == "t-2" && e.Content == "t-2" {
			preserved = true
		}
	}
	if !preserved {
		t.Fatal("unrelated fact from same conversation lost")
	}
}
