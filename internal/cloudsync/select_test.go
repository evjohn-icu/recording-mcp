package cloudsync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSelectTranscriberLocalBackends(t *testing.T) {
	for _, tc := range []struct{ provider, backend, wantModel, wantRevision string }{
		{"ark", "ark", "Edge0/ARK-ASR-0.6B", defaultARKRevision},
		{"qwen", "qwen", "Qwen/Qwen3-ASR-1.7B", defaultQwenRevision},
		{"local", "qwen", "Qwen/Qwen3-ASR-1.7B", defaultQwenRevision},
	} {
		tr, err := SelectTranscriber(TranscriberConfig{
			Provider: tc.provider, LocalBackend: tc.backend, Python: "python3",
			Dtype: "float16", Device: "cuda:0", WindowMS: 30000, OverlapMS: 2000,
		})
		if err != nil {
			t.Fatalf("%s: %v", tc.provider, err)
		}
		local, ok := tr.(*LocalTranscriber)
		if !ok {
			t.Fatalf("%s: got %T, want *LocalTranscriber", tc.provider, tr)
		}
		if local.Model != tc.wantModel || local.Backend != tc.backend {
			t.Fatalf("%s: backend=%q model=%q", tc.provider, local.Backend, local.Model)
		}
		if local.Revision != tc.wantRevision {
			t.Fatalf("%s: revision=%q, want %q", tc.provider, local.Revision, tc.wantRevision)
		}
		if local.Dtype != "float16" || local.SegmentMS != 30000 || local.OverlapMS != 2000 {
			t.Fatalf("%s: config lost: %+v", tc.provider, local)
		}
	}
}

func TestSelectTranscriberRejectsBadLocalConfig(t *testing.T) {
	if _, err := SelectTranscriber(TranscriberConfig{Provider: "local", LocalBackend: "whisper", Python: "python3"}); err == nil || !strings.Contains(err.Error(), "requires --asr-backend") {
		t.Fatalf("error = %v, want backend validation", err)
	}
	if _, err := SelectTranscriber(TranscriberConfig{Provider: "ark", Python: "python3", Dtype: "bfloat16"}); err == nil || !strings.Contains(err.Error(), "float16 only") {
		t.Fatalf("error = %v, want dtype validation", err)
	}
	if _, err := SelectTranscriber(TranscriberConfig{Provider: "ark", Python: "python3", WindowMS: 2000, OverlapMS: 2000}); err == nil || !strings.Contains(err.Error(), "asr-overlap-ms") {
		t.Fatalf("error = %v, want overlap validation", err)
	}
	if _, err := SelectTranscriber(TranscriberConfig{Provider: "ark", Python: "python3", Model: "custom/model"}); err == nil || !strings.Contains(err.Error(), "not pinned") {
		t.Fatalf("error = %v, want unpinned custom model rejection", err)
	}
}

func TestSelectTranscriberProbesPythonAndScript(t *testing.T) {
	if _, err := SelectTranscriber(TranscriberConfig{
		Provider: "ark", Python: "python3", Script: filepath.Join(t.TempDir(), "missing.py"), ProbePython: true,
	}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("error = %v, want missing script", err)
	}
	script := filepath.Join(t.TempDir(), "runner.py")
	if err := os.WriteFile(script, []byte("print('{}')\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	interpreter := filepath.Join(t.TempDir(), "python-fake")
	if err := os.WriteFile(interpreter, []byte("#!/bin/sh\necho torch,transformers\n exit 3\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := SelectTranscriber(TranscriberConfig{
		Provider: "ark", Python: interpreter, Script: script, ProbePython: true,
	})
	if err == nil || !strings.Contains(err.Error(), "missing modules") {
		t.Fatalf("error = %v, want missing modules", err)
	}
}
