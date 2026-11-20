package main

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"recordingmcp/internal/audioanalysis"
	"recordingmcp/internal/ingest"
	"recordingmcp/internal/memory"
	"recordingmcp/internal/process"
)

// The recover/lease-safety tests pin the plan-4.2.5 rules for audio stage
// recovery: a worker that does not run an audio stage must never terminate the
// pending or leased jobs of a stage a live worker still claims, recovery only
// fires when some live worker claims ingest/ASR (the dependents it unblocks),
// the disabled/unavailable code distinction stays, and FailUnclaimed keeps its
// semantics for jobs nobody owns.

// recoverFixture seeds one recording and the audio-analysis queue rows the
// recovery functions act on.
type recoverFixture struct {
	t           *testing.T
	db          *sql.DB
	audio       *audioanalysis.Store
	recordingID string
}

func newRecoverFixture(t *testing.T) *recoverFixture {
	t.Helper()
	store, err := memory.Open(t.TempDir() + "/memory.db")
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	db := store.DB()
	recordingID := memory.RecordingID("device-x", "provider-x")
	seed := func(query string, args ...any) {
		t.Helper()
		if _, err := db.Exec(query, args...); err != nil {
			t.Fatalf("seed %q: %v", query, err)
		}
	}
	seed(`INSERT INTO conversations(id,title,summary,annotation_version,annotation_basis,recording_id,source_sha256,transcript_version,duration_ms,transcript_json,annotations_json)
 VALUES(?,'Recovery','','v1','',?,?, 'tv',2000,'{}','{}')`,
		memory.ConversationID("device-x", "provider-x"), recordingID, "sha")
	seed(`INSERT INTO recordings(id,conversation_id,source_file,source_sha256,duration_ms,recording_started_at,absolute_time_known)
 VALUES(?,?,'rec.pcm','sha',2000,NULL,0)`, recordingID, memory.ConversationID("device-x", "provider-x"))
	return &recoverFixture{t: t, db: db, audio: audioanalysis.NewStore(db), recordingID: recordingID}
}

func (f *recoverFixture) enqueue(capability, generation string, deps []string) audioanalysis.EnqueueOutput {
	f.t.Helper()
	req := audioanalysis.Request{
		SchemaVersion:       audioanalysis.ContractSchema,
		RecordingID:         f.recordingID,
		SourceAudioSHA256:   strings.Repeat("a", 64),
		InputSHA256:         strings.Repeat("b", 64),
		ModelID:             "synthetic-" + capability,
		ModelRevision:       "rev-1",
		ModelFilesSHA256:    map[string]string{"model.bin": strings.Repeat("c", 64)},
		Capability:          capability,
		StartMS:             0,
		EndMS:               2000,
		ConfigSHA256:        strings.Repeat("d", 64),
		SourceAttachmentIDs: []string{},
	}
	req.RunID = audioanalysis.ComputeRunID(req, generation)
	out, err := f.audio.Enqueue(context.Background(), audioanalysis.EnqueueInput{
		RecordingID: f.recordingID, Capability: capability, Generation: generation,
		Request: req, Dependencies: deps, MaxAttempts: 3,
	})
	if err != nil {
		f.t.Fatalf("Enqueue %s/%s: %v", capability, generation, err)
	}
	return out
}

func (f *recoverFixture) enqueueASR(generation string, deps []string) string {
	f.t.Helper()
	jobID, _, err := process.Enqueue(context.Background(), f.db, process.Spec{
		Stage: "asr", InputSnapshotID: f.recordingID, IdempotencyKey: "asr|" + generation,
		PayloadJSON: "{}", Dependencies: deps, MaxAttempts: 3,
	}, time.Now())
	if err != nil {
		f.t.Fatalf("Enqueue asr: %v", err)
	}
	return jobID
}

func (f *recoverFixture) heartbeat(workerID string, stages []string, now time.Time) {
	f.t.Helper()
	if err := process.WriteHeartbeat(context.Background(), f.db, process.Heartbeat{WorkerID: workerID, Stages: stages}, now); err != nil {
		f.t.Fatalf("heartbeat %s: %v", workerID, err)
	}
}

func (f *recoverFixture) jobStatus(jobID string) string {
	f.t.Helper()
	var status string
	if err := f.db.QueryRow(`SELECT status FROM processing_jobs WHERE id=?`, jobID).Scan(&status); err != nil {
		f.t.Fatal(err)
	}
	return status
}

func (f *recoverFixture) runState(jobID string) (status, errorCode string) {
	f.t.Helper()
	var code sql.NullString
	if err := f.db.QueryRow(`SELECT status,error_code FROM audio_analysis_runs WHERE job_id=?`, jobID).Scan(&status, &code); err != nil {
		f.t.Fatal(err)
	}
	return status, code.String
}

func (f *recoverFixture) runningAttempts(jobID string) int {
	f.t.Helper()
	var n int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM processing_attempts WHERE job_id=? AND outcome='running'`, jobID).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func writeTestHeartbeat(t *testing.T, db *sql.DB, workerID string, stages []string, now time.Time) {
	t.Helper()
	if err := process.WriteHeartbeat(context.Background(), db, process.Heartbeat{WorkerID: workerID, Stages: stages}, now); err != nil {
		t.Fatalf("heartbeat %s: %v", workerID, err)
	}
}

// TestRecoverAudioStagesSkipsStagesWithLiveClaimers is the plan-4.2.5 cross
// worker rule: this worker runs ingest/ASR only, and a live worker claims
// audio_vad, so the pending VAD job must survive the recovery pass.
//
// Pre-fix the recovery fails every pending stage outside its own enabled list
// and kills the job the other worker is about to claim.
func TestRecoverAudioStagesSkipsStagesWithLiveClaimers(t *testing.T) {
	f := newRecoverFixture(t)
	vad := f.enqueue(audioanalysis.CapabilityVAD, "split-1", nil)
	now := time.Now()
	f.heartbeat("asr-worker", []string{"ingest", "asr"}, now)
	f.heartbeat("audio-worker", []string{"audio_vad"}, now)

	if err := recoverAudioStages(context.Background(), f.audio, []string{"ingest", "asr"}, map[string]string{}, now); err != nil {
		t.Fatalf("recoverAudioStages: %v", err)
	}
	if status := f.jobStatus(vad.JobID); status != "queued" {
		t.Fatalf("pre-fix repro: pending VAD job became %q although a live worker claims audio_vad", status)
	}
	if status, code := f.runState(vad.JobID); status != "running" || code != "" {
		t.Fatalf("pending VAD run = %s/%s after recovery", status, code)
	}
}

// TestRecoverAudioStagesWaitsForIngestClaimers pins the gate on the unblocking
// purpose of recovery: without any live ingest/ASR claimer there is no
// dependent to unblock, so a pending audio stage is left alone.
//
// Pre-fix the recovery fires regardless and fails the job.
func TestRecoverAudioStagesWaitsForIngestClaimers(t *testing.T) {
	f := newRecoverFixture(t)
	vad := f.enqueue(audioanalysis.CapabilityVAD, "gate-1", nil)
	now := time.Now()
	f.heartbeat("extract-worker", []string{"extract", "embed"}, now)

	if err := recoverAudioStages(context.Background(), f.audio, []string{"asr"}, map[string]string{}, now); err != nil {
		t.Fatalf("recoverAudioStages: %v", err)
	}
	if status := f.jobStatus(vad.JobID); status != "queued" {
		t.Fatalf("pre-fix repro: pending VAD job became %q although no live worker claims ingest/asr", status)
	}
}

// TestRecoverAudioStagesFailsLegacyVADForASRDependents pins the recovery that
// must keep working: with a live ASR claimer and no audio claimer at all, the
// legacy queued VAD job is terminated with audio_disabled so its ASR dependent
// can proceed; a configured-but-unavailable capability keeps its own code.
func TestRecoverAudioStagesFailsLegacyVADForASRDependents(t *testing.T) {
	f := newRecoverFixture(t)
	vad := f.enqueue(audioanalysis.CapabilityVAD, "legacy-1", nil)
	asrJob := f.enqueueASR("legacy-1", []string{vad.JobID})
	if job, err := process.Claim(context.Background(), f.db, process.ClaimOptions{WorkerID: "asr-worker", Stages: []string{"asr"}, Lease: time.Minute, ProcessorVersion: "test"}); err == nil {
		t.Fatalf("ASR claimed while its VAD parent was pending: %+v", job)
	}
	now := time.Now()
	f.heartbeat("asr-worker", []string{"ingest", "asr"}, now)

	if err := recoverAudioStages(context.Background(), f.audio, []string{"ingest", "asr"}, map[string]string{}, now); err != nil {
		t.Fatalf("recoverAudioStages: %v", err)
	}
	if status := f.jobStatus(vad.JobID); status != "failed" {
		t.Fatalf("legacy VAD job = %q, want failed", status)
	}
	if status, code := f.runState(vad.JobID); status != "failed" || code != audioanalysis.CodeDisabled {
		t.Fatalf("legacy VAD run = %s/%s, want failed/%s", status, code, audioanalysis.CodeDisabled)
	}
	claimed, err := process.Claim(context.Background(), f.db, process.ClaimOptions{WorkerID: "asr-worker", Stages: []string{"asr"}, Lease: time.Minute, ProcessorVersion: "test"})
	if err != nil || claimed.ID != asrJob {
		t.Fatalf("dependent ASR not released: %+v err=%v", claimed, err)
	}

	// A configured-but-unavailable capability reports its own reason instead of
	// the generic disabled code.
	tagging := f.enqueue(audioanalysis.CapabilityTagging, "legacy-2", nil)
	if err := recoverAudioStages(context.Background(), f.audio, []string{"ingest", "asr"},
		map[string]string{"audio_tagging": audioanalysis.CodeModelUnavailable}, now); err != nil {
		t.Fatalf("recoverAudioStages unavailable: %v", err)
	}
	if status, code := f.runState(tagging.JobID); status != "failed" || code != audioanalysis.CodeModelUnavailable {
		t.Fatalf("unavailable tagging run = %s/%s, want failed/%s", status, code, audioanalysis.CodeModelUnavailable)
	}
}

// TestFailUnclaimedSparesLiveWorkerLease pins the update-transaction rule: a
// running audio job whose lease expired is only terminated when no live worker
// still claims the stage.
func TestFailUnclaimedSparesLiveWorkerLease(t *testing.T) {
	t.Run("fresh_claimer", func(t *testing.T) {
		f := newRecoverFixture(t)
		vad := f.enqueue(audioanalysis.CapabilityVAD, "lease-1", nil)
		base := time.Now()
		claimed, err := process.Claim(context.Background(), f.db, process.ClaimOptions{
			WorkerID: "audio-worker", Stages: []string{"audio_vad"}, Lease: time.Millisecond, ProcessorVersion: "test", Now: base,
		})
		if err != nil || claimed.ID != vad.JobID {
			t.Fatalf("claim: %+v err=%v", claimed, err)
		}
		// The owner's heartbeat is fresh at check time; only the lease row is
		// behind (renewal lost or clock skew), not the worker.
		f.heartbeat("audio-worker", []string{"audio_vad"}, base.Add(25*time.Second))
		if err := f.audio.FailUnclaimed(context.Background(), "audio_vad", audioanalysis.CodeDisabled, base.Add(31*time.Second)); err != nil {
			t.Fatalf("FailUnclaimed: %v", err)
		}
		if status := f.jobStatus(vad.JobID); status != "running" {
			t.Fatalf("pre-fix repro: FailUnclaimed terminated a running job to %q while its owner still claims the stage", status)
		}
		if status, code := f.runState(vad.JobID); status != "running" || code != "" {
			t.Fatalf("leased run = %s/%s after FailUnclaimed", status, code)
		}
		if got := f.runningAttempts(vad.JobID); got != 1 {
			t.Fatalf("running attempts = %d, want the owner's attempt untouched", got)
		}
	})

	t.Run("stale_claimer", func(t *testing.T) {
		f := newRecoverFixture(t)
		vad := f.enqueue(audioanalysis.CapabilityVAD, "lease-2", nil)
		base := time.Now()
		if _, err := process.Claim(context.Background(), f.db, process.ClaimOptions{
			WorkerID: "audio-worker", Stages: []string{"audio_vad"}, Lease: time.Millisecond, ProcessorVersion: "test", Now: base,
		}); err != nil {
			t.Fatal(err)
		}
		f.heartbeat("audio-worker", []string{"audio_vad"}, base)
		if err := f.audio.FailUnclaimed(context.Background(), "audio_vad", audioanalysis.CodeDisabled, base.Add(31*time.Second)); err != nil {
			t.Fatalf("FailUnclaimed: %v", err)
		}
		if status, code := f.runState(vad.JobID); status != "failed" || code != audioanalysis.CodeDisabled {
			t.Fatalf("stale-claimer run = %s/%s, want failed/%s", status, code, audioanalysis.CodeDisabled)
		}
		if got := f.runningAttempts(vad.JobID); got != 0 {
			t.Fatalf("running attempts = %d, want the abandoned attempt closed", got)
		}
	})

	t.Run("no_claimer", func(t *testing.T) {
		f := newRecoverFixture(t)
		vad := f.enqueue(audioanalysis.CapabilityVAD, "lease-3", nil)
		base := time.Now()
		if _, err := process.Claim(context.Background(), f.db, process.ClaimOptions{
			WorkerID: "audio-worker", Stages: []string{"audio_vad"}, Lease: time.Millisecond, ProcessorVersion: "test", Now: base,
		}); err != nil {
			t.Fatal(err)
		}
		if err := f.audio.FailUnclaimed(context.Background(), "audio_vad", audioanalysis.CodeDisabled, base.Add(31*time.Second)); err != nil {
			t.Fatalf("FailUnclaimed: %v", err)
		}
		if status := f.jobStatus(vad.JobID); status != "failed" {
			t.Fatalf("unowned job = %q, want failed", status)
		}
	})

	t.Run("live_lease", func(t *testing.T) {
		f := newRecoverFixture(t)
		vad := f.enqueue(audioanalysis.CapabilityVAD, "lease-4", nil)
		base := time.Now()
		if _, err := process.Claim(context.Background(), f.db, process.ClaimOptions{
			WorkerID: "audio-worker", Stages: []string{"audio_vad"}, Lease: time.Minute, ProcessorVersion: "test", Now: base,
		}); err != nil {
			t.Fatal(err)
		}
		f.heartbeat("audio-worker", []string{"audio_vad"}, base)
		if err := f.audio.FailUnclaimed(context.Background(), "audio_vad", audioanalysis.CodeDisabled, base.Add(time.Second)); err != nil {
			t.Fatalf("FailUnclaimed: %v", err)
		}
		if status := f.jobStatus(vad.JobID); status != "running" {
			t.Fatalf("live-lease job = %q, want the original untouched semantics", status)
		}
	})
}

// TestSplitWorkersClaimVADAndASRWithoutCrossKill runs the plan's split-worker
// scenario end to end over a real import: the ASR worker's startup recovery
// must leave the pending VAD job to the audio worker, the audio worker finishes
// VAD, ASR follows once the dependency terminates, and diarization still reads
// the PCM after ASR.
//
// Pre-fix the ASR worker's recovery fails the pending VAD job, so the audio
// worker finds nothing to claim.
func TestSplitWorkersClaimVADAndASRWithoutCrossKill(t *testing.T) {
	env := newWorkerAudioEnv(t,
		sizedDecoder{probe: ingest.AudioInfo{DurationMS: 1000, Format: "wav", SampleRate: 16000, Channels: 1}, pcm: syntheticPCM(16000)},
		syntheticAudioConfig(t, t.TempDir()))
	res := env.importRecording("split.wav", syntheticWAV(syntheticPCM(16000)))
	vad := env.jobByStage("audio_vad", res.RecordingID)
	asr := env.jobByStage("asr", res.ConversationID)

	now := time.Now()
	writeTestHeartbeat(t, env.db, "asr-worker", []string{"ingest", "asr"}, now)
	writeTestHeartbeat(t, env.db, "audio-worker", []string{"audio_vad", "audio_tagging", "audio_diarization"}, now)

	// The ASR worker's own recovery pass.
	if err := recoverAudioStages(context.Background(), env.audio, []string{"ingest", "asr"}, map[string]string{}, now); err != nil {
		t.Fatalf("recoverAudioStages: %v", err)
	}
	if status := jobStatus(t, env.db, vad.ID); status != "queued" {
		t.Fatalf("pre-fix repro: the ASR worker's recovery moved the pending VAD job to %q", status)
	}

	vadAdapter := &hashingAnalyzer{eventType: audioanalysis.EventSpeechActivity}
	vadJob := claimStage(t, env.db, "audio-worker", "audio_vad")
	env.executeAudioStage(t, env.audioStageFor(audioanalysis.CapabilityVAD, vadAdapter), vadJob)
	if status, code, _, _ := runState(t, env.db, vad.ID); status != audioanalysis.StatusSucceeded || code != "" {
		t.Fatalf("VAD run = %s/%s after the audio worker claimed it", status, code)
	}

	asrJob := claimStage(t, env.db, "asr-worker", "asr")
	if asrJob.ID != asr.ID {
		t.Fatalf("claimed %s, want %s", asrJob.ID, asr.ID)
	}
	if _, err := (&ingestStage{svc: env.svc, stage: "asr"}).Execute(context.Background(), asrJob); err != nil {
		t.Fatalf("asr stage: %v", err)
	}
	if err := process.Complete(context.Background(), env.db, asrJob.ID, asrJob.LeaseToken, "asr:1", time.Now()); err != nil {
		t.Fatalf("complete asr: %v", err)
	}

	diarAdapter := &hashingAnalyzer{eventType: audioanalysis.EventSpeakerSegment}
	diarJob := claimStage(t, env.db, "audio-worker", "audio_diarization")
	env.executeAudioStage(t, env.audioStageFor(audioanalysis.CapabilityDiarization, diarAdapter), diarJob)
	if status, code, _, _ := runState(t, env.db, diarJob.ID); status != audioanalysis.StatusSucceeded || code != "" {
		t.Fatalf("diarization run = %s/%s after ASR terminal state", status, code)
	}
}
