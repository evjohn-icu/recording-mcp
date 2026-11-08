package ingest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"recordingmcp/internal/asrinput"
	"recordingmcp/internal/cloudsync"
	"recordingmcp/internal/memory"
	"recordingmcp/internal/process"
)

// fakeDecoder stands in for ffmpeg/ffprobe: Probe returns a configured value
// and ToPCM writes exactly durationMS*32 zero bytes (16 kHz mono s16le).
type fakeDecoder struct {
	info     AudioInfo
	probeErr error
	pcmErr   error
}

func (d *fakeDecoder) Probe(context.Context, string) (AudioInfo, error) {
	if d.probeErr != nil {
		return AudioInfo{}, d.probeErr
	}
	if d.info.DurationMS == 0 {
		d.info = AudioInfo{DurationMS: 3000, Format: "wav", SampleRate: 16000, Channels: 1}
	}
	return d.info, nil
}

func (d *fakeDecoder) ToPCM(_ context.Context, _ string, dst string) error {
	if d.pcmErr != nil {
		return d.pcmErr
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	return os.WriteFile(dst, make([]byte, d.info.DurationMS*32), 0o600)
}

// flakyTranscriber fails the first call with a retryable error and then
// behaves like the deterministic fixture transcriber.
type flakyTranscriber struct {
	failures int
	calls    int
}

func (f *flakyTranscriber) Transcribe(ctx context.Context, pcmPath string, meta cloudsync.MetaBasis) ([]asrinput.Result, error) {
	f.calls++
	if f.calls <= f.failures {
		return nil, errors.New("synthetic ASR outage")
	}
	return cloudsync.FixtureTranscriber{}.Transcribe(ctx, pcmPath, meta)
}

type testEnv struct {
	t      *testing.T
	store  *memory.Store
	svc    *Service
	dir    string
	audio  string
	dbPath string
}

func newTestEnv(t *testing.T, mutate func(*Config)) *testEnv {
	t.Helper()
	dir := t.TempDir()
	audio := filepath.Join(dir, "audio")
	if err := os.MkdirAll(audio, 0o700); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "memory.db")
	store, err := memory.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	decode := &fakeDecoder{}
	cfg := Config{
		Store: store, DB: store.DB(), AudioDir: audio,
		Decoder: decode, Transcription: cloudsync.FixtureTranscriber{},
		ASRProvider: "fixture", TranscriptRevision: "asr-v1",
	}
	if mutate != nil {
		mutate(&cfg)
	}
	if cfg.Decoder == nil {
		cfg.Decoder = decode
	}
	svc, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return &testEnv{t: t, store: store, svc: svc, dir: dir, audio: audio, dbPath: dbPath}
}

func (e *testEnv) importFile(name string, data []byte) (ImportResult, error) {
	return e.svc.Import(context.Background(), bytes.NewReader(data), name, int64(len(data)))
}

func (e *testEnv) job(id string) process.Job {
	e.t.Helper()
	job, err := process.Get(context.Background(), e.store.DB(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return *job
}

func (e *testEnv) runIngest(job process.Job) {
	e.t.Helper()
	if _, err := e.svc.RunIngestJob(context.Background(), job); err != nil {
		e.t.Fatalf("ingest job: %v", err)
	}
}

func (e *testEnv) runASR(job process.Job) error {
	e.t.Helper()
	_, err := e.svc.RunASRJob(context.Background(), job)
	return err
}

func (e *testEnv) count(query string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.store.DB().QueryRow(query, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *testEnv) asrJobID(conversationID string) string {
	e.t.Helper()
	var id string
	if err := e.store.DB().QueryRow(`SELECT id FROM processing_jobs WHERE stage='asr' AND input_snapshot_id=?`, conversationID).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func TestImportNormal(t *testing.T) {
	env := newTestEnv(t, nil)
	res, err := env.importFile("team sync.wav", []byte("wav-bytes"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Duplicate || res.ConversationID == "" || res.RecordingID == "" || res.JobID == "" {
		t.Fatalf("import result: %+v", res)
	}
	if res.Audio.DurationMS != 3000 || res.Audio.Format != "wav" {
		t.Fatalf("audio info: %+v", res.Audio)
	}
	env.runIngest(env.job(res.JobID))
	env.runASR(env.job(env.asrJobID(res.ConversationID)))

	if got := env.count(`SELECT COUNT(*) FROM conversations WHERE id=?`, res.ConversationID); got != 1 {
		t.Fatalf("conversations=%d", got)
	}
	if got := env.count(`SELECT COUNT(*) FROM source_attachments WHERE conversation_id=? AND active=1`, res.ConversationID); got != 1 {
		t.Fatalf("active attachments=%d", got)
	}
	if got := env.count(`SELECT COUNT(*) FROM turns WHERE conversation_id=?`, res.ConversationID); got == 0 {
		t.Fatal("no turns ingested")
	}
	providerID := "import:" + sha256Hex([]byte("wav-bytes"))[:32]
	var stitched string
	if err := env.store.DB().QueryRow(`SELECT stitched_path FROM sync_recordings WHERE recording_id=?`, providerID).Scan(&stitched); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stitched, env.audio+string(os.PathSeparator)) {
		t.Fatalf("stitched path %q outside audio root %q", stitched, env.audio)
	}
	if _, err := os.Stat(stitched); err != nil {
		t.Fatalf("stitched audio missing: %v", err)
	}
	var sourceFile string
	var sourceBytes, durationMS int64
	if err := env.store.DB().QueryRow(`SELECT source_file,source_bytes,duration_ms FROM recordings WHERE id=?`, res.RecordingID).Scan(&sourceFile, &sourceBytes, &durationMS); err != nil {
		t.Fatal(err)
	}
	if sourceFile != "team sync.wav" || sourceBytes != int64(len("wav-bytes")) || durationMS != 3000 {
		t.Fatalf("recording metadata: file=%q bytes=%d duration=%d", sourceFile, sourceBytes, durationMS)
	}
}

func TestImportDuplicateIsIdempotent(t *testing.T) {
	env := newTestEnv(t, nil)
	data := []byte("identical-bytes")
	first, err := env.importFile("a.wav", data)
	if err != nil {
		t.Fatal(err)
	}
	second, err := env.importFile("a.wav", data)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Duplicate || second.ConversationID != first.ConversationID || second.RecordingID != first.RecordingID || second.JobID != first.JobID {
		t.Fatalf("duplicate result=%+v first=%+v", second, first)
	}
	if got := env.count(`SELECT COUNT(*) FROM conversations`); got != 1 {
		t.Fatalf("conversations=%d", got)
	}
	if got := env.count(`SELECT COUNT(*) FROM recordings`); got != 1 {
		t.Fatalf("recordings=%d", got)
	}
	if got := env.count(`SELECT COUNT(*) FROM processing_jobs WHERE stage='ingest'`); got != 1 {
		t.Fatalf("ingest jobs=%d", got)
	}

	env.runIngest(env.job(first.JobID))
	env.runASR(env.job(env.asrJobID(first.ConversationID)))
	if got := env.count(`SELECT COUNT(*) FROM source_attachments WHERE conversation_id=? AND active=1`, first.ConversationID); got != 1 {
		t.Fatalf("active attachments after replay=%d", got)
	}
	if got := env.count(`SELECT COUNT(*) FROM turns WHERE conversation_id=?`, first.ConversationID); got != 1 {
		t.Fatalf("turns=%d", got)
	}
}

func TestImportRejectsUnsupportedFormat(t *testing.T) {
	env := newTestEnv(t, nil)
	_, err := env.importFile("notes.txt", []byte("hello"))
	var ierr *Error
	if !errors.As(err, &ierr) || ierr.Code != "unsupported_format" || ierr.Status != 415 || ierr.Action == "" {
		t.Fatalf("unsupported format error=%v", err)
	}
	if got := env.count(`SELECT COUNT(*) FROM conversations`); got != 0 {
		t.Fatalf("conversations=%d", got)
	}
	if got := env.count(`SELECT COUNT(*) FROM processing_jobs`); got != 0 {
		t.Fatalf("jobs=%d", got)
	}
}

func TestImportRejectsCorruptAudio(t *testing.T) {
	env := newTestEnv(t, func(cfg *Config) {
		cfg.Decoder = &fakeDecoder{probeErr: errors.New("ffprobe: no audio stream")}
	})
	_, err := env.importFile("broken.wav", []byte("not audio"))
	var ierr *Error
	if !errors.As(err, &ierr) || ierr.Code != "invalid_audio" || ierr.Status != 422 {
		t.Fatalf("invalid audio error=%v", err)
	}
	if got := env.count(`SELECT COUNT(*) FROM conversations`); got != 0 {
		t.Fatalf("conversations=%d", got)
	}
	if got := env.count(`SELECT COUNT(*) FROM processing_jobs`); got != 0 {
		t.Fatalf("jobs=%d", got)
	}
}

func TestImportSurvivesServerRestart(t *testing.T) {
	env := newTestEnv(t, nil)
	res, err := env.importFile("restart.wav", []byte("restart-bytes"))
	if err != nil {
		t.Fatal(err)
	}
	if err := env.store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := memory.Open(env.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	svc, err := New(Config{
		Store: store, DB: store.DB(), AudioDir: env.audio,
		Decoder: &fakeDecoder{}, Transcription: cloudsync.FixtureTranscriber{},
		ASRProvider: "fixture", TranscriptRevision: "asr-v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	job, err := process.Get(context.Background(), store.DB(), res.JobID)
	if err != nil {
		t.Fatalf("job lost across restart: %v", err)
	}
	if _, err := svc.RunIngestJob(context.Background(), *job); err != nil {
		t.Fatalf("ingest after restart: %v", err)
	}
	var asrID string
	if err := store.DB().QueryRow(`SELECT id FROM processing_jobs WHERE stage='asr' AND input_snapshot_id=?`, res.ConversationID).Scan(&asrID); err != nil {
		t.Fatal(err)
	}
	asrJob, err := process.Get(context.Background(), store.DB(), asrID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RunASRJob(context.Background(), *asrJob); err != nil {
		t.Fatalf("asr after restart: %v", err)
	}
}

func TestASRJobRecoversAfterWorkerLeaseExpiry(t *testing.T) {
	env := newTestEnv(t, nil)
	res, err := env.importFile("lease.wav", []byte("lease-bytes"))
	if err != nil {
		t.Fatal(err)
	}
	env.runIngest(env.job(res.JobID))

	ctx := context.Background()
	start := time.Now()
	first, err := process.Claim(ctx, env.store.DB(), process.ClaimOptions{
		WorkerID: "worker-a", Stages: []string{"asr"}, Lease: time.Second, Now: start,
	})
	if err != nil || first == nil {
		t.Fatalf("first claim: %v %v", first, err)
	}
	// The lease expires without a renewal; a second worker must recover it.
	second, err := process.Claim(ctx, env.store.DB(), process.ClaimOptions{
		WorkerID: "worker-b", Stages: []string{"asr"}, Lease: time.Minute, Now: start.Add(2 * time.Second),
	})
	if err != nil || second == nil {
		t.Fatalf("second claim: %v %v", second, err)
	}
	if second.ID != first.ID || second.WorkerID != "worker-b" {
		t.Fatalf("recovered job=%+v first=%+v", second, first)
	}
	if err := env.runASR(*second); err != nil {
		t.Fatal(err)
	}
	if got := env.count(`SELECT COUNT(*) FROM turns WHERE conversation_id=?`, res.ConversationID); got != 1 {
		t.Fatalf("turns=%d", got)
	}
}

func TestImportRejectedWhenASRUnavailable(t *testing.T) {
	env := newTestEnv(t, func(cfg *Config) {
		cfg.ASRProvider = "off"
		cfg.Transcription = nil
	})
	_, err := env.importFile("nope.wav", []byte("bytes"))
	var ierr *Error
	if !errors.As(err, &ierr) || ierr.Code != "asr_unavailable" || ierr.Status != 409 || ierr.Action == "" {
		t.Fatalf("asr unavailable error=%v", err)
	}
	if got := env.count(`SELECT COUNT(*) FROM conversations`); got != 0 {
		t.Fatalf("conversations=%d", got)
	}
	if got := env.count(`SELECT COUNT(*) FROM processing_jobs`); got != 0 {
		t.Fatalf("jobs=%d", got)
	}
}

func TestFailedASRJobRetryDoesNotDuplicateRecords(t *testing.T) {
	transcriber := &flakyTranscriber{failures: 1}
	env := newTestEnv(t, func(cfg *Config) { cfg.Transcription = transcriber })
	res, err := env.importFile("retry.wav", []byte("retry-bytes"))
	if err != nil {
		t.Fatal(err)
	}
	env.runIngest(env.job(res.JobID))
	asrID := env.asrJobID(res.ConversationID)

	ctx := context.Background()
	now := time.Now()
	job, err := process.Claim(ctx, env.store.DB(), process.ClaimOptions{WorkerID: "w", Stages: []string{"asr"}, Lease: time.Minute, Now: now})
	if err != nil || job == nil {
		t.Fatalf("claim: %v %v", job, err)
	}
	if err := env.runASR(*job); err == nil {
		t.Fatal("first ASR attempt unexpectedly succeeded")
	}
	if err := process.Fail(ctx, env.store.DB(), job.ID, job.LeaseToken, `{"error":"synthetic ASR outage"}`, true, now); err != nil {
		t.Fatal(err)
	}
	retried, err := process.RetryFailedForConversation(ctx, env.store.DB(), res.ConversationID, asrID, 1, "retry-1", now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if retried.Status != "queued" {
		t.Fatalf("retried status=%s", retried.Status)
	}
	again, err := process.Claim(ctx, env.store.DB(), process.ClaimOptions{WorkerID: "w", Stages: []string{"asr"}, Lease: time.Minute, Now: now.Add(2 * time.Second)})
	if err != nil || again == nil {
		t.Fatalf("second claim: %v %v", again, err)
	}
	if err := env.runASR(*again); err != nil {
		t.Fatalf("second ASR attempt: %v", err)
	}
	if err := process.Complete(ctx, env.store.DB(), again.ID, again.LeaseToken, "asr:1", now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	final, err := process.Get(ctx, env.store.DB(), asrID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != "succeeded" {
		t.Fatalf("final status=%s", final.Status)
	}
	if got := env.count(`SELECT COUNT(*) FROM source_attachments WHERE conversation_id=? AND active=1`, res.ConversationID); got != 1 {
		t.Fatalf("active attachments=%d", got)
	}
	if got := env.count(`SELECT COUNT(*) FROM turns WHERE conversation_id=?`, res.ConversationID); got != 1 {
		t.Fatalf("turns=%d", got)
	}
}

func TestImportDecodesRealFormatsWithFFmpeg(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg/ffprobe not installed")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffmpeg/ffprobe not installed")
	}
	env := newTestEnv(t, func(cfg *Config) {
		cfg.Decoder = &FFmpegDecoder{FFmpeg: ffmpeg, FFprobe: "ffprobe"}
	})
	for _, ext := range []string{"wav", "m4a", "mp3"} {
		path := filepath.Join(t.TempDir(), "tone."+ext)
		cmd := exec.Command(ffmpeg, "-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", "sine=frequency=440:duration=1", path)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("generate %s: %v %s", ext, err, out)
		}
		info, err := env.svc.Decoder().Probe(context.Background(), path)
		if err != nil {
			t.Fatalf("probe %s: %v", ext, err)
		}
		if info.DurationMS < 900 || info.DurationMS > 1200 {
			t.Fatalf("%s duration=%dms", ext, info.DurationMS)
		}
		pcm := filepath.Join(t.TempDir(), "out.pcm")
		if err := env.svc.Decoder().ToPCM(context.Background(), path, pcm); err != nil {
			t.Fatalf("decode %s: %v", ext, err)
		}
		st, err := os.Stat(pcm)
		if err != nil {
			t.Fatal(err)
		}
		if want := info.DurationMS * 32; st.Size() < want/2 || st.Size() > want*3/2 {
			t.Fatalf("%s pcm bytes=%d expected about %d", ext, st.Size(), want)
		}
	}
}

// sha256Hex is shared by tests that need the stable provider id.
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

var _ = sql.ErrNoRows
var _ = json.Marshal
var _ = io.Discard
var _ = fmt.Sprintf
