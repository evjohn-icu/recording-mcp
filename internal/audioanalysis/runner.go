package audioanalysis

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"recordingmcp/internal/gpulock"
	"recordingmcp/internal/process"
)

// EmbeddingAnalyzer is the optional adapter extension that returns speaker
// vectors alongside the public result. The vectors are stored in
// audio_speaker_embeddings and never serialised into result_json.
type EmbeddingAnalyzer interface {
	AnalyzeWithEmbeddings(ctx context.Context, req Request, pcm io.Reader) (Result, map[string][]float32, error)
}

// SubprocessAnalyzer runs the managed Python adapter for one analysis request.
// Every run gets its own temporary directory; the PCM copy and the request file
// are deleted when the child exits.
type SubprocessAnalyzer struct {
	Config     *Config
	ConfigPath string
	Resolved   Resolved
	// Env is an explicit environment inherited by the child. Only the local
	// model stack variables are passed; API keys are never forwarded.
	Env []string
	// VRAMFreeMiB probes available device memory. Nil skips the check.
	VRAMFreeMiB func(ctx context.Context) (int, error)
}

// Analyze implements Analyzer.
func (a SubprocessAnalyzer) Analyze(ctx context.Context, req Request, pcm io.Reader) (Result, error) {
	result, _, err := a.AnalyzeWithEmbeddings(ctx, req, pcm)
	return result, err
}

// AnalyzeWithEmbeddings runs the adapter and reads the optional embedding
// sidecar.
func (a SubprocessAnalyzer) AnalyzeWithEmbeddings(ctx context.Context, req Request, pcm io.Reader) (Result, map[string][]float32, error) {
	if a.Config == nil || a.ConfigPath == "" {
		return Result{}, nil, NewError(CodeDisabled, false, "audio analysis config is not loaded")
	}
	if err := req.Validate(); err != nil {
		return Result{}, nil, err
	}
	windowBytes := (req.EndMS - req.StartMS) * 32
	dir, err := os.MkdirTemp("", "audio-analysis-")
	if err != nil {
		return Result{}, nil, err
	}
	defer os.RemoveAll(dir)
	pcmPath := filepath.Join(dir, "input.pcm")
	file, err := os.OpenFile(pcmPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return Result{}, nil, err
	}
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(file, hash), io.LimitReader(pcm, windowBytes+1))
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil {
		return Result{}, nil, NewError(CodeInputInvalid, false, "cannot stage the PCM window")
	}
	if written != windowBytes {
		return Result{}, nil, NewError(CodeInputInvalid, false, "PCM window length does not match start_ms/end_ms")
	}
	if hex.EncodeToString(hash.Sum(nil)) != req.InputSHA256 {
		return Result{}, nil, NewError(CodeInputInvalid, false, "PCM window hash does not match input_sha256")
	}
	requestPath := filepath.Join(dir, "request.json")
	requestBytes, err := json.Marshal(req)
	if err != nil {
		return Result{}, nil, err
	}
	if err := os.WriteFile(requestPath, requestBytes, 0600); err != nil {
		return Result{}, nil, err
	}
	outPath := filepath.Join(dir, "result.json")

	release := func() {}
	if strings.HasPrefix(strings.ToLower(a.Resolved.Device), "cuda") {
		lock, load, _ := a.Config.Timeouts()
		_ = load
		lockPath := a.Config.GPULock
		releaseLock, err := gpulock.Acquire(ctx, lockPath, lock)
		if err != nil {
			if errors.Is(err, gpulock.ErrBusy) {
				return Result{}, nil, NewError(CodeGPUBusy, true, "another local model holds the GPU lock; retry")
			}
			return Result{}, nil, err
		}
		release = releaseLock
		if a.VRAMFreeMiB != nil {
			free, err := a.VRAMFreeMiB(ctx)
			if err == nil && free < 1024 {
				release()
				return Result{}, nil, NewError(CodeGPUBusy, true, "less than 1024 MiB of device memory is free")
			}
		}
	}
	defer release()

	_, _, runTimeout := a.Config.Timeouts()
	runCtx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, a.Config.Python, a.Config.Runner,
		"--request", requestPath, "--pcm", pcmPath, "--config", a.ConfigPath, "--out", outPath)
	cmd.Env = a.childEnv()
	var stderr bytes.Buffer
	cmd.Stderr = &limitedWriter{w: &stderr, remaining: 4096}
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return Result{}, nil, ctx.Err()
		}
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			return Result{}, nil, NewError(CodeModelUnavailable, true, "audio adapter exceeded its run timeout")
		}
		// The adapter writes a failed Result for model/config problems; a
		// missing document means it could not start.
		if _, statErr := os.Stat(outPath); statErr != nil {
			code := CodeModelUnavailable
			if strings.Contains(stderr.String(), CodeInputInvalid) {
				code = CodeInputInvalid
			}
			return Result{}, nil, NewError(code, true, "audio adapter failed to run")
		}
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		return Result{}, nil, NewError(CodeModelUnavailable, true, "audio adapter produced no result document")
	}
	result, err := ParseResult(data)
	if err != nil {
		return Result{}, nil, NewError(CodeResultInvalid, true, "audio adapter produced an unreadable result")
	}
	if err := result.ValidateResult(req, windowBytes/32); err != nil {
		return Result{}, nil, err
	}
	embeddings := map[string][]float32{}
	if sidecar, err := os.ReadFile(outPath + ".embeddings.json"); err == nil {
		if err := json.Unmarshal(sidecar, &embeddings); err != nil {
			return Result{}, nil, NewError(CodeResultInvalid, false, "embedding sidecar is not valid JSON")
		}
		for eventID, vector := range embeddings {
			if err := validateEmbedding(vector); err != nil {
				return Result{}, nil, err
			}
			found := false
			for _, event := range result.Events {
				if event.ID == eventID {
					found = true
					break
				}
			}
			if !found {
				return Result{}, nil, NewError(CodeResultInvalid, false, "embedding sidecar references an unknown event")
			}
		}
	}
	return result, embeddings, nil
}

// childEnv passes only the local model stack variables. API keys never reach
// the adapter.
func (a SubprocessAnalyzer) childEnv() []string {
	allowed := map[string]bool{
		"PATH": true, "HOME": true, "LANG": true, "LC_ALL": true, "TMPDIR": true, "TZ": true,
		"LD_LIBRARY_PATH": true, "CUDA_HOME": true, "CUDA_PATH": true, "CUDA_VISIBLE_DEVICES": true,
		"XDG_RUNTIME_DIR": true,
	}
	out := []string{"HF_HUB_OFFLINE=1", "TRANSFORMERS_OFFLINE=1"}
	for _, entry := range os.Environ() {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if allowed[key] || strings.HasPrefix(key, "HF_") || strings.HasPrefix(key, "PYTHON") || strings.HasPrefix(key, "ORT_") {
			out = append(out, entry)
		}
	}
	if a.Env != nil {
		out = append(out, a.Env...)
	}
	return out
}

// limitedWriter keeps the stderr capture bounded.
type limitedWriter struct {
	w         io.Writer
	remaining int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.remaining <= 0 {
		return len(p), nil
	}
	if len(p) > l.remaining {
		p = p[:l.remaining]
	}
	l.remaining -= len(p)
	_, err := l.w.Write(p)
	return len(p), err
}

// PCMResolver supplies the analysed window and the original file hash.
type PCMResolver interface {
	RecordingWindow(ctx context.Context, recordingID string, startMS, endMS int64) (io.ReadCloser, error)
	SourceAudioSHA256(ctx context.Context, recordingID string) (string, error)
}

// Runner executes one audio-analysis job and finalises it atomically.
type Runner struct {
	Store    *Store
	Analyzer Analyzer
	Resolver PCMResolver
	Now      func() time.Time
}

// RunJob analyses and commits one job. A handled success or failure is fully
// recorded by the time RunJob returns nil; callers must then skip the generic
// Complete/Fail. Context cancellation is returned so the worker records a
// retryable attempt.
func (r *Runner) RunJob(ctx context.Context, job process.Job) error {
	var payload jobPayload
	if err := json.Unmarshal([]byte(job.PayloadJSON), &payload); err != nil {
		return fmt.Errorf("audioanalysis: decode job payload: %w", err)
	}
	req := payload.Request
	status, err := r.Store.RunStatus(ctx, job.ID)
	if err != nil {
		return err
	}
	if status != StatusRunning {
		return nil
	}
	if r.Resolver == nil {
		return r.fail(ctx, job, NewError(CodeDisabled, false, "no PCM resolver configured"))
	}
	pcm, err := r.Resolver.RecordingWindow(ctx, req.RecordingID, req.StartMS, req.EndMS)
	if err != nil {
		return r.fail(ctx, job, NewError(CodeModelUnavailable, false, "the recording audio is not available locally"))
	}
	defer pcm.Close()
	var result Result
	embeddings := map[string][]float32{}
	if extended, ok := r.Analyzer.(EmbeddingAnalyzer); ok {
		result, embeddings, err = extended.AnalyzeWithEmbeddings(ctx, req, pcm)
	} else {
		result, err = r.Analyzer.Analyze(ctx, req, pcm)
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var typed *Error
		if errors.As(err, &typed) {
			return r.fail(ctx, job, typed)
		}
		return r.fail(ctx, job, NewError(CodeModelUnavailable, true, "audio adapter failed"))
	}
	if _, err := r.Store.Commit(ctx, CommitInput{JobID: job.ID, LeaseToken: job.LeaseToken, Result: result, Embeddings: embeddings, Now: r.now()}); err != nil {
		return r.fail(ctx, job, err)
	}
	return nil
}

// fail records a terminal or retryable failure and reports whether it was
// handled. A store failure is returned so the worker cannot silently succeed.
func (r *Runner) fail(ctx context.Context, job process.Job, cause error) error {
	code := CodeModelUnavailable
	retryable := false
	var typed *Error
	if errors.As(cause, &typed) {
		code = typed.Code
		retryable = typed.Retryable
	}
	if err := r.Store.Fail(ctx, job.ID, job.LeaseToken, code, retryable, r.now()); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		// A lost lease means another worker owns the job; nothing to record.
		var storeErr *Error
		if errors.As(err, &storeErr) && storeErr.Code == CodeLeaseLost {
			return nil
		}
		return err
	}
	return nil
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}
