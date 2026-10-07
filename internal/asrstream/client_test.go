package asrstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestTranscribeAgainstStubService runs a full request against a stub service
// that speaks the wire protocol, so auth headers, frame sequencing, pacing
// and result decoding are covered without the network.
func TestTranscribeAgainstStubService(t *testing.T) {
	type observation struct {
		audioFrames int
		finalSeq    int32
		sawLast     bool
	}
	observed := make(chan observation, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Api-Key"); got != "test-key" {
			t.Errorf("X-Api-Key = %q, want test-key", got)
		}
		if got := r.Header.Get("X-Api-Resource-Id"); got != ResourceASR {
			t.Errorf("X-Api-Resource-Id = %q, want %q", got, ResourceASR)
		}
		if r.Header.Get("X-Api-Request-Id") == "" || r.Header.Get("X-Api-Connect-Id") == "" {
			t.Error("request/connect ids are missing")
		}
		w.Header().Set("X-Tt-Logid", "log-1") // ignored by the stub upgrader, kept for intent
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()

		_, raw, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("read request frame: %v", err)
			return
		}
		start, err := Parse(raw)
		if err != nil {
			t.Errorf("parse request frame: %v", err)
			return
		}
		if start.MessageType != TypeFullRequest || start.Sequence == nil || *start.Sequence != 1 {
			t.Errorf("request frame = %+v, want type 1 sequence 1", start)
		}
		if !strings.Contains(string(start.Payload), `"model_name":"bigmodel"`) ||
			!strings.Contains(string(start.Payload), `"rate":16000`) {
			t.Errorf("request payload = %s", start.Payload)
		}

		ack := buildServerFrame(t, TypeFullResponse, 0, serializationJSON, compressionGzip, 0, nil,
			[]byte(`{"result":{"additions":{"log_id":"log-1"}}}`))
		if err := conn.WriteMessage(websocket.BinaryMessage, ack); err != nil {
			t.Errorf("write ack: %v", err)
			return
		}

		var (
			seenAudioFrames int
			finalSequence   int32
			sawLastFlag     bool
		)
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
			if frame.MessageType != TypeAudioOnly {
				t.Errorf("audio frame type = %d, want 2", frame.MessageType)
			}
			seenAudioFrames++
			if frame.Flags&flagLastPackage != 0 {
				sawLastFlag = true
				if frame.Sequence != nil {
					finalSequence = *frame.Sequence
				}
				break
			}
		}
		observed <- observation{audioFrames: seenAudioFrames, finalSeq: finalSequence, sawLast: sawLastFlag}

		result := buildServerFrame(t, TypeFullResponse, flagHasSequence, serializationJSON, compressionGzip, 1, nil,
			[]byte(`{"result":{"text":"你好","utterances":[{"text":"你好","start_time":10,"end_time":500}]}}`))
		if err := conn.WriteMessage(websocket.BinaryMessage, result); err != nil {
			t.Errorf("write result: %v", err)
			return
		}
		final := buildServerFrame(t, TypeFullResponse, flagHasSequence|flagLastPackage, serializationJSON, compressionGzip, 2, nil,
			[]byte(`{"result":{"text":"你好","utterances":[{"text":"你好","start_time":10,"end_time":500,"definite":true}]}}`))
		if err := conn.WriteMessage(websocket.BinaryMessage, final); err != nil {
			t.Errorf("write final: %v", err)
			return
		}
	}))
	defer srv.Close()

	// Four chunks of 6400 bytes: the last read is short, so the client must
	// send five audio frames with the final flag on the fifth.
	audio := bytes.Repeat([]byte{0x01}, 4*6400+10)
	res, err := Transcribe(context.Background(), SessionConfig{
		APIKey:         "test-key",
		Endpoint:       "ws" + strings.TrimPrefix(srv.URL, "http"),
		PacketInterval: -1,
	}, bytes.NewReader(audio))
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	// The log id travels in a response header the real service sets; gorilla's
	// stub upgrader writes a fixed handshake, so that read path is covered by
	// the live probe instead (client_live_test.go).
	if res.AudioBytes != int64(len(audio)) {
		t.Errorf("audio bytes = %d, want %d", res.AudioBytes, len(audio))
	}
	obs := <-observed
	if !obs.sawLast || obs.finalSeq != -6 {
		t.Errorf("final audio frame: seen=%v sequence=%d, want seen with sequence -6", obs.sawLast, obs.finalSeq)
	}
	if obs.audioFrames != 5 {
		t.Errorf("audio frames = %d, want 5", obs.audioFrames)
	}
	if len(res.Frames) != 3 {
		t.Fatalf("frames = %d, want 3 (ack, result, final)", len(res.Frames))
	}
	last := res.Frames[len(res.Frames)-1]
	if !last.IsLastPackage || last.Code != 0 {
		t.Errorf("last frame = %+v, want a final package", last)
	}
	var payload struct {
		Result struct {
			Text string `json:"text"`
		} `json:"result"`
	}
	if err := json.Unmarshal(last.Payload, &payload); err != nil {
		t.Fatalf("decode final payload: %v", err)
	}
	if payload.Result.Text != "你好" {
		t.Errorf("final text = %q, want 你好", payload.Result.Text)
	}
}

// TestTranscribeSurfacesServerErrors keeps a service error frame a failure
// with its code, not a silent empty transcript.
func TestTranscribeSurfacesServerErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		ack := buildServerFrame(t, TypeFullResponse, 0, serializationJSON, compressionGzip, 0, nil, []byte(`{}`))
		_ = conn.WriteMessage(websocket.BinaryMessage, ack)
		code := int32(45000081)
		bad := buildServerFrame(t, TypeError, 0, serializationJSON, compressionGzip, 0, &code,
			[]byte(`{"error":"waiting next packet timeout"}`))
		_ = conn.WriteMessage(websocket.BinaryMessage, bad)
		// Drain until the client gives up so its writes do not break the
		// connection before it observes the error frame.
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := Transcribe(ctx, SessionConfig{
		APIKey:         "test-key",
		Endpoint:       "ws" + strings.TrimPrefix(srv.URL, "http"),
		PacketInterval: -1,
	}, bytes.NewReader(bytes.Repeat([]byte{0x02}, 6400)))
	var serverErr *ServerError
	if err == nil || !strings.Contains(err.Error(), "45000081") {
		t.Fatalf("err = %v, want a server error carrying code 45000081", err)
	}
	if !errors.As(err, &serverErr) {
		t.Errorf("err = %v, want *ServerError", err)
	}
}
