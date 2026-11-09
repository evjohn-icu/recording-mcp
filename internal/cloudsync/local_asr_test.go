package cloudsync

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeStub(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stub.py")
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func writePCM(t *testing.T, durationMS int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audio.pcm")
	if err := os.WriteFile(path, make([]byte, durationMS*32), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func localTestMeta() MetaBasis {
	return MetaBasis{
		DeviceID: "device:test", RecordingID: "recording:test", ConversationID: "conversation:test",
		Revision: "asr-v1", SourceAudioSHA256: "sha256:abc",
	}
}

func requirePython(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
}

func TestLocalTranscriberMapsWindows(t *testing.T) {
	requirePython(t)
	script := writeStub(t, `import json, os, sys
args = sys.argv[1:]
pcm = args[args.index("--pcm") + 1]
duration = os.path.getsize(pcm) // 32
print(json.dumps({"language": "Chinese", "windows": [
    {"index": 1, "start_ms": 0, "end_ms": 30000, "text": "  first window  "},
    {"index": 2, "start_ms": 28000, "end_ms": duration, "text": "second window"},
]}))
`)
	tr := LocalTranscriber{
		Python: "python3", Script: script, Backend: "ark", Model: "Edge0/ARK-ASR-0.6B",
		Dtype: "float16", Device: "cuda:0", SegmentMS: 30000, OverlapMS: 2000, MaxNewTokens: 512,
	}
	results, err := tr.Transcribe(context.Background(), writePCM(t, 31000), localTestMeta())
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("want 2 windows, got %d", len(results))
	}
	first := results[0]
	if first.Source.Provider != "local-ark" {
		t.Fatalf("provider = %q, want local-ark", first.Source.Provider)
	}
	if first.Source.Model == nil || *first.Source.Model != "Edge0/ARK-ASR-0.6B" {
		t.Fatalf("model = %v, want Edge0/ARK-ASR-0.6B", first.Source.Model)
	}
	if first.Source.SegmentOffsetMS != 0 || first.Source.SegmentDurationMS != 30000 {
		t.Fatalf("segment = %d+%d, want 0+30000", first.Source.SegmentOffsetMS, first.Source.SegmentDurationMS)
	}
	if got := first.Turns[0].Text; got != "first window" {
		t.Fatalf("text = %q, want trimmed text", got)
	}
	if first.Turns[0].Confidence != nil {
		t.Fatal("local ASR must not fabricate confidence")
	}
	second := results[1]
	if second.Source.SegmentOffsetMS != 28000 || second.Source.SegmentDurationMS != 3000 {
		t.Fatalf("second segment = %d+%d, want 28000+3000", second.Source.SegmentOffsetMS, second.Source.SegmentDurationMS)
	}
}

func TestLocalTranscriberPassesFloat16Flags(t *testing.T) {
	requirePython(t)
	script := writeStub(t, `import json, sys
args = sys.argv[1:]
print(json.dumps({"windows": [{"index": 1, "start_ms": 0, "end_ms": 1000,
    "text": " ".join(args)}]}))
`)
	tr := LocalTranscriber{
		Python: "python3", Script: script, Backend: "qwen", Model: "Qwen/Qwen3-ASR-1.7B",
		Dtype: "float16", Device: "cuda:0", Language: "Chinese", Revision: "abc123",
		SegmentMS: 30000, OverlapMS: 2000, MaxNewTokens: 512,
	}
	results, err := tr.Transcribe(context.Background(), writePCM(t, 1000), localTestMeta())
	if err != nil {
		t.Fatal(err)
	}
	text := results[0].Turns[0].Text
	for _, want := range []string{"--backend qwen", "--model Qwen/Qwen3-ASR-1.7B", "--dtype float16", "--device cuda:0", "--language Chinese", "--revision abc123", "--window-ms 30000", "--overlap-ms 2000"} {
		if !strings.Contains(text, want) {
			t.Fatalf("runner args missing %q: %s", want, text)
		}
	}
}

func TestLocalTranscriberDoesNotLeakEnvironment(t *testing.T) {
	requirePython(t)
	t.Setenv("LOCAL_ASR_SECRET", "should-not-reach-runner")
	script := writeStub(t, `import json, os, sys
print(json.dumps({"windows": [{"index": 1, "start_ms": 0, "end_ms": 1000,
    "text": "secret=" + os.environ.get("LOCAL_ASR_SECRET", "absent") + " path=" + str(bool(os.environ.get("PATH")))}]}))
`)
	tr := LocalTranscriber{Python: "python3", Script: script, Backend: "ark", Model: "m", Dtype: "float16"}
	results, err := tr.Transcribe(context.Background(), writePCM(t, 1000), localTestMeta())
	if err != nil {
		t.Fatal(err)
	}
	if text := results[0].Turns[0].Text; text != "secret=absent path=True" {
		t.Fatalf("runner environment = %q, want secrets filtered and PATH kept", text)
	}
}

func TestLocalTranscriberTimeout(t *testing.T) {
	requirePython(t)
	script := writeStub(t, "import time\ntime.sleep(5)\n")
	tr := LocalTranscriber{Python: "python3", Script: script, Backend: "ark", Model: "m", Dtype: "float16", Timeout: 100 * time.Millisecond}
	_, err := tr.Transcribe(context.Background(), writePCM(t, 1000), localTestMeta())
	if err == nil || !strings.Contains(err.Error(), "local ASR runner failed") {
		t.Fatalf("error = %v, want timeout failure", err)
	}
}

func TestLocalTranscriberRejectsRunnerFailure(t *testing.T) {
	requirePython(t)
	script := writeStub(t, "import sys\nprint('model weights missing', file=sys.stderr)\nsys.exit(4)\n")
	tr := LocalTranscriber{Python: "python3", Script: script, Backend: "ark", Model: "m", Dtype: "float16"}
	_, err := tr.Transcribe(context.Background(), writePCM(t, 1000), localTestMeta())
	if err == nil || !strings.Contains(err.Error(), "model weights missing") {
		t.Fatalf("error = %v, want stderr detail", err)
	}
}

func TestLocalTranscriberRejectsInvalidOutput(t *testing.T) {
	requirePython(t)
	tr := LocalTranscriber{Python: "python3", Script: writeStub(t, "print('not json')\n"), Backend: "ark", Model: "m", Dtype: "float16"}
	if _, err := tr.Transcribe(context.Background(), writePCM(t, 1000), localTestMeta()); err == nil || !strings.Contains(err.Error(), "invalid JSON") {
		t.Fatalf("error = %v, want invalid JSON", err)
	}
	tr.Script = writeStub(t, `import json
print(json.dumps({"windows": [{"index": 1, "start_ms": 0, "end_ms": 99999, "text": "x"}]}))
`)
	if _, err := tr.Transcribe(context.Background(), writePCM(t, 1000), localTestMeta()); err == nil || !strings.Contains(err.Error(), "invalid bounds") {
		t.Fatalf("error = %v, want invalid bounds", err)
	}
}
