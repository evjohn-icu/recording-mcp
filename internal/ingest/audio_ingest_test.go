package ingest

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"recordingmcp/internal/audioanalysis"
	"recordingmcp/internal/cloudsync"
	"recordingmcp/internal/memory"
	"recordingmcp/internal/process"
)

// The ingest-side audio tests pin what the ingest stage queues for audio
// analysis (plan 4.2): the audio window and input hash come from the decoded
// PCM, never from the container metadata; recordings whose PCM is shorter than
// one millisecond skip audio analysis and keep ASR; a replay of the ingest
// stage must not clobber the sync/DAG state an earlier attempt already
// committed; and the enqueue honours the effective claim stages. Every fixture
// is synthetic PCM.

// fixedPCMDecoder decodes nothing: it answers Probe with an explicit container
// report and writes an exact PCM body, so the container metadata can disagree
// with the real decoded sample count.
type fixedPCMDecoder struct {
	probe AudioInfo
	pcm   []byte
}

func (d fixedPCMDecoder) Probe(context.Context, string) (AudioInfo, error) { return d.probe, nil }

func (d fixedPCMDecoder) ToPCM(_ context.Context, _ string, dst string) error {
	return os.WriteFile(dst, d.pcm, 0o600)
}

// patternPCM returns deterministic 16 kHz mono s16le bytes for samples
// samples. The pattern is irregular enough that a hash over the wrong byte
// range never matches by accident.
func patternPCM(samples int) []byte {
	pcm := make([]byte, samples*2)
	for i := range pcm {
		pcm[i] = byte(i*7 + i>>9)
	}
	return pcm
}

func wavBytes(pcm []byte) []byte {
	header := make([]byte, 44)
	copy(header[0:], "RIFF")
	header[4] = byte(36 + len(pcm))
	copy(header[8:], "WAVEfmt ")
	copy(header[36:], "data")
	header[40] = byte(len(pcm))
	return append(header, pcm...)
}

// syntheticIngestAudioConfig resolves vad, tagging and diarization against
// fixed-hash synthetic model files.
func syntheticIngestAudioConfig(t *testing.T, base string) *audioanalysis.Config {
	t.Helper()
	capabilities := map[string]audioanalysis.CapabilityConfig{}
	for _, capability := range []string{audioanalysis.CapabilityVAD, audioanalysis.CapabilityTagging, audioanalysis.CapabilityDiarization} {
		data := []byte("synthetic-model-" + capability)
		path := filepath.Join("models", capability+".bin")
		if err := os.MkdirAll(filepath.Join(base, "models"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(base, path), data, 0o600); err != nil {
			t.Fatal(err)
		}
		capabilities[capability] = audioanalysis.CapabilityConfig{
			Enabled: true, ModelID: "synthetic-" + capability, ModelRevision: "rev-1",
			Files:  []audioanalysis.ModelFile{{Path: path, SHA256: sha256Hex(data)}},
			Device: "cpu", FrontendID: "synthetic",
		}
	}
	return &audioanalysis.Config{SchemaVersion: audioanalysis.ConfigSchema, BaseDir: base, Capabilities: capabilities}
}

// audioStageGate wires plan 4.2.4's effective-stage gate onto the importer.
// Config.AudioStages does not exist before the implementation wave, so the
// helper sets it by name and the gated tests skip with an explicit reason
// instead of breaking the package build. The recover-side counterpart of the
// same scenarios runs pre-fix in cmd/context-worker.
func audioStageGate(t *testing.T, cfg *Config, stages func(context.Context) (map[string]bool, error)) {
	t.Helper()
	field := reflect.ValueOf(cfg).Elem().FieldByName("AudioStages")
	want := reflect.TypeOf(stages)
	if !field.IsValid() || field.Type() != want || !field.CanSet() {
		t.Skipf("ingest.Config.AudioStages (plan 4.2.4) is not implemented yet; the claimer gate cannot be exercised pre-fix")
	}
	field.Set(reflect.ValueOf(stages))
}

// newGatedAudioEnv is newTestEnv for tests that need the gate wired before the
// importer is built. It returns the env plus the gate function so callers can
// flip it between attempts.
func newGatedAudioEnv(t *testing.T, decoder Decoder, audioCfg *audioanalysis.Config, gate func(context.Context) (map[string]bool, error)) *testEnv {
	t.Helper()
	dir := t.TempDir()
	audio := filepath.Join(dir, "audio")
	if err := os.MkdirAll(audio, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := memory.Open(filepath.Join(dir, "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	cfg := Config{
		Store: store, DB: store.DB(), AudioDir: audio,
		Decoder: decoder, Transcription: cloudsync.FixtureTranscriber{},
		ASRProvider: "fixture", TranscriptRevision: DefaultTranscriptRevision,
		Audio: audioCfg,
	}
	if gate != nil {
		audioStageGate(t, &cfg, gate)
	}
	svc, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return &testEnv{t: t, store: store, svc: svc, dir: dir, audio: audio}
}

// queuedAudioRequest decodes one audio job's canonical payload.
func queuedAudioRequest(t *testing.T, db *sql.DB, jobID string) (audioanalysis.Request, string) {
	t.Helper()
	var payload string
	if err := db.QueryRow(`SELECT payload_json FROM processing_jobs WHERE id=?`, jobID).Scan(&payload); err != nil {
		t.Fatalf("audio payload %s: %v", jobID, err)
	}
	var raw struct {
		Request    audioanalysis.Request `json:"request"`
		Generation string                `json:"generation"`
	}
	if err := json.Unmarshal([]byte(payload), &raw); err != nil {
		t.Fatalf("decode audio payload: %v", err)
	}
	return raw.Request, raw.Generation
}

func (e *testEnv) audioJobCount() int {
	return e.count(`SELECT COUNT(*) FROM processing_jobs WHERE stage LIKE 'audio\_%' ESCAPE '\'`)
}

func (e *testEnv) firstAudioJob(capability string) (string, bool) {
	var id string
	err := e.store.DB().QueryRow(`SELECT id FROM processing_jobs WHERE stage=?`, "audio_"+capability).Scan(&id)
	if err != nil {
		return "", false
	}
	return id, true
}

func (e *testEnv) syncState(providerID string) (state, transcription string) {
	e.t.Helper()
	if err := e.store.DB().QueryRow(`SELECT state,transcription_state FROM sync_recordings WHERE recording_id=?`, providerID).
		Scan(&state, &transcription); err != nil {
		e.t.Fatalf("sync state %s: %v", providerID, err)
	}
	return state, transcription
}

// TestIngestAudioWindowHashesDecodedPCMNotContainer pins plan 4.2.3 at enqueue
// time: a 16001-sample WAV whose container metadata claims 1001 ms must queue
// a 1000 ms window (floor of bytes/32) whose input hash covers exactly the
// first 32000 bytes — the bytes an adapter will actually read.
//
// Pre-fix the queued request takes the container duration and hashes the whole
// file including the ragged 16-bit tail, so the worker-side run can never
// reconcile window and hash.
func TestIngestAudioWindowHashesDecodedPCMNotContainer(t *testing.T) {
	pcm := patternPCM(16001) // 32002 bytes: one ragged 16-bit sample
	env := newTestEnv(t, func(c *Config) {
		c.Decoder = fixedPCMDecoder{probe: AudioInfo{DurationMS: 1001, Format: "wav", SampleRate: 16000, Channels: 1}, pcm: pcm}
		c.Audio = syntheticIngestAudioConfig(t, t.TempDir())
	})
	res, err := env.importFile("clip.wav", wavBytes(pcm))
	if err != nil {
		t.Fatal(err)
	}
	env.runIngest(env.job(res.JobID))

	for _, capability := range []string{audioanalysis.CapabilityVAD, audioanalysis.CapabilityTagging, audioanalysis.CapabilityDiarization} {
		jobID, ok := env.firstAudioJob(capability)
		if !ok {
			t.Fatalf("no %s job was queued", capability)
		}
		req, _ := queuedAudioRequest(t, env.store.DB(), jobID)
		if req.StartMS != 0 || req.EndMS != 1000 {
			t.Fatalf("pre-fix repro: %s window is [%d,%d], want [0,%d] derived from the decoded PCM instead of the container metadata",
				capability, req.StartMS, req.EndMS, len(pcm)/32)
		}
		wantInput := sha256Hex(pcm[:1000*32])
		if req.InputSHA256 != wantInput {
			t.Fatalf("pre-fix repro: %s input_sha256 %s does not hash the %d bytes an adapter reads (%s)",
				capability, req.InputSHA256, 1000*32, wantInput)
		}
		if req.SourceAudioSHA256 != sha256Hex(wavBytes(pcm)) {
			t.Fatalf("%s kept %s as source hash, want the upload hash", capability, req.SourceAudioSHA256)
		}
	}
}

// TestIngestSkipsAudioJobsForSubWindowPCM pins the zero-millisecond rule: a
// decoded PCM shorter than one millisecond reports audio_input_invalid for
// audio analysis, the ingest stage queues no audio job for it, and ASR still
// runs.
//
// Pre-fix the container duration produces a window that exceeds the file, so
// three audio jobs are queued and fail at runtime.
func TestIngestSkipsAudioJobsForSubWindowPCM(t *testing.T) {
	pcm := patternPCM(10) // 20 bytes: less than one millisecond
	env := newTestEnv(t, func(c *Config) {
		c.Decoder = fixedPCMDecoder{probe: AudioInfo{DurationMS: 50, Format: "wav", SampleRate: 16000, Channels: 1}, pcm: pcm}
		c.Audio = syntheticIngestAudioConfig(t, t.TempDir())
	})
	res, err := env.importFile("tiny.wav", wavBytes(pcm))
	if err != nil {
		t.Fatal(err)
	}
	env.runIngest(env.job(res.JobID))

	if got := env.audioJobCount(); got != 0 {
		t.Fatalf("pre-fix repro: %d audio jobs queued for a sub-millisecond PCM, want none", got)
	}
	asr := env.asrJobID(res.ConversationID)
	if _, err := env.svc.RunASRJob(context.Background(), env.job(asr)); err != nil {
		t.Fatalf("ASR after skipped audio: %v", err)
	}
}

// TestIngestAudioSourceFallsBackToWindowHash pins the legacy fallback: a
// payload whose source hash is not a legal digest still queues audio analysis
// with the window hash as source_audio_sha256, so old rows keep working.
func TestIngestAudioSourceFallsBackToWindowHash(t *testing.T) {
	pcm := patternPCM(16000) // exactly one whole window
	env := newTestEnv(t, func(c *Config) {
		c.Decoder = fixedPCMDecoder{probe: AudioInfo{DurationMS: 1000, Format: "wav", SampleRate: 16000, Channels: 1}, pcm: pcm}
		c.Audio = syntheticIngestAudioConfig(t, t.TempDir())
	})
	providerID := "import:legacyfallback0000000000000000"
	payload, err := json.Marshal(ingestPayload{
		ConversationID: memory.ConversationID(DeviceID, providerID), RecordingID: memory.RecordingID(DeviceID, providerID),
		ProviderRecordingID: providerID, ProviderConversationID: providerID,
		DeviceID: DeviceID, AudioPath: filepath.Join("imports", providerID+".wav"),
		SourceSHA256: "short-legacy-hash", DurationMS: 1000, FileName: "legacy.wav",
	})
	if err != nil {
		t.Fatal(err)
	}
	// The payload references the stored upload the way a real ingest job does,
	// and the legacy identity rows the FK guarantees expect.
	conv := memory.ConversationID(DeviceID, providerID)
	rec := memory.RecordingID(DeviceID, providerID)
	seed := func(query string, args ...any) {
		t.Helper()
		if _, err := env.store.DB().Exec(query, args...); err != nil {
			t.Fatalf("seed %q: %v", query, err)
		}
	}
	seed(`INSERT INTO conversations(id,title,summary,annotation_version,annotation_basis,recording_id,source_sha256,transcript_version,duration_ms,transcript_json,annotations_json)
 VALUES(?,'Legacy','','v1','',?,?, 'pending',1000,'{}','{}')`, conv, rec, "short-legacy-hash")
	seed(`INSERT INTO recordings(id,conversation_id,source_file,source_sha256,duration_ms,recording_started_at,absolute_time_known)
 VALUES(?,?,'legacy.wav','short-legacy-hash',1000,NULL,0)`, rec, conv)
	if err := os.WriteFile(filepath.Join(env.audio, "imports", providerID+".wav"), wavBytes(pcm), 0o600); err != nil {
		t.Fatal(err)
	}
	jobID, _, err := process.Enqueue(context.Background(), env.store.DB(), process.Spec{
		Stage: "ingest", InputSnapshotID: memory.ConversationID(DeviceID, providerID),
		IdempotencyKey: "ingest|" + providerID + "|legacy", PayloadJSON: string(payload), MaxAttempts: 3,
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	env.runIngest(env.job(jobID))

	jobID, ok := env.firstAudioJob(audioanalysis.CapabilityVAD)
	if !ok {
		t.Fatal("no VAD job for the legacy payload")
	}
	req, _ := queuedAudioRequest(t, env.store.DB(), jobID)
	if req.SourceAudioSHA256 != req.InputSHA256 || req.InputSHA256 != sha256Hex(pcm) {
		t.Fatalf("legacy source hash = %q input = %q, want the window hash %s as the existing fallback",
			req.SourceAudioSHA256, req.InputSHA256, sha256Hex(pcm))
	}
}

// TestIngestReplayAfterCommittedASRDoesNotClobberSync pins the plan-4.2.4
// replay guard: once the asr job of `asr|<providerRecordingID>|<sourceSHA>`
// exists, a replayed ingest attempt must return the original success reference
// and leave sync_recordings and the committed audio DAG untouched.
//
// Pre-fix the replay re-decodes, resets sync_recordings to
// downloaded/pending and re-runs the enqueue path, overwriting the state ASR
// already committed.
func TestIngestReplayAfterCommittedASRDoesNotClobberSync(t *testing.T) {
	pcm := patternPCM(16000)
	env := newTestEnv(t, func(c *Config) {
		c.Decoder = fixedPCMDecoder{probe: AudioInfo{DurationMS: 1000, Format: "wav", SampleRate: 16000, Channels: 1}, pcm: pcm}
		c.Audio = syntheticIngestAudioConfig(t, t.TempDir())
	})
	res, err := env.importFile("meeting.wav", wavBytes(pcm))
	if err != nil {
		t.Fatal(err)
	}
	env.runIngest(env.job(res.JobID))
	if _, err := env.svc.RunASRJob(context.Background(), env.job(env.asrJobID(res.ConversationID))); err != nil {
		t.Fatalf("asr: %v", err)
	}
	providerID := "import:" + sha256Hex(wavBytes(pcm))[:32]
	state, transcription := env.syncState(providerID)
	if state != "ingested" || transcription != "done" {
		t.Fatalf("precondition: sync = %s/%s", state, transcription)
	}
	beforeJobs := env.count(`SELECT COUNT(*) FROM processing_jobs`)
	beforeAttempts := env.count(`SELECT attempt_count FROM processing_jobs WHERE id=?`, env.asrJobID(res.ConversationID))

	replay, err := env.svc.RunIngestJob(context.Background(), env.job(res.JobID))
	if err != nil {
		t.Fatalf("replay must stay idempotent: %v", err)
	}
	if want := "ingested:" + res.ConversationID; replay != want {
		t.Fatalf("replay reference = %q, want %q", replay, want)
	}
	state, transcription = env.syncState(providerID)
	if state != "ingested" || transcription != "done" {
		t.Fatalf("pre-fix repro: replay clobbered committed sync state to %s/%s", state, transcription)
	}
	if after := env.count(`SELECT COUNT(*) FROM processing_jobs`); after != beforeJobs {
		t.Fatalf("replay changed the job count %d -> %d", beforeJobs, after)
	}
	if after := env.count(`SELECT attempt_count FROM processing_jobs WHERE id=?`, env.asrJobID(res.ConversationID)); after != beforeAttempts {
		t.Fatalf("replay re-attempted the asr job: %d -> %d", beforeAttempts, after)
	}
	if got := env.audioJobCount(); got != 3 {
		t.Fatalf("replay left %d audio jobs, want the original three", got)
	}
}

// TestIngestReplayWithForgottenSourceReturnsBarrier pins the second half of
// the replay guard: when the source recording has a forget tombstone, the
// replayed ingest attempt must return the original barrier error instead of
// treating the scrubbed job as a successful replay.
//
// Pre-fix the replay succeeds and rewrites the sync row.
func TestIngestReplayWithForgottenSourceReturnsBarrier(t *testing.T) {
	pcm := patternPCM(16000)
	env := newTestEnv(t, func(c *Config) {
		c.Decoder = fixedPCMDecoder{probe: AudioInfo{DurationMS: 1000, Format: "wav", SampleRate: 16000, Channels: 1}, pcm: pcm}
		c.Audio = syntheticIngestAudioConfig(t, t.TempDir())
	})
	res, err := env.importFile("forget.wav", wavBytes(pcm))
	if err != nil {
		t.Fatal(err)
	}
	env.runIngest(env.job(res.JobID))
	if _, err := env.svc.RunASRJob(context.Background(), env.job(env.asrJobID(res.ConversationID))); err != nil {
		t.Fatalf("asr: %v", err)
	}
	if _, err := env.store.DB().Exec(`INSERT INTO forget_tombstones(id,scope,recording_id,requested_id,locator_key,expected_state_hash,redaction_done,status,created_at)
 VALUES('forget:replay','recording',?,?,'locator:replay','hash',0,'pending_external_cleanup',1)`,
		res.RecordingID, res.RecordingID); err != nil {
		t.Fatal(err)
	}

	if _, err := env.svc.RunIngestJob(context.Background(), env.job(res.JobID)); err == nil {
		t.Fatal("pre-fix repro: replay of a forgotten source succeeded; it must return the original barrier error instead")
	}
}

// TestIngestAudioPlansFollowEffectiveClaimers pins the enqueue half of plan
// 4.2.4: audio jobs are only queued for capabilities a live worker claims.
// The worker's callback unions its own valid stages with the stages live
// workers heartbeat; a live heartbeat claiming audio_tagging keeps tagging in
// the union; diarization has no claimer and must not be queued.
func TestIngestAudioPlansFollowEffectiveClaimers(t *testing.T) {
	dir := t.TempDir()
	audioDir := filepath.Join(dir, "audio")
	if err := os.MkdirAll(audioDir, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := memory.Open(filepath.Join(dir, "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	db := store.DB()
	// The worker's wiring: its own verified stages united with live heartbeats.
	gate := func(ctx context.Context) (map[string]bool, error) {
		beats, err := process.ReadHeartbeats(ctx, db, time.Now(), 30*time.Second)
		if err != nil {
			return nil, err
		}
		valid := map[string]bool{"audio_vad": true}
		for _, beat := range beats {
			for _, stage := range beat.Stages {
				valid[stage] = true
			}
		}
		return valid, nil
	}
	cfg := Config{
		Store: store, DB: db, AudioDir: audioDir,
		Decoder:       fixedPCMDecoder{probe: AudioInfo{DurationMS: 1000, Format: "wav", SampleRate: 16000, Channels: 1}, pcm: patternPCM(16000)},
		Transcription: cloudsync.FixtureTranscriber{},
		ASRProvider:   "fixture", TranscriptRevision: DefaultTranscriptRevision,
		Audio: syntheticIngestAudioConfig(t, t.TempDir()), AudioStages: gate,
	}
	svc, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	env := &testEnv{t: t, store: store, svc: svc, dir: dir, audio: audioDir}
	// A second live worker claims tagging, so the union keeps it.
	if err := writeWorkerHeartbeat(t, db, "audio-worker", []string{"audio_tagging"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	res, err := env.importFile("union.wav", wavBytes(patternPCM(16000)))
	if err != nil {
		t.Fatal(err)
	}
	env.runIngest(env.job(res.JobID))

	if _, ok := env.firstAudioJob(audioanalysis.CapabilityVAD); !ok {
		t.Fatal("VAD has a claimer but no VAD job was queued")
	}
	if _, ok := env.firstAudioJob(audioanalysis.CapabilityTagging); !ok {
		t.Fatal("a live worker claims tagging but no tagging job was queued")
	}
	if _, ok := env.firstAudioJob(audioanalysis.CapabilityDiarization); ok {
		t.Fatal("diarization has no claimer but was queued anyway")
	}
}

// TestIngestAudioPlansSurviveSingleConnectionHeartbeatRead mirrors the real
// wiring: the gate callback reads worker heartbeats from the same
// SetMaxOpenConns(1) database inside RunIngestJob. The heartbeat read must
// happen outside the write transaction or the import deadlocks.
func TestIngestAudioPlansSurviveSingleConnectionHeartbeatRead(t *testing.T) {
	dir := t.TempDir()
	audioDir := filepath.Join(dir, "audio")
	if err := os.MkdirAll(audioDir, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := memory.Open(filepath.Join(dir, "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	db := store.DB()
	gate := func(ctx context.Context) (map[string]bool, error) {
		beats, err := process.ReadHeartbeats(ctx, db, time.Now(), 30*time.Second)
		if err != nil {
			return nil, err
		}
		stages := map[string]bool{"audio_vad": true}
		for _, beat := range beats {
			for _, stage := range beat.Stages {
				stages[stage] = true
			}
		}
		return stages, nil
	}
	cfg := Config{
		Store: store, DB: db, AudioDir: audioDir,
		Decoder:       fixedPCMDecoder{probe: AudioInfo{DurationMS: 1000, Format: "wav", SampleRate: 16000, Channels: 1}, pcm: patternPCM(16000)},
		Transcription: cloudsync.FixtureTranscriber{},
		ASRProvider:   "fixture", TranscriptRevision: DefaultTranscriptRevision,
		Audio: syntheticIngestAudioConfig(t, t.TempDir()),
	}
	audioStageGate(t, &cfg, gate)
	env := &testEnv{}
	svc, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	env.t, env.store, env.svc, env.dir, env.audio = t, store, svc, dir, audioDir
	if err := writeWorkerHeartbeat(t, db, "audio-worker", []string{"audio_tagging"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	res, err := env.importFile("beat.wav", wavBytes(patternPCM(16000)))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		job, getErr := process.Get(context.Background(), db, res.JobID)
		if getErr != nil {
			done <- getErr
			return
		}
		_, runErr := svc.RunIngestJob(context.Background(), *job)
		done <- runErr
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("heartbeat read inside RunIngestJob failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RunIngestJob deadlocked reading worker heartbeats on the single-connection database")
	}
	if _, ok := env.firstAudioJob(audioanalysis.CapabilityTagging); !ok {
		t.Fatal("live tagging claimer was ignored in the union")
	}
	if _, ok := env.firstAudioJob(audioanalysis.CapabilityDiarization); ok {
		t.Fatal("diarization has no claimer but was queued anyway")
	}
}

// TestIngestHeartbeatFailureReplaysIngest pins the retry half of plan 4.2.4:
// a heartbeat failure must fail the ingest attempt (so the queue retries it)
// and a replay after the heartbeat recovers enqueues the audio jobs exactly
// once. A replay whose asr job already exists must succeed without consulting
// the heartbeat at all.
//
// The gate field does not exist before the implementation wave; the test skips
// until then.
func TestIngestHeartbeatFailureReplaysIngest(t *testing.T) {
	var failBeat = true
	gate := func(context.Context) (map[string]bool, error) {
		if failBeat {
			return nil, fmt.Errorf("synthetic heartbeat outage")
		}
		return map[string]bool{"audio_vad": true, "audio_tagging": true, "audio_diarization": true}, nil
	}
	env := newGatedAudioEnv(t,
		fixedPCMDecoder{probe: AudioInfo{DurationMS: 1000, Format: "wav", SampleRate: 16000, Channels: 1}, pcm: patternPCM(16000)},
		syntheticIngestAudioConfig(t, t.TempDir()), gate)
	res, err := env.importFile("beatfail.wav", wavBytes(patternPCM(16000)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.svc.RunIngestJob(context.Background(), env.job(res.JobID)); err == nil {
		t.Fatal("heartbeat failure must fail the ingest attempt instead of queueing unclaimed audio jobs")
	} else if fatal, ok := err.(interface{ Fatal() bool }); ok && fatal.Fatal() {
		t.Fatalf("heartbeat failure must stay retryable, got fatal: %v", err)
	}
	if got := env.audioJobCount(); got != 0 {
		t.Fatalf("heartbeat failure left %d audio jobs queued", got)
	}

	failBeat = false
	if _, err := env.svc.RunIngestJob(context.Background(), env.job(res.JobID)); err != nil {
		t.Fatalf("replay after the heartbeat recovered: %v", err)
	}
	if got := env.audioJobCount(); got != 3 {
		t.Fatalf("replay queued %d audio jobs, want exactly the three claimable capabilities", got)
	}

	// A later replay with the heartbeat broken again must still succeed: the
	// existing asr job short-circuits before any heartbeat read.
	failBeat = true
	ref, err := env.svc.RunIngestJob(context.Background(), env.job(res.JobID))
	if err != nil {
		t.Fatalf("replay with an existing asr job must not depend on the heartbeat: %v", err)
	}
	if want := "ingested:" + res.ConversationID; ref != want {
		t.Fatalf("short-circuit replay reference = %q, want %q", ref, want)
	}
}

// TestIngestNilAudioGateKeepsExistingRules pins the compatibility rule: a nil
// AudioStages gate keeps the current behaviour of queueing every configured
// capability, so direct users of ingest.Service do not change.
func TestIngestNilAudioGateKeepsExistingRules(t *testing.T) {
	pcm := patternPCM(16000)
	env := newTestEnv(t, func(c *Config) {
		c.Decoder = fixedPCMDecoder{probe: AudioInfo{DurationMS: 1000, Format: "wav", SampleRate: 16000, Channels: 1}, pcm: pcm}
		c.Audio = syntheticIngestAudioConfig(t, t.TempDir())
	})
	res, err := env.importFile("legacy.wav", wavBytes(pcm))
	if err != nil {
		t.Fatal(err)
	}
	env.runIngest(env.job(res.JobID))
	if got := env.audioJobCount(); got != 3 {
		t.Fatalf("nil gate queued %d audio jobs, want the configured three", got)
	}
}

func writeWorkerHeartbeat(t *testing.T, db *sql.DB, workerID string, stages []string, now time.Time) error {
	t.Helper()
	return process.WriteHeartbeat(context.Background(), db, process.Heartbeat{WorkerID: workerID, Stages: stages}, now)
}
