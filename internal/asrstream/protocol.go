// Package asrstream implements the upstream transport of the Volcengine SAUC
// streaming ASR service (豆包流式语音识别): the binary frame codec and one
// full-duplex recognition session over WebSocket. It is the ingress that
// replaces the old scripts/asr_full.py: it reads audio, talks to the plan
// endpoint, and yields the parsed response frames that internal/adapters/doubao
// normalizes. It never interprets utterances.
package asrstream

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Message types used by the wire protocol. Client frames are 1 (full request)
// and 2 (audio only); server frames are 9 (full response) and 15 (error).
const (
	TypeFullRequest   = 1
	TypeAudioOnly     = 2
	TypeFullResponse  = 9
	TypeError         = 15
	flagHasSequence   = 1
	flagLastPackage   = 2
	headerSize        = 4
	serializationJSON = 1
	compressionGzip   = 1
)

// Frame is one decoded server frame, in the exact JSON shape
// internal/adapters/doubao consumes from a parsed-response log.
type Frame struct {
	MessageType   int             `json:"message_type"`
	Flags         int             `json:"flags"`
	Sequence      *int32          `json:"sequence,omitempty"`
	IsLastPackage bool            `json:"is_last_package"`
	Code          int64           `json:"code"`
	PayloadSize   *uint32         `json:"payload_size,omitempty"`
	Payload       json.RawMessage `json:"payload,omitempty"`
	PayloadBase64 string          `json:"payload_base64,omitempty"`
}

// EncodeFull builds a full client request frame (type 1): JSON payload, gzip
// compressed, carrying the request sequence number.
func EncodeFull(seq int32, request any) ([]byte, error) {
	body, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("asrstream: marshal request: %w", err)
	}
	return encodeFrame(TypeFullRequest, flagHasSequence, seq, body)
}

// EncodeAudio builds an audio-only frame (type 2). The final frame of a
// request carries the last-package flag and the negated sequence number, which
// is how the service delimits the audio stream.
func EncodeAudio(seq int32, chunk []byte, final bool) ([]byte, error) {
	flags, wireSeq := flagHasSequence, seq
	if final {
		flags |= flagLastPackage
		wireSeq = -seq
	}
	return encodeFrame(TypeAudioOnly, flags, wireSeq, chunk)
}

func encodeFrame(messageType, flags int, seq int32, body []byte) ([]byte, error) {
	var z bytes.Buffer
	w := gzip.NewWriter(&z)
	if _, err := w.Write(body); err != nil {
		return nil, fmt.Errorf("asrstream: compress payload: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("asrstream: compress payload: %w", err)
	}
	out := make([]byte, 0, 3*headerSize+z.Len())
	out = append(out, 0x11, byte(messageType<<4|flags), 0x11, 0x00)
	out = binary.BigEndian.AppendUint32(out, uint32(seq))
	out = binary.BigEndian.AppendUint32(out, uint32(z.Len()))
	return append(out, z.Bytes()...), nil
}

// Parse decodes one server frame. The layout is: 4-byte header, the sequence
// number when flagged, the error code and payload size for response/error
// frames, then the (optionally gzip-compressed) payload.
func Parse(raw []byte) (Frame, error) {
	if len(raw) < headerSize {
		return Frame{}, errors.New("asrstream: frame shorter than its header")
	}
	headerWords := int(raw[0] & 0x0f)
	if headerWords*4 < headerSize || headerWords*4 > len(raw) {
		return Frame{}, fmt.Errorf("asrstream: invalid header size %d", headerWords*4)
	}
	var (
		messageType   = int(raw[1] >> 4)
		flags         = int(raw[1] & 0x0f)
		serialization = int(raw[2] >> 4)
		compression   = int(raw[2] & 0x0f)
		off           = headerWords * 4
	)
	f := Frame{MessageType: messageType, Flags: flags, IsLastPackage: flags&flagLastPackage != 0}

	need := func(n int, what string) error {
		if len(raw)-off < n {
			return fmt.Errorf("asrstream: truncated frame: %s", what)
		}
		return nil
	}
	if flags&flagHasSequence != 0 {
		if err := need(4, "sequence"); err != nil {
			return Frame{}, err
		}
		seq := int32(binary.BigEndian.Uint32(raw[off : off+4]))
		f.Sequence = &seq
		off += 4
	}
	switch messageType {
	case TypeFullRequest, TypeAudioOnly, TypeFullResponse:
		if err := need(4, "payload size"); err != nil {
			return Frame{}, err
		}
		size := binary.BigEndian.Uint32(raw[off : off+4])
		f.PayloadSize = &size
		off += 4
	case TypeError:
		if err := need(8, "code and payload size"); err != nil {
			return Frame{}, err
		}
		f.Code = int64(int32(binary.BigEndian.Uint32(raw[off : off+4])))
		off += 4
		size := binary.BigEndian.Uint32(raw[off : off+4])
		f.PayloadSize = &size
		off += 4
	}

	body := raw[off:]
	if compression == compressionGzip && len(body) > 0 {
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return Frame{}, fmt.Errorf("asrstream: decompress payload: %w", err)
		}
		decoded, err := io.ReadAll(zr)
		if err != nil {
			return Frame{}, fmt.Errorf("asrstream: decompress payload: %w", err)
		}
		if err := zr.Close(); err != nil {
			return Frame{}, fmt.Errorf("asrstream: decompress payload: %w", err)
		}
		body = decoded
	}
	switch {
	case messageType == TypeAudioOnly:
		// Audio frames carry raw PCM even though the header advertises JSON
		// serialization, so they are never decoded as JSON.
		if len(body) > 0 {
			f.PayloadBase64 = base64.StdEncoding.EncodeToString(body)
		}
	case serialization == serializationJSON && len(body) > 0:
		if !json.Valid(body) {
			return Frame{}, errors.New("asrstream: payload is not valid JSON")
		}
		f.Payload = json.RawMessage(append([]byte(nil), body...))
	case len(body) > 0:
		f.PayloadBase64 = base64.StdEncoding.EncodeToString(body)
	}
	return f, nil
}
