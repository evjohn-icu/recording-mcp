package app

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"path/filepath"
	"testing"
)

func TestWriteWAVHeaderLayout(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteWAVHeader(&buf, 32000); err != nil {
		t.Fatal(err)
	}
	b := buf.Bytes()
	if len(b) != 44 {
		t.Fatalf("header must be 44 bytes, got %d", len(b))
	}
	if string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" || string(b[12:16]) != "fmt " || string(b[36:40]) != "data" {
		t.Fatalf("unexpected magic: %q", b[:44])
	}
	checks := []struct {
		name string
		got  uint32
		want uint32
	}{
		{"riff size", binary.LittleEndian.Uint32(b[4:8]), 36 + 32000},
		{"fmt size", binary.LittleEndian.Uint32(b[16:20]), 16},
		{"byte rate", binary.LittleEndian.Uint32(b[28:32]), 32000},
		{"data size", binary.LittleEndian.Uint32(b[40:44]), 32000},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Fatalf("%s: got %d want %d", c.name, c.got, c.want)
		}
	}
	if got := binary.LittleEndian.Uint16(b[20:22]); got != 1 {
		t.Fatalf("audio format: got %d want PCM(1)", got)
	}
	if got := binary.LittleEndian.Uint16(b[22:24]); got != 1 {
		t.Fatalf("channels: got %d want 1", got)
	}
	if got := binary.LittleEndian.Uint32(b[24:28]); got != 16000 {
		t.Fatalf("sample rate: got %d want 16000", got)
	}
	if got := binary.LittleEndian.Uint16(b[32:34]); got != 2 {
		t.Fatalf("block align: got %d want 2", got)
	}
	if got := binary.LittleEndian.Uint16(b[34:36]); got != 16 {
		t.Fatalf("bits per sample: got %d want 16", got)
	}
}

func TestAudioClipReadsOnlyTheTurnRange(t *testing.T) {
	f := newFixture(t)
	path := filepath.Join(f.audioDir, "rec-provider-1.pcm")
	pcm := writeAudioFile(t, path, 30000) // 30s
	f.insertSync(t, "ingested", path)

	turnID := f.turnRowID(t, "seg-a", "t-1")
	clip, err := f.svc.AudioClip(context.Background(), turnID)
	if err != nil {
		t.Fatal(err)
	}
	defer clip.Close()
	if clip.StartMS != 1000 || clip.EndMS != 2000 {
		t.Fatalf("unexpected clip window: %+v", clip)
	}
	if clip.Size != 1000*pcmBytesPerMS {
		t.Fatalf("unexpected clip size: %d", clip.Size)
	}
	// The file handle must already sit at the clip start: reading Size bytes
	// has to equal the source slice, which catches a missing seek.
	got := make([]byte, clip.Size)
	if _, err := io.ReadFull(clip.PCM, got); err != nil {
		t.Fatal(err)
	}
	want := pcm[1000*pcmBytesPerMS : 2000*pcmBytesPerMS]
	if !bytes.Equal(got, want) {
		t.Fatal("served bytes do not match the requested slice (missing seek?)")
	}
	if pos, err := clip.PCM.Seek(0, io.SeekCurrent); err != nil || pos != 2000*pcmBytesPerMS {
		t.Fatalf("file position after the clip: %d (%v)", pos, err)
	}
}

func TestAudioClipClampsEndToFileSize(t *testing.T) {
	f := newFixture(t)
	path := filepath.Join(f.audioDir, "rec-provider-1.pcm")
	writeAudioFile(t, path, 1500) // shorter than the turn's 2000ms end
	f.insertSync(t, "ingested", path)
	clip, err := f.svc.AudioClip(context.Background(), f.turnRowID(t, "seg-a", "t-1"))
	if err != nil {
		t.Fatal(err)
	}
	defer clip.Close()
	if clip.Size != 500*pcmBytesPerMS {
		t.Fatalf("end was not clamped to the file: %d", clip.Size)
	}
}

func TestAudioClipFailureModes(t *testing.T) {
	ctx := context.Background()

	t.Run("offset unknown", func(t *testing.T) {
		f := newFixture(t)
		_, err := f.svc.AudioClip(ctx, f.turnRowID(t, "seg-b", "t-3"))
		e := appErr(t, err)
		if e.Code != CodeAudioOffsetUnknown || e.Status != 409 {
			t.Fatalf("unexpected error: %+v", e)
		}
	})

	t.Run("deleted recording", func(t *testing.T) {
		f := newFixture(t)
		f.insertSync(t, "deleted", "")
		_, err := f.svc.AudioClip(ctx, f.turnRowID(t, "seg-a", "t-1"))
		e := appErr(t, err)
		if e.Code != CodeAudioDeleted || e.Status != 410 {
			t.Fatalf("unexpected error: %+v", e)
		}
	})

	t.Run("file missing", func(t *testing.T) {
		f := newFixture(t)
		f.insertSync(t, "ingested", filepath.Join(f.audioDir, "somewhere.pcm"))
		_, err := f.svc.AudioClip(ctx, f.turnRowID(t, "seg-a", "t-1"))
		e := appErr(t, err)
		if e.Code != CodeAudioMissing || e.Status != 404 {
			t.Fatalf("unexpected error: %+v", e)
		}
	})

	t.Run("path escapes audio dir", func(t *testing.T) {
		f := newFixture(t)
		outside := filepath.Join(filepath.Dir(f.audioDir), "escape.pcm")
		writeAudioFile(t, outside, 30000)
		f.insertSync(t, "ingested", outside)
		_, err := f.svc.AudioClip(ctx, f.turnRowID(t, "seg-a", "t-1"))
		e := appErr(t, err)
		if e.Code != CodeAudioMissing || e.Status != 404 {
			t.Fatalf("unexpected error: %+v", e)
		}
	})

	t.Run("wrong suffix", func(t *testing.T) {
		f := newFixture(t)
		path := filepath.Join(f.audioDir, "rec-provider-1.wav")
		writeAudioFile(t, path, 30000)
		f.insertSync(t, "ingested", path)
		_, err := f.svc.AudioClip(ctx, f.turnRowID(t, "seg-a", "t-1"))
		e := appErr(t, err)
		if e.Code != CodeAudioMissing || e.Status != 404 {
			t.Fatalf("unexpected error: %+v", e)
		}
	})

	t.Run("no sync row", func(t *testing.T) {
		f := newFixture(t)
		_, err := f.svc.AudioClip(ctx, f.turnRowID(t, "seg-a", "t-1"))
		e := appErr(t, err)
		if e.Code != CodeAudioUnavailable || e.Status != 404 {
			t.Fatalf("unexpected error: %+v", e)
		}
	})

	t.Run("offset past end of file", func(t *testing.T) {
		f := newFixture(t)
		path := filepath.Join(f.audioDir, "rec-provider-1.pcm")
		writeAudioFile(t, path, 500) // 500ms file, turn starts at 1000ms
		f.insertSync(t, "ingested", path)
		_, err := f.svc.AudioClip(ctx, f.turnRowID(t, "seg-a", "t-1"))
		e := appErr(t, err)
		if e.Code != CodeAudioOffsetOutOfRange || e.Status != 409 {
			t.Fatalf("unexpected error: %+v", e)
		}
	})

	t.Run("empty turn window", func(t *testing.T) {
		f := newFixture(t)
		path := filepath.Join(f.audioDir, "rec-provider-1.pcm")
		writeAudioFile(t, path, 30000)
		f.insertSync(t, "ingested", path)
		turnID := f.turnRowID(t, "seg-a", "t-1")
		f.setRecordingTimes(t, turnID, 2000, 2000)
		_, err := f.svc.AudioClip(ctx, turnID)
		e := appErr(t, err)
		if e.Code != CodeAudioOffsetOutOfRange || e.Status != 409 {
			t.Fatalf("unexpected error: %+v", e)
		}
	})

	t.Run("unknown turn", func(t *testing.T) {
		f := newFixture(t)
		_, err := f.svc.AudioClip(ctx, "turn:nope")
		e := appErr(t, err)
		if e.Code != CodeNotFound || e.Status != 404 {
			t.Fatalf("unexpected error: %+v", e)
		}
	})
}

func TestConversationViewReportsRecordingAudioState(t *testing.T) {
	f := newFixture(t)
	path := filepath.Join(f.audioDir, "rec-provider-1.pcm")
	writeAudioFile(t, path, 30000)
	f.insertSync(t, "ingested", path)
	view, err := f.svc.ConversationView(context.Background(), f.conversationID)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Recordings) != 1 || view.Recordings[0].LocalAudio != AudioAvailable {
		t.Fatalf("unexpected recording audio state: %+v", view.Recordings)
	}
	page, err := f.svc.TurnPage(context.Background(), f.conversationID, f.recordingID, false, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range page.Items {
		if item.GlobalStartMS == nil {
			continue
		}
		if item.AudioStatus != AudioAvailable {
			t.Fatalf("turn %s should be playable, got %q", item.ID, item.AudioStatus)
		}
	}
}

func TestTurnPageAudioStatusOutOfRange(t *testing.T) {
	f := newFixture(t)
	path := filepath.Join(f.audioDir, "rec-provider-1.pcm")
	writeAudioFile(t, path, 500)
	f.insertSync(t, "ingested", path)
	page, err := f.svc.TurnPage(context.Background(), f.conversationID, f.recordingID, false, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range page.Items {
		if item.GlobalStartMS == nil {
			continue
		}
		if item.AudioStatus != AudioOutOfRange {
			t.Fatalf("turn %s: got %q want %q", item.ID, item.AudioStatus, AudioOutOfRange)
		}
	}
}
