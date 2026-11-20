// Package audioanalysis implements the versioned, non-ASR audio analysis
// contract (audio-analysis-v1): speech activity, acoustic tags, diarization
// segments and speaker embeddings produced by a local subprocess adapter.
//
// ASR keeps writing source_attachments only. Nothing in this package becomes a
// turn or a memory candidate; results are a separate overlay that is versioned,
// invalidated on source supersede and removed on permanent forget.
package audioanalysis

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
)

// ContractSchema is the frozen internal schema identifier.
const ContractSchema = "audio-analysis-v1"

// Capabilities.
const (
	CapabilityVAD         = "vad"
	CapabilityTagging     = "tagging"
	CapabilityDiarization = "diarization"
	CapabilitySpeaker     = "speaker"
)

// Capabilities lists every supported capability in stable order.
var Capabilities = []string{CapabilityVAD, CapabilityTagging, CapabilityDiarization, CapabilitySpeaker}

// Result status values.
const (
	StatusRunning     = "running" // internal persistence only, never public
	StatusSucceeded   = "succeeded"
	StatusFailed      = "failed"
	StatusInvalidated = "invalidated"
)

// Event types.
const (
	EventSpeechActivity          = "speech_activity"
	EventAcoustic                = "acoustic_event"
	EventSpeakerSegment          = "speaker_segment"
	EventSpeakerIdentityProposal = "speaker_identity_proposal"
)

// Error codes. They are stable identifiers used by workers, the safe error
// surface and the Web/MCP views.
const (
	CodeRequestConflict       = "audio_request_conflict"
	CodeInputInvalid          = "audio_input_invalid"
	CodeDisabled              = "audio_disabled"
	CodeModelUnavailable      = "audio_model_unavailable"
	CodeDependencyUnavailable = "audio_dependency_unavailable"
	CodeResultInvalid         = "audio_result_invalid"
	CodeSourceSuperseded      = "audio_source_superseded"
	CodeBarrierBlocked        = "audio_barrier_blocked"
	CodeLeaseLost             = "audio_lease_lost"
	CodeGPUEmbeddingConflict  = "audio_gpu_embedding_conflict"
	CodeGPUBusy               = "audio_gpu_busy"
)

// Request is one analysis run's complete, pinned input.
type Request struct {
	SchemaVersion        string            `json:"schema_version"`
	RunID                string            `json:"run_id"`
	RecordingID          string            `json:"recording_id"`
	SourceAudioSHA256    string            `json:"source_audio_sha256"`
	InputSHA256          string            `json:"input_sha256"`
	TranscriptRevision   *string           `json:"transcript_revision"`
	ModelID              string            `json:"model_id"`
	ModelRevision        string            `json:"model_revision"`
	ModelFilesSHA256     map[string]string `json:"model_files_sha256"`
	Capability           string            `json:"capability"`
	StartMS              int64             `json:"start_ms"`
	EndMS                int64             `json:"end_ms"`
	ConfigSHA256         string            `json:"config_sha256"`
	SourceAttachmentIDs  []string          `json:"source_attachment_ids"`
	SourceSnapshotSHA256 string            `json:"source_snapshot_sha256"`
}

// Event is one timestamped analysis result.
type Event struct {
	ID         string         `json:"id"`
	EventType  string         `json:"event_type"`
	StartMS    int64          `json:"start_ms"`
	EndMS      int64          `json:"end_ms"`
	Confidence *float64       `json:"confidence"`
	Payload    map[string]any `json:"payload"`
}

// Provenance records how a result was produced. It never carries source text.
type Provenance struct {
	AdapterVersion       string            `json:"adapter_version"`
	FrontendID           string            `json:"frontend_id"`
	Device               string            `json:"device"`
	InputSHA256          string            `json:"input_sha256"`
	ModelFilesSHA256     map[string]string `json:"model_files_sha256"`
	ParentRunIDs         []string          `json:"parent_run_ids"`
	SourceSnapshotSHA256 string            `json:"source_snapshot_sha256"`
}

// Error is the safe failure surface. It carries no stderr text.
type Error struct {
	Code      string `json:"code"`
	Retryable bool   `json:"retryable"`
	Action    string `json:"action"`
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	return e.Code
}

// Result is one adapter result. Terminal statuses only: running is a database
// projection, never a public result.
type Result struct {
	SchemaVersion        string            `json:"schema_version"`
	RunID                string            `json:"run_id"`
	RecordingID          string            `json:"recording_id"`
	SourceAudioSHA256    string            `json:"source_audio_sha256"`
	InputSHA256          string            `json:"input_sha256"`
	TranscriptRevision   *string           `json:"transcript_revision"`
	ModelID              string            `json:"model_id"`
	ModelRevision        string            `json:"model_revision"`
	ModelFilesSHA256     map[string]string `json:"model_files_sha256"`
	Capability           string            `json:"capability"`
	StartMS              int64             `json:"start_ms"`
	EndMS                int64             `json:"end_ms"`
	ConfigSHA256         string            `json:"config_sha256"`
	SourceAttachmentIDs  []string          `json:"source_attachment_ids"`
	SourceSnapshotSHA256 string            `json:"source_snapshot_sha256"`
	Status               string            `json:"status"`
	Events               []Event           `json:"events"`
	Provenance           Provenance        `json:"provenance"`
	Error                *Error            `json:"error,omitempty"`
}

// Analyzer is the adapter boundary. pcm is the managed 16 kHz mono s16le
// stream; the adapter reads at most EndMS-StartMS worth of samples.
type Analyzer interface {
	Analyze(ctx context.Context, req Request, pcm io.Reader) (Result, error)
}

// IsCapability reports whether name is a supported capability.
func IsCapability(name string) bool {
	for _, capability := range Capabilities {
		if capability == name {
			return true
		}
	}
	return false
}

// NewError builds a safe structured error.
func NewError(code string, retryable bool, action string) *Error {
	return &Error{Code: code, Retryable: retryable, Action: action}
}

// Validate checks a request against the contract.
func (r Request) Validate() error {
	if r.SchemaVersion != ContractSchema {
		return NewError(CodeInputInvalid, false, fmt.Sprintf("schema_version must be %s", ContractSchema))
	}
	if strings.TrimSpace(r.RunID) == "" || strings.TrimSpace(r.RecordingID) == "" {
		return NewError(CodeInputInvalid, false, "run_id and recording_id are required")
	}
	if !validHash(r.SourceAudioSHA256) || !validHash(r.InputSHA256) || !validHash(r.ConfigSHA256) {
		return NewError(CodeInputInvalid, false, "source_audio_sha256, input_sha256 and config_sha256 must be 64 hex characters")
	}
	if !IsCapability(r.Capability) {
		return NewError(CodeInputInvalid, false, "unknown capability")
	}
	if r.ModelID == "" || r.ModelRevision == "" {
		return NewError(CodeInputInvalid, false, "model_id and model_revision are required")
	}
	if len(r.ModelFilesSHA256) == 0 {
		return NewError(CodeInputInvalid, false, "model_files_sha256 must pin every model file")
	}
	for name, hash := range r.ModelFilesSHA256 {
		if strings.TrimSpace(name) == "" || !validHash(hash) {
			return NewError(CodeInputInvalid, false, "model_files_sha256 entries must be name to 64 hex hash")
		}
	}
	if r.StartMS < 0 || r.EndMS <= r.StartMS {
		return NewError(CodeInputInvalid, false, "start_ms/end_ms must satisfy 0 <= start < end")
	}
	if r.SourceSnapshotSHA256 == "" {
		if len(r.SourceAttachmentIDs) != 0 {
			return NewError(CodeInputInvalid, false, "source_attachment_ids requires source_snapshot_sha256")
		}
	} else if !validHash(r.SourceSnapshotSHA256) {
		return NewError(CodeInputInvalid, false, "source_snapshot_sha256 must be 64 hex characters")
	}
	if r.Capability != CapabilitySpeaker && len(r.SourceAttachmentIDs) != 0 {
		return NewError(CodeInputInvalid, false, "only speaker mapping runs may pin source attachments")
	}
	return nil
}

// ValidateResult checks a result against its request and the real PCM
// duration. A succeeded result may legitimately carry zero events.
func (r Result) ValidateResult(req Request, pcmDurationMS int64) error {
	if r.SchemaVersion != ContractSchema || r.RunID != req.RunID {
		return NewError(CodeResultInvalid, false, "result identity does not match the request")
	}
	for _, pair := range []struct{ name, got, want string }{
		{"recording_id", r.RecordingID, req.RecordingID},
		{"source_audio_sha256", r.SourceAudioSHA256, req.SourceAudioSHA256},
		{"input_sha256", r.InputSHA256, req.InputSHA256},
		{"model_id", r.ModelID, req.ModelID},
		{"model_revision", r.ModelRevision, req.ModelRevision},
		{"capability", r.Capability, req.Capability},
		{"config_sha256", r.ConfigSHA256, req.ConfigSHA256},
		{"source_snapshot_sha256", r.SourceSnapshotSHA256, req.SourceSnapshotSHA256},
	} {
		if pair.got != pair.want {
			return NewError(CodeResultInvalid, false, pair.name+" does not match the request")
		}
	}
	if (r.TranscriptRevision == nil) != (req.TranscriptRevision == nil) ||
		(r.TranscriptRevision != nil && *r.TranscriptRevision != *req.TranscriptRevision) {
		return NewError(CodeResultInvalid, false, "transcript_revision does not match the request")
	}
	if strings.Join(r.SourceAttachmentIDs, "\x00") != strings.Join(req.SourceAttachmentIDs, "\x00") {
		return NewError(CodeResultInvalid, false, "source_attachment_ids do not match the request")
	}
	switch r.Status {
	case StatusSucceeded:
		if r.Error != nil {
			return NewError(CodeResultInvalid, false, "succeeded result must not carry an error")
		}
	case StatusFailed, StatusInvalidated:
		if len(r.Events) != 0 {
			return NewError(CodeResultInvalid, false, "failed/invalidated result must not carry events")
		}
	default:
		return NewError(CodeResultInvalid, false, "status must be succeeded, failed or invalidated")
	}
	seen := map[string]bool{}
	for _, event := range r.Events {
		if event.ID == "" || seen[event.ID] {
			return NewError(CodeResultInvalid, false, "event ids must be present and unique")
		}
		seen[event.ID] = true
		switch event.EventType {
		case EventSpeechActivity, EventAcoustic, EventSpeakerSegment, EventSpeakerIdentityProposal:
		default:
			return NewError(CodeResultInvalid, false, "unknown event type")
		}
		if event.StartMS < 0 || event.EndMS <= event.StartMS || event.EndMS > pcmDurationMS {
			return NewError(CodeResultInvalid, false, "event time is outside the analysed PCM")
		}
		if event.Confidence != nil {
			value := *event.Confidence
			if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1 {
				return NewError(CodeResultInvalid, false, "confidence must be within [0,1] or null")
			}
		}
		if event.EventType == EventSpeakerIdentityProposal {
			if status, _ := event.Payload["status"].(string); status != "proposed" {
				return NewError(CodeResultInvalid, false, "identity proposals must carry status=proposed")
			}
		}
	}
	return nil
}

// ComputeRunID derives the stable run identity. generation is the caller's
// request id; it is the only way to force a new run for the same audio.
func ComputeRunID(req Request, generation string) string {
	canonical, _ := json.Marshal(struct {
		RecordingID          string   `json:"recording_id"`
		SourceAudioSHA256    string   `json:"source_audio_sha256"`
		InputSHA256          string   `json:"input_sha256"`
		Capability           string   `json:"capability"`
		ModelID              string   `json:"model_id"`
		ModelRevision        string   `json:"model_revision"`
		ConfigSHA256         string   `json:"config_sha256"`
		SourceSnapshotSHA256 string   `json:"source_snapshot_sha256"`
		Generation           string   `json:"generation"`
		Attachments          []string `json:"source_attachment_ids"`
	}{req.RecordingID, req.SourceAudioSHA256, req.InputSHA256, req.Capability, req.ModelID,
		req.ModelRevision, req.ConfigSHA256, req.SourceSnapshotSHA256, generation, req.SourceAttachmentIDs})
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

// SnapshotHash hashes sorted attachment id/revision pairs. An empty set hashes
// the empty array, so a pure acoustic run still pins a stable value.
func SnapshotHash(pairs []AttachmentRef) (string, error) {
	sorted := append([]AttachmentRef(nil), pairs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	for i := 1; i < len(sorted); i++ {
		if sorted[i].ID == sorted[i-1].ID {
			return "", NewError(CodeInputInvalid, false, "duplicate attachment id in snapshot")
		}
	}
	canonical, err := json.Marshal(sorted)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// AttachmentRef pins one attachment id and revision.
type AttachmentRef struct {
	ID       string `json:"id"`
	Revision string `json:"revision"`
}

// UniqueRevision returns the shared revision when every attachment has the same
// value, or nil when they differ (or the set is empty).
func UniqueRevision(pairs []AttachmentRef) *string {
	revision := ""
	for _, pair := range pairs {
		if revision == "" {
			revision = pair.Revision
			continue
		}
		if pair.Revision != revision {
			return nil
		}
	}
	if revision == "" {
		return nil
	}
	value := revision
	return &value
}

// validHash reports a lowercase-or-uppercase 64 hex character digest.
func validHash(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// ParseResult decodes one adapter result with strict field handling.
func ParseResult(data []byte) (Result, error) {
	var result Result
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return Result{}, errors.New("audio-analysis: decode result: " + err.Error())
	}
	return result, nil
}
