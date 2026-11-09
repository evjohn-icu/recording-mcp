// Package ingest turns an uploaded audio file into queued transcription work
// and runs the two worker stages that feed the existing memory ingestion path.
// It deliberately reuses process.Spec for queueing, cloudsync.Transcriber for
// ASR and memory.Store.IngestTranscript for persistence: there is no second
// queue and no second transcript writer.
package ingest

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"recordingmcp/internal/cloudsync"
	"recordingmcp/internal/memory"
)

// Upload and identity constants.
const (
	// MaxUploadBytes is the hard bound for one multipart upload (2 GiB).
	MaxUploadBytes int64 = 2 << 30
	// DefaultTranscriptRevision is stamped on every result produced here.
	DefaultTranscriptRevision = "asr-v1"
	// ImportOwner identifies rows written by the local upload path.
	ImportOwner = "local-import"
	// DeviceID is the stable device scope for local uploads.
	DeviceID = "device:local-import"
)

// AudioInfo is the probe result for one uploaded file.
type AudioInfo struct {
	DurationMS int64  `json:"duration_ms"`
	Format     string `json:"format"`
	SampleRate int    `json:"sample_rate"`
	Channels   int    `json:"channels"`
}

// ImportResult describes one accepted upload. Duplicate is true when the same
// bytes were already imported and nothing new was queued.
type ImportResult struct {
	ConversationID string    `json:"conversation_id"`
	RecordingID    string    `json:"recording_id"`
	JobID          string    `json:"job_id"`
	Duplicate      bool      `json:"duplicate"`
	Audio          AudioInfo `json:"audio"`
	FileName       string    `json:"filename"`
	Bytes          int64     `json:"bytes"`
}

// Error is the structured import/stage error. Code and Action are shown to the
// user verbatim; FatalFlag marks errors that must not be retried.
type Error struct {
	Code      string
	Message   string
	Action    string
	Status    int
	FatalFlag bool
	Err       error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Err)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Unwrap exposes the underlying cause.
func (e *Error) Unwrap() error { return e.Err }

// Fatal reports whether the worker must stop retrying this job.
func (e *Error) Fatal() bool { return e.FatalFlag }

// ErrorDetails returns the wire-level code and action for the attempt log.
func (e *Error) ErrorDetails() map[string]string {
	out := map[string]string{"code": e.Code}
	if e.Action != "" {
		out["action"] = e.Action
	}
	return out
}

// Config configures a Service. Store, DB and AudioDir are required.
type Config struct {
	Store    *memory.Store
	DB       *sql.DB
	AudioDir string
	Decoder  Decoder
	// Transcription is the ASR adapter; nil means uploads are rejected.
	Transcription cloudsync.Transcriber

	ASRProvider        string
	ASRModel           string
	ASRResourceID      string
	TranscriptRevision string
	Now                func() time.Time
}

// validate checks the required dependencies.
func (c Config) validate() error {
	if c.Store == nil {
		return fmt.Errorf("ingest: store is required")
	}
	if c.DB == nil {
		return fmt.Errorf("ingest: database is required")
	}
	if c.AudioDir == "" {
		return fmt.Errorf("ingest: audio directory is required")
	}
	return nil
}

// Decoder probes and decodes audio without shelling out through a shell.
type Decoder interface {
	Probe(ctx context.Context, path string) (AudioInfo, error)
	ToPCM(ctx context.Context, src, dst string) error
}
