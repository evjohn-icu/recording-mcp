package app

import (
	"bytes"
	"context"
	"io"
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
