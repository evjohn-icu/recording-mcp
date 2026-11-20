package audioanalysis

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
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

// TestAudioEnqueueRunIDIdentity pins plan 4.1: the manual entry point must
// complete the schema/recording/capability fields, derive
// ComputeRunID(req, generation) and fill an empty run_id with it before
// validation; an explicit run_id that disagrees with the request identity is
// audio_input_invalid, not a validation error about a missing field.
func TestAudioEnqueueRunIDIdentity(t *testing.T) {
	ctx := context.Background()
	f := newAudioFixture(t)

	req := f.request(CapabilityVAD, "gen-fill", nil)
	req.RunID = ""
	out, err := f.audio.Enqueue(ctx, EnqueueInput{
		RecordingID: f.recording, Capability: CapabilityVAD,
		Generation: "gen-fill", Request: req, MaxAttempts: 3,
	})
	if err != nil {
		t.Fatalf("Enqueue without run_id: %v", err)
	}
	want := ComputeRunID(req, "gen-fill")
	if out.RunID != want {
		t.Fatalf("out.RunID = %q, want computed %q", out.RunID, want)
	}
	var storedRun string
	if err := f.db.QueryRowContext(ctx, `SELECT id FROM audio_analysis_runs WHERE job_id=?`, out.JobID).Scan(&storedRun); err != nil {
		t.Fatal(err)
	}
	if storedRun != want {
		t.Fatalf("stored run id = %q, want %q", storedRun, want)
	}
	var payloadJSON string
	if err := f.db.QueryRowContext(ctx, `SELECT payload_json FROM processing_jobs WHERE id=?`, out.JobID).Scan(&payloadJSON); err != nil {
		t.Fatal(err)
	}
	var decoded jobPayload
	if err := json.Unmarshal([]byte(payloadJSON), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Request.RunID != want {
		t.Fatalf("queued payload run_id = %q, want %q", decoded.Request.RunID, want)
	}
	// The same empty-run_id request replays as the identical canonical payload.
	repeat, err := f.audio.Enqueue(ctx, EnqueueInput{
		RecordingID: f.recording, Capability: CapabilityVAD,
		Generation: "gen-fill", Request: req, MaxAttempts: 3,
	})
	if err != nil {
		t.Fatalf("replay without run_id: %v", err)
	}
	if repeat.Inserted || repeat.JobID != out.JobID || repeat.RunID != out.RunID {
		t.Fatalf("replay = %+v, want idempotent %+v", repeat, out)
	}

	// An explicit run_id that does not match the derived identity is rejected
	// even though every other field validates.
	bad := f.request(CapabilityVAD, "gen-mismatch", nil)
	bad.RunID = strings.Repeat("f", 64)
	if _, err := f.audio.Enqueue(ctx, EnqueueInput{
		RecordingID: f.recording, Capability: CapabilityVAD,
		Generation: "gen-mismatch", Request: bad, MaxAttempts: 3,
	}); ErrorCodeOf(err) != CodeInputInvalid {
		t.Fatalf("mismatched run_id error = %v, want %q", err, CodeInputInvalid)
	}
	var runs int
	if err := f.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audio_analysis_runs WHERE capability=?`, CapabilityVAD).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if runs != 1 {
		t.Fatalf("mismatched run_id inserted a run: %d", runs)
	}
}

// TestAudioEnqueueReplayDependencySet pins plan 4.1: a same-generation replay
// compares the canonical payload first and then still runs the queue's
// dependency-set validation. A changed parent set is audio_request_conflict
// (process.ErrConflict mapped), a reordered or deduplicated set replays the
// same job and run, and no replay may move the desired head.
func TestAudioEnqueueReplayDependencySet(t *testing.T) {
	ctx := context.Background()
	f := newAudioFixture(t)
	p1 := f.enqueue(CapabilityVAD, "p1", nil)
	p2 := f.enqueue(CapabilityVAD, "p2", nil)
	first := f.enqueue(CapabilityTagging, "child", []string{p1.JobID, p2.JobID})
	if !first.Inserted {
		t.Fatalf("first enqueue = %+v", first)
	}

	// Reordered and duplicated dependencies normalise to the stored set: the
	// replay returns the existing job and run without changing the head.
	replay, err := f.audio.Enqueue(ctx, EnqueueInput{
		RecordingID: f.recording, Capability: CapabilityTagging, Generation: "child",
		Request: f.request(CapabilityTagging, "child", nil),
		Dependencies: []string{p2.JobID, p1.JobID, p1.JobID}, MaxAttempts: 3,
	})
	if err != nil {
		t.Fatalf("reordered replay: %v", err)
	}
	if replay.Inserted || replay.JobID != first.JobID || replay.RunID != first.RunID {
		t.Fatalf("reordered replay = %+v, want idempotent %+v", replay, first)
	}

	// Dropping a parent changes the dependency set: conflict, no silent rewire.
	if _, err := f.audio.Enqueue(ctx, EnqueueInput{
		RecordingID: f.recording, Capability: CapabilityTagging, Generation: "child",
		Request: f.request(CapabilityTagging, "child", nil),
		Dependencies: []string{p1.JobID}, MaxAttempts: 3,
	}); ErrorCodeOf(err) != CodeRequestConflict {
		t.Fatalf("dropped-parent replay error = %v, want %q", err, CodeRequestConflict)
	}
	// An unknown parent id is also a dependency-set change, not a silent accept.
	if _, err := f.audio.Enqueue(ctx, EnqueueInput{
		RecordingID: f.recording, Capability: CapabilityTagging, Generation: "child",
		Request: f.request(CapabilityTagging, "child", nil),
		Dependencies: []string{"job:phantom"}, MaxAttempts: 3,
	}); ErrorCodeOf(err) != CodeRequestConflict {
		t.Fatalf("phantom-parent replay error = %v, want %q", err, CodeRequestConflict)
	}

	var desired, current sql.NullString
	if err := f.db.QueryRowContext(ctx,
		`SELECT desired_job_id,current_run_id FROM audio_analysis_heads WHERE recording_id=? AND capability=?`,
		f.recording, CapabilityTagging).Scan(&desired, &current); err != nil {
		t.Fatal(err)
	}
	if !desired.Valid || desired.String != first.JobID || current.Valid {
		t.Fatalf("head moved by replays: desired=%v current=%v", desired, current)
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

// --- Plan 4.6: Runner.RunJob claim checks and failure semantics. ---

// probeResolver counts how often RunJob obtained the PCM window.
type probeResolver struct{ calls int }

func (r *probeResolver) RecordingWindow(context.Context, string, int64, int64) (io.ReadCloser, error) {
	r.calls++
	return io.NopCloser(bytes.NewReader(bytes.Repeat([]byte{0x01, 0x02}, 2000*16))), nil
}

func (r *probeResolver) SourceAudioSHA256(context.Context, string) (string, error) { return "", nil }

// probeAnalyzer counts how often RunJob invoked the adapter.
type probeAnalyzer struct{ calls int }

func (a *probeAnalyzer) Analyze(context.Context, Request, io.Reader) (Result, error) {
	a.calls++
	return Result{SchemaVersion: ContractSchema, Status: StatusSucceeded}, nil
}

// fixedResultAnalyzer always returns the prepared result, standing in for an
// adapter that ran to completion and reported a terminal failure.
type fixedResultAnalyzer struct{ result Result }

func (a fixedResultAnalyzer) Analyze(context.Context, Request, io.Reader) (Result, error) {
	return a.result, nil
}

// claimAt claims a specific job at an explicit instant, so retry backoff and
// lease recovery are deterministic.
func (f *audioFixture) claimAt(jobID string, now time.Time, workerID string) process.Job {
	f.t.Helper()
	job, err := process.Claim(context.Background(), f.db, process.ClaimOptions{
		WorkerID: workerID,
		Stages:   []string{"audio_vad", "audio_tagging", "audio_diarization", "audio_speaker"},
		Lease:    time.Minute, ProcessorVersion: "test", Now: now,
	})
	if err != nil {
		f.t.Fatalf("Claim: %v", err)
	}
	if job.ID != jobID {
		f.t.Fatalf("claimed %s, want %s", job.ID, jobID)
	}
	return *job
}

// claimLease claims a specific job with a custom lease length.
func (f *audioFixture) claimLease(jobID string, lease time.Duration) process.Job {
	f.t.Helper()
	job, err := process.Claim(context.Background(), f.db, process.ClaimOptions{
		WorkerID: "worker-a",
		Stages:   []string{"audio_vad", "audio_tagging", "audio_diarization", "audio_speaker"},
		Lease:    lease, ProcessorVersion: "test",
	})
	if err != nil {
		f.t.Fatalf("Claim: %v", err)
	}
	if job.ID != jobID {
		f.t.Fatalf("claimed %s, want %s", job.ID, jobID)
	}
	return *job
}

// jobStatusOf reads one processing job's status.
func jobStatusOf(t *testing.T, db *sql.DB, jobID string) string {
	t.Helper()
	var status string
	if err := db.QueryRowContext(context.Background(), `SELECT status FROM processing_jobs WHERE id=?`, jobID).Scan(&status); err != nil {
		t.Fatalf("job status %s: %v", jobID, err)
	}
	return status
}

// runRow reads one audio run row through its job id.
func runRow(t *testing.T, db *sql.DB, jobID string) (status string, active int, errorCode sql.NullString) {
	t.Helper()
	if err := db.QueryRowContext(context.Background(),
		`SELECT status,active,error_code FROM audio_analysis_runs WHERE job_id=?`, jobID).
		Scan(&status, &active, &errorCode); err != nil {
		t.Fatalf("run row %s: %v", jobID, err)
	}
	return status, active, errorCode
}

// lastErrorCode returns the code recorded on the newest attempt error.
func lastErrorCode(t *testing.T, db *sql.DB, jobID string) string {
	t.Helper()
	entry, err := process.LastError(context.Background(), db, jobID)
	if err != nil {
		t.Fatal(err)
	}
	return entry.Code
}

// TestAudioRunnerRetryableThenSucceeds pins plan 4.6: a retryable adapter
// failure parks the job in retry_wait within the attempt budget while the run
// row stays running, and the next attempt after the backoff completes both.
func TestAudioRunnerRetryableThenSucceeds(t *testing.T) {
	ctx := context.Background()
	f := newAudioFixture(t)
	out, err := f.audio.Enqueue(ctx, EnqueueInput{
		RecordingID: f.recording, Capability: CapabilityVAD, Generation: "retry-1",
		Request: f.request(CapabilityVAD, "retry-1", nil), MaxAttempts: 3,
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	runner := &Runner{Store: f.audio, Analyzer: scriptedAnalyzer{err: NewError(CodeGPUBusy, true, "gpu busy")}, Resolver: bytesResolver{data: f.pcm}}
	job1 := f.claim(out.JobID)
	if err := runner.RunJob(ctx, job1); err != nil {
		t.Fatalf("RunJob retryable: %v", err)
	}
	if status := jobStatusOf(t, f.db, out.JobID); status != "retry_wait" {
		t.Fatalf("job status = %q, want retry_wait", status)
	}
	if status, _, _ := runRow(t, f.db, out.JobID); status != StatusRunning {
		t.Fatalf("run status = %q, want running within the attempt budget", status)
	}
	if code := lastErrorCode(t, f.db, out.JobID); code != CodeGPUBusy {
		t.Fatalf("attempt error code = %q, want %q", code, CodeGPUBusy)
	}
	job2 := f.claimAt(out.JobID, time.Now().Add(6*time.Second), "worker-test")
	runner.Analyzer = scriptedAnalyzer{events: []Event{{ID: "e1", EventType: EventSpeechActivity, StartMS: 0, EndMS: 500, Payload: map[string]any{"state": "speech"}}}}
	if err := runner.RunJob(ctx, job2); err != nil {
		t.Fatalf("RunJob after retry: %v", err)
	}
	if status := jobStatusOf(t, f.db, out.JobID); status != "succeeded" {
		t.Fatalf("job status = %q, want succeeded", status)
	}
	if status, active, _ := runRow(t, f.db, out.JobID); status != StatusSucceeded || active != 1 {
		t.Fatalf("run = %s/%d, want succeeded/1", status, active)
	}
	if _, ok := f.current(CapabilityVAD); !ok {
		t.Fatal("recovered attempt did not publish a current run")
	}
}

// TestAudioRunnerRetryableExhaustedSyncsRunAndReleasesChild pins plan 4.6:
// when the retry budget is exhausted the job enters terminal failed and the
// run row is synced to failed with the adapter's code even though the adapter
// reported the failure as retryable; the failed parent releases its child.
func TestAudioRunnerRetryableExhaustedSyncsRunAndReleasesChild(t *testing.T) {
	ctx := context.Background()
	f := newAudioFixture(t)
	parent, err := f.audio.Enqueue(ctx, EnqueueInput{
		RecordingID: f.recording, Capability: CapabilityVAD, Generation: "exh-1",
		Request: f.request(CapabilityVAD, "exh-1", nil), MaxAttempts: 2,
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	child, _, err := process.Enqueue(ctx, f.db, process.Spec{
		Stage: "audio_speaker", InputSnapshotID: f.recording,
		IdempotencyKey: "exh-child", Dependencies: []string{parent.JobID},
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	runner := &Runner{Store: f.audio, Analyzer: scriptedAnalyzer{err: NewError(CodeGPUBusy, true, "gpu busy")}, Resolver: bytesResolver{data: f.pcm}}
	job1 := f.claim(parent.JobID)
	if err := runner.RunJob(ctx, job1); err != nil {
		t.Fatalf("first RunJob: %v", err)
	}
	if status := jobStatusOf(t, f.db, parent.JobID); status != "retry_wait" {
		t.Fatalf("job status = %q, want retry_wait", status)
	}
	job2 := f.claimAt(parent.JobID, time.Now().Add(6*time.Second), "worker-test")
	if err := runner.RunJob(ctx, job2); err != nil {
		t.Fatalf("second RunJob: %v", err)
	}
	if status := jobStatusOf(t, f.db, parent.JobID); status != "failed" {
		t.Fatalf("exhausted job status = %q, want failed", status)
	}
	status, active, errorCode := runRow(t, f.db, parent.JobID)
	if status != StatusFailed || active != 0 || !errorCode.Valid || errorCode.String != CodeGPUBusy {
		t.Fatalf("exhausted run = %s/%d/%v, want failed/0/%s", status, active, errorCode, CodeGPUBusy)
	}
	if code := lastErrorCode(t, f.db, parent.JobID); code != CodeGPUBusy {
		t.Fatalf("attempt error code = %q, want %q", code, CodeGPUBusy)
	}
	claimed, err := process.Claim(ctx, f.db, process.ClaimOptions{
		WorkerID: "worker-test", Stages: []string{"audio_speaker"}, Lease: time.Minute,
	})
	if err != nil || claimed.ID != child {
		t.Fatalf("child not released by the exhausted parent: %+v err=%v", claimed, err)
	}
}

// TestAudioRunnerFatalFailureFailsRun pins the fatal path: a non-retryable
// adapter failure fails the run and the job at once.
func TestAudioRunnerFatalFailureFailsRun(t *testing.T) {
	ctx := context.Background()
	f := newAudioFixture(t)
	out := f.enqueue(CapabilityDiarization, "fatal-1", nil)
	job := f.claim(out.JobID)
	runner := &Runner{Store: f.audio, Analyzer: scriptedAnalyzer{err: NewError(CodeDisabled, false, "capability off")}, Resolver: bytesResolver{data: f.pcm}}
	if err := runner.RunJob(ctx, job); err != nil {
		t.Fatalf("RunJob fatal: %v", err)
	}
	if status := jobStatusOf(t, f.db, out.JobID); status != "failed" {
		t.Fatalf("job status = %q, want failed", status)
	}
	status, active, errorCode := runRow(t, f.db, out.JobID)
	if status != StatusFailed || active != 0 || !errorCode.Valid || errorCode.String != CodeDisabled {
		t.Fatalf("run = %s/%d/%v, want failed/0/%s", status, active, errorCode, CodeDisabled)
	}
	if code := lastErrorCode(t, f.db, out.JobID); code != CodeDisabled {
		t.Fatalf("attempt error code = %q, want %q", code, CodeDisabled)
	}
}

// TestAudioRunnerSupersededGenerationTerminatesOnce pins plan 4.6: while the
// first attempt still holds a live lease, a newer generation supersedes it;
// the stale worker must terminate its attempt exactly once with
// audio_superseded and leave the invalidated run untouched.
func TestAudioRunnerSupersededGenerationTerminatesOnce(t *testing.T) {
	ctx := context.Background()
	f := newAudioFixture(t)
	first := f.enqueue(CapabilityVAD, "sup-1", nil)
	job1 := f.claim(first.JobID)
	// A new generation is desired while job1 is still running on a live lease.
	second := f.enqueue(CapabilityVAD, "sup-2", nil)
	if status, _, _ := runRow(t, f.db, first.JobID); status != StatusInvalidated {
		t.Fatalf("superseded run status = %q, want invalidated", status)
	}
	resolver := &probeResolver{}
	analyzer := &probeAnalyzer{}
	runner := &Runner{Store: f.audio, Analyzer: analyzer, Resolver: resolver}
	// The stale worker must not analyse; the claim check fires first.
	if err := runner.RunJob(ctx, job1); err != nil {
		t.Fatalf("stale RunJob: %v", err)
	}
	if analyzer.calls != 0 || resolver.calls != 0 {
		t.Fatalf("superseded worker analysed the stale input (resolver=%d analyzer=%d)", resolver.calls, analyzer.calls)
	}
	if status := jobStatusOf(t, f.db, first.JobID); status != "failed" {
		t.Fatalf("superseded job status = %q, want failed", status)
	}
	if code := lastErrorCode(t, f.db, first.JobID); code != "audio_superseded" {
		t.Fatalf("attempt error code = %q, want audio_superseded", code)
	}
	if status, active, _ := runRow(t, f.db, first.JobID); status != StatusInvalidated || active != 0 {
		t.Fatalf("superseded run = %s/%d, want invalidated/0", status, active)
	}
	var attempts int
	if err := f.db.QueryRowContext(ctx, `SELECT attempt_count FROM processing_jobs WHERE id=?`, first.JobID).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	// A repeated stale RunJob must not terminate the job a second time.
	if err := runner.RunJob(ctx, job1); err != nil {
		t.Fatalf("second stale RunJob: %v", err)
	}
	var attemptsAfter int
	if err := f.db.QueryRowContext(ctx, `SELECT attempt_count FROM processing_jobs WHERE id=?`, first.JobID).Scan(&attemptsAfter); err != nil {
		t.Fatal(err)
	}
	if attemptsAfter != attempts {
		t.Fatalf("attempt count moved from %d to %d on a repeated stale run", attempts, attemptsAfter)
	}
	if code := lastErrorCode(t, f.db, first.JobID); code != "audio_superseded" {
		t.Fatalf("attempt error code changed to %q", code)
	}
	// The desired generation still reaches a terminal state; the superseded
	// run never becomes current again.
	job2 := f.claim(second.JobID)
	runner.Analyzer = scriptedAnalyzer{err: NewError(CodeDisabled, false, "capability off")}
	runner.Resolver = bytesResolver{data: f.pcm}
	if err := runner.RunJob(ctx, job2); err != nil {
		t.Fatalf("desired RunJob: %v", err)
	}
	if status := jobStatusOf(t, f.db, second.JobID); status != "failed" {
		t.Fatalf("desired job status = %q, want failed", status)
	}
	if status, active, _ := runRow(t, f.db, first.JobID); status != StatusInvalidated || active != 0 {
		t.Fatalf("superseded run mutated by the desired failure: %s/%d", status, active)
	}
	var current []byte
	if err := f.db.QueryRowContext(ctx, `SELECT current_run_id FROM audio_analysis_heads WHERE recording_id=? AND capability=?`, f.recording, CapabilityVAD).Scan(&current); err != nil {
		t.Fatal(err)
	}
	if current != nil {
		t.Fatalf("head points at %s although no current result exists", current)
	}
	if _, ok := f.current(CapabilityVAD); ok {
		t.Fatal("superseded generation became current again")
	}
}

// TestAudioRunnerOldResultPointerStaysInactive pins that a completed older
// generation keeps its result pointer while its terminal job is never mutated
// by a stale worker and its run can never re-activate, even when the desired
// generation fails fatally.
func TestAudioRunnerOldResultPointerStaysInactive(t *testing.T) {
	ctx := context.Background()
	f := newAudioFixture(t)
	first := f.enqueue(CapabilityVAD, "ptr-1", nil)
	job1 := f.claim(first.JobID)
	req1 := f.request(CapabilityVAD, "ptr-1", nil)
	if _, err := f.audio.Commit(ctx, CommitInput{JobID: job1.ID, LeaseToken: job1.LeaseToken, Result: f.result(req1, StatusSucceeded, nil)}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	// A new generation supersedes the succeeded run.
	second := f.enqueue(CapabilityVAD, "ptr-2", nil)
	// A stale worker replaying the already-terminal job must not mutate it.
	if err := (&Runner{Store: f.audio, Analyzer: &probeAnalyzer{}, Resolver: &probeResolver{}}).RunJob(ctx, job1); err != nil {
		t.Fatalf("stale RunJob on terminal job: %v", err)
	}
	if status := jobStatusOf(t, f.db, first.JobID); status != "succeeded" {
		t.Fatalf("terminal job mutated by the stale worker: %q", status)
	}
	var resultJSON string
	if err := f.db.QueryRowContext(ctx, `SELECT result_json FROM audio_analysis_runs WHERE id=?`, first.RunID).Scan(&resultJSON); err != nil {
		t.Fatal(err)
	}
	if resultJSON == "" || resultJSON == "{}" {
		t.Fatalf("old result pointer lost: %q", resultJSON)
	}
	// The desired generation fails fatally; the old run must not re-activate.
	job2 := f.claim(second.JobID)
	runner := &Runner{Store: f.audio, Analyzer: scriptedAnalyzer{err: NewError(CodeDisabled, false, "capability off")}, Resolver: bytesResolver{data: f.pcm}}
	if err := runner.RunJob(ctx, job2); err != nil {
		t.Fatalf("desired RunJob: %v", err)
	}
	if status := jobStatusOf(t, f.db, second.JobID); status != "failed" {
		t.Fatalf("desired job status = %q, want failed", status)
	}
	status, active, _ := runRow(t, f.db, first.JobID)
	if status != StatusInvalidated || active != 0 {
		t.Fatalf("old run re-activated: %s/%d", status, active)
	}
	var current []byte
	if err := f.db.QueryRowContext(ctx, `SELECT current_run_id FROM audio_analysis_heads WHERE recording_id=? AND capability=?`, f.recording, CapabilityVAD).Scan(&current); err != nil {
		t.Fatal(err)
	}
	if current != nil {
		t.Fatalf("head points at %s although the desired generation failed", current)
	}
	if _, ok := f.current(CapabilityVAD); ok {
		t.Fatal("old generation with a result pointer became current again")
	}
}

// TestAudioRunnerLeaseLostSkipsInput pins plan 4.6: a worker that lost its
// lease is detected before any PCM is read; the stale attempt must not mutate
// the new owner's state and the new owner completes normally.
func TestAudioRunnerLeaseLostSkipsInput(t *testing.T) {
	ctx := context.Background()
	f := newAudioFixture(t)
	out := f.enqueue(CapabilityVAD, "lease-2", nil)
	jobA := f.claimLease(out.JobID, 30*time.Millisecond)
	// A claim after the short lease expired re-owns the same job.
	jobB := f.claimAt(out.JobID, time.Now().Add(10*time.Second), "worker-b")
	if jobB.LeaseToken == jobA.LeaseToken {
		t.Fatal("expected worker-b to hold a fresh lease")
	}
	resolver := &probeResolver{}
	analyzer := &probeAnalyzer{}
	runner := &Runner{Store: f.audio, Analyzer: analyzer, Resolver: resolver}
	if err := runner.RunJob(ctx, jobA); err != nil {
		t.Fatalf("stale RunJob: %v", err)
	}
	if resolver.calls != 0 || analyzer.calls != 0 {
		t.Fatalf("lease-lost worker obtained input (resolver=%d analyzer=%d)", resolver.calls, analyzer.calls)
	}
	var status, leaseToken string
	if err := f.db.QueryRowContext(ctx, `SELECT status,lease_token FROM processing_jobs WHERE id=?`, out.JobID).Scan(&status, &leaseToken); err != nil {
		t.Fatal(err)
	}
	if status != "running" || leaseToken != jobB.LeaseToken {
		t.Fatalf("job = %s with token %q, want running with worker-b's token", status, leaseToken)
	}
	// worker-b still owns the job and completes it.
	runner.Analyzer = scriptedAnalyzer{events: []Event{{ID: "e1", EventType: EventSpeechActivity, StartMS: 0, EndMS: 500, Payload: map[string]any{"state": "speech"}}}}
	runner.Resolver = bytesResolver{data: f.pcm}
	if err := runner.RunJob(ctx, jobB); err != nil {
		t.Fatalf("new owner RunJob: %v", err)
	}
	if status := jobStatusOf(t, f.db, out.JobID); status != "succeeded" {
		t.Fatalf("job status = %q, want succeeded", status)
	}
	if _, ok := f.current(CapabilityVAD); !ok {
		t.Fatal("new owner's result is not current")
	}
}

// TestAudioRunnerFailedResultRoutesThroughFailurePath pins plan 4.6: an
// adapter Result with status=failed carries its structured code and retryable
// flag into the same failure machinery as Store.Fail; the job is never
// completed as a success, and only a later successful attempt completes it.
func TestAudioRunnerFailedResultRoutesThroughFailurePath(t *testing.T) {
	ctx := context.Background()
	f := newAudioFixture(t)
	out, err := f.audio.Enqueue(ctx, EnqueueInput{
		RecordingID: f.recording, Capability: CapabilityVAD, Generation: "failres-1",
		Request: f.request(CapabilityVAD, "failres-1", nil), MaxAttempts: 3,
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	job1 := f.claim(out.JobID)
	req := f.request(CapabilityVAD, "failres-1", nil)
	failedResult := f.result(req, StatusFailed, nil)
	failedResult.Error = NewError(CodeModelUnavailable, true, "adapter says retry")
	runner := &Runner{Store: f.audio, Analyzer: fixedResultAnalyzer{result: failedResult}, Resolver: bytesResolver{data: f.pcm}}
	if err := runner.RunJob(ctx, job1); err != nil {
		t.Fatalf("RunJob with failed result: %v", err)
	}
	status := jobStatusOf(t, f.db, out.JobID)
	if status != "retry_wait" {
		t.Fatalf("job status = %q, want retry_wait (a failed adapter result must not complete the job)", status)
	}
	if status, active, _ := runRow(t, f.db, out.JobID); status != StatusRunning || active != 0 {
		t.Fatalf("run = %s/%d, want running/0 within the attempt budget", status, active)
	}
	if code := lastErrorCode(t, f.db, out.JobID); code != CodeModelUnavailable {
		t.Fatalf("attempt error code = %q, want %q", code, CodeModelUnavailable)
	}
	job2 := f.claimAt(out.JobID, time.Now().Add(6*time.Second), "worker-test")
	runner.Analyzer = scriptedAnalyzer{events: []Event{{ID: "e1", EventType: EventSpeechActivity, StartMS: 0, EndMS: 500, Payload: map[string]any{"state": "speech"}}}}
	if err := runner.RunJob(ctx, job2); err != nil {
		t.Fatalf("RunJob after failed result: %v", err)
	}
	if status := jobStatusOf(t, f.db, out.JobID); status != "succeeded" {
		t.Fatalf("job status = %q, want succeeded", status)
	}
	if _, ok := f.current(CapabilityVAD); !ok {
		t.Fatal("recovered attempt is not current")
	}
}

// TestAudioRunnerFailedResultIdentityEnforced pins that the failure response's
// identity is fully validated before it is processed: a failed result naming
// another run is refused as audio_result_invalid, not replayed as a retryable
// model failure.
func TestAudioRunnerFailedResultIdentityEnforced(t *testing.T) {
	ctx := context.Background()
	f := newAudioFixture(t)
	out := f.enqueue(CapabilityVAD, "failres-id", nil)
	job := f.claim(out.JobID)
	wrongReq := f.request(CapabilityVAD, "failres-id-other", nil)
	badResult := f.result(wrongReq, StatusFailed, nil)
	badResult.Error = NewError(CodeModelUnavailable, true, "not this run")
	runner := &Runner{Store: f.audio, Analyzer: fixedResultAnalyzer{result: badResult}, Resolver: bytesResolver{data: f.pcm}}
	if err := runner.RunJob(ctx, job); err != nil {
		t.Fatalf("RunJob with mismatched failed result: %v", err)
	}
	if status := jobStatusOf(t, f.db, out.JobID); status != "failed" {
		t.Fatalf("job status = %q, want failed", status)
	}
	status, active, errorCode := runRow(t, f.db, out.JobID)
	if status != StatusFailed || active != 0 || !errorCode.Valid || errorCode.String != CodeResultInvalid {
		t.Fatalf("run = %s/%d/%v, want failed/0/%s", status, active, errorCode, CodeResultInvalid)
	}
	if code := lastErrorCode(t, f.db, out.JobID); code != CodeResultInvalid {
		t.Fatalf("attempt error code = %q, want %q", code, CodeResultInvalid)
	}
}

// callbackAnalyzer delegates Analyze to a test callback so fixtures can race
// the queue while the adapter is still running.
type callbackAnalyzer struct {
	callback func(ctx context.Context, req Request, pcm io.Reader) (Result, error)
}

func (a callbackAnalyzer) Analyze(ctx context.Context, req Request, pcm io.Reader) (Result, error) {
	return a.callback(ctx, req, pcm)
}

// TestAudioRunnerSupersededDuringInference pins the mid-inference
// supersession: a newer generation enqueued while the adapter is still running
// must terminate the attempt exactly once with audio_superseded for every
// adapter outcome, and the invalidated run must stay untouched.
func TestAudioRunnerSupersededDuringInference(t *testing.T) {
	scenarios := []struct {
		name    string
		outcome func(f *audioFixture, req Request) (Result, error)
	}{
		{
			name: "succeeded_result",
			outcome: func(f *audioFixture, req Request) (Result, error) {
				return f.result(req, StatusSucceeded, nil), nil
			},
		},
		{
			name: "failed_result",
			outcome: func(f *audioFixture, req Request) (Result, error) {
				failed := f.result(req, StatusFailed, nil)
				failed.Error = NewError(CodeModelUnavailable, true, "adapter says retry")
				return failed, nil
			},
		},
		{
			name: "adapter_error",
			outcome: func(f *audioFixture, req Request) (Result, error) {
				return Result{}, NewError(CodeGPUBusy, true, "gpu busy")
			},
		},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			ctx := context.Background()
			f := newAudioFixture(t)
			first := f.enqueue(CapabilityVAD, "supmid-1", nil)
			job1 := f.claim(first.JobID)
			req1 := f.request(CapabilityVAD, "supmid-1", nil)
			secondJob := ""
			adapter := callbackAnalyzer{callback: func(_ context.Context, _ Request, _ io.Reader) (Result, error) {
				// The newer generation lands while gen-1 is mid-inference.
				out, err := f.audio.Enqueue(ctx, EnqueueInput{
					RecordingID: f.recording, Capability: CapabilityVAD, Generation: "supmid-2",
					Request: f.request(CapabilityVAD, "supmid-2", nil), MaxAttempts: 3,
				})
				if err != nil {
					return Result{}, err
				}
				secondJob = out.JobID
				return scenario.outcome(f, req1)
			}}
			runner := &Runner{Store: f.audio, Analyzer: adapter, Resolver: bytesResolver{data: f.pcm}}
			if err := runner.RunJob(ctx, job1); err != nil {
				t.Fatalf("RunJob: %v", err)
			}
			if secondJob == "" {
				t.Fatal("the adapter did not enqueue the newer generation")
			}
			if status := jobStatusOf(t, f.db, first.JobID); status != "failed" {
				t.Fatalf("superseded attempt status = %q, want failed", status)
			}
			if code := lastErrorCode(t, f.db, first.JobID); code != "audio_superseded" {
				t.Fatalf("attempt error code = %q, want audio_superseded", code)
			}
			if status, active, _ := runRow(t, f.db, first.JobID); status != StatusInvalidated || active != 0 {
				t.Fatalf("run = %s/%d, want invalidated/0", status, active)
			}
			var desired string
			if err := f.db.QueryRowContext(ctx, `SELECT desired_job_id FROM audio_analysis_heads WHERE recording_id=? AND capability=?`, f.recording, CapabilityVAD).Scan(&desired); err != nil {
				t.Fatal(err)
			}
			if desired != secondJob {
				t.Fatalf("head desired = %s, want the newer generation %s", desired, secondJob)
			}
		})
	}
}
