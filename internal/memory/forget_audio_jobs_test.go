package memory_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"recordingmcp/internal/audioanalysis"
	"recordingmcp/internal/memory"
	"recordingmcp/internal/process"
)

// forgetAudioFixture seeds one conversation with two recordings (target and
// sibling). Every recording has an active attachment, an attachment-turn link,
// a sync ledger row and a managed PCM file, mirroring a real ingested
// conversation. Audio-analysis state is created through the real
// audioanalysis queue so the forget redaction is verified against the actual
// job/run shape, including jobs whose run row no longer exists.
type forgetAudioFixture struct {
	t            *testing.T
	store        *memory.Store
	db           *sql.DB
	audio        *audioanalysis.Store
	dir          string
	audioDir     string
	conversation string
	target       string
	sibling      string
	pcm          []byte
	pcmHash      string
}

func newForgetAudioFixture(t *testing.T) *forgetAudioFixture {
	t.Helper()
	dir := t.TempDir()
	store, err := memory.Open(filepath.Join(dir, "memory.db"))
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	db := store.DB()
	audioDir := filepath.Join(dir, "audio")
	if err := os.MkdirAll(audioDir, 0o700); err != nil {
		t.Fatal(err)
	}
	pcm := make([]byte, 2000*32)
	for i := range pcm {
		pcm[i] = byte(i % 251)
	}
	sum := sha256.Sum256(pcm)
	hash := hex.EncodeToString(sum[:])
	f := &forgetAudioFixture{t: t, store: store, db: db, audio: audioanalysis.NewStore(db),
		dir: dir, audioDir: audioDir, conversation: "conv:forget-audio",
		target: "rec:forget-target", sibling: "rec:forget-sibling", pcm: pcm, pcmHash: hash}
	for _, id := range []string{f.target, f.sibling} {
		pcmPath := filepath.Join(audioDir, id+".pcm")
		if err := os.WriteFile(pcmPath, pcm, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.Exec(query, args...); err != nil {
			t.Fatalf("exec %q: %v", query, err)
		}
	}
	exec(`INSERT INTO conversations(id,title,summary,annotation_version,annotation_basis,recording_id,source_sha256,transcript_version,duration_ms,transcript_json,annotations_json)
		 VALUES(?,'ForgetAudio','','v1','',?,'sha','tv',2000,'{}','{}')`, f.conversation, f.target)
	attachments := map[string]string{f.target: "att:target", f.sibling: "att:sibling"}
	providers := map[string]string{f.target: "prov-target", f.sibling: "prov-sibling"}
	turns := map[string]string{f.target: "turn:t-1", f.sibling: "turn:s-1"}
	for _, id := range []string{f.target, f.sibling} {
		exec(`INSERT INTO recordings(id,conversation_id,source_file,source_sha256,duration_ms,recording_started_at,absolute_time_known)
			 VALUES(?,?,?,?,2000,NULL,0)`, id, f.conversation, id+".pcm", hash)
		exec(`INSERT INTO turns(id,conversation_id,recording_id,segment_index,global_start_ms,global_end_ms,speaker_scoped_id,speaker_entity_id,content,source_sha256,transcript_version,original_turn_id,turn_json)
			 VALUES(?,?,?,0,0,2000,'speaker:host',NULL,'转写内容',?,'rev-1',?,'{}')`, turns[id], f.conversation, id, hash, turns[id])
		exec(`INSERT INTO source_attachments(id,conversation_id,recording_id,segment_index,segment_id,transcript_revision,source_json,result_json,content_sha256,active,supersedes_revision)
			 VALUES(?,?,?,0,'seg:1','rev-1',?, '{}','hash',1,NULL)`,
			attachments[id], f.conversation, id,
			`{"device_id":"dev-1","recording_id":"`+providers[id]+`"}`)
		exec(`INSERT INTO attachment_turns(attachment_id,turn_id) VALUES(?,?)`, attachments[id], turns[id])
		// 2000 ms of 16 kHz s16le PCM.
		exec(`INSERT INTO sync_recordings(recording_id,owner,device_id,state,total_samples,stitched_path,transcript_revision,transcription_state,updated_at)
			 VALUES(?,'default','dev-1','ingested',64000,?,'asr-v1','done',1)`, providers[id], filepath.Join(audioDir, id+".pcm"))
	}
	return f
}

func (f *forgetAudioFixture) request(recordingID, capability, generation string) audioanalysis.Request {
	f.t.Helper()
	req := audioanalysis.Request{
		SchemaVersion:       audioanalysis.ContractSchema,
		RecordingID:         recordingID,
		SourceAudioSHA256:   f.pcmHash,
		InputSHA256:         f.pcmHash,
		ModelID:             "model",
		ModelRevision:       "rev-1",
		ModelFilesSHA256:    map[string]string{"model.onnx": strings.Repeat("a", 64)},
		Capability:          capability,
		StartMS:             0,
		EndMS:               2000,
		ConfigSHA256:        strings.Repeat("b", 64),
		SourceAttachmentIDs: []string{},
	}
	req.RunID = audioanalysis.ComputeRunID(req, generation)
	return req
}

func (f *forgetAudioFixture) enqueue(recordingID, capability, generation string, dependencies []string) audioanalysis.EnqueueOutput {
	f.t.Helper()
	out, err := f.audio.Enqueue(context.Background(), audioanalysis.EnqueueInput{
		RecordingID: recordingID, Capability: capability, Generation: generation,
		Request:      f.request(recordingID, capability, generation),
		Dependencies: dependencies, MaxAttempts: 3,
	})
	if err != nil {
		f.t.Fatalf("enqueue %s/%s: %v", recordingID, capability, err)
	}
	return out
}

func (f *forgetAudioFixture) claim(stage, jobID string) process.Job {
	f.t.Helper()
	job, err := process.Claim(context.Background(), f.db, process.ClaimOptions{
		WorkerID: "worker-forget", Stages: []string{stage}, Lease: time.Minute, ProcessorVersion: "test",
	})
	if err != nil {
		f.t.Fatalf("claim %s: %v", stage, err)
	}
	if job.ID != jobID {
		f.t.Fatalf("claimed %s, want %s", job.ID, jobID)
	}
	return *job
}

// complete finalises one claimed audio job with the real commit path.
func (f *forgetAudioFixture) complete(job process.Job, req audioanalysis.Request, eventID string, embeddings map[string][]float32) {
	f.t.Helper()
	result := audioanalysis.Result{
		SchemaVersion: audioanalysis.ContractSchema, RunID: req.RunID, RecordingID: req.RecordingID,
		SourceAudioSHA256: req.SourceAudioSHA256, InputSHA256: req.InputSHA256,
		ModelID: req.ModelID, ModelRevision: req.ModelRevision,
		ModelFilesSHA256: req.ModelFilesSHA256, Capability: req.Capability,
		StartMS: req.StartMS, EndMS: req.EndMS, ConfigSHA256: req.ConfigSHA256,
		SourceAttachmentIDs: req.SourceAttachmentIDs, SourceSnapshotSHA256: req.SourceSnapshotSHA256,
		Status: audioanalysis.StatusSucceeded,
		Provenance: audioanalysis.Provenance{
			AdapterVersion: "test", FrontendID: "test", Device: "cpu",
			InputSHA256: req.InputSHA256, ModelFilesSHA256: req.ModelFilesSHA256, SourceSnapshotSHA256: req.SourceSnapshotSHA256,
		},
	}
	if eventID != "" {
		result.Events = []audioanalysis.Event{{
			ID: eventID, EventType: audioanalysis.EventSpeechActivity, StartMS: 0, EndMS: 1000,
			Payload: map[string]any{"origin": "test"},
		}}
	}
	if _, err := f.audio.Commit(context.Background(), audioanalysis.CommitInput{
		JobID: job.ID, LeaseToken: job.LeaseToken, Result: result, Embeddings: embeddings, Now: time.Now(),
	}); err != nil {
		f.t.Fatalf("commit %s: %v", job.ID, err)
	}
}

func forgetAudioCount(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}

// forgetAudioSeedJobs fills the target and sibling recordings with the job
// shapes the purge must handle: a completed run, a failed job with attempt and
// retry state, a dangling legacy audio job with no run row, and sibling jobs
// in every state.
func forgetAudioSeedJobs(t *testing.T, f *forgetAudioFixture) (targetJobs, siblingJobs []string, asrJobID string) {
	t.Helper()
	ctx := context.Background()
	targetVAD := f.enqueue(f.target, audioanalysis.CapabilityVAD, "gen-1", nil)
	targetVADJob := f.claim("audio_vad", targetVAD.JobID)
	f.complete(targetVADJob, f.request(f.target, audioanalysis.CapabilityVAD, "gen-1"), "ev:target-1", nil)

	targetTag := f.enqueue(f.target, audioanalysis.CapabilityTagging, "gen-1", nil)
	targetTagJob := f.claim("audio_tagging", targetTag.JobID)
	if err := f.audio.Fail(ctx, targetTag.JobID, targetTagJob.LeaseToken, audioanalysis.CodeDependencyUnavailable, false, time.Now()); err != nil {
		t.Fatalf("fail target tagging: %v", err)
	}
	nowMS := time.Now().UnixMilli()
	if _, err := f.db.Exec(`INSERT INTO processing_retry_requests(request_id,job_id,expected_attempt,created_at) VALUES('rr-forget-audio',?,0,?)`,
		targetTag.JobID, nowMS); err != nil {
		t.Fatal(err)
	}
	siblingVAD := f.enqueue(f.sibling, audioanalysis.CapabilityVAD, "gen-1", nil)
	siblingVADJob := f.claim("audio_vad", siblingVAD.JobID)
	f.complete(siblingVADJob, f.request(f.sibling, audioanalysis.CapabilityVAD, "gen-1"), "ev:sibling-1",
		map[string][]float32{"ev:sibling-1": {0.25, -0.5, 0.75, 1}})
	siblingTag := f.enqueue(f.sibling, audioanalysis.CapabilityTagging, "gen-1", nil) // stays queued

	// A legacy audio job that has no run row at all. It is inserted after every
	// claim so the queue never hands it to the worker during setup.
	dangling := "job:forget-audio-dangling"
	if _, err := f.db.Exec(`INSERT INTO processing_jobs(id,stage,input_snapshot_id,idempotency_key,status,payload_json,max_attempts,attempt_count,available_at,created_at,updated_at)
		 VALUES(?,?,?,?,?,?,3,0,?,?,?)`, dangling, "audio_vad", f.target,
		"audio|"+f.target+"|vad|legacy", "queued", "{}", nowMS, nowMS, nowMS); err != nil {
		t.Fatal(err)
	}

	// A non-audio job of the conversation that depends on the forgotten audio
	// job: the edge must go, the job itself keeps the scrub treatment.
	asrID, _, err := process.Enqueue(ctx, f.db, process.Spec{
		Stage: "asr", InputSnapshotID: f.conversation, IdempotencyKey: "asr-forget-audio",
		PayloadJSON:  `{"conversation_id":"` + f.conversation + `"}`,
		Dependencies: []string{targetVAD.JobID},
	}, time.Now())
	if err != nil {
		t.Fatalf("enqueue asr: %v", err)
	}

	// Preconditions so later failures stay attributable to the forget.
	if got := forgetAudioCount(t, f.db, `SELECT COUNT(*) FROM processing_jobs WHERE stage LIKE 'audio\_%' ESCAPE '\' AND input_snapshot_id=?`, f.target); got != 3 {
		t.Fatalf("target audio jobs before forget = %d, want 3", got)
	}
	var active int
	if err := f.db.QueryRow(`SELECT active FROM audio_analysis_runs WHERE id=?`, siblingVAD.RunID).Scan(&active); err != nil || active != 1 {
		t.Fatalf("sibling run not active before forget: active=%d err=%v", active, err)
	}
	return []string{targetVAD.JobID, targetTag.JobID, dangling},
		[]string{siblingVAD.JobID, siblingTag.JobID}, asrID
}

func TestForgetRecordingPurgesAudioJobsResultsAndKeepsSibling(t *testing.T) {
	ctx := context.Background()
	f := newForgetAudioFixture(t)
	targetJobs, siblingJobs, asrJobID := forgetAudioSeedJobs(t, f)

	req := memory.ForgetRequest{Scope: "recording", RecordingID: f.target}
	preview, err := f.store.PreviewForget(ctx, req)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	req.ExpectedState = preview.ExpectedState
	result, err := f.store.CommitForget(ctx, req)
	if err != nil {
		t.Fatalf("commit forget: %v", err)
	}
	if !result.RedactionDone {
		t.Fatalf("redaction not done: %+v", result)
	}

	// Every trace of the forgotten recording's audio jobs is gone: job rows,
	// attempts, retry requests, dependency edges, run references and heads.
	for _, jobID := range targetJobs {
		for _, probe := range []struct{ label, sql string }{
			{"processing_jobs", `SELECT COUNT(*) FROM processing_jobs WHERE id=?`},
			{"processing_attempts", `SELECT COUNT(*) FROM processing_attempts WHERE job_id=?`},
			{"processing_retry_requests", `SELECT COUNT(*) FROM processing_retry_requests WHERE job_id=?`},
			{"audio_analysis_runs", `SELECT COUNT(*) FROM audio_analysis_runs WHERE job_id=?`},
			{"audio_analysis_heads", `SELECT COUNT(*) FROM audio_analysis_heads WHERE desired_job_id=?`},
			{"dependencies_as_child", `SELECT COUNT(*) FROM processing_job_dependencies WHERE job_id=?`},
			{"dependencies_as_parent", `SELECT COUNT(*) FROM processing_job_dependencies WHERE dependency_id=?`},
		} {
			if got := forgetAudioCount(t, f.db, probe.sql, jobID); got != 0 {
				t.Fatalf("forgotten audio job %s still present in %s: %d rows", jobID, probe.label, got)
			}
		}
	}
	if got := forgetAudioCount(t, f.db, `SELECT COUNT(*) FROM processing_jobs WHERE stage LIKE 'audio\_%' ESCAPE '\' AND input_snapshot_id=?`, f.target); got != 0 {
		t.Fatalf("target audio job rows survived forget: %d", got)
	}
	for _, probe := range []struct{ label, sql string }{
		{"runs", `SELECT COUNT(*) FROM audio_analysis_runs WHERE recording_id=?`},
		{"events", `SELECT COUNT(*) FROM audio_analysis_events WHERE run_id IN (SELECT id FROM audio_analysis_runs WHERE recording_id=?)`},
		{"heads", `SELECT COUNT(*) FROM audio_analysis_heads WHERE recording_id=?`},
		{"speaker_embeddings", `SELECT COUNT(*) FROM audio_speaker_embeddings WHERE event_id IN (
		   SELECT e.id FROM audio_analysis_events e JOIN audio_analysis_runs r ON r.id=e.run_id WHERE r.recording_id=?)`},
	} {
		if got := forgetAudioCount(t, f.db, probe.sql, f.target); got != 0 {
			t.Fatalf("forgotten recording still has audio analysis %s: %d rows", probe.label, got)
		}
	}
	if runs, err := f.audio.CurrentRuns(ctx, f.target); err != nil || len(runs) != 0 {
		t.Fatalf("CurrentRuns of forgotten recording = %v, %v", runs, err)
	}

	// The sibling recording keeps its complete job, its current run, events,
	// speaker vector and queued work untouched.
	for _, jobID := range siblingJobs {
		var status, payload string
		if err := f.db.QueryRow(`SELECT status,payload_json FROM processing_jobs WHERE id=?`, jobID).Scan(&status, &payload); err != nil {
			t.Fatalf("sibling job %s vanished: %v", jobID, err)
		}
		if payload == "{}" {
			t.Fatalf("sibling job %s payload was scrubbed: %s", jobID, status)
		}
	}
	var status string
	var active int
	if err := f.db.QueryRow(`SELECT status,active FROM audio_analysis_runs WHERE recording_id=? AND capability=?`, f.sibling, audioanalysis.CapabilityVAD).Scan(&status, &active); err != nil {
		t.Fatalf("sibling run vanished: %v", err)
	}
	if status != audioanalysis.StatusSucceeded || active != 1 {
		t.Fatalf("sibling run state = %s/%d, want succeeded/1", status, active)
	}
	if got := forgetAudioCount(t, f.db, `SELECT COUNT(*) FROM audio_analysis_events e JOIN audio_analysis_runs r ON r.id=e.run_id WHERE r.recording_id=?`, f.sibling); got != 1 {
		t.Fatalf("sibling events = %d, want 1", got)
	}
	if got := forgetAudioCount(t, f.db, `SELECT COUNT(*) FROM audio_speaker_embeddings v JOIN audio_analysis_events e ON e.id=v.event_id JOIN audio_analysis_runs r ON r.id=e.run_id WHERE r.recording_id=?`, f.sibling); got != 1 {
		t.Fatalf("sibling speaker embedding = %d, want 1", got)
	}
	var turns int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM turns WHERE recording_id=?`, f.sibling).Scan(&turns); err != nil || turns != 1 {
		t.Fatalf("sibling turns = %d err=%v, want 1", turns, err)
	}
	var syncState string
	var stitched sql.NullString
	if err := f.db.QueryRow(`SELECT state,stitched_path FROM sync_recordings WHERE recording_id=?`, "prov-sibling").Scan(&syncState, &stitched); err != nil {
		t.Fatalf("sibling sync row vanished: %v", err)
	}
	if syncState != "ingested" || !stitched.Valid || stitched.String == "" {
		t.Fatalf("sibling sync row changed: state=%s stitched=%v", syncState, stitched)
	}
	if err := f.db.QueryRow(`SELECT state FROM sync_recordings WHERE recording_id=?`, "prov-target").Scan(&syncState); err != nil || syncState != "deleted" {
		t.Fatalf("forgotten sync row = %q err=%v, want deleted", syncState, err)
	}

	// Non-audio jobs keep the legacy scrub treatment: the asr job of the
	// conversation is cancelled with an empty payload and its edge to the
	// deleted audio job is gone.
	job, err := process.Get(ctx, f.db, asrJobID)
	if err != nil {
		t.Fatalf("asr job vanished: %v", err)
	}
	if job.Status != "cancelled" || job.PayloadJSON != "{}" {
		t.Fatalf("asr job = %s/%q, want cancelled/{}", job.Status, job.PayloadJSON)
	}
	if got := forgetAudioCount(t, f.db, `SELECT COUNT(*) FROM processing_job_dependencies WHERE job_id=?`, asrJobID); got != 0 {
		t.Fatalf("dependency edge to deleted audio job survived: %d", got)
	}
}

func TestForgetTurnPurgesWholeRecordingAudioJobsWithoutTouchingSibling(t *testing.T) {
	ctx := context.Background()
	f := newForgetAudioFixture(t)
	targetVAD := f.enqueue(f.target, audioanalysis.CapabilityVAD, "gen-1", nil)
	f.complete(f.claim("audio_vad", targetVAD.JobID), f.request(f.target, audioanalysis.CapabilityVAD, "gen-1"), "ev:target-1", nil)
	siblingVAD := f.enqueue(f.sibling, audioanalysis.CapabilityVAD, "gen-1", nil)
	f.complete(f.claim("audio_vad", siblingVAD.JobID), f.request(f.sibling, audioanalysis.CapabilityVAD, "gen-1"), "ev:sibling-1",
		map[string][]float32{"ev:sibling-1": {0.25, -0.5, 0.75, 1}})

	req := memory.ForgetRequest{Scope: "turn", TurnID: "turn:t-1"}
	preview, err := f.store.PreviewForget(ctx, req)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if preview.RequiresBroadenedAudio {
		t.Fatalf("known turn offsets must allow interval cleanup: %+v", preview)
	}
	req.ExpectedState = preview.ExpectedState
	if _, err := f.store.CommitForget(ctx, req); err != nil {
		t.Fatalf("commit forget: %v", err)
	}

	// A turn-level forget invalidates the whole target recording's acoustic
	// analysis, including its jobs; the sibling is untouched.
	if got := forgetAudioCount(t, f.db, `SELECT COUNT(*) FROM processing_jobs WHERE stage LIKE 'audio\_%' ESCAPE '\' AND input_snapshot_id=?`, f.target); got != 0 {
		t.Fatalf("target audio job rows survived turn forget: %d", got)
	}
	if got := forgetAudioCount(t, f.db, `SELECT COUNT(*) FROM audio_analysis_runs WHERE recording_id=?`, f.target); got != 0 {
		t.Fatalf("target audio runs survived turn forget: %d", got)
	}
	if got := forgetAudioCount(t, f.db, `SELECT COUNT(*) FROM processing_attempts WHERE job_id=?`, targetVAD.JobID); got != 0 {
		t.Fatalf("target audio attempt survived turn forget: %d", got)
	}
	if got := forgetAudioCount(t, f.db, `SELECT COUNT(*) FROM audio_analysis_runs WHERE recording_id=?`, f.sibling); got != 1 {
		t.Fatalf("sibling runs = %d, want 1", got)
	}
	if got := forgetAudioCount(t, f.db, `SELECT COUNT(*) FROM audio_speaker_embeddings v JOIN audio_analysis_events e ON e.id=v.event_id JOIN audio_analysis_runs r ON r.id=e.run_id WHERE r.recording_id=?`, f.sibling); got != 1 {
		t.Fatalf("sibling speaker embedding = %d, want 1", got)
	}
	var siblingStatus string
	if err := f.db.QueryRow(`SELECT status FROM processing_jobs WHERE id=?`, siblingVAD.JobID).Scan(&siblingStatus); err != nil || siblingStatus != "succeeded" {
		t.Fatalf("sibling job = %s err=%v, want succeeded", siblingStatus, err)
	}
	var siblingTurns int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM turns WHERE recording_id=?`, f.sibling).Scan(&siblingTurns); err != nil || siblingTurns != 1 {
		t.Fatalf("sibling turns = %d err=%v, want 1", siblingTurns, err)
	}
}
