package asrstream

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Plan-scoped service endpoints and resource ids. The pay-per-use
// https://ark.cn-beijing.volces.com/api/v3 endpoint is intentionally absent:
// the agent plan serves speech over these WebSocket URLs instead.
const (
	EndpointFullDuplex = "wss://openspeech.bytedance.com/api/v3/plan/sauc/bigmodel_async"
	EndpointStreamOut  = "wss://openspeech.bytedance.com/api/v3/plan/sauc/bigmodel_nostream"

	ResourceASR = "volc.seedasr.sauc.duration" // 豆包流式语音识别模型2.0
	ResourceTTS = "seed-tts-2.0"               // 豆包语音合成模型2.0
)

// SessionConfig configures one full-duplex recognition request.
type SessionConfig struct {
	APIKey         string        // X-Api-Key
	Endpoint       string        // WebSocket URL; empty uses EndpointFullDuplex
	ResourceID     string        // X-Api-Resource-Id; empty uses ResourceASR
	ModelName      string        // request.model_name; empty uses "bigmodel"
	UID            string        // request user uid; empty uses "recording-mcp"
	Rate           int           // sample rate in Hz; empty uses 16000
	Bits           int           // bits per sample; empty uses 16
	Channels       int           // channel count; empty uses 1
	PacketInterval time.Duration // real-time pacing between audio frames; empty uses 200ms, negative disables pacing
}

func (c SessionConfig) withDefaults() SessionConfig {
	if c.Endpoint == "" {
		c.Endpoint = EndpointFullDuplex
	}
	if c.ResourceID == "" {
		c.ResourceID = ResourceASR
	}
	if c.ModelName == "" {
		c.ModelName = "bigmodel"
	}
	if c.UID == "" {
		c.UID = "recording-mcp"
	}
	if c.Rate == 0 {
		c.Rate = 16000
	}
	if c.Bits == 0 {
		c.Bits = 16
	}
	if c.Channels == 0 {
		c.Channels = 1
	}
	if c.PacketInterval == 0 {
		c.PacketInterval = 200 * time.Millisecond
	}
	return c
}

// ChunkBytes is the size of one paced audio frame.
func (c SessionConfig) ChunkBytes() int {
	return c.Rate * (c.Bits / 8) * c.Channels * 200 / 1000
}

// Result is one completed recognition request: every parsed response frame
// plus the identifiers needed for the source envelope.
type Result struct {
	RequestID   string
	ConnectID   string
	ServerLogID string
	Frames      []Frame
	AudioBytes  int64
}

// ServerError reports a service error frame; the job is failed, not retried
// blindly, so callers can inspect the code.
type ServerError struct {
	Code    int64
	Payload string
}

func (e *ServerError) Error() string {
	return fmt.Sprintf("asrstream: server error code=%d payload=%s", e.Code, e.Payload)
}

// Transcribe runs one full-duplex recognition request: it opens the WebSocket,
// sends the request frame, streams the PCM audio in paced frames (the final
// frame carries the negated sequence), and collects every parsed response
// frame until the last package arrives.
func Transcribe(ctx context.Context, cfg SessionConfig, audio io.Reader) (Result, error) {
	cfg = cfg.withDefaults()
	if cfg.APIKey == "" {
		return Result{}, errors.New("asrstream: API key is required")
	}

	header := http.Header{}
	header.Set("X-Api-Key", cfg.APIKey)
	header.Set("X-Api-Resource-Id", cfg.ResourceID)
	requestID, connectID := randomID(), randomID()
	header.Set("X-Api-Request-Id", requestID)
	header.Set("X-Api-Connect-Id", connectID)
	header.Set("X-Api-Sequence", "-1")

	// websocket.Dialer's zero value does not read the environment, unlike
	// websocket.DefaultDialer: keep HTTPS_PROXY support so dialing behaves like
	// the rest of the local tooling.
	dialer := websocket.Dialer{
		HandshakeTimeout: 30 * time.Second,
		Proxy:            http.ProxyFromEnvironment,
	}
	conn, resp, err := dialer.DialContext(ctx, cfg.Endpoint, header)
	if err != nil {
		return Result{}, fmt.Errorf("asrstream: dial %s: %w", cfg.Endpoint, err)
	}
	defer conn.Close()
	serverLogID := resp.Header.Get("X-Tt-Logid")

	var (
		mu          sync.Mutex
		frames      []Frame
		lastPackage bool
		fatal       error
		sent        int64
	)
	ack := make(chan struct{})
	final := make(chan struct{})
	var ackOnce, finalOnce sync.Once
	fail := func(err error) {
		mu.Lock()
		if fatal == nil {
			fatal = err
		}
		mu.Unlock()
		finalOnce.Do(func() { close(final) })
	}
	// result snapshots the identifiers and every frame received so far, so a
	// failed request still yields the evidence needed to diagnose it.
	result := func() Result {
		mu.Lock()
		defer mu.Unlock()
		return Result{
			RequestID:   requestID,
			ConnectID:   connectID,
			ServerLogID: serverLogID,
			Frames:      append([]Frame(nil), frames...),
			AudioBytes:  sent,
		}
	}

	go func() {
		for {
			messageType, raw, err := conn.ReadMessage()
			if err != nil {
				mu.Lock()
				seen := lastPackage
				mu.Unlock()
				if !seen {
					fail(fmt.Errorf("asrstream: read response: %w", err))
				} else {
					finalOnce.Do(func() { close(final) })
				}
				return
			}
			if messageType != websocket.BinaryMessage {
				continue
			}
			frame, err := Parse(raw)
			if err != nil {
				fail(err)
				return
			}
			mu.Lock()
			frames = append(frames, frame)
			mu.Unlock()
			if frame.MessageType == TypeFullResponse {
				ackOnce.Do(func() { close(ack) })
			}
			if frame.MessageType == TypeError || frame.Code != 0 {
				payload := ""
				if len(frame.Payload) > 0 {
					payload = string(frame.Payload)
				}
				fail(&ServerError{Code: frame.Code, Payload: payload})
				return
			}
			if frame.IsLastPackage {
				mu.Lock()
				lastPackage = true
				mu.Unlock()
				finalOnce.Do(func() { close(final) })
				return
			}
		}
	}()

	request := map[string]any{
		"user": map[string]any{"uid": cfg.UID},
		"audio": map[string]any{
			"format": "pcm", "codec": "raw",
			"rate": cfg.Rate, "bits": cfg.Bits, "channel": cfg.Channels,
		},
		"request": map[string]any{
			"model_name": cfg.ModelName, "enable_nonstream": true,
			"enable_speaker_info": true, "show_utterances": true,
			"result_type": "full", "enable_itn": true, "enable_punc": true,
		},
	}
	full, err := EncodeFull(1, request)
	if err != nil {
		return Result{}, err
	}
	if err := conn.WriteMessage(websocket.BinaryMessage, full); err != nil {
		return Result{}, fmt.Errorf("asrstream: send request: %w", err)
	}

	select {
	case <-ack:
	case <-ctx.Done():
		return result(), fmt.Errorf("asrstream: waiting for init ack: %w", ctx.Err())
	case <-time.After(20 * time.Second):
		return result(), errors.New("asrstream: init ack timeout")
	}

	// The service fails the request on a server error frame; stop feeding it.
	mu.Lock()
	pending := fatal
	mu.Unlock()
	if pending != nil {
		<-final
		return result(), pending
	}

	chunk := make([]byte, cfg.ChunkBytes())
	start := time.Now()
	for seq := int32(2); ; seq++ {
		n, err := io.ReadFull(audio, chunk)
		last := errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
		if err != nil && !last {
			return result(), fmt.Errorf("asrstream: read audio: %w", err)
		}
		// The final frame is always sent, even with an empty tail: it carries
		// the last-package flag and the negated sequence that end the stream.
		frame, err := EncodeAudio(seq, chunk[:n], last)
		if err != nil {
			return result(), err
		}
		if err := conn.WriteMessage(websocket.BinaryMessage, frame); err != nil {
			return result(), fmt.Errorf("asrstream: send audio: %w", err)
		}
		sent += int64(n)
		if last {
			break
		}
		// A server error frame fails the request; stop feeding the stream
		// instead of writing into a session the service already ended.
		mu.Lock()
		pending := fatal
		mu.Unlock()
		if pending != nil {
			<-final
			return result(), pending
		}
		if cfg.PacketInterval > 0 {
			due := start.Add(time.Duration(seq-1) * cfg.PacketInterval)
			if wait := time.Until(due); wait > 0 {
				select {
				case <-time.After(wait):
				case <-ctx.Done():
					return result(), ctx.Err()
				}
			}
		}
	}

	// The tail deadline scales with audio length, like the reference client.
	timeout := 60 * time.Second
	if scaled := time.Duration(float64(sent)/32000*0.3) * time.Second; scaled > timeout {
		timeout = scaled
	}
	select {
	case <-final:
	case <-ctx.Done():
		return result(), fmt.Errorf("asrstream: waiting for final package: %w", ctx.Err())
	case <-time.After(timeout):
		return result(), errors.New("asrstream: final package timeout")
	}

	mu.Lock()
	failed, complete := fatal, lastPackage
	mu.Unlock()
	if failed != nil {
		return result(), failed
	}
	if !complete {
		return result(), errors.New("asrstream: final package missing")
	}
	return result(), nil
}

func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("asrstream: random id: %v", err))
	}
	return hex.EncodeToString(b[:])
}
