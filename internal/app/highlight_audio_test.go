package app

import (
	"context"
	"path/filepath"
	"recordingmcp/internal/memory"
	"testing"
)

func TestHighlightAudioUsesManagedAbsoluteOffsetsAndForgetFence(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	turn := f.turnRowID(t, "seg-a", "t-1")
	source, e := f.store.TranscriptSourceForTurn(ctx, turn)
	if e != nil {
		t.Fatal(e)
	}
	start, end := int64(1000), int64(1500)
	h, e := f.svc.CreateHighlight(ctx, memory.HighlightCreate{RequestID: "audio-highlight", RecordingID: f.recordingID, AnchorKind: "range", StartMS: &start, EndMS: &end, TurnID: turn, SourceAttachmentID: source.SourceAttachmentID, TranscriptRevision: source.Result.Source.TranscriptRevision, Tag: "important"})
	if e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(f.audioDir, "synthetic.pcm")
	writeAudioFile(t, path, 6000)
	f.insertSync(t, "ingested", path)
	clip, e := f.svc.HighlightAudioClip(ctx, h.ID)
	if e != nil {
		t.Fatal(e)
	}
	if clip.StartMS != 1000 || clip.EndMS != 1500 || clip.Size != 500*32 {
		t.Fatalf("wrong clip: %+v", clip)
	}
	clip.Close()
	// A narrow tombstone crossing the playback window must block audio even
	// before an interrupted cleanup has removed the highlight row.
	_, e = f.store.DB().Exec(`INSERT INTO forget_tombstones(id,scope,conversation_id,recording_id,requested_id,start_ms,end_ms,locator_key,expected_state_hash,redaction_done,status,created_at) VALUES('audio-fence','turn',?,?,'fixture',1200,1300,'audio-fence','fixture',0,'pending_external_cleanup',1)`, f.conversationID, f.recordingID)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.svc.HighlightAudioClip(ctx, h.ID); e == nil {
		t.Fatal("forgotten interval playable")
	}
}
