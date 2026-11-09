package ingest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"recordingmcp/internal/asrinput"
	"recordingmcp/internal/cloudsync"
	"recordingmcp/internal/memory"
	"recordingmcp/internal/pipeline"
	"recordingmcp/internal/process"
	"recordingmcp/internal/semantic"
	"recordingmcp/internal/worker"
)

// stubChunkTranscriber implements cloudsync.ChunkTranscriber without a model.
// It plans windows exactly like the runner, so the ingest checkpoint logic is
// exercised for real; only inference is faked.
type stubChunkTranscriber struct {
	windowMS  int64
	overlapMS int64
	// emitLimit stops after N new windows with a retryable error (0 = no limit).
	emitLimit int
	// failAt fails on that 1-based window with failErr.
	failAt  int
	failErr error
	// shape lets a test observe what was skipped.
	calls   int
	skipped []int
}

func (s *stubChunkTranscriber) WindowPlan() (int64, int64) { return s.windowMS, s.overlapMS }

func (s *stubChunkTranscriber) Transcribe(context.Context, string, cloudsync.MetaBasis) ([]asrinput.Result, error) {
	return nil, errors.New("whole-file path must not be used for a chunk transcriber")
}

func (s *stubChunkTranscriber) TranscribeChunks(_ context.Context, pcmPath string, meta cloudsync.MetaBasis, skip []int, onResult func(asrinput.Result) error) error {
	s.calls++
	s.skipped = append(s.skipped, skip...)
	info, err := os.Stat(pcmPath)
	if err != nil {
		return err
	}
	plan, err := cloudsync.PlanWindows(info.Size()/32, s.windowMS, s.overlapMS)
	if err != nil {
		return err
	}
	skipSet := map[int]bool{}
	for _, index := range skip {
		skipSet[index] = true
	}
	emitted := 0
	for _, window := range plan {
		if skipSet[window.Index] {
			continue
		}
		if s.failAt == window.Index && s.failErr != nil {
			return s.failErr
		}
		if s.emitLimit > 0 && emitted >= s.emitLimit {
			return errors.New("synthetic interruption")
		}
		if err := onResult(stubWindowResult(window, meta)); err != nil {
			return err
		}
		emitted++
	}
	return nil
}

func stubWindowResult(window cloudsync.Window, meta cloudsync.MetaBasis) asrinput.Result {
	start, end := window.StartMS, window.EndMS
	return asrinput.Result{
		ContractVersion: asrinput.ContractVersion,
		Source: asrinput.Source{
			Provider:           "local-test",
			ProviderRequestID:  fmt.Sprintf("%s/segment-%02d", meta.RecordingID, window.Index),
			DeviceID:           meta.DeviceID,
			RecordingID:        meta.RecordingID,
			ConversationID:     meta.ConversationID,
			SegmentID:          fmt.Sprintf("segment-%02d", window.Index),
			SegmentOffsetMS:    start,
			SegmentDurationMS:  end - start,
			TranscriptRevision: meta.Revision,
			CompletionStatus:   "final",
		},
		Turns: []asrinput.Turn{{
			ID:            fmt.Sprintf("local-%02d", window.Index),
			OriginalIndex: 1,
			Text:          fmt.Sprintf("window %d", window.Index),
			LocalStartMS:  new(int64(0)),
			LocalEndMS:    new(end - start),
			GlobalStartMS: new(start),
			GlobalEndMS:   new(end),
		}},
	}
}

// chunkEnv imports one 31 s recording and returns the ASR job. The stub window
// geometry makes the window count explicit (7 windows of 5 s, no overlap).
func chunkEnv(t *testing.T, stub *stubChunkTranscriber) (*testEnv, process.Job, ImportResult) {
	t.Helper()
	env := newTestEnv(t, func(cfg *Config) {
		cfg.Decoder = &fakeDecoder{info: AudioInfo{DurationMS: 31000, Format: "wav", SampleRate: 16000, Channels: 1}}
		cfg.Transcription = stub
		cfg.ASRProvider = "ark"
	})
	res, err := env.importFile("long.wav", []byte("long-recording-bytes"))
	if err != nil {
		t.Fatal(err)
	}
	env.runIngest(env.job(res.JobID))
	return env, env.job(env.asrJobID(res.ConversationID)), res
}

func attachmentCount(t *testing.T, env *testEnv, res ImportResult) int {
	t.Helper()
	recordingID := memory.RecordingID(DeviceID, providerRecordingID(res))
	return env.count(`SELECT COUNT(*) FROM source_attachments WHERE recording_id=? AND active=1`, recordingID)
}

func TestRunASRJobResumesCommittedWindows(t *testing.T) {
	stub := &stubChunkTranscriber{windowMS: 5000}
	env, job, res := chunkEnv(t, stub)
	stub.emitLimit = 2
	if err := env.runASR(job); err == nil {
		t.Fatal("first attempt must fail after two committed windows")
	}
	state, completed, failed, expected := asrProgress(t, env, res)
	if state != "partial" || completed != 2 || failed != 0 || expected != 7 {
		t.Fatalf("after interruption state=%q completed=%d failed=%d expected=%d", state, completed, failed, expected)
	}
	if got := attachmentCount(t, env, res); got != 2 {
		t.Fatalf("active attachments = %d, want 2", got)
	}
	resume := &stubChunkTranscriber{windowMS: 5000}
	env.svc.trans = resume
	if err := env.runASR(job); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if len(resume.skipped) != 2 || resume.skipped[0] != 1 || resume.skipped[1] != 2 {
		t.Fatalf("resume skipped = %v, want [1 2]", resume.skipped)
	}
	if got := attachmentCount(t, env, res); got != 7 {
		t.Fatalf("active attachments after resume = %d, want 7 (no duplicates)", got)
	}
	state, completed, failed, _ = asrProgress(t, env, res)
	if state != "done" || completed != 7 || failed != 0 {
		t.Fatalf("after resume state=%q completed=%d failed=%d", state, completed, failed)
	}
}

func TestRunASRJobCoverageAccounting(t *testing.T) {
	stub := &stubChunkTranscriber{windowMS: 30000, overlapMS: 2000}
	env, job, res := chunkEnv(t, stub)
	if err := env.runASR(job); err != nil {
		t.Fatal(err)
	}
	state, completed, _, expected := asrProgress(t, env, res)
	if state != "done" || completed != 2 || expected != 2 {
		t.Fatalf("state=%q completed=%d expected=%d", state, completed, expected)
	}
	var coverage int64
	if err := env.store.DB().QueryRow(`SELECT coverage_ms FROM sync_recordings WHERE recording_id=?`, providerRecordingID(res)).Scan(&coverage); err != nil {
		t.Fatal(err)
	}
	if coverage != 31000 {
		t.Fatalf("coverage = %dms, want 31000ms (union, not the 32000ms window sum)", coverage)
	}
}

func TestRunASRJobKeepsPartialStateOnWindowFailure(t *testing.T) {
	stub := &stubChunkTranscriber{windowMS: 5000}
	env, job, res := chunkEnv(t, stub)
	stub.failAt = 3
	stub.failErr = &cloudsync.RunnerError{Code: "asr_timeout", Message: "cloudsync: local ASR runner failed: window timeout", Index: 3}
	_ = env.runASR(job)
	state, completed, failed, expected := asrProgress(t, env, res)
	if state != "partial" || completed != 2 || failed != 1 || expected != 7 {
		t.Fatalf("after window failure state=%q completed=%d failed=%d expected=%d", state, completed, failed, expected)
	}
	resume := &stubChunkTranscriber{windowMS: 5000}
	env.svc.trans = resume
	if err := env.runASR(job); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if len(resume.skipped) != 2 || resume.skipped[0] != 1 || resume.skipped[1] != 2 {
		t.Fatalf("retry skipped = %v, want [1 2]", resume.skipped)
	}
	state, completed, failed, _ = asrProgress(t, env, res)
	if state != "done" || completed != 7 || failed != 1 {
		t.Fatalf("after retry state=%q completed=%d failed=%d", state, completed, failed)
	}
}

func TestImportDuplicateBytesReuseJob(t *testing.T) {
	env := newTestEnv(t, nil)
	first, err := env.importFile("same.wav", []byte("identical-bytes"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := env.importFile("same.wav", []byte("identical-bytes"))
	if err != nil {
		t.Fatal(err)
	}
	if !second.Duplicate || second.ConversationID != first.ConversationID || second.JobID != first.JobID {
		t.Fatalf("duplicate import = %+v, want the same conversation/job with duplicate=true", second)
	}
	if got := env.count(`SELECT COUNT(*) FROM recordings`); got != 1 {
		t.Fatalf("recordings = %d, want 1", got)
	}
	if got := env.count(`SELECT COUNT(*) FROM processing_jobs WHERE stage='ingest'`); got != 1 {
		t.Fatalf("ingest jobs = %d, want 1", got)
	}
	if got := env.count(`SELECT COUNT(*) FROM source_attachments`); got != 0 {
		t.Fatalf("attachments written by duplicate = %d, want 0", got)
	}
}

func TestRunASRJobMapsCUDAOutOfMemory(t *testing.T) {
	stub := &stubChunkTranscriber{windowMS: 5000}
	env, job, _ := chunkEnv(t, stub)
	stub.failAt = 2
	stub.failErr = &cloudsync.RunnerError{Code: "asr_oom", Message: "cloudsync: local ASR runner failed: CUDA out of memory", Index: 2}
	_, err := env.svc.RunASRJob(context.Background(), job)
	var structured *Error
	if !errors.As(err, &structured) {
		t.Fatalf("error = %v, want *ingest.Error", err)
	}
	details := structured.ErrorDetails()
	if details["code"] != "asr_oom" {
		t.Fatalf("code = %q, want asr_oom", details["code"])
	}
	if action := details["action"]; !strings.Contains(action, "显存") {
		t.Fatalf("action = %q, want a VRAM hint", action)
	}
}

func TestProcessingStatusProjectsASRProgress(t *testing.T) {
	stub := &stubChunkTranscriber{windowMS: 5000}
	env, job, res := chunkEnv(t, stub)
	stub.emitLimit = 2
	_ = env.runASR(job)
	status, err := env.store.GetProcessingStatus(context.Background(), res.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	if status.ASRState != "partial" || status.ASRCompletedChunks != 2 || status.ASRExpectedChunks != 7 {
		t.Fatalf("status ASR projection = %+v", status)
	}
	if status.ASRCoverageMS != 10000 || status.ASRDurationMS != 31000 {
		t.Fatalf("coverage/duration = %d/%dms, want 10000/31000", status.ASRCoverageMS, status.ASRDurationMS)
	}
}

func TestEmbedJobResumesAfterTranscriptCommit(t *testing.T) {
	stub := &stubChunkTranscriber{windowMS: 5000}
	env, job, res := chunkEnv(t, stub)
	if err := env.runASR(job); err != nil {
		t.Fatal(err)
	}
	// The transcript commit enqueues the embed stage; simulate a worker crash
	// on the first attempt, then restart with the real handler.
	var queued int
	if err := env.store.DB().QueryRow(`SELECT COUNT(*) FROM processing_jobs WHERE stage='embed' AND input_snapshot_id=?`, res.ConversationID).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued == 0 {
		t.Fatal("transcript commit did not enqueue an embed job")
	}
	crash := worker.Config{
		DB: env.store.DB(), WorkerID: "crash-worker", Stages: []string{"embed"},
		Lease: time.Minute, ProcessorVersion: "embed-v1",
		Executor: failingExecutor{err: errors.New("worker killed")},
	}
	if _, err := worker.RunOnce(context.Background(), crash); err != nil {
		t.Fatal(err)
	}
	restart := worker.Config{
		DB: env.store.DB(), WorkerID: "restart-worker", Stages: []string{"embed"},
		Lease: time.Minute, ProcessorVersion: "embed-v1",
		Executor: embedExecutor{h: &pipeline.EmbedHandler{DB: env.store.DB(), Provider: semantic.NewHashProvider(semantic.DefaultHashDims)}},
	}
	for range 10 {
		more, err := worker.RunOnce(context.Background(), restart)
		if err != nil {
			t.Fatal(err)
		}
		if !more {
			break
		}
	}
	status, err := env.store.GetProcessingStatus(context.Background(), res.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	if status.IndexingStatus != "ready" {
		t.Fatalf("indexing_status = %q, want ready after the restarted embed worker", status.IndexingStatus)
	}
}

type failingExecutor struct{ err error }

func (f failingExecutor) Execute(context.Context, process.Job) (string, error) { return "", f.err }

type embedExecutor struct{ h *pipeline.EmbedHandler }

func (e embedExecutor) Execute(ctx context.Context, job process.Job) (string, error) {
	return e.h.Execute(ctx, job)
}

func providerRecordingID(res ImportResult) string {
	return "import:" + sha256Hex([]byte("long-recording-bytes"))[:32]
}

func asrProgress(t *testing.T, env *testEnv, res ImportResult) (state string, completed, failed, expected int) {
	t.Helper()
	if err := env.store.DB().QueryRow(`SELECT transcription_state, completed_chunks, failed_chunks, total_chunks FROM sync_recordings WHERE recording_id=?`, providerRecordingID(res)).
		Scan(&state, &completed, &failed, &expected); err != nil {
		t.Fatal(err)
	}
	return state, completed, failed, expected
}
