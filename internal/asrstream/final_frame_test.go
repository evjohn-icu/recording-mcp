package asrstream

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

// TestTranscribeSendsFinalFrameForExactMultipleChunks pins the bug that made
// real requests hang: when the audio length is an exact multiple of the chunk
// size, the last full chunk reads with err==nil and the next read is an empty
// EOF, so a client that only flags "short" reads as final never sends the
// last-package frame and the service waits out its idle watchdog.
func TestTranscribeSendsFinalFrameForExactMultipleChunks(t *testing.T) {
	type finalFrame struct {
		audioFrames int
		lastSeq     int32
		lastPayload int
	}
	observed := make(chan finalFrame, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil { // full request frame
			return
		}
		ack := buildServerFrame(t, TypeFullResponse, 0, serializationJSON, compressionGzip, 0, nil, []byte(`{}`))
		if err := conn.WriteMessage(websocket.BinaryMessage, ack); err != nil {
			return
		}
		var obs finalFrame
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				t.Errorf("read audio frame: %v", err)
				return
			}
			frame, err := Parse(raw)
			if err != nil {
				t.Errorf("parse audio frame: %v", err)
				return
			}
			obs.audioFrames++
			if frame.Flags&flagLastPackage != 0 {
				if frame.Sequence != nil {
					obs.lastSeq = *frame.Sequence
				}
				obs.lastPayload = len(frame.PayloadBase64)
				break
			}
		}
		observed <- obs
		final := buildServerFrame(t, TypeFullResponse, flagHasSequence|flagLastPackage, serializationJSON, compressionGzip, 2, nil,
			[]byte(`{"result":{"text":"done"}}`))
		_ = conn.WriteMessage(websocket.BinaryMessage, final)
	}))
	defer srv.Close()

	// Exactly two chunks: the third read is an empty EOF.
	audio := bytes.Repeat([]byte{0x03}, 2*6400)
	res, err := Transcribe(context.Background(), SessionConfig{
		APIKey:         "test-key",
		Endpoint:       "ws" + strings.TrimPrefix(srv.URL, "http"),
		PacketInterval: -1,
	}, bytes.NewReader(audio))
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	obs := <-observed
	if obs.audioFrames != 3 {
		t.Errorf("audio frames = %d, want 3 (two data frames plus the empty final frame)", obs.audioFrames)
	}
	if obs.lastSeq != -4 {
		t.Errorf("final sequence = %d, want -4", obs.lastSeq)
	}
	if obs.lastPayload != 0 {
		t.Errorf("final frame carried %d base64 payload chars, want an empty tail", obs.lastPayload)
	}
	if len(res.Frames) == 0 || !res.Frames[len(res.Frames)-1].IsLastPackage {
		t.Fatalf("frames = %+v, want a last-package response", res.Frames)
	}
}
