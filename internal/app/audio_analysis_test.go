package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"recordingmcp/internal/audioanalysis"
)

// seedAudioRun inserts one current, succeeded pure-acoustic run with one event.
func seedAudioRun(t *testing.T, f *fixture) string {
	t.Helper()
	db := f.store.DB()
	if _, err := db.Exec(`INSERT INTO processing_jobs(id,stage,input_snapshot_id,idempotency_key,status,priority,payload_json,max_attempts,attempt_count,available_at,created_at,updated_at)
 VALUES('job:audio','audio_vad',?,'audio-key','succeeded',0,'{}',3,1,1,1,1)`, f.recordingID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO audio_analysis_runs(id,job_id,recording_id,source_audio_sha256,input_sha256,source_attachment_ids_json,source_snapshot_sha256,capability,start_ms,end_ms,model_id,model_revision,config_sha256,generation,status,active,result_json,created_at,updated_at)
 VALUES('run:1','job:audio',?,?,?,'[]','','vad',0,30000,'silero','rev-1',?,'gen-1','succeeded',1,'{}',1,1)`,
		f.recordingID, repeatHash("a"), repeatHash("b"), repeatHash("c")); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO audio_analysis_heads(recording_id,capability,desired_job_id,current_run_id) VALUES(?,'vad','job:audio','run:1')`, f.recordingID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO audio_analysis_events(id,run_id,event_type,start_ms,end_ms,confidence,payload_json)
 VALUES('ev:1','run:1','speech_activity',1000,2000,0.9,'{"state":"speech"}')`); err != nil {
		t.Fatal(err)
	}
	return "ev:1"
}

func repeatHash(value string) string {
	out := ""
	for len(out) < 64 {
		out += value
	}
	return out[:64]
}

// TestEventAudioClipReadsExactRange proves the event endpoint's core slice:
// the clip is the event's recording-clock window and reading it yields exactly
// the source PCM bytes.
func TestEventAudioClipReadsExactRange(t *testing.T) {
	f := newFixture(t)
	path := filepath.Join(f.audioDir, "rec-provider-1.pcm")
	pcm := writeAudioFile(t, path, 30000)
	f.insertSync(t, "ingested", path)
	eventID := seedAudioRun(t, f)
	f.svc.AudioRuns = audioanalysis.NewStore(f.store.DB())

	clip, err := f.svc.EventAudioClip(context.Background(), eventID)
	if err != nil {
		t.Fatalf("EventAudioClip: %v", err)
	}
	defer clip.Close()
	if clip.StartMS != 1000 || clip.EndMS != 2000 || clip.Size != 1000*pcmBytesPerMS {
		t.Fatalf("clip = %+v", clip)
	}
	got := make([]byte, clip.Size)
	if _, err := io.ReadFull(clip.PCM, got); err != nil {
		t.Fatal(err)
	}
	want := pcm[1000*pcmBytesPerMS : 2000*pcmBytesPerMS]
	if !bytes.Equal(got, want) {
		t.Fatal("event clip bytes do not match the recording window")
	}
	// A permanent forget makes the same event unreadable.
	if _, err := f.store.DB().Exec(`INSERT INTO forget_tombstones(id,scope,recording_id,requested_id,locator_key,expected_state_hash,redaction_done,status,created_at)
 VALUES('forget:audio','turn',?,'t','locator:audio','hash',0,'pending_external_cleanup',1)`, f.recordingID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.EventAudioClip(context.Background(), eventID); err == nil {
		t.Fatal("forgotten event remained playable")
	}
}

// TestAudioAnalysisViewReportsCapabilityStates pins the projection states a
// consumer sees when no run is current.
func TestAudioAnalysisViewReportsCapabilityStates(t *testing.T) {
	f := newFixture(t)
	f.svc.AudioRuns = audioanalysis.NewStore(f.store.DB())
	view, err := f.svc.AudioAnalysis(context.Background(), f.recordingID)
	if err != nil {
		t.Fatalf("AudioAnalysis: %v", err)
	}
	if len(view.Capabilities) != len(audioanalysis.Capabilities) {
		t.Fatalf("capabilities = %+v", view.Capabilities)
	}
	for _, capability := range view.Capabilities {
		if capability.Status != "disabled" {
			t.Fatalf("capability %s status = %q, want disabled without a config", capability.Capability, capability.Status)
		}
	}
}

// Plan 4.7: the Web/MCP audio projection and the event clip endpoint filter
// by the model identity (model_id, model_revision, config_sha256) that
// AudioConfig.Resolve produces right now. A succeeded run from an older
// configuration reads as invalidated and its events stop being playable;
// disabled and unavailable configurations keep their original statuses.

func modelDigest(t *testing.T, data []byte) string {
	t.Helper()
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func audioCapabilityEntry(enabled bool, modelPath, digest string) map[string]any {
	return map[string]any{
		"enabled":        enabled,
		"model_id":       "campplus",
		"model_revision": "rev-2",
		"files":          []map[string]string{{"path": modelPath, "sha256": digest}},
		"device":         "cpu",
		"frontend_id":    "campplus",
	}
}

// audioModelConfig builds a real, resolvable audio analysis config in a
// temporary directory; missing=true leaves the pinned model file absent.
func audioModelConfig(t *testing.T, missing bool) (*audioanalysis.Config, audioanalysis.Resolved) {
	t.Helper()
	dir := t.TempDir()
	model := filepath.Join(dir, "campplus.onnx")
	modelBytes := []byte("campplus-model-bytes")
	digest := modelDigest(t, modelBytes)
	if !missing {
		if err := os.WriteFile(model, modelBytes, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	document := map[string]any{
		"schema_version": "audio-analysis-config-v1",
		"capabilities":   map[string]any{"vad": audioCapabilityEntry(true, model, digest)},
	}
	documentBytes, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "audio-config.json")
	if err := os.WriteFile(configPath, documentBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := audioanalysis.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	resolved, err := cfg.Resolve("vad")
	if missing {
		if audioanalysis.ErrorCodeOf(err) != audioanalysis.CodeModelUnavailable {
			t.Fatalf("Resolve with a missing model file = %v, want %s", err, audioanalysis.CodeModelUnavailable)
		}
		return cfg, audioanalysis.Resolved{}
	}
	if err != nil {
		t.Fatalf("Resolve(vad): %v", err)
	}
	return cfg, resolved
}

// disabledAudioConfig builds a config whose vad capability is switched off.
func disabledAudioConfig(t *testing.T) *audioanalysis.Config {
	t.Helper()
	dir := t.TempDir()
	document := map[string]any{
		"schema_version": "audio-analysis-config-v1",
		"capabilities":   map[string]any{"vad": map[string]any{"enabled": false, "files": []any{}}},
	}
	documentBytes, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "audio-config.json")
	if err := os.WriteFile(configPath, documentBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := audioanalysis.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	return cfg
}

// seedAudioRunIdentity seeds one current succeeded run with an explicit model
// identity and returns its event id.
func seedAudioRunIdentity(t *testing.T, f *fixture, modelID, modelRevision, configSHA, suffix string) string {
	t.Helper()
	db := f.store.DB()
	jobID, runID, eventID := "job:audio-"+suffix, "run:"+suffix, "ev:"+suffix
	if _, err := db.Exec(`INSERT INTO processing_jobs(id,stage,input_snapshot_id,idempotency_key,status,priority,payload_json,max_attempts,attempt_count,available_at,created_at,updated_at)
 VALUES(?, 'audio_vad', ?, ?, 'succeeded', 0, '{}', 3, 1, 1, 1, 1)`, jobID, f.recordingID, "audio-key-"+suffix); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO audio_analysis_runs(id,job_id,recording_id,source_audio_sha256,input_sha256,source_attachment_ids_json,source_snapshot_sha256,capability,start_ms,end_ms,model_id,model_revision,config_sha256,generation,status,active,result_json,created_at,updated_at)
 VALUES(?,?,?,?,?,'[]','','vad',0,30000,?,?,?,?,'succeeded',1,'{}',1,1)`,
		runID, jobID, f.recordingID, repeatHash("a"), repeatHash("b"), modelID, modelRevision, configSHA, "gen-"+suffix); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO audio_analysis_heads(recording_id,capability,desired_job_id,current_run_id) VALUES(?,'vad',?,?)`, f.recordingID, jobID, runID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO audio_analysis_events(id,run_id,event_type,start_ms,end_ms,confidence,payload_json)
 VALUES(?,?,'speech_activity',1000,2000,0.9,'{"state":"speech"}')`, eventID, runID); err != nil {
		t.Fatal(err)
	}
	return eventID
}

func capabilityEntry(t *testing.T, view AudioAnalysisView, capability string) AudioCapabilityView {
	t.Helper()
	for _, entry := range view.Capabilities {
		if entry.Capability == capability {
			return entry
		}
	}
	t.Fatalf("capability %s missing from view %+v", capability, view.Capabilities)
	return AudioCapabilityView{}
}

// TestAudioAnalysisInvalidatesRunsFromOlderModelConfig proves a succeeded run
// produced by an older model configuration no longer reads as succeeded and
// its events are no longer playable once the config resolves a new identity.
func TestAudioAnalysisInvalidatesRunsFromOlderModelConfig(t *testing.T) {
	f := newFixture(t)
	cfg, _ := audioModelConfig(t, false)
	eventID := seedAudioRunIdentity(t, f, "silero", "rev-1", repeatHash("c"), "old")
	f.svc.AudioRuns = audioanalysis.NewStore(f.store.DB())
	f.svc.AudioConfig = cfg
	// Managed PCM provenance so the old event is otherwise fully playable.
	path := filepath.Join(f.audioDir, "rec-provider-1.pcm")
	writeAudioFile(t, path, 30000)
	f.insertSync(t, "ingested", path)
	view, err := f.svc.AudioAnalysis(context.Background(), f.recordingID)
	if err != nil {
		t.Fatalf("AudioAnalysis: %v", err)
	}
	if entry := capabilityEntry(t, view, "vad"); entry.Status != "invalidated" {
		t.Fatalf("capability status = %q, want invalidated: a run from an older model config must not read as succeeded", entry.Status)
	}
	for _, run := range view.Runs {
		if run.Capability == "vad" && run.Status == "succeeded" {
			t.Fatalf("older model run %s surfaced as succeeded", run.RunID)
		}
	}
	if _, err := f.svc.EventAudioClip(context.Background(), eventID); err == nil {
		t.Fatal("an event of an older model run remained playable")
	} else {
		var appErr *Error
		if !errors.As(err, &appErr) || appErr.Status != 404 {
			t.Fatalf("event clip error = %v, want a 404 for an event of an older model run", err)
		}
	}
}

// TestAudioAnalysisKeepsRunMatchingCurrentModelConfig proves the filter only
// removes runs whose identity no longer matches: the exact resolved config
// still reports succeeded and plays its events.
func TestAudioAnalysisKeepsRunMatchingCurrentModelConfig(t *testing.T) {
	f := newFixture(t)
	cfg, resolved := audioModelConfig(t, false)
	eventID := seedAudioRunIdentity(t, f, resolved.ModelID, resolved.ModelRevision, resolved.ConfigSHA256, "match")
	f.svc.AudioRuns = audioanalysis.NewStore(f.store.DB())
	f.svc.AudioConfig = cfg
	path := filepath.Join(f.audioDir, "rec-provider-1.pcm")
	writeAudioFile(t, path, 30000)
	f.insertSync(t, "ingested", path)
	view, err := f.svc.AudioAnalysis(context.Background(), f.recordingID)
	if err != nil {
		t.Fatalf("AudioAnalysis: %v", err)
	}
	if entry := capabilityEntry(t, view, "vad"); entry.Status != "succeeded" {
		t.Fatalf("capability status = %q, want succeeded for the exact resolved config", entry.Status)
	}
	clip, err := f.svc.EventAudioClip(context.Background(), eventID)
	if err != nil {
		t.Fatalf("EventAudioClip: %v", err)
	}
	defer clip.Close()
	if clip.StartMS != 1000 || clip.EndMS != 2000 || clip.Size != 1000*pcmBytesPerMS {
		t.Fatalf("clip = %+v", clip)
	}
}

// TestAudioAnalysisReportsConfigStatesWithoutValidModel pins the original
// statuses when the config is present but cannot resolve a model.
func TestAudioAnalysisReportsConfigStatesWithoutValidModel(t *testing.T) {
	f := newFixture(t)
	cfg, _ := audioModelConfig(t, true)
	f.svc.AudioRuns = audioanalysis.NewStore(f.store.DB())
	f.svc.AudioConfig = cfg
	view, err := f.svc.AudioAnalysis(context.Background(), f.recordingID)
	if err != nil {
		t.Fatalf("AudioAnalysis: %v", err)
	}
	if entry := capabilityEntry(t, view, "vad"); entry.Status != "unavailable" {
		t.Fatalf("capability status = %q, want unavailable without the pinned model file", entry.Status)
	}

	f2 := newFixture(t)
	f2.svc.AudioRuns = audioanalysis.NewStore(f2.store.DB())
	f2.svc.AudioConfig = disabledAudioConfig(t)
	view2, err := f2.svc.AudioAnalysis(context.Background(), f2.recordingID)
	if err != nil {
		t.Fatalf("AudioAnalysis: %v", err)
	}
	if entry := capabilityEntry(t, view2, "vad"); entry.Status != "disabled" {
		t.Fatalf("capability status = %q, want disabled for a switched-off capability", entry.Status)
	}
}
