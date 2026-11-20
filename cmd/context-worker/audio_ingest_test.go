package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"recordingmcp/internal/audioanalysis"
	"recordingmcp/internal/cloudsync"
	"recordingmcp/internal/ingest"
	"recordingmcp/internal/memory"
	"recordingmcp/internal/process"
	"recordingmcp/internal/worker"
)

// The audio ingest tests pin the worker half of the audio source-resolution
// contract (plan 4.2): a real local-import ingest state — recordings row with
// the upload hash, one sync_recordings row, and no ASR attachment yet — must be
// readable by the audio worker, the opened path must stay inside the audio
// root, and VAD must run before ASR while diarization still reads the PCM
// after ASR reached its terminal state. Every fixture is synthetic PCM.

// sizedDecoder stands in for ffmpeg/ffprobe with an explicit probe answer and
// an exact decoded PCM body, so container metadata can disagree with the real
// decoded sample count the way lossy containers do.
type sizedDecoder struct {
	probe ingest.AudioInfo
	pcm   []byte
}

func (d sizedDecoder) Probe(context.Context, string) (ingest.AudioInfo, error) {
	return d.probe, nil
}

func (d sizedDecoder) ToPCM(_ context.Context, _ string, dst string) error {
	return os.WriteFile(dst, d.pcm, 0o600)
}

// syntheticPCM returns deterministic 16 kHz mono s16le bytes for samples
// samples. The pattern is irregular enough that a hash over the wrong byte
// range never matches by accident.
func syntheticPCM(samples int) []byte {
	pcm := make([]byte, samples*2)
	for i := range pcm {
		pcm[i] = byte(i*7 + i>>9)
	}
	return pcm
}

// syntheticWAV wraps samples of PCM in a canonical 44-byte RIFF header so the
// upload is a real WAV document.
func syntheticWAV(pcm []byte) []byte {
	header := make([]byte, 44)
	copy(header[0:], "RIFF")
	putLE32(header[4:], uint32(36+len(pcm)))
	copy(header[8:], "WAVEfmt ")
	putLE32(header[16:], 16)
	putLE16(header[20:], 1) // PCM
	putLE16(header[22:], 1) // mono
	putLE32(header[24:], 16000)
	putLE32(header[28:], 32000)
	putLE16(header[32:], 2)
	putLE16(header[34:], 16)
	copy(header[36:], "data")
	putLE32(header[40:], uint32(len(pcm)))
	return append(header, pcm...)
}

func putLE16(dst []byte, v uint16) { dst[0] = byte(v); dst[1] = byte(v >> 8) }
func putLE32(dst []byte, v uint32) {
	dst[0], dst[1], dst[2], dst[3] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24)
}

func sha256HexBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// syntheticAudioConfig builds an audio-analysis config whose vad, tagging and
// diarization capabilities resolve against fixed-hash synthetic model files.
func syntheticAudioConfig(t *testing.T, base string) *audioanalysis.Config {
	t.Helper()
	capabilities := map[string]audioanalysis.CapabilityConfig{}
	for _, capability := range []string{audioanalysis.CapabilityVAD, audioanalysis.CapabilityTagging, audioanalysis.CapabilityDiarization} {
		data := []byte("synthetic-model-" + capability)
		sum := sha256.Sum256(data)
		path := filepath.Join("models", capability+".bin")
		if err := os.MkdirAll(filepath.Join(base, "models"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(base, path), data, 0o600); err != nil {
			t.Fatal(err)
		}
		capabilities[capability] = audioanalysis.CapabilityConfig{
			Enabled:       true,
			ModelID:       "synthetic-" + capability,
			ModelRevision: "rev-1",
			Files:         []audioanalysis.ModelFile{{Path: path, SHA256: hex.EncodeToString(sum[:])}},
			Device:        "cpu",
			FrontendID:    "synthetic",
		}
	}
	return &audioanalysis.Config{
		SchemaVersion: audioanalysis.ConfigSchema,
		BaseDir:       base,
		Capabilities:  capabilities,
	}
}

// workerAudioEnv is one worker-side ingest state: a real database plus the
// managed audio directory the resolver opens.
type workerAudioEnv struct {
	t        *testing.T
	db       *sql.DB
	svc      *ingest.Service
	audioDir string
	resolver *recordingAudioResolver
	audio    *audioanalysis.Store
}

func newWorkerAudioEnv(t *testing.T, decoder ingest.Decoder, audioCfg *audioanalysis.Config) *workerAudioEnv {
	t.Helper()
	dir := t.TempDir()
	audioDir := filepath.Join(dir, "audio")
	if err := os.MkdirAll(filepath.Join(audioDir, "imports"), 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := memory.Open(filepath.Join(dir, "memory.db"))
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	db := store.DB()
	svc, err := ingest.New(ingest.Config{
		Store: store, DB: db, AudioDir: audioDir,
		Decoder:       decoder,
		Transcription: cloudsync.FixtureTranscriber{},
		ASRProvider:   "fixture", TranscriptRevision: ingest.DefaultTranscriptRevision,
		Audio: audioCfg,
	})
	if err != nil {
		t.Fatalf("ingest.New: %v", err)
	}
	return &workerAudioEnv{
		t: t, db: db, svc: svc, audioDir: audioDir,
		resolver: &recordingAudioResolver{db: db, audioRoot: audioDir},
		audio:    audioanalysis.NewStore(db),
	}
}

// importRecording runs the real Import plus the ingest stage so the state is
// the one production produces: pending recording rows, the decoded PCM and the
// queued ingest/asr/audio jobs.
func (e *workerAudioEnv) importRecording(name string, data []byte) ingest.ImportResult {
	e.t.Helper()
	res, err := e.svc.Import(context.Background(), bytes.NewReader(data), name, int64(len(data)))
	if err != nil {
		e.t.Fatalf("Import: %v", err)
	}
	if res.Duplicate {
		e.t.Fatalf("Import reported a duplicate for fresh bytes: %+v", res)
	}
	e.runIngest(res.JobID)
	return res
}

func (e *workerAudioEnv) runIngest(jobID string) {
	e.t.Helper()
	job, err := process.Get(context.Background(), e.db, jobID)
	if err != nil {
		e.t.Fatalf("process.Get(ingest job): %v", err)
	}
	if _, err := e.svc.RunIngestJob(context.Background(), *job); err != nil {
		e.t.Fatalf("ingest stage: %v", err)
	}
}

// jobByStage loads one job of a stage. Audio stages snapshot the memory
// recording id; ingest and asr snapshot the conversation id.
func (e *workerAudioEnv) jobByStage(stage, snapshotID string) process.Job {
	e.t.Helper()
	var id string
	if err := e.db.QueryRow(`SELECT id FROM processing_jobs WHERE stage=? AND input_snapshot_id=?`, stage, snapshotID).Scan(&id); err != nil {
		e.t.Fatalf("job %s/%s: %v", stage, snapshotID, err)
	}
	job, err := process.Get(context.Background(), e.db, id)
	if err != nil {
		e.t.Fatalf("process.Get(%s): %v", id, err)
	}
	return *job
}

// audioJobIDs lists the audio-analysis jobs of one recording.
func (e *workerAudioEnv) audioJobIDs(recordingID string) []string {
	e.t.Helper()
	rows, err := e.db.Query(`SELECT id FROM processing_jobs WHERE stage LIKE 'audio\_%' ESCAPE '\' AND input_snapshot_id=? ORDER BY id`, recordingID)
	if err != nil {
		e.t.Fatalf("audio jobs: %v", err)
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			e.t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

func (e *workerAudioEnv) dependencyIDs(jobID string) []string {
	e.t.Helper()
	rows, err := e.db.Query(`SELECT dependency_id FROM processing_job_dependencies WHERE job_id=? ORDER BY dependency_id`, jobID)
	if err != nil {
		e.t.Fatalf("dependencies: %v", err)
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			e.t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

func claimStage(t *testing.T, db *sql.DB, workerID string, stages ...string) process.Job {
	t.Helper()
	job, err := process.Claim(context.Background(), db, process.ClaimOptions{
		WorkerID: workerID, Stages: stages, Lease: time.Minute, ProcessorVersion: "test",
	})
	if err != nil {
		t.Fatalf("claim %v: %v", stages, err)
	}
	return *job
}

func jobStatus(t *testing.T, db *sql.DB, jobID string) string {
	t.Helper()
	var status string
	if err := db.QueryRow(`SELECT status FROM processing_jobs WHERE id=?`, jobID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

func runState(t *testing.T, db *sql.DB, jobID string) (status, errorCode, inputSHA string, endMS int64) {
	t.Helper()
	var code sql.NullString
	if err := db.QueryRow(`SELECT status,error_code,input_sha256,end_ms FROM audio_analysis_runs WHERE job_id=?`, jobID).
		Scan(&status, &code, &inputSHA, &endMS); err != nil {
		t.Fatalf("audio run for job %s: %v", jobID, err)
	}
	return status, code.String, inputSHA, endMS
}

func asrAttempts(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM processing_attempts a JOIN processing_jobs j ON j.id=a.job_id WHERE j.stage='asr'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// audioStageFor mirrors the audioStage that buildAudioStages wires, with the
// subprocess adapter replaced by the synthetic analyzer under test.
func (e *workerAudioEnv) audioStageFor(capability string, analyzer audioanalysis.Analyzer) *audioStage {
	return &audioStage{
		capability: capability,
		runner: &audioanalysis.Runner{
			Store:    e.audio,
			Analyzer: analyzer,
			Resolver: e.resolver,
		},
	}
}

// hashingAnalyzer is the synthetic adapter: it consumes the exact PCM window
// the runner hands it, remembers what it read and returns one fixed event so
// the commit succeeds. The recorded digest and length prove the adapter input.
type hashingAnalyzer struct {
	eventType string

	mu     sync.Mutex
	digest string
	length int64
}

func (a *hashingAnalyzer) Analyze(_ context.Context, req audioanalysis.Request, pcm io.Reader) (audioanalysis.Result, error) {
	data, err := io.ReadAll(pcm)
	if err != nil {
		return audioanalysis.Result{}, err
	}
	a.mu.Lock()
	a.digest = sha256HexBytes(data)
	a.length = int64(len(data))
	a.mu.Unlock()
	return audioanalysis.Result{
		SchemaVersion:     audioanalysis.ContractSchema,
		RunID:             req.RunID,
		RecordingID:       req.RecordingID,
		SourceAudioSHA256: req.SourceAudioSHA256, InputSHA256: req.InputSHA256,
		ModelID: req.ModelID, ModelRevision: req.ModelRevision,
		ModelFilesSHA256: req.ModelFilesSHA256, Capability: req.Capability,
		StartMS: req.StartMS, EndMS: req.EndMS, ConfigSHA256: req.ConfigSHA256,
		SourceAttachmentIDs:  req.SourceAttachmentIDs,
		SourceSnapshotSHA256: req.SourceSnapshotSHA256, Status: audioanalysis.StatusSucceeded,
		Events: []audioanalysis.Event{{
			ID: "ev-" + req.Capability, EventType: a.eventType,
			StartMS: 0, EndMS: 100, Payload: map[string]any{"state": "synthetic"},
		}},
		Provenance: audioanalysis.Provenance{AdapterVersion: "synthetic", FrontendID: "test", Device: "cpu", InputSHA256: req.InputSHA256},
	}, nil
}

func (a *hashingAnalyzer) read() (string, int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.digest, a.length
}

func (e *workerAudioEnv) executeAudioStage(t *testing.T, stage *audioStage, job process.Job) {
	t.Helper()
	if _, err := stage.Execute(context.Background(), job); !errors.Is(err, worker.ErrFinalized) {
		t.Fatalf("audio stage %s returned %v, want ErrFinalized", stage.capability, err)
	}
}

// TestAudioWorkerRunsVADBeforeASRAndDiarizationAfterASR is the plan-4.2
// sequence: with a real ingest state and two fixed-hash synthetic adapters the
// VAD attempt succeeds before ASR starts, ASR commits its attachments, and
// diarization still reads the PCM after ASR reached its terminal state.
//
// Pre-fix the first audio attempt cannot resolve the PCM at all: the resolver
// only looked through source_attachments, which the ASR stage writes later, so
// the VAD job dies with audio_model_unavailable before ASR ever runs.
func TestAudioWorkerRunsVADBeforeASRAndDiarizationAfterASR(t *testing.T) {
	env := newWorkerAudioEnv(t,
		sizedDecoder{probe: ingest.AudioInfo{DurationMS: 1000, Format: "wav", SampleRate: 16000, Channels: 1}, pcm: syntheticPCM(16000)},
		syntheticAudioConfig(t, t.TempDir()))
	res := env.importRecording("meeting.wav", syntheticWAV(syntheticPCM(16000)))

	vad := env.jobByStage("audio_vad", res.RecordingID)
	asr := env.jobByStage("asr", res.ConversationID)
	diarization := env.jobByStage("audio_diarization", res.RecordingID)
	if got := env.dependencyIDs(asr.ID); len(got) != 1 || got[0] != vad.ID {
		t.Fatalf("asr dependencies = %v, want [%s]", got, vad.ID)
	}
	if got := env.dependencyIDs(diarization.ID); len(got) != 1 || got[0] != asr.ID {
		t.Fatalf("diarization dependencies = %v, want [%s]", got, asr.ID)
	}
	for _, root := range []string{vad.ID, env.jobByStage("audio_tagging", res.RecordingID).ID} {
		if got := env.dependencyIDs(root); len(got) != 0 {
			t.Fatalf("acoustic roots have parents: %v", got)
		}
	}

	vadAdapter := &hashingAnalyzer{eventType: audioanalysis.EventSpeechActivity}
	vadJob := claimStage(t, env.db, "audio-worker", "audio_vad")
	if vadJob.ID != vad.ID {
		t.Fatalf("claimed %s, want %s", vadJob.ID, vad.ID)
	}
	env.executeAudioStage(t, env.audioStageFor(audioanalysis.CapabilityVAD, vadAdapter), vadJob)

	status, code, _, _ := runState(t, env.db, vad.ID)
	if status != audioanalysis.StatusSucceeded || code != "" {
		t.Fatalf("pre-fix repro: VAD run = %s/%s; the resolver could not read the imported PCM before ASR wrote attachments", status, code)
	}
	if got := jobStatus(t, env.db, vad.ID); got != "succeeded" {
		t.Fatalf("VAD job status = %q, want succeeded", got)
	}
	if got := asrAttempts(t, env.db); got != 0 {
		t.Fatalf("ASR started before VAD finished: %d attempts", got)
	}

	// ASR runs afterwards and reaches its terminal state through the real
	// queue finalization.
	asrJob := claimStage(t, env.db, "asr-worker", "asr")
	if asrJob.ID != asr.ID {
		t.Fatalf("claimed %s, want %s", asrJob.ID, asr.ID)
	}
	stage := &ingestStage{svc: env.svc, stage: "asr"}
	if _, err := stage.Execute(context.Background(), asrJob); err != nil {
		t.Fatalf("asr stage: %v", err)
	}
	if err := process.Complete(context.Background(), env.db, asrJob.ID, asrJob.LeaseToken, "asr:1", time.Now()); err != nil {
		t.Fatalf("complete asr: %v", err)
	}
	if got := jobStatus(t, env.db, asr.ID); got != "succeeded" {
		t.Fatalf("ASR job status = %q, want succeeded", got)
	}

	// Diarization is claimable only after ASR terminated and still reads the
	// same PCM window.
	diarAdapter := &hashingAnalyzer{eventType: audioanalysis.EventSpeakerSegment}
	diarJob := claimStage(t, env.db, "audio-worker", "audio_diarization")
	if diarJob.ID != diarization.ID {
		t.Fatalf("claimed %s, want %s", diarJob.ID, diarization.ID)
	}
	env.executeAudioStage(t, env.audioStageFor(audioanalysis.CapabilityDiarization, diarAdapter), diarJob)
	status, code, inputSHA, endMS := runState(t, env.db, diarization.ID)
	if status != audioanalysis.StatusSucceeded || code != "" {
		t.Fatalf("diarization run after ASR terminal state = %s/%s", status, code)
	}
	if endMS != 1000 {
		t.Fatalf("diarization window end_ms = %d, want 1000", endMS)
	}
	for name, adapter := range map[string]*hashingAnalyzer{"vad": vadAdapter, "diarization": diarAdapter} {
		digest, length := adapter.read()
		if length != 32000 {
			t.Fatalf("%s adapter read %d bytes, want the full 32000-byte window", name, length)
		}
		if digest != inputSHA {
			t.Fatalf("%s adapter digest %s does not match the run input %s", name, digest, inputSHA)
		}
	}
	if got := env.countRows(`SELECT COUNT(*) FROM source_attachments WHERE conversation_id=? AND active=1`, res.ConversationID); got == 0 {
		t.Fatal("ASR wrote no active attachments")
	}
}

func (e *workerAudioEnv) countRows(query string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRow(query, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// TestAudioWorkerAdapterSeesExactWindowOf16001SamplePCM covers plan 4.2.3: a
// 16001-sample WAV whose container metadata claims a different duration must
// produce a window of floor(bytes/32) milliseconds and an adapter input hash
// over exactly the bytes the adapter reads.
//
// Pre-fix the queued request hashes the whole decoded file and takes the
// container duration, so the resolver refuses the window that exceeds the file
// and the job dies with audio_input_invalid.
func TestAudioWorkerAdapterSeesExactWindowOf16001SamplePCM(t *testing.T) {
	pcm := syntheticPCM(16001) // 32002 bytes: one ragged 16-bit sample
	env := newWorkerAudioEnv(t,
		sizedDecoder{probe: ingest.AudioInfo{DurationMS: 1001, Format: "wav", SampleRate: 16000, Channels: 1}, pcm: pcm},
		syntheticAudioConfig(t, t.TempDir()))
	res := env.importRecording("clip.wav", syntheticWAV(pcm))

	vad := env.jobByStage("audio_vad", res.RecordingID)
	adapter := &hashingAnalyzer{eventType: audioanalysis.EventSpeechActivity}
	job := claimStage(t, env.db, "audio-worker", "audio_vad")
	if job.ID != vad.ID {
		t.Fatalf("claimed %s, want %s", job.ID, vad.ID)
	}
	env.executeAudioStage(t, env.audioStageFor(audioanalysis.CapabilityVAD, adapter), job)

	status, code, inputSHA, endMS := runState(t, env.db, vad.ID)
	if status != audioanalysis.StatusSucceeded || code != "" {
		t.Fatalf("pre-fix repro: VAD run = %s/%s; the 16001-sample window did not decode, hash and resolve as one unit", status, code)
	}
	if endMS != 1000 {
		t.Fatalf("run window end_ms = %d, want floor(32002/32)=1000 from the decoded PCM, not the container metadata", endMS)
	}
	digest, length := adapter.read()
	if length != 32000 {
		t.Fatalf("adapter read %d bytes, want exactly end_ms*32=32000", length)
	}
	if digest != inputSHA {
		t.Fatalf("adapter digest %s does not match the queued input_sha256 %s", digest, inputSHA)
	}
}

// TestAudioResolverServesImportedPCMWithoutASRAttachment is the plan-4.2 "no
// attachment" state: after Import and the ingest stage the recording has a
// sync_recordings row but ASR has not written any attachment yet, and the
// resolver must still serve the managed PCM and the upload hash.
//
// Pre-fix the resolver only walks source_attachments and reports "no managed
// audio for this recording", so no audio stage can run before ASR.
func TestAudioResolverServesImportedPCMWithoutASRAttachment(t *testing.T) {
	pcm := syntheticPCM(16000)
	env := newWorkerAudioEnv(t,
		sizedDecoder{probe: ingest.AudioInfo{DurationMS: 1000, Format: "wav", SampleRate: 16000, Channels: 1}, pcm: pcm},
		nil)
	res := env.importRecording("talk.wav", syntheticWAV(pcm))
	if got := env.audioJobIDs(res.RecordingID); len(got) != 0 {
		t.Fatalf("audio jobs without audio config: %v", got)
	}
	if got := env.countRows(`SELECT COUNT(*) FROM source_attachments WHERE recording_id=?`, res.RecordingID); got != 0 {
		t.Fatalf("ASR attachments exist in the pre-ASR state: %d", got)
	}
	reader, err := env.resolver.RecordingWindow(context.Background(), res.RecordingID, 0, 1000)
	if err != nil {
		t.Fatalf("pre-fix repro: RecordingWindow without any ASR attachment: %v", err)
	}
	defer reader.Close()
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, pcm) {
		t.Fatalf("window bytes differ: got %d bytes, want the exact %d-byte decoded PCM", len(got), len(pcm))
	}
	source, err := env.resolver.SourceAudioSHA256(context.Background(), res.RecordingID)
	if err != nil {
		t.Fatalf("SourceAudioSHA256: %v", err)
	}
	if want := sha256HexBytes(syntheticWAV(pcm)); source != want {
		t.Fatalf("source hash = %s, want the upload hash %s", source, want)
	}
}

// TestAudioResolverKeepsRealPathInsideAudioRoot pins the os.Root half of the
// contract: the real file the resolver opens must stay inside the audio root
// even when the recorded stitched path travels through a symlink planted in
// the managed tree.
//
// Pre-fix the resolver opens the absolute stitched path directly, so the
// intermediate symlink silently reads a file outside the root.
func TestAudioResolverKeepsRealPathInsideAudioRoot(t *testing.T) {
	pcm := syntheticPCM(16000)
	dir := t.TempDir()
	audioDir := filepath.Join(dir, "audio")
	if err := os.MkdirAll(filepath.Join(audioDir, "imports"), 0o700); err != nil {
		t.Fatal(err)
	}
	upload := syntheticWAV(pcm)
	uploadSHA := sha256HexBytes(upload)
	providerID := "import:" + uploadSHA[:32]
	recordingID := memory.RecordingID(ingest.DeviceID, providerID)

	outside := filepath.Join(dir, "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	escaped := filepath.Join(outside, "rec.pcm")
	if err := os.WriteFile(escaped, pcm, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(audioDir, "imports", "escape")); err != nil {
		t.Fatal(err)
	}

	store, err := memory.Open(filepath.Join(dir, "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	db := store.DB()
	seed := func(query string, args ...any) {
		t.Helper()
		if _, err := db.Exec(query, args...); err != nil {
			t.Fatalf("seed %q: %v", query, err)
		}
	}
	seed(`INSERT INTO conversations(id,title,summary,annotation_version,annotation_basis,recording_id,source_sha256,transcript_version,duration_ms,transcript_json,annotations_json)
 VALUES(?,'Escaped','','v1','',?,?,'pending',1000,'{}','{}')`,
		memory.ConversationID(ingest.DeviceID, providerID), recordingID, uploadSHA)
	seed(`INSERT INTO recordings(id,conversation_id,source_file,source_sha256,source_bytes,duration_ms,recording_started_at,absolute_time_known)
 VALUES(?,?,'clip.wav',?,?,32002,NULL,0)`, recordingID, memory.ConversationID(ingest.DeviceID, providerID), uploadSHA, int64(len(upload)))
	seed(`INSERT INTO sync_recordings(recording_id,owner,device_id,state,total_chunks,total_samples,stitched_path,updated_at)
 VALUES(?,'local-import',?,'downloaded',1,16000,?,1)`,
		providerID, ingest.DeviceID, filepath.Join(audioDir, "imports", "escape", "rec.pcm"))

	resolver := &recordingAudioResolver{db: db, audioRoot: audioDir}
	if _, err := resolver.RecordingWindow(context.Background(), recordingID, 0, 1000); err == nil {
		t.Fatal("pre-fix repro: the resolver opened a real file outside the audio root through an intermediate symlink")
	}
}

// TestAudioResolverRejectsAmbiguousManagedSources covers the fallback path for
// recordings without import provenance: when the active attachments resolve to
// different managed files the resolver must refuse instead of picking one.
//
// Pre-fix the query takes the first row and happily serves one of the files.
func TestAudioResolverRejectsAmbiguousManagedSources(t *testing.T) {
	dir := t.TempDir()
	audioDir := filepath.Join(dir, "audio")
	if err := os.MkdirAll(filepath.Join(audioDir, "imports"), 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := memory.Open(filepath.Join(dir, "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	db := store.DB()

	device := "device-x"
	recordingID := memory.RecordingID(device, "provider-a")
	seed := func(query string, args ...any) {
		t.Helper()
		if _, err := db.Exec(query, args...); err != nil {
			t.Fatalf("seed %q: %v", query, err)
		}
	}
	seed(`INSERT INTO conversations(id,title,summary,annotation_version,annotation_basis,recording_id,source_sha256,transcript_version,duration_ms,transcript_json,annotations_json)
 VALUES(?,'Ambiguous','','v1','',?,?,'tv',1000,'{}','{}')`,
		memory.ConversationID(device, "provider-a"), recordingID, "short")
	seed(`INSERT INTO recordings(id,conversation_id,source_file,source_sha256,duration_ms,recording_started_at,absolute_time_known)
 VALUES(?,?,'rec.pcm','short',1000,NULL,0)`, recordingID, memory.ConversationID(device, "provider-a"))
	fileA, fileB := syntheticPCM(500), syntheticPCM(501)
	for _, spec := range []struct {
		provider, device string
		pcm              []byte
	}{
		{"provider-a", device, fileA},
		{"provider-b", device, fileB},
	} {
		path := filepath.Join(audioDir, "imports", spec.provider+".pcm")
		if err := os.WriteFile(path, spec.pcm, 0o600); err != nil {
			t.Fatal(err)
		}
		seed(`INSERT INTO sync_recordings(recording_id,owner,device_id,state,stitched_path,updated_at)
 VALUES(?,'cloud',?,'ingested',?,1)`, spec.provider, spec.device, path)
		seed(`INSERT INTO source_attachments(id,conversation_id,recording_id,segment_index,segment_id,transcript_revision,source_json,result_json,content_sha256,active,supersedes_revision)
 VALUES(?,?,?,0,?,'tv',?,'{}','h',1,NULL)`,
			"att-"+spec.provider, memory.ConversationID(device, "provider-a"), recordingID,
			"seg-"+spec.provider,
			fmt.Sprintf(`{"device_id":%q,"recording_id":%q}`, spec.device, spec.provider))
	}

	resolver := &recordingAudioResolver{db: db, audioRoot: audioDir}
	if _, err := resolver.RecordingWindow(context.Background(), recordingID, 0, 500); err == nil {
		t.Fatal("pre-fix repro: two active attachments resolving to different managed files did not raise a multi-source ambiguity")
	}
}

// TestAudioResolverPinsProvenanceConstraints locks the rejection cases the
// resolver must keep after the implementation merge: a deleted sync row, an
// escaping path, a forged provider identity, a missing file and an empty
// stitched path all stay unreadable. These hold before and after the change;
// the state uses the attachment fallback path (no import provenance).
func TestAudioResolverPinsProvenanceConstraints(t *testing.T) {
	dir := t.TempDir()
	audioDir := filepath.Join(dir, "audio")
	if err := os.MkdirAll(filepath.Join(audioDir, "imports"), 0o700); err != nil {
		t.Fatal(err)
	}
	pcm := syntheticPCM(500)
	if err := os.WriteFile(filepath.Join(audioDir, "imports", "provider-a.pcm"), pcm, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "outside.pcm"), pcm, 0o600); err != nil {
		t.Fatal(err)
	}

	for name, tc := range map[string]struct {
		syncState   string
		stitched    string
		sourceJSON  string
		recordingID func() string
	}{
		"deleted_sync_row": {syncState: "deleted", stitched: filepath.Join(audioDir, "imports", "provider-a.pcm"),
			sourceJSON: `{"device_id":"device-x","recording_id":"provider-a"}`, recordingID: func() string { return memory.RecordingID("device-x", "provider-a") }},
		"escape_path": {syncState: "ingested", stitched: filepath.Join(dir, "outside.pcm"),
			sourceJSON: `{"device_id":"device-x","recording_id":"provider-a"}`, recordingID: func() string { return memory.RecordingID("device-x", "provider-a") }},
		"forged_identity": {syncState: "ingested", stitched: filepath.Join(audioDir, "imports", "provider-a.pcm"),
			sourceJSON: `{"device_id":"device-z","recording_id":"provider-a"}`, recordingID: func() string { return memory.RecordingID("device-x", "provider-a") }},
		"missing_file": {syncState: "ingested", stitched: filepath.Join(audioDir, "imports", "gone.pcm"),
			sourceJSON: `{"device_id":"device-x","recording_id":"provider-a"}`, recordingID: func() string { return memory.RecordingID("device-x", "provider-a") }},
		"empty_path": {syncState: "ingested", stitched: "",
			sourceJSON: `{"device_id":"device-x","recording_id":"provider-a"}`, recordingID: func() string { return memory.RecordingID("device-x", "provider-a") }},
	} {
		t.Run(name, func(t *testing.T) {
			store, err := memory.Open(filepath.Join(t.TempDir(), "memory.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			db := store.DB()
			recordingID := tc.recordingID()
			seed := func(query string, args ...any) {
				t.Helper()
				if _, err := db.Exec(query, args...); err != nil {
					t.Fatalf("seed %q: %v", query, err)
				}
			}
			seed(`INSERT INTO conversations(id,title,summary,annotation_version,annotation_basis,recording_id,source_sha256,transcript_version,duration_ms,transcript_json,annotations_json)
 VALUES(?,'Pinned','','v1','',?,?,'tv',500,'{}','{}')`,
				memory.ConversationID("device-x", "provider-a"), recordingID, "short")
			seed(`INSERT INTO recordings(id,conversation_id,source_file,source_sha256,duration_ms,recording_started_at,absolute_time_known)
 VALUES(?,?,'rec.pcm','short',500,NULL,0)`, recordingID, memory.ConversationID("device-x", "provider-a"))
			seed(`INSERT INTO sync_recordings(recording_id,owner,device_id,state,stitched_path,updated_at)
 VALUES('provider-a','cloud','device-x',?,?,1)`, tc.syncState, tc.stitched)
			seed(`INSERT INTO source_attachments(id,conversation_id,recording_id,segment_index,segment_id,transcript_revision,source_json,result_json,content_sha256,active,supersedes_revision)
 VALUES('att-1',?, ?,0,'seg','tv',?,'{}','h',1,NULL)`,
				memory.ConversationID("device-x", "provider-a"), recordingID, tc.sourceJSON)

			resolver := &recordingAudioResolver{db: db, audioRoot: audioDir}
			if _, err := resolver.RecordingWindow(context.Background(), recordingID, 0, 500); err == nil {
				t.Fatalf("%s: the resolver served audio it must refuse", name)
			}
		})
	}
}
