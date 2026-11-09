package cloudsync

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"recordingmcp/internal/asrinput"
)

// Window is one ASR window in the shared plan. The Go planner and
// scripts/local-asr.py must agree exactly, because --skip is expressed in these
// 1-based indexes.
type Window struct {
	Index   int
	StartMS int64
	EndMS   int64
}

// PlanWindows mirrors plan_windows() in scripts/local-asr.py.
func PlanWindows(durationMS, windowMS, overlapMS int64) ([]Window, error) {
	if windowMS <= 0 {
		return nil, fmt.Errorf("cloudsync: window length must be positive")
	}
	if overlapMS < 0 || overlapMS >= windowMS {
		return nil, fmt.Errorf("cloudsync: overlap %dms must be in [0, %dms)", overlapMS, windowMS)
	}
	step := windowMS - overlapMS
	var out []Window
	for start := int64(0); start < durationMS; {
		end := start + windowMS
		if end > durationMS {
			end = durationMS
		}
		out = append(out, Window{Index: len(out) + 1, StartMS: start, EndMS: end})
		if end >= durationMS {
			break
		}
		start += step
	}
	return out, nil
}

// ChunkTranscriber is implemented by local ASR backends that can emit each
// window as soon as it finishes, so the ingest stage can persist progress and
// resume instead of re-decoding the whole recording.
type ChunkTranscriber interface {
	TranscribeChunks(ctx context.Context, pcmPath string, meta MetaBasis, skip []int, onResult func(asrinput.Result) error) error
}

// RunnerError classifies a failed local ASR runner invocation. The ingest stage
// maps Code onto a user-visible action; Index is the next uncommitted window.
type RunnerError struct {
	Code    string
	Message string
	Index   int
	Err     error
}

func (e *RunnerError) Error() string {
	message := e.Message
	if message == "" {
		message = "cloudsync: local ASR runner failed"
	}
	var b strings.Builder
	b.WriteString(message)
	if e.Index > 0 {
		fmt.Fprintf(&b, " (window %d)", e.Index)
	}
	if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	return b.String()
}

func (e *RunnerError) Unwrap() error { return e.Err }

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
	// Timeout bounds one runner invocation; zero means two hours.
	Timeout time.Duration
	// LoadTimeout bounds model loading; zero means five minutes.
	LoadTimeout time.Duration
	// WindowTimeout bounds one window; zero means five minutes.
	WindowTimeout time.Duration
	// LockPath serializes local inference across processes; empty disables it.
	LockPath string
	// LockTimeout bounds waiting for LockPath; zero means ten minutes.
	LockTimeout time.Duration
	// Env appends environment variables for the runner.
	Env []string
}

// WindowPlan exposes the configured window geometry to the ingest stage.
func (t LocalTranscriber) WindowPlan() (int64, int64) {
	segment := t.SegmentMS
	if segment <= 0 {
		segment = 30000
	}
	overlap := t.OverlapMS
	if overlap < 0 {
		overlap = 0
	}
	return segment, overlap
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

type localASRWindow struct {
	Index    int    `json:"index"`
	StartMS  int64  `json:"start_ms"`
	EndMS    int64  `json:"end_ms"`
	Text     string `json:"text"`
	Language string `json:"language"`
}

// Transcribe implements Transcriber. It keeps the single-document output the
// existing runner emits, so fixture stubs and older scripts keep working.
func (t LocalTranscriber) Transcribe(ctx context.Context, pcmPath string, meta MetaBasis) ([]asrinput.Result, error) {
	return t.transcribe(ctx, pcmPath, meta, nil, false, nil)
}

// TranscribeChunks implements ChunkTranscriber: one callback per completed
// window, in order, before the next line is read.
func (t LocalTranscriber) TranscribeChunks(ctx context.Context, pcmPath string, meta MetaBasis, skip []int, onResult func(asrinput.Result) error) error {
	if onResult == nil {
		return fmt.Errorf("cloudsync: TranscribeChunks requires a result callback")
	}
	_, err := t.transcribe(ctx, pcmPath, meta, skip, true, onResult)
	return err
}

func (t LocalTranscriber) transcribe(ctx context.Context, pcmPath string, meta MetaBasis, skip []int, stream bool, onResult func(asrinput.Result) error) ([]asrinput.Result, error) {
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
	segmentMS, overlapMS := t.WindowPlan()
	plan, err := PlanWindows(durationMS, segmentMS, overlapMS)
	if err != nil {
		return nil, err
	}
	planned := make(map[int]Window, len(plan))
	for _, window := range plan {
		planned[window.Index] = window
	}
	skipSet := make(map[int]bool, len(skip))
	for _, index := range skip {
		if _, ok := planned[index]; !ok {
			return nil, fmt.Errorf("cloudsync: skip index %d is outside the %d-window plan", index, len(plan))
		}
		skipSet[index] = true
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
	maxTokens := t.MaxNewTokens
	if maxTokens <= 0 {
		maxTokens = 512
	}
	loadTimeout := t.LoadTimeout
	if loadTimeout <= 0 {
		loadTimeout = 5 * time.Minute
	}
	windowTimeout := t.WindowTimeout
	if windowTimeout <= 0 {
		windowTimeout = 5 * time.Minute
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
		"--load-timeout", fmt.Sprint(int64(loadTimeout / time.Second)),
		"--window-timeout", fmt.Sprint(int64(windowTimeout / time.Second)),
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
	if stream {
		args = append(args, "--stream")
	}
	if len(skipSet) > 0 {
		indexes := make([]string, 0, len(skipSet))
		for _, window := range plan {
			if skipSet[window.Index] {
				indexes = append(indexes, strconv.Itoa(window.Index))
			}
		}
		args = append(args, "--skip", strings.Join(indexes, ","))
	}
	timeout := t.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Hour
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if strings.TrimSpace(t.LockPath) != "" {
		release, err := acquireGPULock(ctx, t.LockPath, t.LockTimeout)
		if err != nil {
			return nil, err
		}
		defer release()
	}
	stderr := &tailBuffer{limit: 8 * 1024}
	cmd := exec.CommandContext(runCtx, python, args...)
	cmd.Env = localASREnv(t.Env)
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("cloudsync: local ASR runner stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("cloudsync: start local ASR runner: %w", err)
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var results []asrinput.Result
	var raw bytes.Buffer
	committed := make(map[int]bool, len(plan))
	nextIndex := 1
	var callbackErr error
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		if !stream {
			if raw.Len() > 0 {
				raw.WriteByte('\n')
			}
			raw.Write(line)
			continue
		}
		var window localASRWindow
		if err := json.Unmarshal(line, &window); err != nil {
			callbackErr = fmt.Errorf("cloudsync: local ASR runner returned invalid JSON line: %w", err)
			cancel()
			break
		}
		result, err := t.resultForWindow(window.Index, window.StartMS, window.EndMS, window.Text, durationMS, planned, skipSet, committed, meta)
		if err != nil {
			callbackErr = err
			cancel()
			break
		}
		committed[window.Index] = true
		nextIndex = window.Index + 1
		results = append(results, result)
		if err := onResult(result); err != nil {
			callbackErr = err
			cancel()
			break
		}
	}
	scanErr := scanner.Err()
	waitErr := cmd.Wait()
	if callbackErr != nil {
		return nil, callbackErr
	}
	if scanErr != nil {
		return nil, fmt.Errorf("cloudsync: read local ASR output: %w", scanErr)
	}
	if waitErr != nil {
		return nil, classifyRunnerError(runCtx, waitErr, stderr.String(), nextIndex)
	}
	if stream {
		if len(results) == 0 {
			return nil, fmt.Errorf("cloudsync: local ASR runner returned no windows")
		}
		return results, nil
	}
	var out localASROutput
	if err := json.Unmarshal(raw.Bytes(), &out); err != nil {
		return nil, fmt.Errorf("cloudsync: local ASR runner returned invalid JSON: %w", err)
	}
	if len(out.Windows) == 0 {
		return nil, fmt.Errorf("cloudsync: local ASR runner returned no windows")
	}
	seen := map[int]bool{}
	results = make([]asrinput.Result, 0, len(out.Windows))
	for i, w := range out.Windows {
		index := w.Index
		if index <= 0 {
			index = i + 1
		}
		result, err := t.resultForWindow(index, w.StartMS, w.EndMS, w.Text, durationMS, planned, skipSet, seen, meta)
		if err != nil {
			return nil, err
		}
		seen[index] = true
		results = append(results, result)
	}
	return results, nil
}

func (t LocalTranscriber) resultForWindow(index int, startMS, endMS int64, text string, durationMS int64, planned map[int]Window, skipSet, seen map[int]bool, meta MetaBasis) (asrinput.Result, error) {
	window, ok := planned[index]
	if !ok {
		return asrinput.Result{}, fmt.Errorf("cloudsync: local ASR runner returned window %d outside the %d-window plan", index, len(planned))
	}
	if skipSet[index] {
		return asrinput.Result{}, fmt.Errorf("cloudsync: local ASR runner decoded skipped window %d", index)
	}
	if seen[index] {
		return asrinput.Result{}, fmt.Errorf("cloudsync: local ASR runner returned duplicate window index %d", index)
	}
	if startMS < 0 || endMS <= startMS || endMS > durationMS {
		return asrinput.Result{}, fmt.Errorf("cloudsync: local ASR window %d has invalid bounds %d-%dms (audio %dms)", index, startMS, endMS, durationMS)
	}
	if startMS != window.StartMS || endMS != window.EndMS {
		return asrinput.Result{}, fmt.Errorf("cloudsync: local ASR window %d has bounds %d-%dms, plan says %d-%dms", index, startMS, endMS, window.StartMS, window.EndMS)
	}
	segmentID := fmt.Sprintf("segment-%02d", index)
	model := t.Model
	if meta.Model != nil && strings.TrimSpace(*meta.Model) != "" {
		model = *meta.Model
	}
	return asrinput.Result{
		ContractVersion: asrinput.ContractVersion,
		Source: asrinput.Source{
			Provider:           "local-" + strings.ToLower(strings.TrimSpace(t.Backend)),
			ProviderRequestID:  fmt.Sprintf("%s/%s", meta.RecordingID, segmentID),
			DeviceID:           meta.DeviceID,
			RecordingID:        meta.RecordingID,
			ConversationID:     meta.ConversationID,
			SegmentID:          segmentID,
			SegmentOffsetMS:    startMS,
			SegmentDurationMS:  endMS - startMS,
			TranscriptRevision: meta.Revision,
			CompletionStatus:   "final",
			SourceAudioSHA256:  strPtr(meta.SourceAudioSHA256),
			Model:              strPtr(model),
		},
		Turns: []asrinput.Turn{{
			ID:            fmt.Sprintf("local-%02d", index),
			OriginalIndex: 1,
			Text:          strings.TrimSpace(text),
			LocalStartMS:  new(int64(0)),
			LocalEndMS:    new(endMS - startMS),
			GlobalStartMS: new(startMS),
			GlobalEndMS:   new(endMS),
		}},
	}, nil
}

// classifyRunnerError turns the runner contract (exit 4 = model load, exit 5 =
// window timeout) plus stderr into a code the ingest action map understands.
func classifyRunnerError(runCtx context.Context, waitErr error, detail string, index int) *RunnerError {
	const base = "cloudsync: local ASR runner failed"
	tail := truncateTail(detail, 2000)
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return &RunnerError{Code: "asr_timeout", Message: base + ": timeout", Index: index, Err: waitErr}
	}
	lower := strings.ToLower(detail)
	if strings.Contains(lower, "cuda out of memory") || strings.Contains(detail, "cudaErrorMemoryAllocation") || strings.Contains(detail, "OutOfMemoryError") {
		message := base
		if tail != "" {
			message += ": " + tail
		}
		return &RunnerError{Code: "asr_oom", Message: message, Index: index, Err: waitErr}
	}
	code := 0
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		code = exitErr.ExitCode()
	}
	switch code {
	case 4:
		return &RunnerError{Code: "asr_model_load_failed", Message: base + ": model load failed" + detailSuffix(tail), Index: index, Err: waitErr}
	case 5:
		return &RunnerError{Code: "asr_timeout", Message: base + ": window timeout" + detailSuffix(tail), Index: index, Err: waitErr}
	}
	message := base
	if tail != "" {
		message += ": " + tail
	}
	return &RunnerError{Code: "asr_runner_failed", Message: message, Index: index, Err: waitErr}
}

func detailSuffix(tail string) string {
	if tail == "" {
		return ""
	}
	return ": " + tail
}

func truncateTail(detail string, limit int) string {
	detail = strings.TrimSpace(detail)
	if len(detail) <= limit {
		return detail
	}
	cut := len(detail) - limit
	for cut < len(detail) && !utf8.RuneStart(detail[cut]) {
		cut++
	}
	return detail[cut:]
}

// tailBuffer keeps the last limit bytes written to it.
type tailBuffer struct {
	limit int
	buf   []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.limit {
		b.buf = append(b.buf[:0], b.buf[len(b.buf)-b.limit:]...)
	}
	return len(p), nil
}

func (b *tailBuffer) String() string { return string(b.buf) }

// localASREnv passes only what the runner needs. The worker environment holds
// API keys and the profile password; third-party model code must not inherit
// them.
func localASREnv(extra []string) []string {
	keys := []string{
		"PATH", "HOME", "TMPDIR", "TMP", "TEMP", "LANG", "LC_ALL",
		"CUDA_VISIBLE_DEVICES", "CUDA_HOME", "LD_LIBRARY_PATH",
		"HF_HOME", "HF_HUB_CACHE", "HF_HUB_OFFLINE", "TRANSFORMERS_CACHE", "XDG_CACHE_HOME",
	}
	env := make([]string, 0, len(keys)+len(extra))
	for _, key := range keys {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	return append(env, extra...)
}
