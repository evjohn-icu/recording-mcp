package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"recordingmcp/internal/asrinput"
	"recordingmcp/internal/memory"
)

// testDevice/testProviderRecording are the provider identities the fixture
// ingests; their hash is the memory recording id the service resolves audio
// through.
const (
	testDevice            = "dev-1"
	testProviderRecording = "rec-provider-1"
	testConversation      = "conv-1"
	testStartedAt         = "2026-03-01T09:00:00+08:00"
)

// fixture is one ingested conversation with two segments, two annotated notes
// and an optional audio directory.
type fixture struct {
	store    *memory.Store
	svc      *Service
	audioDir string

	conversationID string // memory conversation id (hashed)
	recordingID    string // memory recording id (hashed)
}

func turnWithTimes(id string, index int, text string, offsetMS, startMS, endMS int64) asrinput.Turn {
	localStart, localEnd := startMS, endMS
	globalStart, globalEnd := offsetMS+startMS, offsetMS+endMS
	return asrinput.Turn{ID: id, OriginalIndex: index, Text: text,
		LocalStartMS: &localStart, LocalEndMS: &localEnd,
		GlobalStartMS: &globalStart, GlobalEndMS: &globalEnd}
}

func sampleSegment(segment string, offsetMS int64, startedAt *string, turns []asrinput.Turn) asrinput.Result {
	return asrinput.Result{
		ContractVersion: asrinput.ContractVersion,
		Source: asrinput.Source{
			Provider:           "provider-x",
			ProviderRequestID:  "req-" + segment,
			DeviceID:           testDevice,
			RecordingID:        testProviderRecording,
			ConversationID:     testConversation,
			SegmentID:          segment,
			SegmentOffsetMS:    offsetMS,
			SegmentDurationMS:  offsetMS + 3000,
			TranscriptRevision: "rev-1",
			CompletionStatus:   "final",
			RecordingStartedAt: startedAt,
			RawEnvelope:        json.RawMessage(`{"vendor_extra":"kept"}`),
		},
		Turns:     turns,
		RawSource: json.RawMessage(`{"provider_payload":{"text":"hello"}}`),
	}
}

// originalTurnID mirrors the ingestion turn identity used to cite evidence.
func originalTurnID(segment, turnID string) string { return segment + "/rev-1/" + turnID }

func newFixture(t *testing.T) *fixture {
	t.Helper()
	return newFixtureWithStart(t, nil)
}

func newFixtureWithStart(t *testing.T, startedAt *string) *fixture {
	t.Helper()
	dir := t.TempDir()
	store, err := memory.Open(filepath.Join(dir, "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	audioDir := filepath.Join(dir, "audio")
	if err := os.MkdirAll(audioDir, 0o755); err != nil {
		t.Fatal(err)
	}
	f := &fixture{store: store, audioDir: audioDir}
	f.svc = New(store, nil, nil, audioDir)
	ctx := context.Background()

	segmentA := sampleSegment("seg-a", 0, startedAt, []asrinput.Turn{
		turnWithTimes("t-1", 1, "第一条原始转写", 0, 1000, 2000),
		turnWithTimes("t-2", 2, "第二条原始转写", 0, 2000, 3000),
	})
	rep, err := store.IngestTranscript(ctx, segmentA)
	if err != nil {
		t.Fatal(err)
	}
	f.conversationID, f.recordingID = rep.ConversationID, rep.RecordingID
	noTimes := asrinput.Turn{ID: "t-3", OriginalIndex: 1, Text: "没有时间信息的转写"}
	segmentB := sampleSegment("seg-b", 0, startedAt, []asrinput.Turn{noTimes})
	segmentB.Source.SegmentID = "seg-b"
	segmentB.Source.ProviderRequestID = "req-seg-b"
	segmentB.Source.SegmentDurationMS = 3000
	if _, err = store.IngestTranscript(ctx, segmentB); err != nil {
		t.Fatal(err)
	}

	ann := memory.Annotations{
		Version:           1,
		ConversationID:    f.conversationID,
		Title:             "测试会话",
		RecordingID:       f.recordingID,
		AnnotationVersion: "v1",
		AnnotationBasis:   "人工解释；未核实",
		Summary:           "两条待确认的归纳",
		Topics:            []memory.TopicAnnotation{{ID: "topic:a", Title: "主题A"}},
		Notes: []memory.NoteAnnotation{
			{ID: "note:one", Kind: "suggestion", TopicIDs: []string{"topic:a"}, Summary: "第一条归纳", EvidenceTurnIDs: []string{originalTurnID("seg-a", "t-1")}},
			{ID: "note:two", Kind: "intention", TopicIDs: []string{"topic:a"}, Summary: "第二条归纳", EvidenceTurnIDs: []string{originalTurnID("seg-a", "t-2")}},
		},
	}
	if err := store.ApplyAnnotations(ctx, f.conversationID, "", ann); err != nil {
		t.Fatal(err)
	}
	return f
}

// turnRowID resolves the stored turn id for a segment turn.
func (f *fixture) turnRowID(t *testing.T, segment, turnID string) string {
	t.Helper()
	var id string
	err := f.store.DB().QueryRow(`SELECT id FROM turns WHERE recording_id=? AND original_turn_id=?`,
		f.recordingID, originalTurnID(segment, turnID)).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// setRecordingTimes overrides one turn's global window, including NULLs.
func (f *fixture) setRecordingTimes(t *testing.T, turnRowID string, start, end any) {
	t.Helper()
	if _, err := f.store.DB().Exec(`UPDATE turns SET global_start_ms=?, global_end_ms=? WHERE id=?`, start, end, turnRowID); err != nil {
		t.Fatal(err)
	}
}

// insertSync records one cloud-sync state row for the fixture recording.
func (f *fixture) insertSync(t *testing.T, state, stitchedPath string) {
	t.Helper()
	if _, err := f.store.DB().Exec(
		`INSERT INTO sync_recordings(recording_id,owner,device_id,state,started_at,stitched_path,transcript_revision,transcription_state,updated_at)
		 VALUES(?, 'default', ?, ?, NULL, ?, 'asr-v1', 'done', 1)`,
		testProviderRecording, testDevice, state, stitchedPath); err != nil {
		t.Fatal(err)
	}
}

// writeAudio writes one PCM file of n milliseconds of silence under dir.
func writeAudioFile(t *testing.T, path string, milliseconds int64) []byte {
	t.Helper()
	pcm := make([]byte, milliseconds*pcmBytesPerMS)
	for i := range pcm {
		pcm[i] = byte(i % 251)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pcm, 0o600); err != nil {
		t.Fatal(err)
	}
	return pcm
}
