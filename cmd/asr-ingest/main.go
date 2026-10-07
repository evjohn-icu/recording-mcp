// Command asr-ingest is the speech ingress adapter: it turns a recording into
// memory turns by talking to the plan-scoped doubao streaming ASR service
// directly, then normalizing and ingesting the result in-process through the
// same packages the offline chain uses (internal/adapters/doubao and
// memory.Store.IngestTranscript). It replaces the scripts/asr_full.py +
// scripts/asr_to_context.py pair, so no external agent or Python runtime is
// needed.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"recordingmcp/internal/adapters/doubao"
	"recordingmcp/internal/asrinput"
	"recordingmcp/internal/asrstream"
	"recordingmcp/internal/memory"
)

const (
	bytesPerMS   = 32 // 16 kHz mono s16le
	maxAttempts  = 3
	fileMode     = 0o600
	dirMode      = 0o700
	speakerScope = "per segment/request only"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "asr-ingest:", err)
		os.Exit(1)
	}
}

type options struct {
	audioPath          string
	format             string
	outDir             string
	key                string
	endpoint           string
	resourceID         string
	modelName          string
	uid                string
	segmentMS          int64
	overlapMS          int64
	packetMS           int
	concurrency        int
	ffmpeg             string
	ffprobe            string
	dbPath             string
	conversationID     string
	recordingID        string
	deviceID           string
	transcriptRevision string
}

func parseFlags(args []string, stderr io.Writer) (options, error) {
	fs := flag.NewFlagSet("asr-ingest", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		o      options
		keyEnv string
	)
	fs.StringVar(&o.audioPath, "audio", "", "audio file to transcribe (required)")
	fs.StringVar(&o.format, "format", "auto", "auto (decode with ffmpeg) or pcm (16 kHz mono s16le raw)")
	fs.StringVar(&o.outDir, "out", "", "run directory (default data/asr/<name>-<timestamp>)")
	fs.StringVar(&keyEnv, "key-env", "VOLC_API_KEY", "environment variable holding the API key")
	fs.StringVar(&o.key, "key", "", "API key (overrides --key-env)")
	fs.StringVar(&o.endpoint, "endpoint", asrstream.EndpointFullDuplex, "WebSocket endpoint (plan-scoped)")
	fs.StringVar(&o.resourceID, "resource-id", asrstream.ResourceASR, "X-Api-Resource-Id")
	fs.StringVar(&o.modelName, "model-name", "bigmodel", "request model_name")
	fs.StringVar(&o.uid, "uid", "recording-mcp", "request user uid")
	fs.Int64Var(&o.segmentMS, "segment-ms", 300000, "segment window length in milliseconds")
	fs.Int64Var(&o.overlapMS, "overlap-ms", 2000, "overlap between neighbouring windows")
	fs.IntVar(&o.packetMS, "packet-ms", 200, "audio frame pacing in milliseconds; 0 sends without pacing (faster for files)")
	fs.IntVar(&o.concurrency, "concurrency", 2, "segments transcribed in parallel (higher values make the service kill unpaced sessions)")
	fs.StringVar(&o.ffmpeg, "ffmpeg", "ffmpeg", "ffmpeg binary for audio decoding")
	fs.StringVar(&o.ffprobe, "ffprobe", "ffprobe", "ffprobe binary for duration probing")
	fs.StringVar(&o.dbPath, "db", "", "when set, ingest finished segments into this memory database")
	fs.StringVar(&o.conversationID, "conversation-id", "", "conversation id recorded on ingested turns")
	fs.StringVar(&o.recordingID, "recording-id", "", "recording id recorded on ingested turns")
	fs.StringVar(&o.deviceID, "device-id", "device:local", "device id recorded in the source envelope")
	fs.StringVar(&o.transcriptRevision, "transcript-revision", "asr-v1", "transcript revision label")
	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	if fs.NArg() != 0 {
		return options{}, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	if strings.TrimSpace(o.audioPath) == "" {
		return options{}, errors.New("--audio is required")
	}
	if o.key == "" {
		o.key = os.Getenv(keyEnv)
	}
	if strings.TrimSpace(o.key) == "" {
		return options{}, fmt.Errorf("no API key: pass --key or set %s", keyEnv)
	}
	switch o.format {
	case "auto", "pcm":
	default:
		return options{}, fmt.Errorf("--format must be auto or pcm, got %q", o.format)
	}
	if o.packetMS < 0 {
		return options{}, errors.New("--packet-ms must be zero (unpaced) or positive")
	}
	if o.concurrency <= 0 {
		return options{}, errors.New("--concurrency must be positive")
	}
	return o, nil
}

// defaultIDs fills the recording and conversation identifiers from the audio
// file name when they are not given: the source envelope requires a recording
// id, and a run without --db still produces the normalized transcript.
func (o *options) defaultIDs() {
	base := strings.TrimSuffix(filepath.Base(o.audioPath), filepath.Ext(o.audioPath))
	if strings.TrimSpace(o.recordingID) == "" {
		o.recordingID = "recording:" + base
	}
	if strings.TrimSpace(o.conversationID) == "" {
		o.conversationID = "conversation:" + base
	}
}

// segmentState is the manifest entry of one recognition window.
type segmentState struct {
	Index    int             `json:"index"`
	StartMS  int64           `json:"start_ms"`
	EndMS    int64           `json:"end_ms"`
	PCM      string          `json:"pcm"`
	Status   string          `json:"status"`
	Attempts []attemptRecord `json:"attempts"`
}

type attemptRecord struct {
	Attempt              int     `json:"attempt"`
	RequestID            string  `json:"request_id,omitempty"`
	ConnectID            string  `json:"connect_id,omitempty"`
	ServerLogID          string  `json:"server_log_id,omitempty"`
	Frames               int     `json:"frames,omitempty"`
	Parsed               string  `json:"parsed,omitempty"`
	FinalPackageReceived bool    `json:"final_package_received"`
	DurationSeconds      float64 `json:"duration_seconds"`
	Error                string  `json:"error,omitempty"`
}

type manifest struct {
	AudioFileName   string         `json:"audio_file_name"`
	AudioSHA256     string         `json:"audio_sha256"`
	DurationSeconds float64        `json:"duration_seconds"`
	Endpoint        string         `json:"endpoint"`
	ResourceID      string         `json:"resource_id"`
	ModelName       string         `json:"model_name"`
	ChunkSeconds    float64        `json:"chunk_seconds"`
	OverlapSeconds  float64        `json:"overlap_seconds"`
	PacketMS        int            `json:"packet_ms"`
	SpeakerScope    string         `json:"speaker_scope"`
	Concurrency     int            `json:"concurrency"`
	StartedAt       string         `json:"started_at"`
	UpdatedAt       string         `json:"updated_at,omitempty"`
	CompletedAt     string         `json:"completed_at,omitempty"`
	Segments        []segmentState `json:"segments"`
	Coverage        map[string]any `json:"coverage,omitempty"`
	AllComplete     bool           `json:"all_segments_complete"`
	APIKeySaved     bool           `json:"api_key_saved"`
	Ingested        *ingestSummary `json:"ingested,omitempty"`
}

type ingestSummary struct {
	Database  string `json:"database"`
	Segments  int    `json:"segments"`
	Turns     int    `json:"turns"`
	Inserted  int    `json:"inserted"`
	Duplicate int    `json:"duplicate"`
}

func run(args []string, stdout, stderr io.Writer) error {
	o, err := parseFlags(args, stderr)
	if err != nil {
		return err
	}
	o.defaultIDs()
	ctx := context.Background()

	source, err := os.Open(o.audioPath)
	if err != nil {
		return err
	}
	defer source.Close()
	audioSHA, err := fileSHA256(o.audioPath)
	if err != nil {
		return err
	}

	outDir := o.outDir
	if outDir == "" {
		base := strings.TrimSuffix(filepath.Base(o.audioPath), filepath.Ext(o.audioPath))
		outDir = filepath.Join("data", "asr", base+"-"+time.Now().Format("060102150405"))
	}
	segDir := filepath.Join(outDir, "segments")
	if err := os.MkdirAll(segDir, dirMode); err != nil {
		return err
	}
	_ = os.Chmod(outDir, dirMode)
	_ = os.Chmod(segDir, dirMode)

	durationMS, err := sourceDurationMS(o, source)
	if err != nil {
		return err
	}
	planned, err := asrstream.PlanSegments(durationMS, o.segmentMS, o.overlapMS)
	if err != nil {
		return err
	}
	if len(planned) == 0 {
		return errors.New("audio is shorter than one millisecond")
	}

	now := func() string { return time.Now().UTC().Format(time.RFC3339) }
	man := manifest{
		AudioFileName:   filepath.Base(o.audioPath),
		AudioSHA256:     audioSHA,
		DurationSeconds: float64(durationMS) / 1000,
		Endpoint:        o.endpoint,
		ResourceID:      o.resourceID,
		ModelName:       o.modelName,
		ChunkSeconds:    float64(o.segmentMS) / 1000,
		OverlapSeconds:  float64(o.overlapMS) / 1000,
		PacketMS:        o.packetMS,
		SpeakerScope:    speakerScope,
		Concurrency:     o.concurrency,
		StartedAt:       now(),
		APIKeySaved:     false,
	}
	for _, s := range planned {
		man.Segments = append(man.Segments, segmentState{
			Index:   s.Index,
			StartMS: s.StartMS,
			EndMS:   s.EndMS,
			PCM:     fmt.Sprintf("segment-%02d.pcm", s.Index),
			Status:  "prepared",
		})
	}
	manifestPath := filepath.Join(outDir, "run.json")
	writeManifest := func() error {
		man.UpdatedAt = now()
		return writeJSON(manifestPath, man)
	}
	if err := writeManifest(); err != nil {
		return err
	}

	// Extract the PCM once, then transcribe the windows in parallel.
	pcmPath := func(s segmentState) string { return filepath.Join(segDir, s.PCM) }
	for i, s := range man.Segments {
		if err := extractSegment(ctx, o, source, pcmPath(s), s.StartMS, s.EndMS); err != nil {
			return fmt.Errorf("segment %02d: extract audio: %w", s.Index, err)
		}
		man.Segments[i].Status = "ready"
	}
	if err := writeManifest(); err != nil {
		return err
	}

	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		sem      = make(chan struct{}, o.concurrency)
		failures []string
	)
	for i := range man.Segments {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			seg := &man.Segments[i]
			for attempt := 1; attempt <= maxAttempts; attempt++ {
				record := transcribeAttempt(ctx, o, seg, pcmPath(*seg), attempt, segDir, man.AudioSHA256)
				mu.Lock()
				seg.Attempts = append(seg.Attempts, record)
				if record.Error == "" {
					seg.Status = "complete"
				} else if attempt < maxAttempts {
					seg.Status = "retrying"
				} else {
					seg.Status = "failed"
					failures = append(failures, fmt.Sprintf("segment %02d: %s", seg.Index, record.Error))
				}
				err := writeManifest()
				mu.Unlock()
				if err != nil {
					return
				}
				if record.Error == "" {
					return
				}
				time.Sleep(time.Duration(attempt*attempt*5) * time.Second)
			}
		}(i)
	}
	wg.Wait()

	man.AllComplete = len(failures) == 0
	man.Coverage = map[string]any{
		"start_ms": 0, "end_ms": durationMS, "duration_seconds": float64(durationMS) / 1000,
	}
	man.CompletedAt = now()
	if err := writeManifest(); err != nil {
		return err
	}
	if !man.AllComplete {
		return fmt.Errorf("incomplete transcription: %s (see %s)", strings.Join(failures, "; "), manifestPath)
	}

	summary, err := ingestSegments(ctx, o, man, segDir)
	if err != nil {
		return err
	}
	if summary != nil {
		man.Ingested = summary
		if err := writeManifest(); err != nil {
			return err
		}
	}
	fmt.Fprintf(stdout, "asr-ingest: segments=%d turns=%s run=%s\n", len(man.Segments), turnsLabel(summary), outDir)
	return nil
}

func turnsLabel(s *ingestSummary) string {
	if s == nil {
		return "-"
	}
	return strconv.Itoa(s.Turns)
}

// transcribeAttempt runs one recognition request for a segment and writes its
// artifacts: the parsed frame log, the server-only frames and envelope the
// offline adapter consumes, and the normalized transcript.
func transcribeAttempt(ctx context.Context, o options, seg *segmentState, pcmFile string, attempt int, segDir, audioSHA string) attemptRecord {
	started := time.Now()
	record := attemptRecord{Attempt: attempt}
	pcm, err := os.Open(pcmFile)
	if err != nil {
		record.Error = err.Error()
		return record
	}
	defer pcm.Close()

	runCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	interval := time.Duration(o.packetMS) * time.Millisecond
	if o.packetMS == 0 {
		interval = -1 // unpaced: the service buffers a file-fed stream
		if attempt > 1 {
			// Under load the service kills unpaced sessions with its 8s idle
			// watchdog (observed with 4 parallel unpaced windows), so retries
			// fall back to real-time pacing, which the reference client used.
			interval = 200 * time.Millisecond
		}
	}
	res, err := asrstream.Transcribe(runCtx, asrstream.SessionConfig{
		APIKey:         o.key,
		Endpoint:       o.endpoint,
		ResourceID:     o.resourceID,
		ModelName:      o.modelName,
		UID:            o.uid,
		PacketInterval: interval,
	}, pcm)
	record.DurationSeconds = time.Since(started).Seconds()
	record.RequestID, record.ConnectID, record.ServerLogID = res.RequestID, res.ConnectID, res.ServerLogID
	record.Frames = len(res.Frames)

	parsedName := fmt.Sprintf("segment-%02d-attempt-%d.jsonl", seg.Index, attempt)
	if len(res.Frames) > 0 {
		if err := writeFrames(filepath.Join(segDir, parsedName), res.Frames); err != nil {
			record.Error = err.Error()
			return record
		}
		record.Parsed = parsedName
	}
	if err != nil {
		record.Error = err.Error()
		return record
	}
	record.FinalPackageReceived = true

	server := make([]asrstream.Frame, 0, len(res.Frames))
	for _, f := range res.Frames {
		if f.MessageType == asrstream.TypeFullResponse && f.Code == 0 {
			server = append(server, f)
		}
	}
	if len(server) == 0 {
		record.Error = "no server response frames"
		return record
	}
	serverName := fmt.Sprintf("segment-%02d-server-only.jsonl", seg.Index)
	if err := writeFrames(filepath.Join(segDir, serverName), server); err != nil {
		record.Error = err.Error()
		return record
	}

	sha := audioSHA
	model, resource, logID := o.modelName, o.resourceID, res.ServerLogID
	meta := asrinput.Metadata{
		Provider:           "doubao",
		ProviderRequestID:  res.RequestID,
		DeviceID:           o.deviceID,
		RecordingID:        o.recordingID,
		ConversationID:     o.conversationID,
		SegmentID:          fmt.Sprintf("segment-%02d", seg.Index),
		SegmentOffsetMS:    seg.StartMS,
		SegmentDurationMS:  seg.EndMS - seg.StartMS,
		TranscriptRevision: o.transcriptRevision,
		CompletionStatus:   "final",
		Model:              &model,
		ResourceID:         &resource,
		ServerLogID:        &logID,
	}
	if sha != "" {
		meta.SourceAudioSHA256 = &sha
	}
	envelopeName := fmt.Sprintf("segment-%02d-envelope.json", seg.Index)
	if err := writeJSON(filepath.Join(segDir, envelopeName), meta); err != nil {
		record.Error = err.Error()
		return record
	}

	serverFrames, err := os.Open(filepath.Join(segDir, serverName))
	if err != nil {
		record.Error = err.Error()
		return record
	}
	defer serverFrames.Close()
	normalized, err := doubao.Convert(meta, serverFrames)
	if err != nil {
		record.Error = fmt.Sprintf("normalize: %v", err)
		return record
	}
	if err := writeJSON(filepath.Join(segDir, fmt.Sprintf("segment-%02d-normalized.json", seg.Index)), normalized); err != nil {
		record.Error = err.Error()
		return record
	}
	return record
}

// ingestSegments replays every finished segment's normalized transcript into
// the memory database, in segment order.
func ingestSegments(ctx context.Context, o options, man manifest, segDir string) (*ingestSummary, error) {
	if o.dbPath == "" {
		return nil, nil
	}
	store, err := memory.Open(o.dbPath)
	if err != nil {
		return nil, err
	}
	defer store.Close()

	summary := &ingestSummary{Database: o.dbPath}
	for _, seg := range man.Segments {
		if seg.Status != "complete" {
			return nil, fmt.Errorf("segment %02d is %s; not ingesting a partial run", seg.Index, seg.Status)
		}
		path := filepath.Join(segDir, fmt.Sprintf("segment-%02d-normalized.json", seg.Index))
		result, err := readNormalized(path)
		if err != nil {
			return nil, err
		}
		report, err := store.IngestTranscript(ctx, result)
		if err != nil {
			return nil, fmt.Errorf("segment %02d: ingest: %w", seg.Index, err)
		}
		summary.Segments++
		summary.Turns += report.Turns
		if report.Inserted {
			summary.Inserted++
		} else {
			summary.Duplicate++
		}
	}
	return summary, nil
}

func readNormalized(path string) (asrinput.Result, error) {
	f, err := os.Open(path)
	if err != nil {
		return asrinput.Result{}, err
	}
	defer f.Close()
	var result asrinput.Result
	if err := json.NewDecoder(f).Decode(&result); err != nil {
		return asrinput.Result{}, fmt.Errorf("decode %s: %w", path, err)
	}
	return result, nil
}

// sourceDurationMS probes the recording length, or derives it from the byte
// length when the input is already raw PCM.
func sourceDurationMS(o options, source *os.File) (int64, error) {
	if o.format == "pcm" {
		info, err := source.Stat()
		if err != nil {
			return 0, err
		}
		return info.Size() / bytesPerMS, nil
	}
	out, err := exec.Command(o.ffprobe, "-v", "error", "-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1", o.audioPath).Output()
	if err != nil {
		return 0, fmt.Errorf("ffprobe %s: %w", o.audioPath, err)
	}
	seconds, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		return 0, fmt.Errorf("ffprobe duration %q: %w", strings.TrimSpace(string(out)), err)
	}
	return int64(seconds * 1000), nil
}

// extractSegment materializes one window as 16 kHz mono s16le PCM.
func extractSegment(ctx context.Context, o options, source *os.File, dest string, startMS, endMS int64) error {
	if o.format == "pcm" {
		data, err := io.ReadAll(io.NewSectionReader(source, startMS*bytesPerMS, (endMS-startMS)*bytesPerMS))
		if err != nil {
			return err
		}
		return os.WriteFile(dest, data, fileMode)
	}
	cmd := exec.CommandContext(ctx, o.ffmpeg, "-nostdin", "-v", "error",
		"-ss", strconv.FormatFloat(float64(startMS)/1000, 'f', 3, 64),
		"-i", o.audioPath,
		"-t", strconv.FormatFloat(float64(endMS-startMS)/1000, 'f', 3, 64),
		"-f", "s16le", "-acodec", "pcm_s16le", "-ar", "16000", "-ac", "1", "-y", dest)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func writeFrames(path string, frames []asrstream.Frame) error {
	var b strings.Builder
	for _, f := range frames {
		line, err := json.Marshal(f)
		if err != nil {
			return err
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	return os.WriteFile(path, []byte(b.String()), fileMode)
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), fileMode)
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
