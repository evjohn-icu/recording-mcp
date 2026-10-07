package asrstream

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"strings"
	"testing"
)

// buildServerFrame assembles wire bytes the way the service does, so Parse is
// tested against the documented layout rather than against its own encoder.
func buildServerFrame(t *testing.T, messageType, flags, serialization, compression int, seq int32, code *int32, payload []byte) []byte {
	t.Helper()
	out := []byte{0x11, byte(messageType<<4 | flags), byte(serialization<<4 | compression), 0x00}
	if flags&flagHasSequence != 0 {
		out = binary.BigEndian.AppendUint32(out, uint32(seq))
	}
	if code != nil {
		out = binary.BigEndian.AppendUint32(out, uint32(*code))
	}
	body := payload
	if compression == compressionGzip {
		var z bytes.Buffer
		w := gzip.NewWriter(&z)
		if _, err := w.Write(payload); err != nil {
			t.Fatalf("compress payload: %v", err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("compress payload: %v", err)
		}
		body = z.Bytes()
	}
	if messageType == TypeFullResponse || messageType == TypeError {
		out = binary.BigEndian.AppendUint32(out, uint32(len(body)))
	}
	return append(out, body...)
}

func TestEncodeFullLayout(t *testing.T) {
	raw, err := EncodeFull(7, map[string]any{"ping": true})
	if err != nil {
		t.Fatalf("EncodeFull: %v", err)
	}
	if !bytes.Equal(raw[:4], []byte{0x11, 0x11, 0x11, 0x00}) {
		t.Fatalf("header = %x, want 11 11 11 00", raw[:4])
	}
	if got := int32(binary.BigEndian.Uint32(raw[4:8])); got != 7 {
		t.Errorf("sequence = %d, want 7", got)
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw[12:]))
	if err != nil {
		t.Fatalf("payload is not gzip: %v", err)
	}
	defer zr.Close()
	var body bytes.Buffer
	if _, err := body.ReadFrom(zr); err != nil {
		t.Fatalf("read payload: %v", err)
	}
	if body.String() != `{"ping":true}` {
		t.Errorf("payload = %q, want {\"ping\":true}", body.String())
	}
	if size := binary.BigEndian.Uint32(raw[8:12]); size != uint32(len(raw)-12) {
		t.Errorf("payload size = %d, want %d", size, len(raw)-12)
	}
}

func TestEncodeAudioFrames(t *testing.T) {
	chunk := []byte{0x01, 0x02, 0x03}
	normal, err := EncodeAudio(3, chunk, false)
	if err != nil {
		t.Fatalf("EncodeAudio: %v", err)
	}
	if normal[1] != 0x21 {
		t.Errorf("audio frame type/flags = %#02x, want 0x21", normal[1])
	}
	if got := int32(binary.BigEndian.Uint32(normal[4:8])); got != 3 {
		t.Errorf("sequence = %d, want 3", got)
	}
	final, err := EncodeAudio(3, chunk, true)
	if err != nil {
		t.Fatalf("EncodeAudio final: %v", err)
	}
	if final[1] != 0x23 {
		t.Errorf("final frame type/flags = %#02x, want 0x23 (last package)", final[1])
	}
	if got := int32(binary.BigEndian.Uint32(final[4:8])); got != -3 {
		t.Errorf("final sequence = %d, want -3 (negated)", got)
	}
}

func TestParseFullResponseFrame(t *testing.T) {
	raw := buildServerFrame(t, TypeFullResponse, flagHasSequence|flagLastPackage,
		serializationJSON, compressionGzip, 42, nil, []byte(`{"result":{"text":"你好"}}`))
	got, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.MessageType != TypeFullResponse || got.Flags != 3 || !got.IsLastPackage {
		t.Errorf("frame = %+v, want type 9 flags 3 last package", got)
	}
	if got.Sequence == nil || *got.Sequence != 42 {
		t.Errorf("sequence = %v, want 42", got.Sequence)
	}
	if got.Code != 0 {
		t.Errorf("code = %d, want 0", got.Code)
	}
	if string(got.Payload) != `{"result":{"text":"你好"}}` {
		t.Errorf("payload = %s", got.Payload)
	}
	if !json.Valid(got.Payload) {
		t.Error("payload is not valid JSON")
	}
}

func TestParseErrorFrame(t *testing.T) {
	code := int32(45000001)
	raw := buildServerFrame(t, TypeError, flagHasSequence, serializationJSON, compressionGzip,
		-1, &code, []byte(`{"error":"bad request"}`))
	got, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.MessageType != TypeError || got.Code != 45000001 {
		t.Errorf("frame = %+v, want error frame with code 45000001", got)
	}
	if string(got.Payload) != `{"error":"bad request"}` {
		t.Errorf("payload = %s", got.Payload)
	}
}

func TestParseBase64Fallback(t *testing.T) {
	raw := buildServerFrame(t, TypeFullResponse, flagHasSequence, 2, 0, 1, nil, []byte{0xff, 0xfe})
	got, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.PayloadBase64 != base64.StdEncoding.EncodeToString([]byte{0xff, 0xfe}) {
		t.Errorf("payload_base64 = %q, want %q", got.PayloadBase64, base64.StdEncoding.EncodeToString([]byte{0xff, 0xfe}))
	}
	if got.Payload != nil {
		t.Errorf("payload = %s, want none for non-JSON serialization", got.Payload)
	}
}

func TestParseRejectsMalformedFrames(t *testing.T) {
	cases := map[string][]byte{
		"empty":            {},
		"short header":     {0x11, 0x91},
		"header too large": {0x1f, 0x91, 0x11, 0x00, 0x00},
	}
	for name, raw := range cases {
		if _, err := Parse(raw); err == nil {
			t.Errorf("%s: Parse accepted malformed frame", name)
		}
	}
	// A response frame whose gzip payload is truncated.
	raw := buildServerFrame(t, TypeFullResponse, flagHasSequence, serializationJSON, compressionGzip, 1, nil, []byte(`{}`))
	if _, err := Parse(raw[:len(raw)-2]); err == nil {
		t.Error("truncated gzip payload accepted")
	}
	if _, err := Parse(buildServerFrame(t, TypeFullResponse, flagHasSequence, serializationJSON, 0, 1, nil, []byte(`{oops`))); err == nil ||
		!strings.Contains(err.Error(), "valid JSON") {
		t.Errorf("invalid JSON payload err = %v, want a JSON error", err)
	}
}
