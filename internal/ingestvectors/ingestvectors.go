// Package ingestvectors deterministically builds the cross-language test
// vectors published under contracts/ingest-v1/vectors. The generator is the
// single source of truth; Go, TypeScript and Kotlin tests all compare against
// its output (see internal/cloudsync and apps/cloud-ingest drift tests).
package ingestvectors

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
)

// Protocol constants shared with contracts/ingest-v1/openapi.yaml.
const (
	ProtocolVersion = "ingest-v1"
	Codec           = "pcm_s16le"
	SampleRate      = 16000
	Channels        = 1
	OwnerDefault    = "default"
	VectorRecording = "7e5a9c3f-2b1d-4e6f-8a9b-0c1d2e3f4a5b"
)

// File is one generated vector artifact.
type File struct {
	Name string
	Data string // JSON document terminated by a newline
}

// Sample generates one S16LE little-endian sample.
type Sample func(i int) int16

// PCM builds sample_count*2 bytes of little-endian S16LE audio.
func PCM(count int, fn Sample) []byte {
	buf := make([]byte, 0, count*2)
	for i := 0; i < count; i++ {
		s := fn(i)
		buf = append(buf, byte(s), byte(s>>8))
	}
	return buf
}

// Sine440 is the canonical 1 s 440 Hz sine vector: round(1000*sin(2*pi*440*i/16000)).
func Sine440(i int) int16 {
	v := math.Round(1000 * math.Sin(2*math.Pi*440*float64(i)/float64(SampleRate)))
	return int16(v)
}

// Silence is the canonical all-zero vector used as the checksum-conflict body.
func Silence(i int) int16 { return 0 }

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func edges(b []byte) (string, string) {
	return hex.EncodeToString(b[:8]), hex.EncodeToString(b[len(b)-8:])
}

func marshal(v any) string {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		panic(fmt.Sprintf("ingestvectors: marshal: %v", err))
	}
	return string(data) + "\n"
}

// ChunkMeta mirrors the X-Chunk-Meta header JSON. Field order is frozen: the
// base64url literal in chunk_meta_ok.json depends on it.
type ChunkMeta struct {
	SampleOffset  int64   `json:"sample_offset"`
	SampleCount   int64   `json:"sample_count"`
	DurationMS    int64   `json:"duration_ms"`
	ContentSHA256 string  `json:"content_sha256"`
	RecordedAt    *string `json:"recorded_at"`
}

// EncodeChunkMeta returns the canonical compact JSON and its base64url form.
func EncodeChunkMeta(m ChunkMeta) (compact string, b64url string) {
	data, err := json.Marshal(m)
	if err != nil {
		panic(fmt.Sprintf("ingestvectors: marshal chunk meta: %v", err))
	}
	compact = string(data)
	b64url = base64.RawURLEncoding.EncodeToString(data)
	return compact, b64url
}

// PCMVector describes a raw PCM vector file.
type PCMVector struct {
	Name          string `json:"name"`
	Description   string `json:"description"`
	Codec         string `json:"codec"`
	SampleRate    int    `json:"sample_rate"`
	Channels      int    `json:"channels"`
	SampleCount   int    `json:"sample_count"`
	ByteLen       int    `json:"byte_len"`
	DurationMS    int    `json:"duration_ms"`
	PCMBase64     string `json:"pcm_base64"`
	ContentSHA256 string `json:"content_sha256"`
	First8Hex     string `json:"first_8_bytes_hex"`
	Last8Hex      string `json:"last_8_bytes_hex"`
}

func pcmVector(name, desc string, fn Sample) (PCMVector, []byte) {
	pcm := PCM(SampleRate, fn)
	first, last := edges(pcm)
	return PCMVector{
		Name:          name,
		Description:   desc,
		Codec:         Codec,
		SampleRate:    SampleRate,
		Channels:      Channels,
		SampleCount:   SampleRate,
		ByteLen:       len(pcm),
		DurationMS:    1000,
		PCMBase64:     base64.StdEncoding.EncodeToString(pcm),
		ContentSHA256: sha256Hex(pcm),
		First8Hex:     first,
		Last8Hex:      last,
	}, pcm
}

// RequestVector describes one protocol request and its expected outcome.
type RequestVector struct {
	Name           string         `json:"name"`
	Method         string         `json:"method"`
	Path           string         `json:"path"`
	Headers        map[string]any `json:"headers"`
	BodyRef        string         `json:"body_ref,omitempty"` // name of the PCMVector providing the body
	JSONBody       any            `json:"json_body,omitempty"`
	ExpectedStatus int            `json:"expected_status"`
	ExpectedError  string         `json:"expected_error,omitempty"`
	ExpectFields   map[string]any `json:"expect_fields,omitempty"`
	ExpectedDetail map[string]any `json:"expected_details,omitempty"`
	Setup          []string       `json:"setup,omitempty"`
	Notes          string         `json:"notes,omitempty"`
}

func deviceHeaders() map[string]any {
	return map[string]any{
		"Authorization":    "Bearer <device_token>",
		"X-Ingest-Version": ProtocolVersion,
	}
}

func syncHeaders() map[string]any {
	return map[string]any{
		"Authorization":    "Bearer <sync_token>",
		"X-Ingest-Version": ProtocolVersion,
	}
}

func chunkPath(seq int) string {
	return fmt.Sprintf("/v1/recordings/%s/chunks/%d", VectorRecording, seq)
}

// Build returns every vector file, deterministically ordered by name.
func Build() []File {
	sine, _ := pcmVector("pcm_1s_440hz",
		"16000 samples (1 s) of 440 Hz sine, amplitude 1000: sample[i]=int16(round(1000*sin(2*pi*440*i/16000)))", Sine440)
	silence, silenceBytes := pcmVector("pcm_1s_silence",
		"16000 zero samples (1 s); canonical different-content body for checksum conflicts", Silence)

	meta0 := ChunkMeta{SampleOffset: 0, SampleCount: int64(sine.SampleCount), DurationMS: 1000, ContentSHA256: sine.ContentSHA256}
	metaCompact0, metaB640 := EncodeChunkMeta(meta0)
	meta1 := meta0
	meta1.SampleOffset = int64(sine.SampleCount)
	_, metaB641 := EncodeChunkMeta(meta1)
	meta2 := meta0
	meta2.SampleOffset = 2 * int64(sine.SampleCount)
	_, metaB642 := EncodeChunkMeta(meta2)

	files := []File{
		{"pcm_1s_440hz.json", marshal(sine)},
		{"pcm_1s_silence.json", marshal(silence)},
		{"chunk_meta_ok.json", marshal(map[string]any{
			"name":                   "chunk_meta_ok",
			"description":            "X-Chunk-Meta headers for the 1 s sine vector (base64url of canonical compact JSON); per-seq offsets document the finish_ok offset chain",
			"chunk_meta_json":        metaCompact0,
			"x_chunk_meta_base64url": metaB640,
			"per_seq": map[string]any{
				"0": metaB640,
				"1": metaB641,
				"2": metaB642,
			},
			"required_request_headers": map[string]string{
				"X-Ingest-Version": ProtocolVersion,
				"Content-Type":     "application/octet-stream",
			},
		})},
		{"put_chunk_ok.json", marshal(RequestVector{
			Name:           "put_chunk_ok",
			Method:         "PUT",
			Path:           chunkPath(0),
			Headers:        withMeta(deviceHeaders(), metaB640),
			BodyRef:        "pcm_1s_440hz",
			ExpectedStatus: 200,
			ExpectFields: map[string]any{
				"seq": 0, "state": "committed", "durable": true,
				"checksum": sine.ContentSHA256,
				"r2_key":   fmt.Sprintf("owners/%s/recordings/%s/chunks/%08d.pcm", OwnerDefault, VectorRecording, 0),
			},
			Setup: []string{"POST /v1/recordings for " + VectorRecording},
			Notes: "first upload of seq 0",
		})},
		{"put_chunk_duplicate.json", marshal(RequestVector{
			Name:           "put_chunk_duplicate",
			Method:         "PUT",
			Path:           chunkPath(0),
			Headers:        withMeta(deviceHeaders(), metaB640),
			BodyRef:        "pcm_1s_440hz",
			ExpectedStatus: 200,
			ExpectFields:   map[string]any{"state": "committed", "duplicate": true, "durable": true},
			Setup:          []string{"put_chunk_ok already committed seq 0"},
			Notes:          "byte-identical replay must not create a second event",
		})},
		{"put_chunk_conflict.json", marshal(RequestVector{
			Name:           "put_chunk_conflict",
			Method:         "PUT",
			Path:           chunkPath(0),
			Headers:        withMeta(deviceHeaders(), metaFor(silenceBytes)),
			BodyRef:        "pcm_1s_silence",
			ExpectedStatus: 409,
			ExpectedError:  "conflict_checksum",
			Setup:          []string{"put_chunk_ok already committed seq 0 with the sine body"},
			Notes:          "same (recording_id, seq), different bytes",
		})},
		{"finish_gap.json", marshal(RequestVector{
			Name:    "finish_gap",
			Method:  "POST",
			Path:    fmt.Sprintf("/v1/recordings/%s/finish", VectorRecording),
			Headers: deviceHeaders(),
			JSONBody: map[string]any{
				"protocol_version": ProtocolVersion, "total_chunks": 3,
				"total_samples": 3 * sine.SampleCount, "total_duration_ms": 3000,
			},
			Setup:          []string{"recording created", "uploaded seq 0 and seq 2 only"},
			ExpectedStatus: 409,
			ExpectedError:  "sequence_gap",
			ExpectedDetail: map[string]any{"missing_sequences": []int{1}},
			Notes:          "recording state must stay recording; completion forbidden while sequences are missing",
		})},
		{"finish_ok.json", marshal(RequestVector{
			Name:    "finish_ok",
			Method:  "POST",
			Path:    fmt.Sprintf("/v1/recordings/%s/finish", VectorRecording),
			Headers: deviceHeaders(),
			JSONBody: map[string]any{
				"protocol_version": ProtocolVersion, "total_chunks": 3,
				"total_samples": 3 * sine.SampleCount, "total_duration_ms": 3000,
			},
			Setup:          []string{"uploaded seq 0,1,2 of the 1 s sine vector with offsets 0,16000,32000"},
			ExpectedStatus: 200,
			ExpectFields:   map[string]any{"state": "complete", "total_chunks": 3, "durable": true},
			Notes:          "replay of the same body returns 200 without emitting a second recording.finished event",
		})},
		{"sync_page.json", marshal(map[string]any{
			"name": "sync_page",
			"request": map[string]any{
				"method":  "GET",
				"path":    "/v1/sync?cursor=0&limit=100",
				"headers": syncHeaders(),
			},
			"expected_status": 200,
			"expected_shape": map[string]any{
				"events":                  "array of {id:int,type:string,recording_id:string,seq:int,payload:object,created_at:rfc3339}",
				"ordering":                "strictly id > cursor, ascending by id",
				"next_cursor":             "id of last returned event; unchanged cursor on empty page",
				"has_more":                "bool",
				"max_event_id":            "current maximum event id",
				"server_protocol_version": ProtocolVersion,
			},
			"expected_event_types_for_finish_ok_setup": []string{
				"recording.created",
				"chunk.committed", "chunk.committed", "chunk.committed",
				"recording.finished",
			},
		})},
		{"cursor_expired.json", marshal(RequestVector{
			Name:           "cursor_expired",
			Method:         "GET",
			Path:           "/v1/sync?cursor=1&limit=100",
			Headers:        syncHeaders(),
			Setup:          []string{"event retention trimmed so the smallest surviving id exceeds cursor+1"},
			ExpectedStatus: 410,
			ExpectedError:  "cursor_expired",
			ExpectedDetail: map[string]any{"min_available_cursor": "smallest surviving event id"},
			Notes:          "consumer must fall back to GET /v1/sync/recordings full snapshot",
		})},
		{"error_codes.json", marshal(map[string]any{
			"name": "error_codes",
			"codes": map[string]int{
				"unsupported_protocol_version": 400,
				"validation_failed":            400,
				"unauthorized":                 401,
				"token_revoked":                401,
				"forbidden_scope":              403,
				"pairing_code_invalid":         400,
				"pairing_code_expired":         400,
				"recording_not_found":          404,
				"conflict_payload":             409,
				"conflict_checksum":            409,
				"sequence_gap":                 409,
				"cursor_expired":               410,
				"payload_too_large":            413,
				"rate_limited":                 429,
				"internal":                     500,
			},
			"error_body_shape": map[string]any{
				"error": map[string]any{
					"code": "string (see codes)", "message": "string", "details": "object",
				},
			},
		})},
	}
	return files
}

func withMeta(h map[string]any, meta string) map[string]any {
	out := make(map[string]any, len(h)+2)
	for k, v := range h {
		out[k] = v
	}
	out["X-Chunk-Meta"] = meta
	out["Content-Type"] = "application/octet-stream"
	return out
}

func metaFor(pcm []byte) string {
	_, b64 := EncodeChunkMeta(ChunkMeta{
		SampleOffset: 0, SampleCount: int64(len(pcm) / 2), DurationMS: 1000,
		ContentSHA256: sha256Hex(pcm),
	})
	return b64
}
