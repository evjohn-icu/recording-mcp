package cloudsync

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"recordingmcp/internal/asrinput"
)

func addTestFence(t *testing.T, e *Engine, providerID string) {
	t.Helper()
	_, err := e.DB.Exec(`INSERT INTO forget_tombstones(id,scope,recording_id,requested_id,locator_key,expected_state_hash,status,created_at) VALUES('tombstone-test','recording',?,'requested-test','locator-test','hash-test','pending_external_cleanup',?)`, providerID, time.Now().UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB.Exec(`INSERT INTO forget_provider_fences(tombstone_id,provider_recording_id,device_id,created_at) VALUES('tombstone-test',?,'',?)`, providerID, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
}

func TestCommitEventFenceSanitizesAndDoesNotRestoreChunk(t *testing.T) {
	store := newTestStore(t)
	f := newFakeCloud(t)
	e := newTestEngine(t, f, store, nil)
	ctx := context.Background()
	ctx = context.WithValue(ctx, audioBaselineKey{}, &audioBaseline{existed: make(map[string]bool)})
	const recID = "rec-fenced"
	pcm := pcmOf(64)
	chunkDir := filepath.Join(e.AudioDir, recID)
	if err := os.MkdirAll(chunkDir, 0o700); err != nil {
		t.Fatal(err)
	}
	chunkPath := filepath.Join(chunkDir, "chunk-00000000.pcm")
	stitchedPath := e.stitchedPath(recID)
	trackAudioPath(ctx, chunkPath)
	trackAudioPath(ctx, stitchedPath)
	if err := os.WriteFile(chunkPath, pcm, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stitchedPath, pcm, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.ensureRecording(ctx, recID, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	addTestFence(t, e, recID)
	ev := &Event{ID: 1, Type: "chunk.committed", RecordingID: recID, Payload: []byte(`{"transcript":"private payload"}`)}
	extraCalled := false
	if err := e.commitEvent(ctx, ev, func(context.Context, *sql.Tx) error {
		extraCalled = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if extraCalled {
		t.Fatal("event side effect ran after the forget fence")
	}
	if string(ev.Payload) != `{}` {
		t.Fatalf("event payload after fence = %s, want sanitized object", ev.Payload)
	}
	if got := queryInt(t, e.DB, `SELECT COUNT(*) FROM sync_events WHERE event_id=1 AND payload_json='{}'`); got != 1 {
		t.Fatalf("sanitized ledger rows = %d, want 1", got)
	}
	if got := queryInt(t, e.DB, `SELECT COUNT(*) FROM sync_chunks WHERE recording_id=?`, recID); got != 0 {
		t.Fatalf("chunk rows = %d, want 0", got)
	}
	if state := queryString(t, e.DB, `SELECT state FROM sync_recordings WHERE recording_id=?`, recID); state != stateDeleted {
		t.Fatalf("state = %q, want deleted", state)
	}
	for _, path := range []string{chunkPath, stitchedPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("forgotten audio remains at %s (stat err %v)", path, err)
		}
	}
}

func TestPartialForgetFencePreservesRegisteredAudioFiles(t *testing.T) {
	store := newTestStore(t)
	f := newFakeCloud(t)
	e := newTestEngine(t, f, store, nil)
	ctx := context.Background()
	const recID = "rec-partial"
	pcm := pcmOf(16000)
	chunkDir := filepath.Join(e.AudioDir, recID)
	if err := os.MkdirAll(chunkDir, 0o700); err != nil {
		t.Fatal(err)
	}
	chunkPath := e.chunkPath(recID, 0)
	stitchedPath := e.stitchedPath(recID)
	for _, path := range []string{chunkPath, stitchedPath} {
		if err := os.WriteFile(path, pcm, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.ensureRecording(ctx, recID, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB.Exec(`UPDATE sync_recordings SET stitched_path=? WHERE recording_id=?`, stitchedPath, recID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB.Exec(`INSERT INTO forget_tombstones(id,scope,recording_id,source_segment_id,segment_index,requested_id,start_ms,end_ms,locator_key,expected_state_hash,status,created_at) VALUES('tombstone-partial','turn',?,'segment-0',0,'turn-0',100,200,'locator-partial','hash-partial','pending_external_cleanup',?)`, recID, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{chunkPath, stitchedPath} {
		if _, err := e.DB.Exec(`INSERT INTO forget_files(tombstone_id,path,start_byte,end_byte,remove_file) VALUES('tombstone-partial',?,3200,6400,0)`, path); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.DB.Exec(`INSERT INTO forget_provider_fences(tombstone_id,provider_recording_id,device_id,created_at) VALUES('tombstone-partial',?,'',?)`, recID, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := e.commitEvent(ctx, &Event{ID: 1, Type: "recording.finished", RecordingID: recID, Payload: []byte(`{"private":"event body"}`)}, nil); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{chunkPath, stitchedPath} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("registered partial audio file %s was removed: %v", path, err)
		}
	}
	if got := queryInt(t, e.DB, `SELECT COUNT(*) FROM forget_files WHERE tombstone_id='tombstone-partial' AND start_byte=3200 AND end_byte=6400 AND remove_file=0`); got != 2 {
		t.Fatalf("partial byte-range entries = %d, want 2", got)
	}
	if got := queryString(t, e.DB, `SELECT stitched_path FROM sync_recordings WHERE recording_id=?`, recID); got != stitchedPath {
		t.Fatalf("partial forget cleared stitched_path: got %q want %q", got, stitchedPath)
	}
}

type delayedTranscriber struct {
	started chan struct{}
	release chan struct{}
}

func (d delayedTranscriber) Transcribe(ctx context.Context, path string, meta MetaBasis) ([]asrinput.Result, error) {
	close(d.started)
	select {
	case <-d.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return FixtureTranscriber{}.Transcribe(ctx, path, meta)
}

func TestForgetFenceWhileTranscriberRunsCleansStitchedAudio(t *testing.T) {
	f := newFakeCloud(t)
	f.seedRecording("rec-delayed")
	f.seedCreated("rec-delayed")
	f.seedChunk("rec-delayed", 0, pcmOf(16000))
	f.seedFinished("rec-delayed", 1, 16000)
	store := newTestStore(t)
	d := delayedTranscriber{started: make(chan struct{}), release: make(chan struct{})}
	e := newTestEngine(t, f, store, d)
	done := make(chan error, 1)
	go func() {
		_, err := e.RunOnce(context.Background())
		done <- err
	}()
	select {
	case <-d.started:
	case <-time.After(5 * time.Second):
		t.Fatal("transcriber did not start")
	}
	addTestFence(t, e, "rec-delayed")
	close(d.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("sync did not finish")
	}
	if got := queryInt(t, store.DB(), `SELECT COUNT(*) FROM turns`); got != 0 {
		t.Fatalf("turns after fenced transcription = %d, want 0", got)
	}
	if state := queryString(t, store.DB(), `SELECT state FROM sync_recordings WHERE recording_id='rec-delayed'`); state != stateDeleted {
		t.Fatalf("recording state = %q, want deleted", state)
	}
	for _, path := range []string{e.stitchedPath("rec-delayed"), filepath.Join(e.AudioDir, "rec-delayed")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("forgotten audio remains at %s (stat err %v)", path, err)
		}
	}
	var payload string
	if err := store.DB().QueryRow(`SELECT payload_json FROM sync_events WHERE recording_id='rec-delayed' ORDER BY event_id DESC LIMIT 1`).Scan(&payload); err == nil {
		var decoded map[string]any
		if err := json.Unmarshal([]byte(payload), &decoded); err != nil || len(decoded) != 0 {
			t.Errorf("last event payload = %s, want {}", payload)
		}
	}
}

func TestForgetFenceDuringDelayedDownloadPreventsChunkPublication(t *testing.T) {
	f := newFakeCloud(t)
	f.seedRecording("rec-download")
	f.seedCreated("rec-download")
	f.seedChunk("rec-download", 0, pcmOf(16000))
	f.audioStarted = make(chan struct{}, 1)
	f.audioRelease = make(chan struct{})
	store := newTestStore(t)
	e := newTestEngine(t, f, store, nil)
	done := make(chan error, 1)
	go func() {
		_, err := e.RunOnce(context.Background())
		done <- err
	}()
	select {
	case <-f.audioStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("chunk request did not start")
	}
	addTestFence(t, e, "rec-download")
	close(f.audioRelease)
	select {
	case err := <-done:
		if err != errLocallyForgotten {
			t.Fatalf("RunOnce error = %v, want local forget fence", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("sync did not finish")
	}
	chunkPath := e.chunkPath("rec-download", 0)
	if _, err := os.Stat(chunkPath); !os.IsNotExist(err) {
		t.Fatalf("chunk was published after fence (stat err %v)", err)
	}
	if got := queryInt(t, store.DB(), `SELECT COUNT(*) FROM sync_chunks WHERE recording_id='rec-download'`); got != 0 {
		t.Fatalf("chunk manifest rows = %d, want 0", got)
	}
	if _, err := e.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if payload := queryString(t, store.DB(), `SELECT payload_json FROM sync_events WHERE event_id=2`); payload != `{}` {
		t.Fatalf("replayed event payload = %s, want {}", payload)
	}
}
