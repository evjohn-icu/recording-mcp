package cloudsync

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"recordingmcp/internal/adapters/doubao"
	"recordingmcp/internal/asrinput"
	"recordingmcp/internal/asrstream"
)

// MetaBasis carries the source identity a transcriber needs to stamp onto
// every asrinput.Result it produces. Pointer fields keep unknown time unknown
// (the contract forbids fabricating timestamps).
type MetaBasis struct {
	DeviceID          string
	RecordingID       string
	ConversationID    string
	Revision          string
	SourceAudioSHA256 string

	RecordedAt  *string
	Timezone    *string
	Model       *string
	ResourceID  *string
	ServerLogID *string
}

// Transcriber turns one stitched PCM file into normalized segment results
// ready for memory.Store.IngestTranscript.
type Transcriber interface {
	Transcribe(ctx context.Context, pcmPath string, meta MetaBasis) ([]asrinput.Result, error)
}

// pcmDurationMS derives the recording duration from the file size: S16LE
// mono at 16 kHz is exactly 32 bytes per millisecond.
func pcmDurationMS(size int64) int64 {
	return size / 32
}

func fileSize(path string) (int64, error) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

// fixtureSegmentMS is the fixed window length of the deterministic fixture
// transcriber: one turn per five seconds of audio.
const fixtureSegmentMS = 5000

// FixtureTranscriber produces deterministic, zero-network transcripts used by
// tests and the local e2e loop. One turn per 5 s window; the text encodes the
// recording id and window bounds so e2e assertions can search for it.
type FixtureTranscriber struct{}

// Transcribe implements Transcriber.
func (FixtureTranscriber) Transcribe(ctx context.Context, pcmPath string, meta MetaBasis) ([]asrinput.Result, error) {
	size, err := fileSize(pcmPath)
	if err != nil {
		return nil, err
	}
	durationMS := pcmDurationMS(size)
	if durationMS <= 0 {
		return nil, nil
	}
	var results []asrinput.Result
	for i := int64(0); ; i++ {
		start := i * fixtureSegmentMS
		end := start + fixtureSegmentMS
		if end > durationMS {
			end = durationMS
		}
		results = append(results, fixtureResult(meta, int(i), start, end))
		if end >= durationMS {
			break
		}
	}
	return results, nil
}

// fixtureResult builds one deterministic segment result. The window text
// stays in lockstep with the e2e assertions.
func fixtureResult(meta MetaBasis, i int, startMS, endMS int64) asrinput.Result {
	segmentID := fmt.Sprintf("segment-%02d", i)
	text := fmt.Sprintf("fixture transcript recording=%s segment=%02d window=%d-%dms", meta.RecordingID, i, startMS, endMS)
	providerRequestID := fmt.Sprintf("%s/%s", meta.RecordingID, segmentID)
	return asrinput.Result{
		ContractVersion: asrinput.ContractVersion,
		Source: asrinput.Source{
			Provider:           "fixture",
			ProviderRequestID:  providerRequestID,
			DeviceID:           meta.DeviceID,
			RecordingID:        meta.RecordingID,
			ConversationID:     meta.ConversationID,
			SegmentID:          segmentID,
			SegmentOffsetMS:    startMS,
			SegmentDurationMS:  endMS - startMS,
			TranscriptRevision: meta.Revision,
			CompletionStatus:   "final",
			SourceAudioSHA256:  strPtr(meta.SourceAudioSHA256),
		},
		Turns: []asrinput.Turn{{
			ID:            fmt.Sprintf("win-%02d", i),
			OriginalIndex: 1,
			Text:          text,
			LocalStartMS:  new(int64(0)),
			LocalEndMS:    new(endMS - startMS),
			GlobalStartMS: new(startMS),
			GlobalEndMS:   new(endMS),
			Confidence:    new(1.0),
		}},
	}
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// DoubaoTranscriber runs the real 豆包 streaming ASR over pre-segmented
// windows of the stitched PCM, reusing internal/asrstream and
// internal/adapters/doubao unchanged.
type DoubaoTranscriber struct {
	APIKey     string
	Endpoint   string
	ResourceID string
	ModelName  string
	UID        string
	SegmentMS  int64
	OverlapMS  int64
	// MaxConcurrent limits in-flight recognition sessions; windows still
	// produce results in segment order.
	MaxConcurrent int
}

// Transcribe implements Transcriber.
func (t DoubaoTranscriber) Transcribe(ctx context.Context, pcmPath string, meta MetaBasis) ([]asrinput.Result, error) {
	size, err := fileSize(pcmPath)
	if err != nil {
		return nil, err
	}
	durationMS := pcmDurationMS(size)
	if durationMS <= 0 {
		return nil, fmt.Errorf("cloudsync: pcm file %s is empty", pcmPath)
	}
	segments, err := asrstream.PlanSegments(durationMS, t.SegmentMS, t.OverlapMS)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(pcmPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	results := make([]asrinput.Result, len(segments))
	errs := make([]error, len(segments))
	limit := t.MaxConcurrent
	if limit <= 0 {
		limit = 1
	}
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i, seg := range segments {
		wg.Add(1)
		go func(i int, seg asrstream.Segment) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i], errs[i] = t.transcribeWindow(ctx, f, meta, i, seg)
		}(i, seg)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			return nil, fmt.Errorf("segment %d: %w", i, err)
		}
	}
	return results, nil
}

// transcribeWindow recognizes one [StartMS,EndMS) window: up to three
// attempts (offline pacing first, then the 200 ms realtime pacing), frames
// filtered to full-response success frames, JSONL-fed into the frozen doubao
// adapter.
func (t DoubaoTranscriber) transcribeWindow(ctx context.Context, f *os.File, meta MetaBasis, i int, seg asrstream.Segment) (asrinput.Result, error) {
	offsetBytes := seg.StartMS * 32
	lengthBytes := (seg.EndMS - seg.StartMS) * 32
	var res asrstream.Result
	var err error
	intervals := []time.Duration{-1, 200 * time.Millisecond, 200 * time.Millisecond}
	for attempt, interval := range intervals {
		if attempt > 0 {
			if _, err := f.Seek(0, io.SeekStart); err != nil {
				return asrinput.Result{}, err
			}
		}
		reader := io.NewSectionReader(f, offsetBytes, lengthBytes)
		res, err = asrstream.Transcribe(ctx, asrstream.SessionConfig{
			APIKey:         t.APIKey,
			Endpoint:       t.Endpoint,
			ResourceID:     t.ResourceID,
			ModelName:      t.ModelName,
			UID:            t.UID,
			PacketInterval: interval,
		}, reader)
		if err == nil {
			break
		}
	}
	if err != nil {
		return asrinput.Result{}, err
	}
	server := filterServerFrames(res.Frames)
	if len(server) == 0 {
		return asrinput.Result{}, fmt.Errorf("no server response frames")
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, frame := range server {
		if err := enc.Encode(frame); err != nil {
			return asrinput.Result{}, err
		}
	}
	dmeta := asrinput.Metadata{
		Provider:           "doubao",
		ProviderRequestID:  res.RequestID,
		DeviceID:           meta.DeviceID,
		RecordingID:        meta.RecordingID,
		ConversationID:     meta.ConversationID,
		SegmentID:          fmt.Sprintf("segment-%02d", i),
		SegmentOffsetMS:    seg.StartMS,
		SegmentDurationMS:  seg.EndMS - seg.StartMS,
		TranscriptRevision: meta.Revision,
		CompletionStatus:   "final",
		Model:              meta.Model,
		ResourceID:         meta.ResourceID,
		ServerLogID:        meta.ServerLogID,
	}
	if meta.SourceAudioSHA256 != "" {
		dmeta.SourceAudioSHA256 = &meta.SourceAudioSHA256
	}
	if dmeta.Model == nil {
		dmeta.Model = strPtr(t.ModelName)
	}
	if dmeta.ResourceID == nil {
		dmeta.ResourceID = strPtr(t.ResourceID)
	}
	if dmeta.ServerLogID == nil {
		dmeta.ServerLogID = strPtr(res.ServerLogID)
	}
	return doubao.Convert(dmeta, &buf)
}

// filterServerFrames keeps exactly the frames the ingest path consumes:
// full-response frames (message_type 9) with a zero error code. This is the
// same rule cmd/asr-ingest applies before normalizing.
func filterServerFrames(frames []asrstream.Frame) []asrstream.Frame {
	out := make([]asrstream.Frame, 0, len(frames))
	for _, f := range frames {
		if f.MessageType == asrstream.TypeFullResponse && f.Code == 0 {
			out = append(out, f)
		}
	}
	return out
}
