package audioanalysis

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"recordingmcp/internal/asrinput"
	"recordingmcp/internal/memory"
	"recordingmcp/internal/process"
	"recordingmcp/internal/worker"
)

// audioFixture seeds one recording with a managed PCM file and one active
// attachment.
type audioFixture struct {
	t            *testing.T
	store        *memory.Store
	db           *sql.DB
	audio        *Store
	root         string
	recording    string
	conversation string
	pcm          []byte
	attachmentID string
}

func newAudioFixture(t *testing.T) *audioFixture {
	t.Helper()
	dir := t.TempDir()
	store, err := memory.Open(filepath.Join(dir, "audio.db"))
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	db := store.DB()
	recordingID := memory.RecordingID("device", "provider")
	conversationID := memory.ConversationID("device", "provider-conv")
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.Exec(query, args...); err != nil {
			t.Fatalf("exec %q: %v", query, err)
		}
	}
	exec(`INSERT INTO conversations(id,title,summary,annotation_version,annotation_basis,recording_id,source_sha256,transcript_version,duration_ms,transcript_json,annotations_json)
 VALUES(?,'Alpha','','v1','',?,'sha','tv',2000,'{}','{}')`, conversationID, recordingID)
	exec(`INSERT INTO recordings(id,conversation_id,source_file,source_sha256,duration_ms,recording_started_at,absolute_time_known)
 VALUES(?,?,'file.pcm','sha',2000,NULL,0)`, recordingID, conversationID)
	exec(`INSERT INTO turns(id,conversation_id,recording_id,segment_index,global_start_ms,global_end_ms,speaker_scoped_id,speaker_entity_id,content,source_sha256,transcript_version,original_turn_id,turn_json)
 VALUES('t:1',?,?,0,0,2000,'speaker:host',NULL,'音频分析生命周期','sha','rev-1','t:1','{}')`, conversationID, recordingID)
	pcm := bytes.Repeat([]byte{0x01, 0x02}, 2000*16)
	audioDir := filepath.Join(dir, "audio")
	if err := os.MkdirAll(audioDir, 0700); err != nil {
		t.Fatal(err)
	}
	pcmPath := filepath.Join(audioDir, "rec-a.pcm")
	if err := os.WriteFile(pcmPath, pcm, 0600); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO source_attachments(id,conversation_id,recording_id,segment_index,segment_id,transcript_revision,source_json,result_json,content_sha256,active,supersedes_revision)
 VALUES('att:1',?,?,0,'seg:1','rev-1','{"device_id":"device","recording_id":"provider"}','{}','hash',1,NULL)`, conversationID, recordingID)
	exec(`INSERT INTO sync_recordings(recording_id,owner,device_id,state,stitched_path,updated_at) VALUES('provider','owner','device','ingested',?,1)`, pcmPath)
	return &audioFixture{t: t, store: store, db: db, audio: NewStore(db), root: dir,
		recording: recordingID, conversation: conversationID, pcm: pcm, attachmentID: "att:1"}
}

func (f *audioFixture) request(capability, generation string, attachments []string) Request {
	hash := sha256.Sum256(f.pcm)
	req := Request{
		SchemaVersion:     ContractSchema,
		RecordingID:       f.recording,
		SourceAudioSHA256: hex.EncodeToString(hash[:]),
		InputSHA256:       hex.EncodeToString(hash[:]),
		ModelID:           "model",
		ModelRevision:     "rev-1",
		ModelFilesSHA256:  map[string]string{"model.onnx": string(bytes.Repeat([]byte("a"), 64))},
		Capability:        capability,
		StartMS:           0,
		EndMS:             2000,
		ConfigSHA256:      string(bytes.Repeat([]byte("b"), 64)),
	}
	if len(attachments) > 0 {
		refs := []AttachmentRef{}
		for _, id := range attachments {
			refs = append(refs, AttachmentRef{ID: id, Revision: "rev-1"})
		}
		hash, err := SnapshotHash(refs)
		if err != nil {
			f.t.Fatal(err)
		}
		req.SourceAttachmentIDs = attachments
		req.SourceSnapshotSHA256 = hash
		req.TranscriptRevision = UniqueRevision(refs)
	}
	req.RunID = ComputeRunID(req, generation)
	return req
}

func (f *audioFixture) enqueue(capability, generation string, deps []string) EnqueueOutput {
	f.t.Helper()
	out, err := f.audio.Enqueue(context.Background(), EnqueueInput{
		RecordingID: f.recording, Capability: capability, Generation: generation,
		Request: f.request(capability, generation, nil), Dependencies: deps, MaxAttempts: 3,
	})
	if err != nil {
		f.t.Fatalf("Enqueue: %v", err)
	}
	return out
}

// claim takes the job through the real queue so the lease token is valid.
func (f *audioFixture) claim(jobID string) process.Job {
	f.t.Helper()
	job, err := process.Claim(context.Background(), f.db, process.ClaimOptions{
		WorkerID: "worker-test", Stages: []string{"audio_vad", "audio_tagging", "audio_diarization", "audio_speaker"},
		Lease: time.Minute, ProcessorVersion: "test",
	})
	if err != nil {
		f.t.Fatalf("Claim: %v", err)
	}
	if job.ID != jobID {
		f.t.Fatalf("claimed %s, want %s", job.ID, jobID)
	}
	return *job
}

func (f *audioFixture) result(req Request, status string, events []Event) Result {
	return Result{
		SchemaVersion: ContractSchema, RunID: req.RunID, RecordingID: req.RecordingID,
		SourceAudioSHA256: req.SourceAudioSHA256, InputSHA256: req.InputSHA256,
		TranscriptRevision: req.TranscriptRevision, ModelID: req.ModelID, ModelRevision: req.ModelRevision,
		ModelFilesSHA256: req.ModelFilesSHA256, Capability: req.Capability, StartMS: req.StartMS, EndMS: req.EndMS,
		ConfigSHA256: req.ConfigSHA256, SourceAttachmentIDs: req.SourceAttachmentIDs,
		SourceSnapshotSHA256: req.SourceSnapshotSHA256, Status: status, Events: events,
		Provenance: Provenance{AdapterVersion: "test", FrontendID: "test", Device: "cpu", InputSHA256: req.InputSHA256, ModelFilesSHA256: req.ModelFilesSHA256, SourceSnapshotSHA256: req.SourceSnapshotSHA256},
	}
}

func (f *audioFixture) current(capability string) (RunView, bool) {
	f.t.Helper()
	runs, err := f.audio.CurrentRuns(context.Background(), f.recording)
	if err != nil {
		f.t.Fatalf("CurrentRuns: %v", err)
	}
	for _, run := range runs {
		if run.Capability == capability {
			return run, true
		}
	}
	return RunView{}, false
}

func TestAudioAnalysisLifecycle(t *testing.T) {
	ctx := context.Background()
	f := newAudioFixture(t)
	first := f.enqueue(CapabilityVAD, "gen-1", nil)
	if !first.Inserted || first.JobID == "" || first.RunID == "" {
		t.Fatalf("first enqueue = %+v", first)
	}
	repeat := f.enqueue(CapabilityVAD, "gen-1", nil)
	if repeat.Inserted || repeat.JobID != first.JobID || repeat.RunID != first.RunID {
		t.Fatalf("repeat enqueue = %+v, want idempotent %+v", repeat, first)
	}
	// A different payload under the same generation is a conflict.
	conflicting := f.request(CapabilityVAD, "gen-1", nil)
	conflicting.ModelRevision = "rev-2"
	conflicting.RunID = ComputeRunID(conflicting, "gen-1")
	if _, err := f.audio.Enqueue(ctx, EnqueueInput{RecordingID: f.recording, Capability: CapabilityVAD, Generation: "gen-1", Request: conflicting}); ErrorCodeOf(err) != CodeRequestConflict {
		t.Fatalf("conflicting enqueue error = %v", err)
	}

	job := f.claim(first.JobID)
	req := f.request(CapabilityVAD, "gen-1", nil)
	events := []Event{
		{ID: "e1", EventType: EventSpeechActivity, StartMS: 0, EndMS: 500, Confidence: new(0.9), Payload: map[string]any{"state": "speech"}},
		{ID: "e2", EventType: EventSpeechActivity, StartMS: 900, EndMS: 1200, Confidence: new(0.8), Payload: map[string]any{"state": "speech"}},
	}
	runID, err := f.audio.Commit(ctx, CommitInput{JobID: job.ID, LeaseToken: job.LeaseToken, Result: f.result(req, StatusSucceeded, events)})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	current, ok := f.current(CapabilityVAD)
	if !ok || current.RunID != runID || len(current.Events) != 2 || !current.Current {
		t.Fatalf("current run = %+v ok=%v", current, ok)
	}
	var jobStatus string
	if err := f.db.QueryRowContext(ctx, `SELECT status FROM processing_jobs WHERE id=?`, job.ID).Scan(&jobStatus); err != nil || jobStatus != "succeeded" {
		t.Fatalf("job status = %q err=%v", jobStatus, err)
	}
	// A failed result is terminal and distinct from success with no events.
	failedReq := f.request(CapabilityTagging, "gen-fail", nil)
	failedJob := f.enqueue(CapabilityTagging, "gen-fail", nil)
	claimed := f.claim(failedJob.JobID)
	failed := f.result(failedReq, StatusFailed, nil)
	failed.Error = NewError(CodeModelUnavailable, false, "model missing")
	if _, err := f.audio.Commit(ctx, CommitInput{JobID: claimed.ID, LeaseToken: claimed.LeaseToken, Result: failed}); err != nil {
		t.Fatalf("Commit failed result: %v", err)
	}
	if _, ok := f.current(CapabilityTagging); ok {
		t.Fatal("failed run must not be current")
	}
}

func TestAudioAnalysisForgetDuringCommit(t *testing.T) {
	ctx := context.Background()
	f := newAudioFixture(t)
	out := f.enqueue(CapabilityDiarization, "gen-1", nil)
	job := f.claim(out.JobID)
	// A permanent forget lands while the worker is analysing.
	if _, err := f.db.ExecContext(ctx,
		`INSERT INTO forget_tombstones(id,scope,recording_id,requested_id,locator_key,expected_state_hash,redaction_done,status,created_at)
		 VALUES('forget:1','turn',?,'t:1','locator:1','hash',0,'pending_external_cleanup',1)`, f.recording); err != nil {
		t.Fatal(err)
	}
	req := f.request(CapabilityDiarization, "gen-1", nil)
	_, err := f.audio.Commit(ctx, CommitInput{JobID: job.ID, LeaseToken: job.LeaseToken, Result: f.result(req, StatusSucceeded, nil)})
	if ErrorCodeOf(err) != CodeBarrierBlocked {
		t.Fatalf("Commit error = %v, want barrier blocked", err)
	}
	if _, ok := f.current(CapabilityDiarization); ok {
		t.Fatal("blocked commit produced a current run")
	}
	// The worker records a fatal failure so the job does not spin forever.
	if err := f.audio.Fail(ctx, job.ID, job.LeaseToken, CodeBarrierBlocked, false, time.Now()); err != nil {
		t.Fatalf("Fail: %v", err)
	}
	var status, errorCode string
	if err := f.db.QueryRowContext(ctx, `SELECT status,error_code FROM audio_analysis_runs WHERE job_id=?`, job.ID).Scan(&status, &errorCode); err != nil {
		t.Fatal(err)
	}
	if status != StatusFailed || errorCode != CodeBarrierBlocked {
		t.Fatalf("run = %s/%s", status, errorCode)
	}
}

func TestAudioAnalysisRevisionInvalidation(t *testing.T) {
	ctx := context.Background()
	f := newAudioFixture(t)
	// A speaker mapping run pins the current attachment.
	req := f.request(CapabilitySpeaker, "map-1", []string{f.attachmentID})
	out, err := f.audio.Enqueue(ctx, EnqueueInput{RecordingID: f.recording, Capability: CapabilitySpeaker, Generation: "map-1", Request: req})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	job := f.claim(out.JobID)
	if _, err := f.audio.Commit(ctx, CommitInput{JobID: job.ID, LeaseToken: job.LeaseToken,
		Result: f.result(req, StatusSucceeded, []Event{{ID: "e1", EventType: EventSpeakerSegment, StartMS: 0, EndMS: 1000, Payload: map[string]any{"speaker": "S1"}}})}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if _, ok := f.current(CapabilitySpeaker); !ok {
		t.Fatal("mapping run should be current before the supersede")
	}
	// Supersede the pinned attachment through the real ingest transaction.
	report, err := f.store.IngestTranscript(ctx, revisionResult("rev-2", "rev-1"))
	if err != nil {
		t.Fatalf("IngestTranscript: %v", err)
	}
	_ = report
	var status string
	if err := f.db.QueryRowContext(ctx, `SELECT status FROM audio_analysis_runs WHERE id=?`, out.RunID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != StatusInvalidated {
		t.Fatalf("mapping run status = %q, want invalidated", status)
	}
	var current sql.NullString
	if err := f.db.QueryRowContext(ctx, `SELECT current_run_id FROM audio_analysis_heads WHERE recording_id=? AND capability=?`, f.recording, CapabilitySpeaker).Scan(&current); err != nil {
		t.Fatal(err)
	}
	if current.Valid {
		t.Fatalf("head still points at %q", current.String)
	}
	if _, ok := f.current(CapabilitySpeaker); ok {
		t.Fatal("invalidated mapping run remained current")
	}
	// Pure acoustic runs are not pinned to attachments and stay valid.
	pure := f.enqueue(CapabilityVAD, "vad-1", nil)
	pureJob := f.claim(pure.JobID)
	pureReq := f.request(CapabilityVAD, "vad-1", nil)
	if _, err := f.audio.Commit(ctx, CommitInput{JobID: pureJob.ID, LeaseToken: pureJob.LeaseToken, Result: f.result(pureReq, StatusSucceeded, nil)}); err != nil {
		t.Fatalf("pure commit: %v", err)
	}
	if _, ok := f.current(CapabilityVAD); !ok {
		t.Fatal("pure acoustic run was invalidated by an unrelated supersede")
	}
}

func TestAudioAnalysisModelReplacement(t *testing.T) {
	ctx := context.Background()
	f := newAudioFixture(t)
	first := f.enqueue(CapabilityVAD, "model-a", nil)
	job := f.claim(first.JobID)
	req := f.request(CapabilityVAD, "model-a", nil)
	if _, err := f.audio.Commit(ctx, CommitInput{JobID: job.ID, LeaseToken: job.LeaseToken, Result: f.result(req, StatusSucceeded, nil)}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	// A model upgrade uses a new generation and supersedes the old result.
	upgraded := f.request(CapabilityVAD, "model-b", nil)
	upgraded.ModelRevision = "rev-2"
	upgraded.ModelFilesSHA256 = map[string]string{"model.onnx": string(bytes.Repeat([]byte("c"), 64))}
	upgraded.RunID = ComputeRunID(upgraded, "model-b")
	out, err := f.audio.Enqueue(ctx, EnqueueInput{RecordingID: f.recording, Capability: CapabilityVAD, Generation: "model-b", Request: upgraded})
	if err != nil {
		t.Fatalf("Enqueue upgraded: %v", err)
	}
	if _, ok := f.current(CapabilityVAD); ok {
		t.Fatal("old result stayed current after a new generation was desired")
	}
	job2 := f.claim(out.JobID)
	if _, err := f.audio.Commit(ctx, CommitInput{JobID: job2.ID, LeaseToken: job2.LeaseToken, Result: f.result(upgraded, StatusSucceeded, nil)}); err != nil {
		t.Fatalf("Commit upgraded: %v", err)
	}
	current, ok := f.current(CapabilityVAD)
	if !ok || current.ModelRevision != "rev-2" {
		t.Fatalf("current = %+v ok=%v", current, ok)
	}
}

func TestAudioAnalysisIdentityNeedsReview(t *testing.T) {
	ctx := context.Background()
	f := newAudioFixture(t)
	req := f.request(CapabilitySpeaker, "id-1", nil)
	out, err := f.audio.Enqueue(ctx, EnqueueInput{RecordingID: f.recording, Capability: CapabilitySpeaker, Generation: "id-1", Request: req})
	if err != nil {
		t.Fatal(err)
	}
	job := f.claim(out.JobID)
	events := []Event{{
		ID: "proposal-1", EventType: EventSpeakerIdentityProposal, StartMS: 0, EndMS: 1000,
		Payload: map[string]any{"status": "proposed", "person_id": "person:1", "source_kind": "unknown"},
	}}
	if _, err := f.audio.Commit(ctx, CommitInput{JobID: job.ID, LeaseToken: job.LeaseToken, Result: f.result(req, StatusSucceeded, events)}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	current, ok := f.current(CapabilitySpeaker)
	if !ok || len(current.Events) != 1 || current.Events[0].Payload["status"] != "proposed" {
		t.Fatalf("current = %+v", current)
	}
	// A proposal must not create a confirmed speaker binding.
	projection, err := f.store.SpeakerProjection(ctx, f.recording, "rev-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(projection) != 0 {
		t.Fatalf("proposal leaked into the confirmed projection: %v", projection)
	}
	// Confirmed person ids are only available through the review flow; the
	// projection stays empty until then.
	if person, ok := projection["speaker:host"]; ok {
		t.Fatalf("unreviewed person %q became confirmed", person)
	}
}

// scriptedAnalyzer returns one fixed result derived from the request.
type scriptedAnalyzer struct {
	events []Event
	err    error
}

func (a scriptedAnalyzer) Analyze(_ context.Context, req Request, pcm io.Reader) (Result, error) {
	if a.err != nil {
		return Result{}, a.err
	}
	return Result{
		SchemaVersion: ContractSchema, RunID: req.RunID, RecordingID: req.RecordingID,
		SourceAudioSHA256: req.SourceAudioSHA256, InputSHA256: req.InputSHA256,
		TranscriptRevision: req.TranscriptRevision, ModelID: req.ModelID, ModelRevision: req.ModelRevision,
		ModelFilesSHA256: req.ModelFilesSHA256, Capability: req.Capability, StartMS: req.StartMS, EndMS: req.EndMS,
		ConfigSHA256: req.ConfigSHA256, SourceAttachmentIDs: req.SourceAttachmentIDs,
		SourceSnapshotSHA256: req.SourceSnapshotSHA256, Status: StatusSucceeded, Events: a.events,
		Provenance: Provenance{AdapterVersion: "scripted", FrontendID: "test", Device: "cpu"},
	}, nil
}

// bytesResolver returns the fixture window.
type bytesResolver struct{ data []byte }

func (r bytesResolver) RecordingWindow(context.Context, string, int64, int64) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(r.data)), nil
}

func (r bytesResolver) SourceAudioSHA256(context.Context, string) (string, error) { return "", nil }

// TestAudioAnalysisEndToEnd runs the real queue and worker loop with a
// scripted analyzer: the run, events and vectors are committed atomically and
// the worker does not finalise the job twice.
func TestAudioAnalysisEndToEnd(t *testing.T) {
	ctx := context.Background()
	f := newAudioFixture(t)
	out := f.enqueue(CapabilitySpeaker, "e2e-1", nil)
	runner := &Runner{
		Store:    f.audio,
		Analyzer: scriptedAnalyzer{events: []Event{{ID: "emb-1", EventType: EventSpeakerSegment, StartMS: 0, EndMS: 1000, Payload: map[string]any{"speaker": "S1"}}}},
		Resolver: bytesResolver{data: f.pcm},
	}
	executor := finalizedExecutor{runner: runner}
	cfg := worker.Config{DB: f.db, WorkerID: "worker-e2e", Stages: []string{"audio_speaker"}, Lease: time.Minute, Executor: executor}
	ran, err := worker.RunOnce(ctx, cfg)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if !ran {
		t.Fatal("worker did not run the audio job")
	}
	var jobStatus string
	if err := f.db.QueryRowContext(ctx, `SELECT status FROM processing_jobs WHERE id=?`, out.JobID).Scan(&jobStatus); err != nil {
		t.Fatal(err)
	}
	if jobStatus != "succeeded" {
		t.Fatalf("job status = %q", jobStatus)
	}
	current, ok := f.current(CapabilitySpeaker)
	if !ok || len(current.Events) != 1 {
		t.Fatalf("current run = %+v ok=%v", current, ok)
	}
	var vectors int
	if err := f.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audio_speaker_embeddings`).Scan(&vectors); err != nil {
		t.Fatal(err)
	}
	if vectors != 0 {
		t.Fatalf("scripted result without embeddings stored %d vectors", vectors)
	}
	var resultJSON string
	if err := f.db.QueryRowContext(ctx, `SELECT result_json FROM audio_analysis_runs WHERE id=?`, out.RunID).Scan(&resultJSON); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains([]byte(resultJSON), []byte("embedding")) {
		t.Fatalf("result_json leaked a vector field: %s", resultJSON)
	}
	// The worker must not double-complete a finalized job; the second claim
	// finds nothing because the job already succeeded.
	if again, err := worker.RunOnce(ctx, cfg); err != nil || again {
		t.Fatalf("second RunOnce = %v, %v", again, err)
	}
}

// TestAudioTaggingJobReachesTerminalState pins the stage name the acceptance
// harness must pass in its explicit --stages list: an enabled tagging
// capability whose job is never claimed cannot be scored.
func TestAudioTaggingJobReachesTerminalState(t *testing.T) {
	ctx := context.Background()
	f := newAudioFixture(t)
	out := f.enqueue(CapabilityTagging, "tag-e2e", nil)
	runner := &Runner{
		Store:    f.audio,
		Analyzer: scriptedAnalyzer{events: []Event{{ID: "ev-1", EventType: EventAcoustic, StartMS: 100, EndMS: 600, Payload: map[string]any{"label": "speech"}}}},
		Resolver: bytesResolver{data: f.pcm},
	}
	cfg := worker.Config{DB: f.db, WorkerID: "worker-tagging", Stages: []string{"audio_tagging"},
		Lease: time.Minute, Executor: finalizedExecutor{runner: runner}}
	ran, err := worker.RunOnce(ctx, cfg)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if !ran {
		t.Fatal("the worker did not claim the tagging job")
	}
	var jobStatus string
	if err := f.db.QueryRowContext(ctx, `SELECT status FROM processing_jobs WHERE id=?`, out.JobID).Scan(&jobStatus); err != nil {
		t.Fatal(err)
	}
	if jobStatus != "succeeded" {
		t.Fatalf("tagging job status = %q, want succeeded", jobStatus)
	}
	current, ok := f.current(CapabilityTagging)
	if !ok || len(current.Events) != 1 || current.Events[0].EventType != EventAcoustic {
		t.Fatalf("current tagging run = %+v ok=%v", current, ok)
	}
}

// finalizedExecutor maps the runner's handled outcome to worker.ErrFinalized.
type finalizedExecutor struct{ runner *Runner }

func (e finalizedExecutor) Execute(ctx context.Context, job process.Job) (string, error) {
	if err := e.runner.RunJob(ctx, job); err != nil {
		return "", err
	}
	return "", worker.ErrFinalized
}

// revisionResult builds one final ASR segment for the supersede path.
func revisionResult(revision, supersedes string) asrinput.Result {
	return asrinput.Result{
		ContractVersion: asrinput.ContractVersion,
		Source: asrinput.Source{
			Provider: "fixture", ProviderRequestID: "req-" + revision, DeviceID: "device",
			RecordingID: "provider", ConversationID: "provider-conv", SegmentID: "seg:1",
			SegmentOffsetMS: 0, SegmentDurationMS: 2000, TranscriptRevision: revision,
			CompletionStatus: "final", SupersedesRevision: supersedes,
		},
		Turns: []asrinput.Turn{{ID: "utt-1", OriginalIndex: 1, Text: "修订后音频分析",
			LocalStartMS: new(int64(0)), LocalEndMS: new(int64(1000)),
			GlobalStartMS: new(int64(0)), GlobalEndMS: new(int64(1000))}},
	}
}

// TestAudioAnalysisRejectsInvalidResult covers the structural guards: an
// out-of-window event, a NaN confidence and a failed result carrying events
// must all be refused before anything is written.
func TestAudioAnalysisRejectsInvalidResult(t *testing.T) {
	ctx := context.Background()
	f := newAudioFixture(t)
	req := f.request(CapabilityVAD, "invalid-1", nil)
	for name, events := range map[string][]Event{
		"out_of_window": {{ID: "e1", EventType: EventSpeechActivity, StartMS: 0, EndMS: 5000}},
		"nan":           {{ID: "e1", EventType: EventSpeechActivity, StartMS: 0, EndMS: 10, Confidence: new(math.NaN())}},
		"too_high":      {{ID: "e1", EventType: EventSpeechActivity, StartMS: 0, EndMS: 10, Confidence: new(1.5)}},
		"duplicate_id":  {{ID: "e1", EventType: EventSpeechActivity, StartMS: 0, EndMS: 10}, {ID: "e1", EventType: EventSpeechActivity, StartMS: 20, EndMS: 30}},
	} {
		if err := f.result(req, StatusSucceeded, events).ValidateResult(req, 2000); ErrorCodeOf(err) != CodeResultInvalid {
			t.Fatalf("%s: error = %v, want %q", name, err, CodeResultInvalid)
		}
	}
	failedWithEvents := f.result(req, StatusFailed, []Event{{ID: "e1", EventType: EventSpeechActivity, StartMS: 0, EndMS: 10}})
	if err := failedWithEvents.ValidateResult(req, 2000); ErrorCodeOf(err) != CodeResultInvalid {
		t.Fatalf("failed result with events error = %v", err)
	}
	// A stale lease token cannot commit.
	out := f.enqueue(CapabilityVAD, "lease-1", nil)
	job := f.claim(out.JobID)
	wrong := f.result(f.request(CapabilityVAD, "lease-1", nil), StatusSucceeded, nil)
	if _, err := f.audio.Commit(ctx, CommitInput{JobID: job.ID, LeaseToken: "wrong", Result: wrong}); ErrorCodeOf(err) != CodeLeaseLost {
		t.Fatalf("wrong lease error = %v, want %q", err, CodeLeaseLost)
	}
}

// TestAudioFailUnclaimedReleasesDependents proves a stage that this worker does
// not run is terminated with a safe code so a dependent job can proceed.
func TestAudioFailUnclaimedReleasesDependents(t *testing.T) {
	ctx := context.Background()
	f := newAudioFixture(t)
	parent := f.enqueue(CapabilityDiarization, "disabled-1", nil)
	child, _, err := process.Enqueue(ctx, f.db, process.Spec{
		Stage: "audio_speaker", InputSnapshotID: f.recording, IdempotencyKey: "speaker-child",
		Dependencies: []string{parent.JobID},
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// The child cannot be claimed while the parent waits.
	if job, err := process.Claim(ctx, f.db, process.ClaimOptions{WorkerID: "w", Stages: []string{"audio_speaker"}, Lease: time.Minute}); err == nil {
		t.Fatalf("child claimed while parent pending: %+v", job)
	}
	if err := f.audio.FailUnclaimed(ctx, "audio_diarization", CodeDisabled, time.Now()); err != nil {
		t.Fatalf("FailUnclaimed: %v", err)
	}
	var status, errorCode string
	if err := f.db.QueryRowContext(ctx, `SELECT j.status,r.error_code FROM processing_jobs j JOIN audio_analysis_runs r ON r.job_id=j.id WHERE j.id=?`, parent.JobID).Scan(&status, &errorCode); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || errorCode != CodeDisabled {
		t.Fatalf("parent = %s/%s", status, errorCode)
	}
	claimed, err := process.Claim(ctx, f.db, process.ClaimOptions{WorkerID: "w", Stages: []string{"audio_speaker"}, Lease: time.Minute})
	if err != nil || claimed.ID != child {
		t.Fatalf("child not released: %+v err=%v", claimed, err)
	}
}
