package cloudsync

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"recordingmcp/internal/asrinput"
)

// LocalTranscriber runs an offline ASR model through the Python runner in
// scripts/local-asr.py. The runner owns model loading and inference; this type
// owns the provider contract: segment identity, timestamps, and the immutable
// asrinput.Result shape the ingest path accepts.
type LocalTranscriber struct {
	// Python is the interpreter (default "python3").
	Python string
	// Script is the runner path (default "scripts/local-asr.py").
	Script string
	// Backend selects the model family: "ark" or "qwen".
	Backend string
	// Model is a local directory or Hugging Face id.
	Model string
	// Revision pins the Hugging Face snapshot (commit) when set.
	Revision string
	// Dtype is passed to the runner, e.g. "float16".
	Dtype string
	// Device is the inference device, e.g. "cuda:0".
	Device string
	// Language optionally pins recognition language (empty = auto).
	Language string
	// SegmentMS and OverlapMS describe the window plan.
	SegmentMS int64
	OverlapMS int64
	// MaxNewTokens bounds generated tokens per window.
	MaxNewTokens int
	// Env appends environment variables for the runner.
	Env []string
}

type localASROutput struct {
	Language string `json:"language"`
	Windows  []struct {
		Index   int    `json:"index"`
		StartMS int64  `json:"start_ms"`
		EndMS   int64  `json:"end_ms"`
		Text    string `json:"text"`
	} `json:"windows"`
}

// Transcribe implements Transcriber.
func (t LocalTranscriber) Transcribe(ctx context.Context, pcmPath string, meta MetaBasis) ([]asrinput.Result, error) {
	if strings.TrimSpace(t.Backend) == "" {
		return nil, fmt.Errorf("cloudsync: local ASR backend is required")
	}
	if strings.TrimSpace(t.Model) == "" {
		return nil, fmt.Errorf("cloudsync: local ASR model is required")
	}
	size, err := fileSize(pcmPath)
	if err != nil {
		return nil, err
	}
	durationMS := pcmDurationMS(size)
	if durationMS <= 0 {
		return nil, fmt.Errorf("cloudsync: pcm file %s is empty", pcmPath)
	}
	python := t.Python
	if strings.TrimSpace(python) == "" {
		python = "python3"
	}
	script := t.Script
	if strings.TrimSpace(script) == "" {
		script = "scripts/local-asr.py"
	}
	dtype := t.Dtype
	if strings.TrimSpace(dtype) == "" {
		dtype = "float16"
	}
	segmentMS := t.SegmentMS
	if segmentMS <= 0 {
		segmentMS = 30000
	}
	overlapMS := t.OverlapMS
	if overlapMS < 0 {
		overlapMS = 0
	}
	maxTokens := t.MaxNewTokens
	if maxTokens <= 0 {
		maxTokens = 512
	}
	args := []string{
		script,
		"--backend", t.Backend,
		"--model", t.Model,
		"--pcm", pcmPath,
		"--dtype", dtype,
		"--window-ms", fmt.Sprint(segmentMS),
		"--overlap-ms", fmt.Sprint(overlapMS),
		"--max-new-tokens", fmt.Sprint(maxTokens),
	}
	if strings.TrimSpace(t.Device) != "" {
		args = append(args, "--device", t.Device)
	}
	if strings.TrimSpace(t.Language) != "" {
		args = append(args, "--language", t.Language)
	}
	if strings.TrimSpace(t.Revision) != "" {
		args = append(args, "--revision", t.Revision)
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, python, args...)
	cmd.Env = append(os.Environ(), t.Env...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if len(detail) > 2000 {
			detail = detail[len(detail)-2000:]
		}
		if detail == "" {
			return nil, fmt.Errorf("cloudsync: local ASR runner failed: %w", err)
		}
		return nil, fmt.Errorf("cloudsync: local ASR runner failed: %w: %s", err, detail)
	}
	var out localASROutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		return nil, fmt.Errorf("cloudsync: local ASR runner returned invalid JSON: %w", err)
	}
	if len(out.Windows) == 0 {
		return nil, fmt.Errorf("cloudsync: local ASR runner returned no windows")
	}
	seen := map[int]bool{}
	results := make([]asrinput.Result, 0, len(out.Windows))
	for i, w := range out.Windows {
		index := w.Index
		if index <= 0 {
			index = i + 1
		}
		if seen[index] {
			return nil, fmt.Errorf("cloudsync: local ASR runner returned duplicate window index %d", index)
		}
		seen[index] = true
		if w.StartMS < 0 || w.EndMS <= w.StartMS || w.EndMS > durationMS {
			return nil, fmt.Errorf("cloudsync: local ASR window %d has invalid bounds %d-%dms (audio %dms)", index, w.StartMS, w.EndMS, durationMS)
		}
		segmentID := fmt.Sprintf("segment-%02d", index)
		model := t.Model
		if meta.Model != nil && strings.TrimSpace(*meta.Model) != "" {
			model = *meta.Model
		}
		results = append(results, asrinput.Result{
			ContractVersion: asrinput.ContractVersion,
			Source: asrinput.Source{
				Provider:           "local-" + strings.ToLower(strings.TrimSpace(t.Backend)),
				ProviderRequestID:  fmt.Sprintf("%s/%s", meta.RecordingID, segmentID),
				DeviceID:           meta.DeviceID,
				RecordingID:        meta.RecordingID,
				ConversationID:     meta.ConversationID,
				SegmentID:          segmentID,
				SegmentOffsetMS:    w.StartMS,
				SegmentDurationMS:  w.EndMS - w.StartMS,
				TranscriptRevision: meta.Revision,
				CompletionStatus:   "final",
				SourceAudioSHA256:  strPtr(meta.SourceAudioSHA256),
				Model:              strPtr(model),
			},
			Turns: []asrinput.Turn{{
				ID:            fmt.Sprintf("local-%02d", index),
				OriginalIndex: 1,
				Text:          strings.TrimSpace(w.Text),
				LocalStartMS:  new(int64(0)),
				LocalEndMS:    new(w.EndMS - w.StartMS),
				GlobalStartMS: new(w.StartMS),
				GlobalEndMS:   new(w.EndMS),
			}},
		})
	}
	return results, nil
}
