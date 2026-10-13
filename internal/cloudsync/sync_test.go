package cloudsync

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"testing"

	"recordingmcp/internal/ingestvectors"
	"recordingmcp/internal/memory"
)

// sha256Hex replicates the wire checksum for seeded fixtures.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func pcmOf(samples int) []byte {
	return ingestvectors.PCM(samples, ingestvectors.Sine440)
}

// stableKeyTest replicates the memory package's stable id derivation so the
// tests can assert the exact processing_jobs idempotency keys.
func stableKeyTest(kind string, parts ...string) string {
	joined := append([]string{"recording-mcp/asr-input/v1", kind}, parts...)
	var key string
	for i, p := range joined {
		if i > 0 {
			key += "\x00"
		}
		key += p
	}
	sum := sha256.Sum256([]byte(key))
	return kind + ":" + hex.EncodeToString(sum[:])
}

type fakeRecording struct {
	chunks map[int64][]byte
}

type ackRecord struct {
	RecordingID string
	LastEventID int64
	State       string
}

// fakeCloud is an in-memory ingest-v1 server: an event log, a chunk store,
// a snapshot listing and an ack sink. It speaks just enough of the frozen
// contract for the sync engine, asserting the transport headers on every call.
type fakeCloud struct {
	mu         sync.Mutex
	tb         testing.TB
	srv        *httptest.Server
	events     []Event
	recs       map[string]*fakeRecording
	maxEventID int64
	// trimmedMin emulates event-log retention: events with id < trimmedMin
	// are no longer served by GET /v1/sync.
	trimmedMin int64
	acks       []ackRecord
}

func newFakeCloud(t *testing.T) *fakeCloud {
	f := &fakeCloud{recs: map[string]*fakeRecording{}, tb: t}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sync", f.handleSync)
	mux.HandleFunc("GET /v1/recordings/{id}/chunks/{seq}/audio", f.handleAudio)
	mux.HandleFunc("GET /v1/sync/recordings", f.handleSnapshot)
	mux.HandleFunc("POST /v1/sync/recordings/{id}/ack", f.handleAck)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeCloud) client() *Client {
	return &Client{BaseURL: f.srv.URL, SyncToken: "test-sync-token"}
}

func (f *fakeCloud) addEvent(typ, recID string, seq int64, payload any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	f.maxEventID++
	f.events = append(f.events, Event{
		ID: f.maxEventID, Type: typ, RecordingID: recID, Seq: seq,
		Payload: raw, CreatedAt: "2026-10-14T00:00:00Z",
	})
}

func (f *fakeCloud) seedRecording(recID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recs[recID] = &fakeRecording{chunks: map[int64][]byte{}}
}

// seedChunk stores the chunk bytes and appends the chunk.committed event.
func (f *fakeCloud) seedChunk(recID string, seq int64, pcm []byte) {
	f.mu.Lock()
	f.recs[recID].chunks[seq] = pcm
	f.mu.Unlock()
	f.addEvent("chunk.committed", recID, seq, ChunkCommittedPayload{
		Seq:           seq,
		SampleOffset:  seq * 16000,
		SampleCount:   int64(len(pcm) / 2),
		Bytes:         int64(len(pcm)),
		DurationMS:    int64(len(pcm) / 32),
		ContentSHA256: sha256Hex(pcm),
		R2Key:         fmt.Sprintf("owners/default/recordings/%s/chunks/%08d.pcm", recID, seq),
	})
}

// corruptChunk swaps the served bytes for garbage while keeping the committed
// event's checksum (the durable manifest stays authoritative).
func (f *fakeCloud) corruptChunk(recID string, seq int64, pcm []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	bad := make([]byte, len(pcm))
	for i := range bad {
		bad[i] = byte(255 - i)
	}
	f.recs[recID].chunks[seq] = bad
}

func (f *fakeCloud) seedCreated(recID string) {
	f.addEvent("recording.created", recID, -1, RecordingCreatedPayload{Codec: "pcm_s16le", SampleRate: 16000, Channels: 1})
}

func (f *fakeCloud) seedFinished(recID string, totalChunks, totalSamples int64) {
	f.addEvent("recording.finished", recID, -1, RecordingFinishedPayload{
		TotalChunks: totalChunks, TotalSamples: totalSamples, TotalDurationMS: totalSamples / 16,
	})
}

func (f *fakeCloud) seedDeleted(recID string) {
	f.addEvent("recording.deleted", recID, -1, RecordingDeletedPayload{Reason: "user"})
}

func (f *fakeCloud) acksFor(recID string) []ackRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []ackRecord
	for _, a := range f.acks {
		if a.RecordingID == recID {
			out = append(out, a)
		}
	}
	return out
}

func (f *fakeCloud) handleSync(w http.ResponseWriter, r *http.Request) {
	requireSyncHeaders(f.tb, r)
	cursor, _ := strconv.ParseInt(r.URL.Query().Get("cursor"), 10, 64)
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		limit, _ = strconv.Atoi(v)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if cursor > 0 && f.trimmedMin > 0 && cursor < f.trimmedMin-1 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusGone)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"code":    "cursor_expired",
				"message": "cursor older than retention window",
				"details": map[string]any{"min_available_cursor": f.trimmedMin},
			},
		})
		return
	}
	var page []Event
	next := cursor
	for _, ev := range f.events {
		if f.trimmedMin > 0 && ev.ID < f.trimmedMin {
			continue
		}
		if ev.ID > cursor && int64(len(page)) < int64(limit) {
			page = append(page, ev)
			next = ev.ID
		}
	}
	hasMore := len(page) > 0 && next < f.maxEventID
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(EventPage{
		Events:                page,
		NextCursor:            next,
		HasMore:               hasMore,
		MaxEventID:            f.maxEventID,
		ServerProtocolVersion: ProtocolVersion,
	})
}

func (f *fakeCloud) handleAudio(w http.ResponseWriter, r *http.Request) {
	requireSyncHeaders(f.tb, r)
	recID, seqStr := r.PathValue("id"), r.PathValue("seq")
	seq, _ := strconv.ParseInt(seqStr, 10, 64)
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.recs[recID]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	pcm, ok := rec.chunks[seq]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("X-Content-SHA256", sha256Hex(pcm))
	w.Header().Set("Content-Type", "audio/L16")
	w.Write(pcm)
}

func (f *fakeCloud) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	requireSyncHeaders(f.tb, r)
	after := r.URL.Query().Get("after")
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := make([]string, 0, len(f.recs))
	for id := range f.recs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var out []RecordingDetail
	next := ""
	for _, id := range ids {
		if after != "" && id <= after {
			continue
		}
		rec := f.recs[id]
		detail := RecordingDetail{RecordingID: id, State: "recording", Codec: "pcm_s16le", SampleRate: 16000, Channels: 1, CreatedAt: "2026-10-14T00:00:00Z"}
		var totalSamples int64
		seqs := make([]int64, 0, len(rec.chunks))
		for seq := range rec.chunks {
			seqs = append(seqs, seq)
		}
		sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
		for _, seq := range seqs {
			pcm := rec.chunks[seq]
			detail.Chunks = append(detail.Chunks, ChunkInfo{
				Seq: seq, Bytes: int64(len(pcm)), SampleOffset: seq * 16000,
				SampleCount: int64(len(pcm) / 2), ContentSHA256: sha256Hex(pcm),
				UploadedAt: "2026-10-14T00:00:00Z",
			})
			totalSamples += int64(len(pcm) / 2)
		}
		if f.maxEventID >= 3 {
			// A recording is complete once its finished event exists; the
			// fixture tests only create single-recording servers.
			detail.State = "complete"
			detail.TotalChunks = new(int64(len(seqs)))
			detail.TotalSamples = new(totalSamples)
			detail.TotalDurationMS = new(totalSamples / 16)
		}
		out = append(out, detail)
		next = id
	}
	var nextAfter *string
	if next != "" {
		nextAfter = new(next)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(RecordingPage{Recordings: out, NextAfter: nextAfter, HasMore: false})
}

func (f *fakeCloud) handleAck(w http.ResponseWriter, r *http.Request) {
	requireSyncHeaders(f.tb, r)
	var body struct {
		ProtocolVersion    string `json:"protocol_version"`
		LastEventID        int64  `json:"last_event_id"`
		TranscriptionState string `json:"transcription_state"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	f.acks = append(f.acks, ackRecord{RecordingID: r.PathValue("id"), LastEventID: body.LastEventID, State: body.TranscriptionState})
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"synced_at": "2026-10-14T00:00:00Z", "transcription_state": body.TranscriptionState})
}

func newTestStore(t *testing.T) *memory.Store {
	t.Helper()
	store, err := memory.Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func newTestEngine(t *testing.T, f *fakeCloud, store *memory.Store, transcriber Transcriber) *Engine {
	t.Helper()
	audioDir := filepath.Join(t.TempDir(), "audio")
	if err := os.MkdirAll(audioDir, 0o700); err != nil {
		t.Fatal(err)
	}
	return &Engine{
		DB:               store.DB(),
		Store:            store,
		Client:           f.client(),
		AudioDir:         audioDir,
		Transcriber:      transcriber,
		Revision:         "asr-v1",
		DeviceIDFallback: "device:cloud",
		DisableAck:       false,
		Log:              log.New(io.Discard, "", 0),
	}
}

func mustRun(t *testing.T, e *Engine) Report {
	t.Helper()
	rep, err := e.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	return rep
}

func queryInt(t *testing.T, db *sql.DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("query %s: %v", q, err)
	}
	return n
}

func queryString(t *testing.T, db *sql.DB, q string, args ...any) string {
	t.Helper()
	var s string
	if err := db.QueryRow(q, args...).Scan(&s); err != nil {
		t.Fatalf("query %s: %v", q, err)
	}
	return s
}

func TestSyncNormalFlow(t *testing.T) {
	f := newFakeCloud(t)
	f.seedRecording("rec-1")
	f.seedCreated("rec-1")
	chunk0, chunk1 := pcmOf(16000), pcmOf(16000)
	f.seedChunk("rec-1", 0, chunk0)
	f.seedChunk("rec-1", 1, chunk1)
	f.seedFinished("rec-1", 2, 32000)

	store := newTestStore(t)
	e := newTestEngine(t, f, store, FixtureTranscriber{})
	rep := mustRun(t, e)

	if rep.Cursor != 4 || rep.EventsProcessed != 4 || rep.ChunksDownloaded != 2 || rep.RecordingsIngested != 1 || rep.Errors != 0 {
		t.Fatalf("report = %+v", rep)
	}
	if got := queryInt(t, e.DB, `SELECT cursor FROM sync_state WHERE id=1`); got != 4 {
		t.Errorf("cursor = %d, want 4", got)
	}
	// Stitched recording exists; chunk scratch dir is gone.
	stitched := filepath.Join(e.AudioDir, "rec-1.pcm")
	st, err := os.Stat(stitched)
	if err != nil {
		t.Fatalf("stitched file: %v", err)
	}
	if st.Size() != int64(len(chunk0)+len(chunk1)) {
		t.Fatalf("stitched size = %d, want %d", st.Size(), len(chunk0)+len(chunk1))
	}
	if _, err := os.Stat(filepath.Join(e.AudioDir, "rec-1")); !os.IsNotExist(err) {
		t.Error("chunk dir must be removed after stitching")
	}
	// Exactly one turn ingested through the real pipeline.
	if turns := queryInt(t, e.DB, `SELECT COUNT(*) FROM turns`); turns != 1 {
		t.Errorf("turns = %d, want 1", turns)
	}
	if atts := queryInt(t, e.DB, `SELECT COUNT(*) FROM source_attachments WHERE segment_id='segment-00' AND transcript_revision='asr-v1'`); atts != 1 {
		t.Errorf("attachments = %d", atts)
	}
	// The extract and embed jobs must carry the exact idempotency keys that
	// memory.IngestTranscript derives.
	recKey := stableKeyTest("recording", "device:cloud", "rec-1")
	attKey := stableKeyTest("attachment", recKey, "segment-00", "asr-v1")
	extractKey := stableKeyTest("job", "extract", attKey, "extract-v1")
	embedKey := "embed|" + attKey
	if n := queryInt(t, e.DB, `SELECT COUNT(*) FROM processing_jobs WHERE stage='extract' AND idempotency_key=?`, extractKey); n != 1 {
		t.Errorf("extract job with key %s: %d rows", extractKey, n)
	}
	if n := queryInt(t, e.DB, `SELECT COUNT(*) FROM processing_jobs WHERE stage='embed' AND idempotency_key=?`, embedKey); n != 1 {
		t.Errorf("embed job with key %s: %d rows", embedKey, n)
	}
	// The sync state reached the server through an ingested ack.
	acks := f.acksFor("rec-1")
	if len(acks) != 1 || acks[0].State != "ingested" {
		t.Errorf("acks = %+v", acks)
	}

	// Replay: nothing new is inserted anywhere.
	before := queryInt(t, e.DB, `SELECT COUNT(*) FROM sync_events`)
	turnsBefore := queryInt(t, e.DB, `SELECT COUNT(*) FROM turns`)
	rep2 := mustRun(t, e)
	if rep2.Cursor != 4 || rep2.EventsProcessed != 0 || rep2.ChunksDownloaded != 0 || rep2.RecordingsIngested != 0 {
		t.Fatalf("replay report = %+v", rep2)
	}
	if got := queryInt(t, e.DB, `SELECT COUNT(*) FROM sync_events`); got != before {
		t.Errorf("sync_events grew on replay: %d -> %d", before, got)
	}
	if got := queryInt(t, e.DB, `SELECT COUNT(*) FROM turns`); got != turnsBefore {
		t.Errorf("turns grew on replay: %d -> %d", turnsBefore, got)
	}
}

func TestSyncChecksumMismatchContinues(t *testing.T) {
	f := newFakeCloud(t)
	f.seedRecording("rec-bad")
	f.seedRecording("rec-good")
	f.seedCreated("rec-bad")
	f.seedChunk("rec-bad", 0, pcmOf(16000))
	f.corruptChunk("rec-bad", 0, pcmOf(16000))
	f.seedCreated("rec-good")
	f.seedChunk("rec-good", 0, pcmOf(16000))
	f.seedFinished("rec-good", 1, 16000)

	store := newTestStore(t)
	e := newTestEngine(t, f, store, FixtureTranscriber{})
	rep := mustRun(t, e)

	state := queryString(t, e.DB, `SELECT state FROM sync_recordings WHERE recording_id='rec-bad'`)
	if state != stateFailed {
		t.Errorf("rec-bad state = %q, want failed", state)
	}
	if lastErr := queryString(t, e.DB, `SELECT last_error FROM sync_recordings WHERE recording_id='rec-bad'`); lastErr != "checksum_mismatch" {
		t.Errorf("rec-bad last_error = %q", lastErr)
	}
	if rep.RecordingsIngested != 1 || rep.Errors != 1 {
		t.Errorf("report = %+v", rep)
	}
	// The bad event is settled; the stream never wedged.
	if n := queryInt(t, e.DB, `SELECT COUNT(*) FROM sync_events WHERE event_id=2`); n != 1 {
		t.Errorf("bad event not settled")
	}
	if rep.Cursor != 5 {
		t.Errorf("cursor = %d, want 5", rep.Cursor)
	}
	if turns := queryInt(t, e.DB, `SELECT COUNT(*) FROM turns`); turns != 1 {
		t.Errorf("turns = %d, want 1 (rec-good ingested)", turns)
	}
}

func TestSyncCursorExpiredSnapshotRecovery(t *testing.T) {
	f := newFakeCloud(t)
	f.seedRecording("rec-1")
	f.seedCreated("rec-1")
	f.seedChunk("rec-1", 0, pcmOf(16000))
	f.seedFinished("rec-1", 1, 16000)
	f.mu.Lock()
	f.trimmedMin = 3 // events 1-2 trimmed by retention
	f.mu.Unlock()

	store := newTestStore(t)
	e := newTestEngine(t, f, store, FixtureTranscriber{})
	// A stale cursor older than the retention window triggers 410.
	if _, err := e.DB.Exec(`INSERT INTO sync_state(id,cursor,updated_at) VALUES(1,1,0)`); err != nil {
		t.Fatal(err)
	}
	rep := mustRun(t, e)

	if rep.Cursor != f.maxEventID {
		t.Errorf("cursor = %d, want max_event_id %d", rep.Cursor, f.maxEventID)
	}
	state := queryString(t, e.DB, `SELECT state FROM sync_recordings WHERE recording_id='rec-1'`)
	if state != stateIngested {
		t.Errorf("state = %q, want ingested", state)
	}
	if turns := queryInt(t, e.DB, `SELECT COUNT(*) FROM turns`); turns != 1 {
		t.Errorf("turns = %d, want 1", turns)
	}
	if rep.ChunksDownloaded != 1 {
		t.Errorf("chunks downloaded during recovery = %d, want 1", rep.ChunksDownloaded)
	}
	if _, err := os.Stat(filepath.Join(e.AudioDir, "rec-1", "chunk-00000000.pcm")); !os.IsNotExist(err) {
		t.Error("recovery must remove chunk files after stitching")
	}
}

func TestSyncDeletedRemovesLocalAudio(t *testing.T) {
	f := newFakeCloud(t)
	f.seedRecording("rec-1")
	f.seedCreated("rec-1")
	f.seedChunk("rec-1", 0, pcmOf(16000))
	f.seedDeleted("rec-1")

	store := newTestStore(t)
	e := newTestEngine(t, f, store, FixtureTranscriber{})
	rep := mustRun(t, e)

	if rep.Cursor != 3 || rep.EventsProcessed != 3 {
		t.Fatalf("report = %+v", rep)
	}
	state := queryString(t, e.DB, `SELECT state FROM sync_recordings WHERE recording_id='rec-1'`)
	if state != stateDeleted {
		t.Errorf("state = %q, want deleted", state)
	}
	if n := queryInt(t, e.DB, `SELECT COUNT(*) FROM sync_chunks WHERE recording_id='rec-1'`); n != 0 {
		t.Errorf("sync_chunks rows = %d, want 0", n)
	}
	if _, err := os.Stat(filepath.Join(e.AudioDir, "rec-1")); !os.IsNotExist(err) {
		t.Error("chunk dir must be removed")
	}
	if _, err := os.Stat(filepath.Join(e.AudioDir, "rec-1.pcm")); !os.IsNotExist(err) {
		t.Error("stitched file must be removed")
	}
}

func TestSyncIncompleteFinishHoldsCursor(t *testing.T) {
	f := newFakeCloud(t)
	f.seedRecording("rec-1")
	f.seedCreated("rec-1")
	f.seedChunk("rec-1", 0, pcmOf(16000))
	f.seedFinished("rec-1", 2, 32000) // chunk 1 never arrives

	store := newTestStore(t)
	e := newTestEngine(t, f, store, FixtureTranscriber{})
	rep := mustRun(t, e)

	if rep.Cursor != 2 {
		t.Errorf("cursor = %d, want 2 (finish event holds)", rep.Cursor)
	}
	state := queryString(t, e.DB, `SELECT state FROM sync_recordings WHERE recording_id='rec-1'`)
	if state != stateDownloading {
		t.Errorf("state = %q, want downloading", state)
	}
	if n := queryInt(t, e.DB, `SELECT COUNT(*) FROM sync_events WHERE event_id=3`); n != 0 {
		t.Error("unsettled finish event must not be marked processed")
	}
	if lastErr := queryString(t, e.DB, `SELECT last_error FROM sync_state WHERE id=1`); lastErr == "" {
		t.Error("gap must be recorded in sync_state.last_error")
	}
}

func TestSyncTranscriptionPendingSweep(t *testing.T) {
	f := newFakeCloud(t)
	f.seedRecording("rec-1")
	f.seedCreated("rec-1")
	f.seedChunk("rec-1", 0, pcmOf(16000))
	f.seedFinished("rec-1", 1, 16000)

	store := newTestStore(t)
	// First engine has no transcriber: recording parks in
	// transcription_pending and the audio is stitched locally.
	off := newTestEngine(t, f, store, nil)
	rep := mustRun(t, off)
	if rep.Cursor != 3 || rep.RecordingsIngested != 0 {
		t.Fatalf("off report = %+v", rep)
	}
	state := queryString(t, store.DB(), `SELECT state FROM sync_recordings WHERE recording_id='rec-1'`)
	if state != stateTranscriptionPending {
		t.Fatalf("state = %q, want transcription_pending", state)
	}
	if turns := queryInt(t, store.DB(), `SELECT COUNT(*) FROM turns`); turns != 0 {
		t.Fatalf("turns = %d, want 0", turns)
	}
	acks := f.acksFor("rec-1")
	if len(acks) != 1 || acks[0].State != "pending" {
		t.Fatalf("acks = %+v, want one pending ack", acks)
	}

	// Re-running with a transcriber completes ingestion from the sweep.
	on := newTestEngine(t, f, store, FixtureTranscriber{})
	rep2 := mustRun(t, on)
	if rep2.RecordingsIngested != 1 {
		t.Fatalf("sweep report = %+v", rep2)
	}
	state = queryString(t, store.DB(), `SELECT state FROM sync_recordings WHERE recording_id='rec-1'`)
	if state != stateIngested {
		t.Fatalf("state = %q, want ingested", state)
	}
	if turns := queryInt(t, store.DB(), `SELECT COUNT(*) FROM turns`); turns != 1 {
		t.Fatalf("turns = %d, want 1", turns)
	}

	// Sweeping again is idempotent.
	rep3 := mustRun(t, on)
	if rep3.RecordingsIngested != 0 {
		t.Fatalf("second sweep report = %+v", rep3)
	}
	if turns := queryInt(t, store.DB(), `SELECT COUNT(*) FROM turns`); turns != 1 {
		t.Fatalf("turns grew to %d", turns)
	}
}

func TestSyncUnknownEventTypeHoldsCursor(t *testing.T) {
	f := newFakeCloud(t)
	f.seedRecording("rec-1")
	f.seedCreated("rec-1")
	f.seedChunk("rec-1", 0, pcmOf(16000))
	f.seedFinished("rec-1", 1, 16000)
	f.addEvent("wormhole.opened", "rec-1", -1, map[string]any{})
	f.seedRecording("rec-2")
	f.seedCreated("rec-2")

	store := newTestStore(t)
	e := newTestEngine(t, f, store, FixtureTranscriber{})
	rep := mustRun(t, e)

	if rep.Cursor != 3 {
		t.Errorf("cursor = %d, want 3 (unknown event holds)", rep.Cursor)
	}
	if rep.Errors != 1 {
		t.Errorf("errors = %d, want 1", rep.Errors)
	}
	if lastErr := queryString(t, e.DB, `SELECT last_error FROM sync_state WHERE id=1`); lastErr == "" {
		t.Error("unknown event type must be recorded in last_error")
	}
	if n := queryInt(t, e.DB, `SELECT COUNT(*) FROM sync_events WHERE event_id=5`); n != 0 {
		t.Error("events after an unknown event must not be processed (strict id order)")
	}
	if n := queryInt(t, e.DB, `SELECT COUNT(*) FROM sync_recordings WHERE recording_id='rec-2'`); n != 0 {
		t.Error("later recordings must not be processed past an unknown event")
	}
}

func TestCompletenessGap(t *testing.T) {
	chunks := []chunkRow{{Seq: 0, SampleCount: 16000}, {Seq: 1, SampleCount: 16000}}
	if gap := completenessGap(chunks, 2, 32000); gap != "" {
		t.Errorf("complete set reported gap %q", gap)
	}
	if gap := completenessGap(chunks[:1], 2, 16000); gap == "" {
		t.Error("missing seq must be reported")
	}
	if gap := completenessGap(chunks, 2, 48000); gap == "" {
		t.Error("sample sum mismatch must be reported")
	}
	dup := []chunkRow{{Seq: 0, SampleCount: 16000}, {Seq: 0, SampleCount: 16000}}
	if gap := completenessGap(dup, 1, 32000); gap == "" {
		t.Error("duplicate seq must be reported")
	}
}
