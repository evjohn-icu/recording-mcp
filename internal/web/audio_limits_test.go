package web

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"recordingmcp/internal/app"
	"recordingmcp/internal/memory"
)

func TestAudioRejectsOversizedWAVBeforeSuccess(t *testing.T) {
	env := newTestEnv(t, "")
	page, err := env.store.Query(context.Background(), memory.Query{ConversationID: env.convID, IncludeTurns: true})
	if err != nil {
		t.Fatal(err)
	}
	var turnID string
	for _, item := range page.Items {
		if item.Kind == "turn" {
			turnID = item.ID
			break
		}
	}
	if turnID == "" {
		t.Fatal("no fixture turn")
	}

	// A sparse PCM fixture avoids allocating or writing multiple gigabytes.
	const pcmSize int64 = 1<<32 + 32
	pcmPath := filepath.Join(env.audioDir, "oversized.pcm")
	f, err := os.Create(pcmPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(pcmSize); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	// Extend the synthetic turn to cover the oversized sparse PCM. Keep the
	// provenance bridge and provider sync row consistent with its recording.
	endMS := pcmSize / 32
	if _, err := env.store.DB().Exec(`UPDATE turns SET global_start_ms=0, global_end_ms=? WHERE id=?`, endMS, turnID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.DB().Exec(`INSERT INTO source_attachments(id,conversation_id,recording_id,segment_index,segment_id,transcript_revision,source_json,result_json,content_sha256,active,supersedes_revision)
		VALUES('att-oversized-audio',?,?,'1','seg-oversized','rev-oversized','{"device_id":"dev-1","recording_id":"rec-1"}','{}','x',1,NULL)`, env.convID, memory.RecordingID("dev-1", "rec-1")); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.DB().Exec(`INSERT INTO sync_recordings(recording_id,owner,device_id,state,stitched_path,updated_at) VALUES('rec-1','default','dev-1','ingested',?,1)`, pcmPath); err != nil {
		t.Fatal(err)
	}

	status, data := env.do(t, http.MethodGet, "/api/turns/"+turnID+"/audio", "", nil)
	if status != http.StatusBadRequest {
		t.Fatalf("oversized WAV clip: got status %d, want 400 (body %s)", status, data)
	}
	if code := decodeError(t, data).Code; code != app.CodeValidationFailed {
		t.Fatalf("oversized WAV error code = %q, want %q", code, app.CodeValidationFailed)
	}
}
