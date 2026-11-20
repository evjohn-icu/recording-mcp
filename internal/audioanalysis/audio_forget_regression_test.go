package audioanalysis

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"recordingmcp/internal/memory"
	"recordingmcp/internal/process"
)

// forgetRegFixture seeds one conversation with two recordings (target and
// sibling) in a file-backed database, mirroring a real ingested conversation
// with active attachments, sync ledger rows and managed PCM files.
type forgetRegFixture struct {
	t            *testing.T
	store        *memory.Store
	db           *sql.DB
	audio        *Store
	dir          string
	audioDir     string
	conversation string
	target       string
	sibling      string
	pcm          []byte
	pcmHash      string
}

func newForgetRegFixture(t *testing.T) *forgetRegFixture {
	t.Helper()
	dir := t.TempDir()
	store, err := memory.Open(filepath.Join(dir, "audio.db"))
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
	f := &forgetRegFixture{t: t, store: store, db: db, audio: NewStore(db),
		dir: dir, audioDir: audioDir, conversation: "conv:forget-reg",
		target: "rec:forget-reg-target", sibling: "rec:forget-reg-sibling", pcm: pcm, pcmHash: hash}
	for _, id := range []string{f.target, f.sibling} {
		if err := os.WriteFile(filepath.Join(audioDir, id+".pcm"), pcm, 0o600); err != nil {
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
		 VALUES(?,'ForgetReg','','v1','',?,'sha','tv',2000,'{}','{}')`, f.conversation, f.target)
	for _, id := range []string{f.target, f.sibling} {
		exec(`INSERT INTO recordings(id,conversation_id,source_file,source_sha256,duration_ms,recording_started_at,absolute_time_known)
			 VALUES(?,?,?,?,2000,NULL,0)`, id, f.conversation, id+".pcm", hash)
		exec(`INSERT INTO turns(id,conversation_id,recording_id,segment_index,global_start_ms,global_end_ms,speaker_scoped_id,speaker_entity_id,content,source_sha256,transcript_version,original_turn_id,turn_json)
			 VALUES(?,?,?,0,0,2000,'speaker:host',NULL,'转写内容',?,'rev-1',?,'{}')`,
			"turn:"+id, f.conversation, id, hash, "turn:"+id)
		exec(`INSERT INTO source_attachments(id,conversation_id,recording_id,segment_index,segment_id,transcript_revision,source_json,result_json,content_sha256,active,supersedes_revision)
			 VALUES(?,?,?,0,'seg:1','rev-1',?,'{}','hash',1,NULL)`,
			"att:"+id, f.conversation, id,
			`{"device_id":"dev-1","recording_id":"prov-`+id+`"}`)
		exec(`INSERT INTO attachment_turns(attachment_id,turn_id) VALUES(?,?)`, "att:"+id, "turn:"+id)
		exec(`INSERT INTO sync_recordings(recording_id,owner,device_id,state,total_samples,stitched_path,transcript_revision,transcription_state,updated_at)
			 VALUES(?,'default','dev-1','ingested',64000,?,'asr-v1','done',1)`,
			"prov-"+id, filepath.Join(audioDir, id+".pcm"))
	}
	return f
}

func (f *forgetRegFixture) request(recordingID, capability, generation string) Request {
	f.t.Helper()
	req := Request{
		SchemaVersion:     ContractSchema,
		RecordingID:       recordingID,
		SourceAudioSHA256: f.pcmHash,
		InputSHA256:       f.pcmHash,
		ModelID:           "model",
		ModelRevision:     "rev-1",
		ModelFilesSHA256:  map[string]string{"model.onnx": strings.Repeat("a", 64)},
		Capability:        capability,
		StartMS:           0,
		EndMS:             2000,
		ConfigSHA256:      strings.Repeat("b", 64),
		SourceAttachmentIDs: []string{},
	}
	req.RunID = ComputeRunID(req, generation)
	return req
}

func (f *forgetRegFixture) enqueue(recordingID, capability, generation string) EnqueueOutput {
	f.t.Helper()
	out, err := f.audio.Enqueue(context.Background(), EnqueueInput{
		RecordingID: recordingID, Capability: capability, Generation: generation,
		Request: f.request(recordingID, capability, generation), MaxAttempts: 3,
	})
	if err != nil {
		f.t.Fatalf("enqueue %s/%s: %v", recordingID, capability, err)
	}
	return out
}

func (f *forgetRegFixture) claim(stage, jobID string) process.Job {
	f.t.Helper()
	job, err := process.Claim(context.Background(), f.db, process.ClaimOptions{
		WorkerID: "worker-forget-reg", Stages: []string{stage}, Lease: time.Minute, ProcessorVersion: "test",
	})
	if err != nil {
		f.t.Fatalf("claim %s: %v", stage, err)
	}
	if job.ID != jobID {
		f.t.Fatalf("claimed %s, want %s", job.ID, jobID)
	}
	return *job
}

// forgetRegResultFor builds the valid succeeded result for one request. It is
// shared by the fixture and the blocking adapter.
func forgetRegResultFor(req Request, eventID string) Result {
	result := Result{
		SchemaVersion: ContractSchema, RunID: req.RunID, RecordingID: req.RecordingID,
		SourceAudioSHA256: req.SourceAudioSHA256, InputSHA256: req.InputSHA256,
		ModelID: req.ModelID, ModelRevision: req.ModelRevision,
		ModelFilesSHA256: req.ModelFilesSHA256, Capability: req.Capability,
		StartMS: req.StartMS, EndMS: req.EndMS, ConfigSHA256: req.ConfigSHA256,
		SourceAttachmentIDs: req.SourceAttachmentIDs, SourceSnapshotSHA256: req.SourceSnapshotSHA256,
		Status: StatusSucceeded,
		Provenance: Provenance{
			AdapterVersion: "test", FrontendID: "test", Device: "cpu",
			InputSHA256: req.InputSHA256, ModelFilesSHA256: req.ModelFilesSHA256, SourceSnapshotSHA256: req.SourceSnapshotSHA256,
		},
	}
	if eventID != "" {
		result.Events = []Event{{
			ID: eventID, EventType: EventSpeechActivity, StartMS: 0, EndMS: 1000,
			Payload: map[string]any{"origin": "test"},
		}}
	}
	return result
}

func (f *forgetRegFixture) result(req Request, eventID string) Result {
	return forgetRegResultFor(req, eventID)
}

// completeSeededSibling finishes the sibling recording's VAD run with one
// event and a speaker vector so late-publish tests can prove sibling state
// survives the target recording's forget.
func (f *forgetRegFixture) completeSeededSibling() {
	f.t.Helper()
	siblingVAD := f.enqueue(f.sibling, CapabilityVAD, "gen-1")
	siblingJob := f.claim("audio_vad", siblingVAD.JobID)
	req := f.request(f.sibling, CapabilityVAD, "gen-1")
	result := f.result(req, "ev:sibling-1")
	if _, err := f.audio.Commit(context.Background(), CommitInput{
		JobID: siblingJob.ID, LeaseToken: siblingJob.LeaseToken, Result: result,
		Embeddings: map[string][]float32{"ev:sibling-1": {0.25, -0.5, 0.75, 1}}, Now: time.Now(),
	}); err != nil {
		f.t.Fatalf("commit sibling run: %v", err)
	}
}

// forgetTargetRecording previews and commits a recording-scope forget through
// the real memory store.
func (f *forgetRegFixture) forgetTargetRecording() {
	f.t.Helper()
	req := memory.ForgetRequest{Scope: "recording", RecordingID: f.target}
	preview, err := f.store.PreviewForget(context.Background(), req)
	if err != nil {
		f.t.Fatalf("preview forget: %v", err)
	}
	req.ExpectedState = preview.ExpectedState
	if _, err := f.store.CommitForget(context.Background(), req); err != nil {
		f.t.Fatalf("commit forget: %v", err)
	}
}

func (f *forgetRegFixture) count(query string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.db.QueryRow(query, args...).Scan(&n); err != nil {
		f.t.Fatalf("count %q: %v", query, err)
	}
	return n
}

func (f *forgetRegFixture) assertSiblingIntact(label string) {
	f.t.Helper()
	ctx := context.Background()
	var status string
	var active int
	if err := f.db.QueryRow(`SELECT status,active FROM audio_analysis_runs WHERE recording_id=? AND capability=?`,
		f.sibling, CapabilityVAD).Scan(&status, &active); err != nil {
		f.t.Fatalf("%s: sibling run vanished: %v", label, err)
	}
	if status != StatusSucceeded || active != 1 {
		f.t.Fatalf("%s: sibling run = %s/%d, want succeeded/1", label, status, active)
	}
	if got := f.count(`SELECT COUNT(*) FROM audio_analysis_events e JOIN audio_analysis_runs r ON r.id=e.run_id WHERE r.recording_id=?`, f.sibling); got != 1 {
		f.t.Fatalf("%s: sibling events = %d, want 1", label, got)
	}
	if got := f.count(`SELECT COUNT(*) FROM audio_speaker_embeddings v JOIN audio_analysis_events e ON e.id=v.event_id JOIN audio_analysis_runs r ON r.id=e.run_id WHERE r.recording_id=?`, f.sibling); got != 1 {
		f.t.Fatalf("%s: sibling speaker embedding = %d, want 1", label, got)
	}
	runs, err := f.audio.CurrentRuns(ctx, f.sibling)
	if err != nil || len(runs) != 1 || runs[0].Capability != CapabilityVAD {
		f.t.Fatalf("%s: sibling CurrentRuns = %v, %v", label, runs, err)
	}
}

// forgetRegResolver serves the fixture's PCM window.
type forgetRegResolver struct{ data []byte }

func (r forgetRegResolver) RecordingWindow(context.Context, string, int64, int64) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(r.data)), nil
}

func (r forgetRegResolver) SourceAudioSHA256(context.Context, string) (string, error) { return "", nil }

// forgetRegGateAnalyzer blocks until released, then returns a valid result as
// if the adapter finished long after the forget committed.
type forgetRegGateAnalyzer struct {
	entered chan struct{}
	release chan struct{}
}

func (a forgetRegGateAnalyzer) Analyze(_ context.Context, req Request, _ io.Reader) (Result, error) {
	close(a.entered)
	<-a.release
	return forgetRegResultFor(req, "ev:late-1"), nil
}

// TestForgetDuringRunBlocksLateResultPublish runs the real worker path: an
// adapter is blocked mid-analysis while the recording is permanently
// forgotten. The late adapter output must not publish anything, every audio
// job of the forgotten recording must be gone afterwards, and the sibling
// recording's current run, events, vector and jobs must stay untouched.
func TestForgetDuringRunBlocksLateResultPublish(t *testing.T) {
	ctx := context.Background()
	f := newForgetRegFixture(t)
	f.completeSeededSibling()
	targetVAD := f.enqueue(f.target, CapabilityVAD, "gen-1")
	job := f.claim("audio_vad", targetVAD.JobID)

	gate := forgetRegGateAnalyzer{entered: make(chan struct{}), release: make(chan struct{})}
	runner := &Runner{Store: f.audio, Analyzer: gate, Resolver: forgetRegResolver{data: f.pcm}}
	done := make(chan error, 1)
	go func() { done <- runner.RunJob(ctx, job) }()
	<-gate.entered

	f.forgetTargetRecording()
	close(gate.release)
	if err := <-done; err != nil {
		t.Fatalf("RunJob after forget = %v, want handled nil", err)
	}

	// The forgotten recording has neither jobs nor results left.
	if got := f.count(`SELECT COUNT(*) FROM processing_jobs WHERE stage LIKE 'audio\_%' ESCAPE '\' AND input_snapshot_id=?`, f.target); got != 0 {
		t.Fatalf("forgotten recording kept %d audio job rows", got)
	}
	for _, probe := range []struct{ label, sql string }{
		{"runs", `SELECT COUNT(*) FROM audio_analysis_runs WHERE recording_id=?`},
		{"events", `SELECT COUNT(*) FROM audio_analysis_events WHERE run_id IN (SELECT id FROM audio_analysis_runs WHERE recording_id=?)`},
		{"heads", `SELECT COUNT(*) FROM audio_analysis_heads WHERE recording_id=?`},
	} {
		if got := f.count(probe.sql, f.target); got != 0 {
			t.Fatalf("forgotten recording kept audio analysis %s: %d rows", probe.label, got)
		}
	}
	if runs, err := f.audio.CurrentRuns(ctx, f.target); err != nil || len(runs) != 0 {
		t.Fatalf("CurrentRuns of forgotten recording = %v, %v", runs, err)
	}

	// A direct replay of the late result must be refused with a barrier code,
	// never a successful publish.
	_, err := f.audio.Commit(ctx, CommitInput{
		JobID: job.ID, LeaseToken: job.LeaseToken,
		Result: f.result(f.request(f.target, CapabilityVAD, "gen-1"), "ev:late-1"), Now: time.Now(),
	})
	if err == nil {
		t.Fatal("late commit after forget succeeded")
	}
	var typed *Error
	if !errors.As(err, &typed) || (typed.Code != CodeBarrierBlocked && typed.Code != CodeLeaseLost) {
		t.Fatalf("late commit error = %v, want %s or %s", err, CodeBarrierBlocked, CodeLeaseLost)
	}

	// The sibling recording is untouched by the target's forget.
	f.assertSiblingIntact("after late publish refusal")
}

// TestForgetRecordingBlocksNewEnqueue pins the admission checks every enqueue
// path shares with the claim guards: a forgotten recording, a recording that
// no longer exists and a retired profile all reject with CodeBarrierBlocked
// instead of resurrecting work, while unaffected recordings keep enqueuing.
func TestForgetRecordingBlocksNewEnqueue(t *testing.T) {
	t.Run("tombstone", func(t *testing.T) {
		f := newForgetRegFixture(t)
		targetVAD := f.enqueue(f.target, CapabilityVAD, "gen-1")
		siblingVAD := f.enqueue(f.sibling, CapabilityVAD, "gen-1")
		f.forgetTargetRecording()

		if got := f.count(`SELECT COUNT(*) FROM processing_jobs WHERE id=?`, targetVAD.JobID); got != 0 {
			t.Fatalf("forgotten recording kept its queued audio job: %d rows", got)
		}
		if _, err := f.audio.Enqueue(context.Background(), EnqueueInput{
			RecordingID: f.target, Capability: CapabilityVAD, Generation: "gen-2",
			Request: f.request(f.target, CapabilityVAD, "gen-2"), MaxAttempts: 3,
		}); errorCodeOf(t, err) != CodeBarrierBlocked {
			t.Fatalf("enqueue for forgotten recording = %v, want %s", err, CodeBarrierBlocked)
		}
		if _, err := f.audio.Enqueue(context.Background(), EnqueueInput{
			RecordingID: f.sibling, Capability: CapabilityVAD, Generation: "gen-2",
			Request: f.request(f.sibling, CapabilityVAD, "gen-2"), MaxAttempts: 3,
		}); err != nil {
			t.Fatalf("sibling enqueue after forget: %v", err)
		}
		if got := f.count(`SELECT COUNT(*) FROM processing_jobs WHERE id=?`, siblingVAD.JobID); got != 1 {
			t.Fatalf("sibling job lost after forget: %d rows", got)
		}
	})

	t.Run("missing recording", func(t *testing.T) {
		f := newForgetRegFixture(t)
		if _, err := f.audio.Enqueue(context.Background(), EnqueueInput{
			RecordingID: "rec:never-existed", Capability: CapabilityVAD, Generation: "gen-1",
			Request: f.request("rec:never-existed", CapabilityVAD, "gen-1"), MaxAttempts: 3,
		}); errorCodeOf(t, err) != CodeBarrierBlocked {
			t.Fatalf("enqueue for missing recording = %v, want %s", err, CodeBarrierBlocked)
		}
	})

	t.Run("retired profile", func(t *testing.T) {
		f := newForgetRegFixture(t)
		if _, err := f.db.Exec(`INSERT INTO profile_control(id,retired,tombstone_id,created_at) VALUES(1,1,'forget:retired',1)`); err != nil {
			t.Fatal(err)
		}
		if _, err := f.audio.Enqueue(context.Background(), EnqueueInput{
			RecordingID: f.target, Capability: CapabilityVAD, Generation: "gen-1",
			Request: f.request(f.target, CapabilityVAD, "gen-1"), MaxAttempts: 3,
		}); errorCodeOf(t, err) != CodeBarrierBlocked {
			t.Fatalf("enqueue for retired profile = %v, want %s", err, CodeBarrierBlocked)
		}
	})
}

func errorCodeOf(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		return ""
	}
	var typed *Error
	if !errors.As(err, &typed) {
		return ""
	}
	return typed.Code
}
