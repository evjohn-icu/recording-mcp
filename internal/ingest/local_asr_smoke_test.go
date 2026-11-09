package ingest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"recordingmcp/internal/cloudsync"
)

// TestLocalASREndToEndSmoke proves the real float16 runner feeds the normal
// ingest path. It is gated because it needs downloaded model weights and a GPU.
//
//	LOCAL_ASR_SMOKE=1 LOCAL_ASR_SAMPLE=/path/speech.wav \
//	LOCAL_ASR_BACKEND=ark go test ./internal/ingest -run TestLocalASREndToEndSmoke -v
func TestLocalASREndToEndSmoke(t *testing.T) {
	if os.Getenv("LOCAL_ASR_SMOKE") != "1" {
		t.Skip("set LOCAL_ASR_SMOKE=1 with LOCAL_ASR_SAMPLE to run the real model smoke")
	}
	sample := os.Getenv("LOCAL_ASR_SAMPLE")
	if sample == "" {
		t.Skip("LOCAL_ASR_SAMPLE is required")
	}
	backend := strings.ToLower(envOr("LOCAL_ASR_BACKEND", "ark"))
	model := os.Getenv("LOCAL_ASR_MODEL")
	if model == "" {
		if backend == "qwen" {
			model = "Qwen/Qwen3-ASR-1.7B"
		} else {
			model = "Edge0/ARK-ASR-0.6B"
		}
	}
	transcriber := &cloudsync.LocalTranscriber{
		Python:  envOr("LOCAL_ASR_PYTHON", filepath.Join("..", "..", ".venv-asr", "bin", "python")),
		Script:  envOr("LOCAL_ASR_SCRIPT", filepath.Join("..", "..", "scripts", "local-asr.py")),
		Backend: backend, Model: model, Dtype: "float16",
		Device:    envOr("LOCAL_ASR_DEVICE", "cuda:0"),
		SegmentMS: 30000, OverlapMS: 2000, MaxNewTokens: 512,
	}
	env := newTestEnv(t, func(cfg *Config) {
		cfg.Decoder = &FFmpegDecoder{}
		cfg.Transcription = transcriber
		cfg.ASRProvider = backend
		cfg.ASRModel = model
	})
	data, err := os.ReadFile(sample)
	if err != nil {
		t.Fatal(err)
	}
	res, err := env.importFile(filepath.Base(sample), data)
	if err != nil {
		t.Fatal(err)
	}
	env.runIngest(env.job(res.JobID))
	if err := env.runASR(env.job(env.asrJobID(res.ConversationID))); err != nil {
		t.Fatalf("asr job: %v", err)
	}
	var text string
	if err := env.store.DB().QueryRowContext(context.Background(),
		`SELECT content FROM turns WHERE conversation_id=? ORDER BY global_start_ms LIMIT 1`,
		res.ConversationID).Scan(&text); err != nil {
		t.Fatalf("no turn ingested: %v", err)
	}
	if strings.TrimSpace(text) == "" {
		t.Fatal("turn text is empty")
	}
	if want := os.Getenv("LOCAL_ASR_EXPECT"); want != "" && !strings.Contains(text, want) {
		t.Fatalf("transcript %q does not contain %q", text, want)
	}
	var resultJSON string
	if err := env.store.DB().QueryRowContext(context.Background(),
		`SELECT result_json FROM source_attachments WHERE conversation_id=? AND active=1 LIMIT 1`,
		res.ConversationID).Scan(&resultJSON); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resultJSON, `"provider":"local-`+backend+`"`) {
		t.Fatalf("attachment does not record the local provider: %s", resultJSON)
	}
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
