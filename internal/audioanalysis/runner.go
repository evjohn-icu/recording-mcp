package audioanalysis

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"recordingmcp/internal/gpulock"
	"recordingmcp/internal/process"
	"recordingmcp/internal/retention"
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
	// Before any PCM is copied, verify the resolved capability against the
	// configuration this worker would use right now: model id/revision, the
	// config digest and the ordered verified file mapping must match the
	// resolve this analyzer was built with, and the request must pin exactly
	// that identity. The adapter re-checks the config file it is handed, so
	// changes after this point are still caught there.
	if err := a.verifyModelIdentity(req); err != nil {
		return Result{}, nil, err
	}
	windowBytes := (req.EndMS - req.StartMS) * 32
	runtime := analysisRuntimeFromContext(ctx)
	staging, err := openAnalysisStaging(ctx, req)
	if err != nil {
		return Result{}, nil, err
	}
	defer staging.close()
	file, err := staging.create("input.pcm")
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
	requestBytes, err := json.Marshal(req)
	if err != nil {
		return Result{}, nil, err
	}
	if err := staging.writeJSON("request.json", requestBytes); err != nil {
		return Result{}, nil, err
	}
	outPath := staging.absPath("result.json")

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
		"--request", staging.absPath("request.json"), "--pcm", staging.absPath("input.pcm"),
		"--config", a.ConfigPath, "--out", outPath)
	// The child inherits the runner's shared retention lock: a worker that is
	// killed keeps the audio root locked until the child exits.
	if runtime.lockFile != nil {
		cmd.ExtraFiles = []*os.File{runtime.lockFile}
	}
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
	data, err := staging.read("result.json")
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
	if sidecar, err := staging.read("result.json.embeddings.json"); err == nil {
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

// verifyModelIdentity re-resolves the pinned capability from the current
// configuration and refuses a stale run before anything is staged: the model
// id/revision, config digest and ordered verified file mapping must equal the
// resolve the analyzer was constructed with, and the request must pin exactly
// that identity (absolute model paths to their verified hashes).
func (a SubprocessAnalyzer) verifyModelIdentity(req Request) error {
	capability := a.Resolved.Capability
	if capability == "" {
		return NewError(CodeModelUnavailable, false, "the audio run has no resolved capability")
	}
	resolved, err := a.Config.Resolve(capability)
	if err != nil {
		return err
	}
	if resolved.ModelID != a.Resolved.ModelID ||
		resolved.ModelRevision != a.Resolved.ModelRevision ||
		resolved.ConfigSHA256 != a.Resolved.ConfigSHA256 ||
		!sameModelFiles(resolved.Files, a.Resolved.Files) {
		return NewError(CodeModelUnavailable, false, "the resolved audio model no longer matches the audio analysis configuration")
	}
	if req.ModelID != resolved.ModelID ||
		req.ModelRevision != resolved.ModelRevision ||
		req.ConfigSHA256 != resolved.ConfigSHA256 {
		return NewError(CodeModelUnavailable, false, "the audio run does not pin the resolved model")
	}
	if len(req.ModelFilesSHA256) != len(resolved.Files) {
		return NewError(CodeModelUnavailable, false, "the audio run does not pin the resolved model files")
	}
	for _, file := range resolved.Files {
		hash, ok := req.ModelFilesSHA256[file.Path]
		if !ok || !strings.EqualFold(hash, file.SHA256) {
			return NewError(CodeModelUnavailable, false, "the audio run does not pin the resolved model files")
		}
	}
	return nil
}

// sameModelFiles compares the ordered verified file mapping by absolute path
// and hash.
func sameModelFiles(got, want []ModelFile) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i].Path != want[i].Path || !strings.EqualFold(got[i].SHA256, want[i].SHA256) {
			return false
		}
	}
	return true
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

// analysisRuntime carries the runner's managed audio root and the shared
// retention lock file to the subprocess analyzer through the package context.
type analysisRuntime struct {
	audioRoot string
	lockFile  *os.File
}

type analysisRuntimeKey struct{}

func analysisRuntimeFromContext(ctx context.Context) analysisRuntime {
	if runtime, ok := ctx.Value(analysisRuntimeKey{}).(analysisRuntime); ok {
		return runtime
	}
	return analysisRuntime{}
}

// analysisStaging is one adapter run's on-disk workspace. Under a managed
// audio root it lives at .analysis-tmp/<sha256(recordingID)>/<runID> with
// 0700 directories and 0600 files, opened and removed through os.Root; the
// anonymous temp directory remains for in-memory fixtures.
type analysisStaging struct {
	runtime analysisRuntime
	root    *os.Root // nil in legacy temp mode
	runRel  string   // relative workspace inside the managed root
	abs     string   // absolute workspace handed to the child process
}

func openAnalysisStaging(ctx context.Context, req Request) (*analysisStaging, error) {
	runtime := analysisRuntimeFromContext(ctx)
	if runtime.audioRoot == "" {
		dir, err := os.MkdirTemp("", "audio-analysis-")
		if err != nil {
			return nil, err
		}
		return &analysisStaging{runtime: runtime, abs: dir}, nil
	}
	root, err := os.OpenRoot(runtime.audioRoot)
	if err != nil {
		return nil, err
	}
	bucketRel := retention.AnalysisStagingRel(req.RecordingID)
	if err := root.MkdirAll(bucketRel, 0700); err != nil {
		root.Close()
		return nil, err
	}
	// A unique per-attempt directory: a retried attempt of the same run id
	// must never pre-clear a killed worker's staging while its adapter child
	// may still be writing, and it must never share a directory with that
	// child's late output. The exclusive mkdir makes the random suffix a
	// fence; stale directories stay resumably cleanable by the exclusive
	// forget cleanup, which removes the whole recording bucket.
	for {
		runRel := filepath.Join(bucketRel, req.RunID+"."+uniqueAttemptSuffix())
		err := root.Mkdir(runRel, 0700)
		if err == nil {
			return &analysisStaging{runtime: runtime, root: root, runRel: runRel, abs: filepath.Join(runtime.audioRoot, runRel)}, nil
		}
		if !errors.Is(err, os.ErrExist) {
			root.Close()
			return nil, err
		}
	}
}

// uniqueAttemptSuffix disambiguates concurrent attempts of the same run id.
func uniqueAttemptSuffix() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(buf[:])
}

// name resolves a workspace file: root-relative under managed staging,
// absolute inside the legacy temp directory.
func (s *analysisStaging) name(name string) string {
	if s.root == nil {
		return filepath.Join(s.abs, name)
	}
	return filepath.Join(s.runRel, name)
}

// absPath resolves a workspace file for the child process, which runs outside
// the os.Root boundary.
func (s *analysisStaging) absPath(name string) string {
	return filepath.Join(s.abs, name)
}

func (s *analysisStaging) create(name string) (*os.File, error) {
	if s.root == nil {
		return os.OpenFile(s.absPath(name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	}
	return s.root.OpenFile(s.name(name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
}

func (s *analysisStaging) writeJSON(name string, data []byte) error {
	if s.root == nil {
		return os.WriteFile(s.absPath(name), data, 0600)
	}
	return s.root.WriteFile(s.name(name), data, 0600)
}

func (s *analysisStaging) read(name string) ([]byte, error) {
	if s.root == nil {
		return os.ReadFile(s.absPath(name))
	}
	return s.root.ReadFile(s.name(name))
}

// close removes the run workspace. Under a managed root the per-recording
// staging directory is removed too when this was its last run; a Remove that
// fails because other runs are still staged stays a no-op.
func (s *analysisStaging) close() {
	if s.root == nil {
		_ = os.RemoveAll(s.abs)
		return
	}
	_ = s.root.RemoveAll(s.runRel)
	_ = s.root.Remove(filepath.Dir(s.runRel))
	_ = s.root.Close()
}

// Runner executes one audio-analysis job and finalises it atomically.
type Runner struct {
	Store    *Store
	Analyzer Analyzer
	Resolver PCMResolver
	Now      func() time.Time
	// AudioRoot is the managed audio root. A non-empty root makes the runner
	// hold a shared retention lock on it for the whole job — acquired before
	// the first claim/forget guard and held until the adapter child exited
	// and the commit or failure was recorded — and hand the open lock file to
	// the adapter child, so a killed worker leaves the child holding the
	// lock. The same root is the analysis staging workspace. An empty value
	// keeps the pure in-memory fixture behavior.
	AudioRoot string
}

// acquireAudioLock takes the shared retention lock on the managed audio root
// and returns the analysis runtime context plus a release function.
func (r *Runner) acquireAudioLock(ctx context.Context) (context.Context, func(), error) {
	if r.AudioRoot == "" {
		return ctx, func() {}, nil
	}
	lockFile, err := retention.LockFile(ctx, r.AudioRoot, false)
	if err != nil {
		return ctx, nil, err
	}
	return context.WithValue(ctx, analysisRuntimeKey{}, analysisRuntime{audioRoot: r.AudioRoot, lockFile: lockFile}),
		func() { _ = lockFile.Close() }, nil
}

// RunJob analyses and commits one job. A handled success or failure is fully
// recorded by the time RunJob returns nil; callers must then skip the generic
// Complete/Fail. Context cancellation is returned so the worker records a
// retryable attempt.
func (r *Runner) RunJob(ctx context.Context, job process.Job) error {
	// The shared retention lock is taken before the first claim/forget guard:
	// cleanup cannot silence or delete the recording while this run holds the
	// audio root, and the adapter child inherits the lock so a killed worker
	// cannot release it mid-run. It is held until the adapter exited and the
	// commit or failure was recorded.
	audioCtx, releaseAudioLock, err := r.acquireAudioLock(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return r.fail(ctx, job, NewError(CodeModelUnavailable, true, "the managed audio root is not readable"))
	}
	defer releaseAudioLock()
	ctx = audioCtx
	// The claim check runs before any input is obtained: a lost lease must not
	// touch the new owner's state, a superseded generation must not analyse the
	// stale window and a forget or retirement barrier must not read the
	// recording at all. The run status comes back from the same transaction,
	// so invalidation can never slip between the claim check and the status
	// read and silently leave a live attempt behind.
	claimStatus, err := r.Store.CheckClaim(ctx, job.ID, job.LeaseToken, r.now())
	if err != nil {
		switch ErrorCodeOf(err) {
		case CodeLeaseLost:
			// Another attempt owns or owned the job; nothing to record.
			return nil
		case CodeBarrierBlocked, CodeAudioSuperseded:
			return r.fail(ctx, job, err)
		default:
			return err
		}
	}
	var payload jobPayload
	if err := json.Unmarshal([]byte(job.PayloadJSON), &payload); err != nil {
		return fmt.Errorf("audioanalysis: decode job payload: %w", err)
	}
	req := payload.Request
	if claimStatus != StatusRunning {
		// The run reached a terminal state while this lease is still live (for
		// example a replayed snapshot after recovery): terminate the attempt
		// instead of silently keeping it, without touching the terminal run.
		return r.fail(ctx, job, NewError(CodeLeaseLost, false, "run is already finalised"))
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
	if result.Status == StatusFailed {
		// The adapter reported a structured failure: validate the response
		// identity fully, then process its code and retryable flag through the
		// same failure machinery as Fail. Only a succeeded result reaches
		// process.CompleteTx.
		if err := r.Store.FailResult(ctx, job.ID, job.LeaseToken, result, r.now()); err != nil {
			return r.fail(ctx, job, err)
		}
		return nil
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
