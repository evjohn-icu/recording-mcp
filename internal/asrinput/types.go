// Package asrinput defines a provider-neutral, immutable ASR segment snapshot.
// It contains source utterances only; topics and semantic interpretations belong elsewhere.
package asrinput

import "encoding/json"

const ContractVersion = "asr-input/0.1"

type Source struct {
	Provider           string          `json:"provider"`
	ProviderRequestID  string          `json:"provider_request_id"`
	DeviceID           string          `json:"device_id"`
	RecordingID        string          `json:"recording_id"`
	ConversationID     string          `json:"conversation_id,omitempty"`
	SegmentID          string          `json:"segment_id"`
	SegmentOffsetMS    int64           `json:"segment_offset_ms"`
	SegmentDurationMS  int64           `json:"segment_duration_ms"`
	TranscriptRevision string          `json:"transcript_revision"`
	SupersedesRevision string          `json:"supersedes_revision,omitempty"`
	CompletionStatus   string          `json:"completion_status"`
	RecordingStartedAt *string         `json:"recording_started_at"`
	CaptureTimezone    *string         `json:"capture_timezone"`
	SourceAudioSHA256  *string         `json:"source_audio_sha256"`
	SourceRef          *string         `json:"source_ref"`
	ReceivedAt         *string         `json:"received_at"`
	Model              *string         `json:"model"`
	ResourceID         *string         `json:"resource_id"`
	ServerLogID        *string         `json:"server_log_id"`
	RawEnvelope        json.RawMessage `json:"raw_envelope"`
}

type Turn struct {
	ID              string   `json:"id"`
	OriginalIndex   int      `json:"original_index"`
	Text            string   `json:"text"`
	LocalStartMS    *int64   `json:"local_start_ms"`
	LocalEndMS      *int64   `json:"local_end_ms"`
	GlobalStartMS   *int64   `json:"global_start_ms"`
	GlobalEndMS     *int64   `json:"global_end_ms"`
	SpeakerScopedID *string  `json:"speaker_scoped_id"`
	SpeakerEntityID *string  `json:"speaker_entity_id"`
	Confidence      *float64 `json:"confidence"`
	Warnings        []string `json:"warnings,omitempty"`
}

type Result struct {
	ContractVersion string          `json:"contract_version"`
	Source          Source          `json:"source"`
	Turns           []Turn          `json:"turns"`
	RawSource       json.RawMessage `json:"raw_source"`
	Warnings        []string        `json:"warnings,omitempty"`
}

// Metadata is the supplier-independent source envelope expected next to a provider response log.
type Metadata struct {
	Provider           string          `json:"provider"`
	ProviderRequestID  string          `json:"provider_request_id"`
	DeviceID           string          `json:"device_id"`
	RecordingID        string          `json:"recording_id"`
	ConversationID     string          `json:"conversation_id,omitempty"`
	SegmentID          string          `json:"segment_id"`
	SegmentOffsetMS    int64           `json:"segment_offset_ms"`
	SegmentDurationMS  int64           `json:"segment_duration_ms"`
	TranscriptRevision string          `json:"transcript_revision"`
	SupersedesRevision string          `json:"supersedes_revision,omitempty"`
	CompletionStatus   string          `json:"completion_status"`
	RecordingStartedAt *string         `json:"recording_started_at,omitempty"`
	CaptureTimezone    *string         `json:"capture_timezone,omitempty"`
	SourceAudioSHA256  *string         `json:"source_audio_sha256,omitempty"`
	SourceRef          *string         `json:"source_ref,omitempty"`
	ReceivedAt         *string         `json:"received_at,omitempty"`
	Model              *string         `json:"model,omitempty"`
	ResourceID         *string         `json:"resource_id,omitempty"`
	ServerLogID        *string         `json:"server_log_id,omitempty"`
	RawEnvelope        json.RawMessage `json:"-"`
}
