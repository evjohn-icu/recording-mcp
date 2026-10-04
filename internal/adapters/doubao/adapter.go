// Package doubao converts already parsed Doubao ASR response frames to asrinput.Result.
// It performs no network calls, credential handling, audio access, or interpretation.
package doubao

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"

	"recordingmcp/internal/asrinput"
)

const SegmentBoundaryToleranceMS int64 = 5

type frame struct {
	MessageType   int             `json:"message_type"`
	Flags         int             `json:"flags"`
	Sequence      *int64          `json:"sequence"`
	IsLastPackage bool            `json:"is_last_package"`
	Code          int64           `json:"code"`
	Payload       json.RawMessage `json:"payload"`
}
type payload struct {
	Result *result `json:"result"`
}
type result struct {
	Text       *string      `json:"text"`
	Utterances *[]utterance `json:"utterances"`
}
type utterance struct {
	Text      *string                    `json:"text"`
	StartTime *int64                     `json:"start_time"`
	EndTime   *int64                     `json:"end_time"`
	Additions map[string]json.RawMessage `json:"additions"`
	Definite  *bool                      `json:"definite"`
	Raw       map[string]json.RawMessage
}

func (u *utterance) UnmarshalJSON(b []byte) error {
	type plain utterance
	var p plain
	if e := json.Unmarshal(b, &p); e != nil {
		return e
	}
	var raw map[string]json.RawMessage
	if e := json.Unmarshal(b, &raw); e != nil {
		return e
	}
	*u = utterance(p)
	u.Raw = raw
	return nil
}

func Convert(meta asrinput.Metadata, src io.Reader) (asrinput.Result, error) {
	if err := validateMetadata(meta); err != nil {
		return asrinput.Result{}, err
	}
	scanner := bufio.NewScanner(src)
	scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)
	var frames int
	var partial int
	var finalCount int
	var final frame
	var finalLine json.RawMessage
	var finalPayloadCanonical []byte
	seenFinal := false
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		frames++
		var f frame
		if err := json.Unmarshal(line, &f); err != nil {
			return asrinput.Result{}, fmt.Errorf("response frame %d: invalid JSON: %w", frames, err)
		}
		if seenFinal && !f.IsLastPackage {
			return asrinput.Result{}, fmt.Errorf("response frame %d: non-final frame follows final package", frames)
		}
		if f.MessageType == 15 || f.Code != 0 {
			return asrinput.Result{}, fmt.Errorf("response frame %d: service error type=%d code=%d", frames, f.MessageType, f.Code)
		}
		if f.MessageType != 9 {
			return asrinput.Result{}, fmt.Errorf("response frame %d: unexpected message_type %d", frames, f.MessageType)
		}
		flagLast := f.Flags&2 != 0
		if flagLast != f.IsLastPackage {
			return asrinput.Result{}, fmt.Errorf("response frame %d: is_last_package disagrees with flags", frames)
		}
		if f.Payload == nil || !json.Valid(f.Payload) {
			return asrinput.Result{}, fmt.Errorf("response frame %d: missing or invalid payload", frames)
		}
		if f.IsLastPackage {
			seenFinal = true
			finalCount++
			canon, err := canonicalFinal(line)
			if err != nil {
				return asrinput.Result{}, err
			}
			if finalCount > 1 && !bytes.Equal(canon, finalPayloadCanonical) {
				return asrinput.Result{}, errors.New("contradictory final packages in one request")
			}
			final = f
			finalLine = append([]byte(nil), line...)
			finalPayloadCanonical = canon
		} else {
			partial++
		}
	}
	if err := scanner.Err(); err != nil {
		return asrinput.Result{}, fmt.Errorf("read response log: %w", err)
	}
	if frames == 0 {
		return asrinput.Result{}, errors.New("response log is empty")
	}
	if finalCount == 0 {
		return asrinput.Result{}, errors.New("final package missing; no normalized output produced")
	}
	var p payload
	if err := json.Unmarshal(final.Payload, &p); err != nil {
		return asrinput.Result{}, fmt.Errorf("invalid final payload: %w", err)
	}
	if p.Result == nil {
		return asrinput.Result{}, errors.New("final payload has no result")
	}
	if p.Result.Text == nil && p.Result.Utterances == nil {
		return asrinput.Result{}, errors.New("final result has neither text nor utterances")
	}
	var utterances []utterance
	if p.Result.Utterances != nil {
		utterances = *p.Result.Utterances
	} else if p.Result.Text != nil && strings.TrimSpace(*p.Result.Text) != "" {
		return asrinput.Result{}, errors.New("final result has transcript text but no utterances array")
	}
	result := asrinput.Result{ContractVersion: asrinput.ContractVersion, Source: asrinput.Source{Provider: meta.Provider, ProviderRequestID: meta.ProviderRequestID, DeviceID: meta.DeviceID, RecordingID: meta.RecordingID, ConversationID: meta.ConversationID, SegmentID: meta.SegmentID, SegmentOffsetMS: meta.SegmentOffsetMS, SegmentDurationMS: meta.SegmentDurationMS, TranscriptRevision: meta.TranscriptRevision, SupersedesRevision: meta.SupersedesRevision, CompletionStatus: meta.CompletionStatus, RecordingStartedAt: meta.RecordingStartedAt, CaptureTimezone: meta.CaptureTimezone, SourceAudioSHA256: meta.SourceAudioSHA256, SourceRef: meta.SourceRef, ReceivedAt: meta.ReceivedAt, Model: meta.Model, ResourceID: meta.ResourceID, ServerLogID: meta.ServerLogID}, Turns: []asrinput.Turn{}}
	metaRaw := meta.RawEnvelope
	if len(metaRaw) == 0 {
		var err error
		metaRaw, err = json.Marshal(meta)
		if err != nil {
			return asrinput.Result{}, err
		}
	}
	result.Source.RawEnvelope = metaRaw
	if meta.SourceAudioSHA256 == nil {
		result.Warnings = append(result.Warnings, "source_audio_sha256 unavailable; no response digest is substituted")
	}
	if meta.RecordingStartedAt == nil {
		result.Warnings = append(result.Warnings, "recording absolute start time unknown; not inferred from filenames or segment offset")
	}
	for i, u := range utterances {
		text := ""
		if u.Text != nil {
			text = *u.Text
		}
		turn := asrinput.Turn{ID: stableTurnID(meta, i+1), OriginalIndex: i + 1, Text: text, LocalStartMS: usableTime(u.StartTime), LocalEndMS: usableTime(u.EndTime)}
		if u.Text == nil {
			turn.Warnings = append(turn.Warnings, "provider utterance text missing; retained as empty; raw provider payload preserved")
		} else if strings.TrimSpace(text) == "" {
			turn.Warnings = append(turn.Warnings, "provider utterance text is empty; retained in source order")
		}
		if u.StartTime != nil && *u.StartTime < 0 {
			turn.Warnings = append(turn.Warnings, "provider start_time is negative/unavailable; raw value retained in raw_source")
		}
		if u.EndTime != nil && *u.EndTime < 0 {
			turn.Warnings = append(turn.Warnings, "provider end_time is negative/unavailable; raw value retained in raw_source")
		}
		if turn.LocalStartMS != nil {
			turn.GlobalStartMS = addOffset(meta.SegmentOffsetMS, *turn.LocalStartMS)
		}
		if turn.LocalEndMS != nil {
			turn.GlobalEndMS = addOffset(meta.SegmentOffsetMS, *turn.LocalEndMS)
		}
		for _, tm := range []*int64{turn.LocalStartMS, turn.LocalEndMS} {
			if tm != nil && *tm > meta.SegmentDurationMS {
				if *tm > meta.SegmentDurationMS+SegmentBoundaryToleranceMS {
					return asrinput.Result{}, fmt.Errorf("utterance %d timestamp exceeds segment duration tolerance", i)
				}
				turn.Warnings = append(turn.Warnings, "provider timestamp exceeds segment duration by at most 5ms; raw value preserved")
			}
		}
		if turn.LocalStartMS != nil && turn.LocalEndMS != nil && *turn.LocalEndMS < *turn.LocalStartMS {
			return asrinput.Result{}, fmt.Errorf("utterance %d end_time precedes start_time", i)
		}
		if rawSpeaker, ok := u.Additions["speaker_id"]; ok && len(rawSpeaker) > 0 && string(rawSpeaker) != "null" {
			v, err := speakerLabel(rawSpeaker)
			if err != nil {
				return asrinput.Result{}, fmt.Errorf("utterance %d speaker_id: %w", i, err)
			}
			if strings.TrimSpace(v) != "" {
				scoped := fmt.Sprintf("%s:%s:%s:%s", meta.Provider, meta.ProviderRequestID, meta.TranscriptRevision, v)
				turn.SpeakerScopedID = &scoped
			}
		}
		result.Turns = append(result.Turns, turn)
	}
	result.RawSource, _ = json.Marshal(map[string]any{"format": "doubao-parsed-response-frame", "frame_count": frames, "partial_frame_count": partial, "final_package_count": finalCount, "final_sequence": final.Sequence, "final_flags": final.Flags, "final_response_frame": json.RawMessage(finalLine), "provider_payload": json.RawMessage(final.Payload), "note": "Partial frames are not concatenated into turns. Original words, additions, and unknown provider payload fields are retained in provider_payload."})
	if finalCount > 1 {
		result.Warnings = append(result.Warnings, "multiple identical final packages collapsed to one snapshot")
	}
	if err := asrinput.Validate(result); err != nil {
		return asrinput.Result{}, err
	}
	return result, nil
}

func canonicalFinal(line []byte) ([]byte, error) {
	var full map[string]json.RawMessage
	if err := json.Unmarshal(line, &full); err != nil {
		return nil, err
	}
	delete(full, "sequence")
	return json.Marshal(full)
}

func validateMetadata(m asrinput.Metadata) error {
	for k, v := range map[string]string{"provider": m.Provider, "provider_request_id": m.ProviderRequestID, "device_id": m.DeviceID, "recording_id": m.RecordingID, "segment_id": m.SegmentID, "transcript_revision": m.TranscriptRevision} {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("metadata %s is required", k)
		}
	}
	if m.Provider != "doubao" {
		return fmt.Errorf("provider must be doubao for this adapter")
	}
	if m.CompletionStatus != "final" {
		return fmt.Errorf("metadata completion_status must be final")
	}
	if m.SegmentOffsetMS < 0 || m.SegmentDurationMS <= 0 || m.SegmentOffsetMS > math.MaxInt64-m.SegmentDurationMS-SegmentBoundaryToleranceMS {
		return errors.New("invalid segment offset or duration")
	}
	if m.SourceAudioSHA256 != nil {
		if len(*m.SourceAudioSHA256) != 64 {
			return errors.New("source_audio_sha256 must be 64 hex characters")
		}
		if _, e := hex.DecodeString(*m.SourceAudioSHA256); e != nil {
			return errors.New("source_audio_sha256 must be hexadecimal")
		}
	}
	if m.RecordingStartedAt != nil {
		if _, e := time.Parse(time.RFC3339, *m.RecordingStartedAt); e != nil {
			return fmt.Errorf("recording_started_at must be RFC3339 or null: %w", e)
		}
	}
	return nil
}
func usableTime(v *int64) *int64 {
	if v == nil || *v < 0 {
		return nil
	}
	x := *v
	return &x
}
func addOffset(offset, local int64) *int64 { x := offset + local; return &x }
func speakerLabel(raw json.RawMessage) (string, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return "", nil
		}
		return s, nil
	}
	var n json.Number
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := d.Decode(&n); err == nil {
		return n.String(), nil
	}
	return "", fmt.Errorf("expected string or number")
}
func stableTurnID(m asrinput.Metadata, index int) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{m.Provider, m.RecordingID, m.SegmentID, m.ProviderRequestID, m.TranscriptRevision, strconv.Itoa(index)}, "\x00")))
	return "turn:" + hex.EncodeToString(sum[:])
}
