package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"recordingmcp/internal/audioanalysis"
	"recordingmcp/internal/memory"
)

// cliEnv seeds a synthetic profile-free database, a managed audio directory and
// a verified audio-analysis config, then builds the real context-audio binary.
type cliEnv struct {
	t          *testing.T
	dir        string
	dbPath     string
	audioDir   string
	configPath string
	recording  string
	pcm        []byte
	store      *memory.Store
	binary     string
}

func newCLIEnv(t *testing.T) *cliEnv {
	t.Helper()
	dir := t.TempDir()
	store, err := memory.Open(filepath.Join(dir, "context-memory.db"))
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	db := store.DB()
	recordingID := memory.RecordingID("device", "provider")
	conversationID := memory.ConversationID("device", "provider-conv")
	mustExec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.Exec(query, args...); err != nil {
			t.Fatalf("exec %q: %v", query, err)
		}
	}
	mustExec(`INSERT INTO conversations(id,title,summary,annotation_version,annotation_basis,recording_id,source_sha256,transcript_version,duration_ms,transcript_json,annotations_json)
	VALUES(?,'Alpha','','v1','',?,'sha','tv',2000,'{}','{}')`, conversationID, recordingID)
	mustExec(`INSERT INTO recordings(id,conversation_id,source_file,source_sha256,duration_ms,recording_started_at,absolute_time_known)
	VALUES(?,?,'file.pcm','sha',2000,NULL,0)`, recordingID, conversationID)
	pcm := bytes.Repeat([]byte{0x01, 0x02}, 2000*16)
	audioDir := filepath.Join(dir, "audio")
	if err := os.MkdirAll(audioDir, 0700); err != nil {
		t.Fatal(err)
	}
	pcmPath := filepath.Join(audioDir, "rec-a.pcm")
	if err := os.WriteFile(pcmPath, pcm, 0600); err != nil {
		t.Fatal(err)
	}
	mustExec(`INSERT INTO source_attachments(id,conversation_id,recording_id,segment_index,segment_id,transcript_revision,source_json,result_json,content_sha256,active,supersedes_revision)
	VALUES('att:1',?,?,0,'seg:1','rev-1','{"device_id":"device","recording_id":"provider"}','{}','hash',1,NULL)`, conversationID, recordingID)
	mustExec(`INSERT INTO sync_recordings(recording_id,owner,device_id,state,stitched_path,updated_at) VALUES('provider','owner','device','ingested',?,1)`, pcmPath)

	model := bytes.Repeat([]byte("m"), 256)
	if err := os.WriteFile(filepath.Join(dir, "model.bin"), model, 0600); err != nil {
		t.Fatal(err)
	}
	modelSum := sha256.Sum256(model)
	modelHash := hex.EncodeToString(modelSum[:])
	capabilities := map[string]any{}
	for _, name := range audioanalysis.Capabilities {
		capabilities[name] = map[string]any{
			"enabled":        true,
			"model_id":       "fixture-model",
			"model_revision": "rev-1",
			"files":          []map[string]string{{"path": "model.bin", "sha256": modelHash}},
			"device":         "cpu",
			"frontend_id":    "fixture",
		}
	}
	config := map[string]any{
		"schema_version":       audioanalysis.ConfigSchema,
		"python":               "python3",
		"runner":               "runner.py",
		"lock_timeout_seconds": 1,
		"load_timeout_seconds": 1,
		"run_timeout_seconds":  1,
		"capabilities":         capabilities,
	}
	configBytes, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "audio-analysis-config.json")
	if err := os.WriteFile(configPath, configBytes, 0600); err != nil {
		t.Fatal(err)
	}

	binary := filepath.Join(dir, "context-audio")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	return &cliEnv{t: t, dir: dir, dbPath: filepath.Join(dir, "context-memory.db"),
		audioDir: audioDir, configPath: configPath, recording: recordingID,
		pcm: pcm, store: store, binary: binary}
}

// cliOutput is the decoded enqueue result document.
type cliOutput struct {
	RecordingID string                     `json:"recording_id"`
	Generation  string                     `json:"generation"`
	Jobs        []audioanalysis.EnqueueOutput `json:"jobs"`
}

// cliRun runs the real binary once. It returns the decoded stdout document (or
// nil when the command failed) together with the decoded stderr error code.
func (e *cliEnv) cliRun(t *testing.T, requestID, capabilities string) (*cliOutput, string, error) {
	t.Helper()
	cmd := exec.Command(e.binary, "enqueue",
		"--db", e.dbPath, "--audio-dir", e.audioDir, "--recording-id", e.recording,
		"--capabilities", capabilities, "--request-id", requestID, "--config", e.configPath)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	var out *cliOutput
	if runErr == nil {
		out = &cliOutput{}
		if err := json.Unmarshal(stdout.Bytes(), out); err != nil {
			t.Fatalf("decode stdout %s: %v", stdout.String(), err)
		}
	}
	var surfaced struct {
		Error struct {
			Code   string `json:"code"`
			Detail string `json:"detail"`
		} `json:"error"`
	}
	_ = json.Unmarshal(stderr.Bytes(), &surfaced)
	return out, surfaced.Error.Code, runErr
}

// expectedRequest rebuilds the request the CLI must pin for capability, so the
// test can check run_id against ComputeRunID without trusting the CLI.
func (e *cliEnv) expectedRequest(t *testing.T, capability string) audioanalysis.Request {
	t.Helper()
	cfg, err := audioanalysis.LoadConfig(e.configPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	resolved, err := cfg.Resolve(capability)
	if err != nil {
		t.Fatalf("Resolve(%s): %v", capability, err)
	}
	pcmSum := sha256.Sum256(e.pcm)
	pcmHash := hex.EncodeToString(pcmSum[:])
	req := audioanalysis.Request{
		SchemaVersion:     audioanalysis.ContractSchema,
		RecordingID:       e.recording,
		SourceAudioSHA256: pcmHash,
		InputSHA256:       pcmHash,
		ModelID:           resolved.ModelID,
		ModelRevision:     resolved.ModelRevision,
		ModelFilesSHA256:  map[string]string{},
		Capability:        capability,
		StartMS:           0,
		EndMS:             2000,
		ConfigSHA256:      resolved.ConfigSHA256,
	}
	for _, file := range resolved.Files {
		req.ModelFilesSHA256[file.Path] = file.SHA256
	}
	if capability == audioanalysis.CapabilitySpeaker {
		refs := []audioanalysis.AttachmentRef{{ID: "att:1", Revision: "rev-1"}}
		snapshot, err := audioanalysis.SnapshotHash(refs)
		if err != nil {
			t.Fatal(err)
		}
		req.SourceAttachmentIDs = []string{"att:1"}
		req.SourceSnapshotSHA256 = snapshot
		req.TranscriptRevision = audioanalysis.UniqueRevision(refs)
	} else {
		req.SourceAttachmentIDs = []string{}
		req.SourceSnapshotSHA256 = ""
	}
	return req
}

// TestContextAudioEnqueueCLI is the plan 4.1 real-CLI synthetic regression: an
// explicit enqueue must create the audio job, its running run row and the
// desired head with the computed run id; a same-request-id re-run is
// idempotent and a new request id supersedes the old run without touching the
// head beyond moving it to the new desired job.
func TestContextAudioEnqueueCLI(t *testing.T) {
	e := newCLIEnv(t)
	out, _, err := e.cliRun(t, "gen-1", "vad")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if len(out.Jobs) != 1 || !out.Jobs[0].Inserted {
		t.Fatalf("jobs = %+v", out.Jobs)
	}
	jobID, runID := out.Jobs[0].JobID, out.Jobs[0].RunID
	if jobID == "" || runID == "" {
		t.Fatalf("empty ids: %+v", out.Jobs[0])
	}
	if want := audioanalysis.ComputeRunID(e.expectedRequest(t, audioanalysis.CapabilityVAD), "gen-1"); runID != want {
		t.Fatalf("run_id = %q, want computed %q", runID, want)
	}
	db := e.store.DB()
	var stage, jobStatus string
	if err := db.QueryRow(`SELECT stage,status FROM processing_jobs WHERE id=?`, jobID).Scan(&stage, &jobStatus); err != nil {
		t.Fatal(err)
	}
	if stage != "audio_"+audioanalysis.CapabilityVAD || jobStatus != "queued" {
		t.Fatalf("job = %s/%s", stage, jobStatus)
	}
	var runCapability, runStatus string
	var active int
	if err := db.QueryRow(`SELECT capability,status,active FROM audio_analysis_runs WHERE job_id=?`, jobID).Scan(&runCapability, &runStatus, &active); err != nil {
		t.Fatal(err)
	}
	if runCapability != audioanalysis.CapabilityVAD || runStatus != "running" || active != 0 {
		t.Fatalf("run = %s/%s/%d", runCapability, runStatus, active)
	}
	var desired string
	var current []byte
	if err := db.QueryRow(`SELECT desired_job_id,current_run_id FROM audio_analysis_heads WHERE recording_id=? AND capability=?`,
		e.recording, audioanalysis.CapabilityVAD).Scan(&desired, &current); err != nil {
		t.Fatal(err)
	}
	if desired != jobID || current != nil {
		t.Fatalf("head = %s/%s", desired, current)
	}

	// The identical re-run replays the same job and run.
	repeat, _, err := e.cliRun(t, "gen-1", "vad")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(repeat.Jobs) != 1 || repeat.Jobs[0].Inserted || repeat.Jobs[0].JobID != jobID || repeat.Jobs[0].RunID != runID {
		t.Fatalf("replay = %+v, want idempotent %s/%s", repeat.Jobs, jobID, runID)
	}

	// A new request id forces a new run and supersedes the previous one.
	next, _, err := e.cliRun(t, "gen-2", "vad")
	if err != nil {
		t.Fatalf("second generation: %v", err)
	}
	if len(next.Jobs) != 1 || !next.Jobs[0].Inserted || next.Jobs[0].JobID == jobID {
		t.Fatalf("second generation jobs = %+v", next.Jobs)
	}
	var oldStatus string
	if err := db.QueryRow(`SELECT status FROM audio_analysis_runs WHERE id=?`, runID).Scan(&oldStatus); err != nil {
		t.Fatal(err)
	}
	if oldStatus != audioanalysis.StatusInvalidated {
		t.Fatalf("superseded run status = %q, want invalidated", oldStatus)
	}
	var desired2 string
	if err := db.QueryRow(`SELECT desired_job_id FROM audio_analysis_heads WHERE recording_id=? AND capability=?`,
		e.recording, audioanalysis.CapabilityVAD).Scan(&desired2); err != nil {
		t.Fatal(err)
	}
	if desired2 != next.Jobs[0].JobID {
		t.Fatalf("head desired = %s, want %s", desired2, next.Jobs[0].JobID)
	}
}

// TestContextAudioEnqueueCLISpeakerDependency pins the dependency contract of
// the manual speaker entry: without a desired diarization run the CLI refuses
// with audio_dependency_unavailable; with one the speaker job waits on the
// diarization job; replaying the same request id after the tagging head
// appears changes the parent set and must be audio_request_conflict.
func TestContextAudioEnqueueCLISpeakerDependency(t *testing.T) {
	e := newCLIEnv(t)
	_, code, err := e.cliRun(t, "gen-s1", "speaker")
	if err == nil {
		t.Fatal("speaker enqueue without diarization succeeded")
	}
	if code != audioanalysis.CodeDependencyUnavailable {
		t.Fatalf("error code = %q, want %q", code, audioanalysis.CodeDependencyUnavailable)
	}
	db := e.store.DB()
	var speakers int
	if err := db.QueryRow(`SELECT COUNT(*) FROM processing_jobs WHERE stage=?`, "audio_speaker").Scan(&speakers); err != nil {
		t.Fatal(err)
	}
	if speakers != 0 {
		t.Fatalf("rejected speaker enqueue left %d jobs", speakers)
	}

	diaz, _, err := e.cliRun(t, "gen-d", "diarization")
	if err != nil {
		t.Fatalf("diarization enqueue: %v", err)
	}
	speaker, _, err := e.cliRun(t, "gen-s2", "speaker")
	if err != nil {
		t.Fatalf("speaker enqueue: %v", err)
	}
	if len(speaker.Jobs) != 1 || !speaker.Jobs[0].Inserted {
		t.Fatalf("speaker jobs = %+v", speaker.Jobs)
	}
	var parents []string
	rows, err := db.Query(`SELECT dependency_id FROM processing_job_dependencies WHERE job_id=?`, speaker.Jobs[0].JobID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var parent string
		if err := rows.Scan(&parent); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		parents = append(parents, parent)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(parents) != 1 || parents[0] != diaz.Jobs[0].JobID {
		t.Fatalf("speaker parents = %v, want [%s]", parents, diaz.Jobs[0].JobID)
	}

	if _, _, err := e.cliRun(t, "gen-t", "tagging"); err != nil {
		t.Fatalf("tagging enqueue: %v", err)
	}
	// Same request id, but the parent set the CLI derives now differs.
	_, code, err = e.cliRun(t, "gen-s2", "speaker")
	if err == nil {
		t.Fatal("speaker replay with a changed parent set succeeded")
	}
	if code != audioanalysis.CodeRequestConflict {
		t.Fatalf("error code = %q, want %q", code, audioanalysis.CodeRequestConflict)
	}
	// The conflicting replay must not have created a second speaker job.
	var afterConflict int
	if err := db.QueryRow(`SELECT COUNT(*) FROM processing_jobs WHERE stage=?`, "audio_speaker").Scan(&afterConflict); err != nil {
		t.Fatal(err)
	}
	if afterConflict != 1 {
		t.Fatalf("conflicting speaker replay left %d speaker jobs, want 1", afterConflict)
	}
}
