package cloudsync

import (
	"context"
	"os"
	"reflect"
	"testing"

	"recordingmcp/internal/asrinput"
	"recordingmcp/internal/asrstream"
	"recordingmcp/internal/ingestvectors"
)

func writeTestPCM(t *testing.T, samples int) string {
	t.Helper()
	path := t.TempDir() + "/audio.pcm"
	if err := os.WriteFile(path, ingestvectors.PCM(samples, ingestvectors.Sine440), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestFixtureTranscriberDeterministic(t *testing.T) {
	// 6 s of audio: two fixture windows 0-5000ms and 5000-6000ms.
	path := writeTestPCM(t, 6*16000)
	meta := MetaBasis{
		DeviceID:          "dev_test",
		RecordingID:       "rec-1",
		ConversationID:    "rec-1",
		Revision:          "asr-v1",
		SourceAudioSHA256: "abc123",
	}
	first, err := FixtureTranscriber{}.Transcribe(context.Background(), path, meta)
	if err != nil {
		t.Fatal(err)
	}
	second, err := FixtureTranscriber{}.Transcribe(context.Background(), path, meta)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 {
		t.Fatalf("want 2 segments, got %d", len(first))
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("fixture transcriber is not deterministic")
	}
	for i, r := range first {
		if err := asrinput.Validate(r); err != nil {
			t.Fatalf("segment %d fails asrinput.Validate: %v", i, err)
		}
	}
	want0 := "fixture transcript recording=rec-1 segment=00 window=0-5000ms"
	want1 := "fixture transcript recording=rec-1 segment=01 window=5000-6000ms"
	if first[0].Turns[0].Text != want0 {
		t.Errorf("segment 0 text = %q, want %q", first[0].Turns[0].Text, want0)
	}
	if first[1].Turns[0].Text != want1 {
		t.Errorf("segment 1 text = %q, want %q", first[1].Turns[0].Text, want1)
	}
	if first[0].Source.SegmentID != "segment-00" || first[1].Source.SegmentID != "segment-01" {
		t.Errorf("segment ids = %q, %q", first[0].Source.SegmentID, first[1].Source.SegmentID)
	}
	if first[0].Source.SegmentOffsetMS != 0 || first[0].Source.SegmentDurationMS != 5000 {
		t.Errorf("segment 0 window = %d+%d", first[0].Source.SegmentOffsetMS, first[0].Source.SegmentDurationMS)
	}
	if first[1].Source.SegmentOffsetMS != 5000 || first[1].Source.SegmentDurationMS != 1000 {
		t.Errorf("segment 1 window = %d+%d", first[1].Source.SegmentOffsetMS, first[1].Source.SegmentDurationMS)
	}
	if first[0].Source.SourceAudioSHA256 == nil || *first[0].Source.SourceAudioSHA256 != "abc123" {
		t.Error("segment 0 source sha256 not carried through")
	}
	if first[0].Source.CompletionStatus != "final" {
		t.Error("fixture completion_status must be final")
	}
}

func TestFixtureTranscriberShortAudio(t *testing.T) {
	// 1 s: exactly one window shorter than the 5 s period.
	path := writeTestPCM(t, 16000)
	results, err := FixtureTranscriber{}.Transcribe(context.Background(), path, MetaBasis{RecordingID: "r", DeviceID: "d", ConversationID: "r", Revision: "asr-v1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("want 1 segment, got %d", len(results))
	}
	if results[0].Source.SegmentDurationMS != 1000 {
		t.Errorf("duration = %d, want 1000", results[0].Source.SegmentDurationMS)
	}
	if err := asrinput.Validate(results[0]); err != nil {
		t.Fatal(err)
	}
}

func TestFilterServerFrames(t *testing.T) {
	frames := []asrstream.Frame{
		{MessageType: asrstream.TypeFullResponse, Code: 0},
		{MessageType: asrstream.TypeFullResponse, Code: 45000001},
		{MessageType: asrstream.TypeError, Code: 1},
		{MessageType: asrstream.TypeAudioOnly, Code: 0},
		{MessageType: asrstream.TypeFullResponse, Code: 0, IsLastPackage: true},
	}
	got := filterServerFrames(frames)
	if len(got) != 2 {
		t.Fatalf("want 2 surviving frames, got %d", len(got))
	}
	if got[0].IsLastPackage || !got[1].IsLastPackage {
		t.Error("filter must keep full-response success frames in order")
	}
}

func TestPCMDurationMS(t *testing.T) {
	// S16LE mono 16 kHz is 32 bytes per millisecond.
	if got := pcmDurationMS(32000); got != 1000 {
		t.Errorf("pcmDurationMS(32000) = %d, want 1000", got)
	}
}
