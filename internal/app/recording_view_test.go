package app

import (
	"context"
	"testing"
)

func TestRecordingViewIncludesSourceMetadata(t *testing.T) {
	f := newFixture(t)
	if _, err := f.store.DB().Exec(`UPDATE recordings SET source_file='import.wav', source_bytes=12345, duration_ms=3000 WHERE conversation_id=?`, f.conversationID); err != nil {
		t.Fatal(err)
	}
	view, err := f.svc.ConversationView(context.Background(), f.conversationID)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Recordings) == 0 {
		t.Fatal("no recordings in view")
	}
	rec := view.Recordings[0]
	if rec.SourceFile != "import.wav" || rec.SourceBytes != 12345 || rec.DurationMS != 3000 {
		t.Fatalf("recording metadata: %+v", rec)
	}
}
