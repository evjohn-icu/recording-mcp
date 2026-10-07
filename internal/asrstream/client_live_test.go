package asrstream

import (
	"bytes"
	"context"
	"os"
	"strconv"
	"testing"
	"time"
)

// TestLiveTranscribe runs one real recognition request against the service.
// It is skipped unless ASR_API_KEY is set; ASR_ENDPOINT selects the route
// (empty uses the plan full-duplex endpoint) and ASR_PCM selects the 16 kHz
// mono s16le sample (empty uses data/sample-validation/intro-90s-16k-mono.pcm).
func TestLiveTranscribe(t *testing.T) {
	key := os.Getenv("ASR_API_KEY")
	if key == "" {
		t.Skip("ASR_API_KEY not set; skipping live ASR probe")
	}
	endpoint := os.Getenv("ASR_ENDPOINT")
	pcmPath := os.Getenv("ASR_PCM")
	if pcmPath == "" {
		pcmPath = "../../data/sample-validation/intro-90s-16k-mono.pcm"
	}
	pcm, err := os.ReadFile(pcmPath)
	if err != nil {
		t.Fatalf("read sample: %v", err)
	}
	// Three seconds of audio is enough to prove auth, framing and results.
	if len(pcm) > 3*32000 {
		pcm = pcm[:3*32000]
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// ASR_PACKET_MS=0 (or negative) sends without real-time pacing.
	packet := 200 * time.Millisecond
	if v := os.Getenv("ASR_PACKET_MS"); v != "" {
		ms, err := strconv.Atoi(v)
		if err != nil {
			t.Fatalf("ASR_PACKET_MS=%q: %v", v, err)
		}
		if ms <= 0 {
			packet = -1
		} else {
			packet = time.Duration(ms) * time.Millisecond
		}
	}
	res, err := Transcribe(ctx, SessionConfig{
		APIKey:         key,
		Endpoint:       endpoint,
		PacketInterval: packet,
	}, bytes.NewReader(pcm))
	t.Logf("endpoint=%q request=%s connect=%s logid=%q frames=%d bytes=%d err=%v",
		endpoint, res.RequestID, res.ConnectID, res.ServerLogID, len(res.Frames), res.AudioBytes, err)
	for i, f := range res.Frames {
		t.Logf("frame %d: type=%d flags=%d last=%v code=%d payload=%s",
			i, f.MessageType, f.Flags, f.IsLastPackage, f.Code, truncate(string(f.Payload), 300))
	}
	if err != nil {
		t.Fatalf("Transcribe(%q): %v", endpoint, err)
	}
	if len(res.Frames) == 0 {
		t.Fatal("no response frames")
	}
	last := res.Frames[len(res.Frames)-1]
	if !last.IsLastPackage {
		t.Fatalf("last frame = %+v, want a final package", last)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
