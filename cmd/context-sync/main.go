// context-sync pulls recording events and audio from an ingest-v1 capture
// service into the local memory database, transcribes stitched recordings
// (fixture or 豆包 ASR) and feeds them through the existing ingestion
// pipeline. It never interprets transcripts and never touches the cloud-side
// memory logic: the local store stays the only memory authority.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"recordingmcp/internal/asrstream"
	"recordingmcp/internal/cloudsync"
	"recordingmcp/internal/memory"
)

type options struct {
	db                 string
	server             string
	syncTokenEnv       string
	audioDir           string
	poll               time.Duration
	once               bool
	asr                string
	keyEnv             string
	endpoint           string
	resourceID         string
	modelName          string
	segmentMS          int64
	overlapMS          int64
	concurrency        int
	transcriptRevision string
	deviceID           string
	noAck              bool
	status             bool
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "context-sync:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("context-sync", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	o := &options{}
	fs.StringVar(&o.db, "db", "data/context-memory.db", "experimental context memory SQLite file")
	fs.StringVar(&o.server, "server", "", "ingest-v1 capture service base URL (required)")
	fs.StringVar(&o.syncTokenEnv, "sync-token-env", "CF_SYNC_TOKEN", "env var holding the sync token")
	fs.StringVar(&o.audioDir, "audio-dir", "data/synced-audio", "controlled directory for downloaded audio")
	fs.DurationVar(&o.poll, "poll", 30*time.Second, "interval between sync passes")
	fs.BoolVar(&o.once, "once", false, "run exactly one sync pass and exit")
	fs.StringVar(&o.asr, "asr", "off", "transcriber: off|fixture|doubao")
	fs.StringVar(&o.keyEnv, "key-env", "VOLC_API_KEY", "env var holding the ASR API key (doubao)")
	fs.StringVar(&o.endpoint, "endpoint", asrstream.EndpointFullDuplex, "ASR WebSocket endpoint")
	fs.StringVar(&o.resourceID, "resource-id", asrstream.ResourceASR, "ASR resource id")
	fs.StringVar(&o.modelName, "model-name", "bigmodel", "ASR model name")
	fs.Int64Var(&o.segmentMS, "segment-ms", 300000, "ASR window length in milliseconds")
	fs.Int64Var(&o.overlapMS, "overlap-ms", 2000, "ASR window overlap in milliseconds")
	fs.IntVar(&o.concurrency, "concurrency", 2, "max concurrent ASR windows")
	fs.StringVar(&o.transcriptRevision, "transcript-revision", "asr-v1", "transcript revision stamped onto ingested segments")
	fs.StringVar(&o.deviceID, "device-id", "device:cloud", "device id fallback for ingested segments")
	fs.BoolVar(&o.noAck, "no-ack", false, "skip best-effort transcription-state acks")
	fs.BoolVar(&o.status, "status", false, "print sync_state + sync_recordings summary JSON and exit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	if o.status {
		return printStatus(o.db)
	}
	if o.server == "" {
		return errors.New("--server is required")
	}
	token := os.Getenv(o.syncTokenEnv)
	if token == "" {
		return fmt.Errorf("env %s is required (sync credential)", o.syncTokenEnv)
	}
	transcriber, err := buildTranscriber(o)
	if err != nil {
		return err
	}
	store, err := memory.Open(o.db)
	if err != nil {
		return err
	}
	defer store.Close()

	engine := &cloudsync.Engine{
		DB:               store.DB(),
		Store:            store,
		Client:           &cloudsync.Client{BaseURL: o.server, SyncToken: token},
		AudioDir:         o.audioDir,
		Transcriber:      transcriber,
		Revision:         o.transcriptRevision,
		DeviceIDFallback: o.deviceID,
		DisableAck:       o.noAck,
		Log:              log.New(os.Stderr, "context-sync ", log.LstdFlags),
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	enc := json.NewEncoder(os.Stdout)
	if o.once {
		rep, err := engine.RunOnce(ctx)
		if err != nil {
			return err
		}
		return enc.Encode(rep)
	}
	for {
		rep, err := engine.RunOnce(ctx)
		if err != nil {
			// Network and protocol failures are logged and retried on the
			// poll interval; unprocessed events are never skipped.
			engine.Log.Printf("sync pass failed: %v", err)
		} else if err := enc.Encode(rep); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(o.poll):
		}
	}
}

// buildTranscriber selects the transcriber; --asr doubao without an API key
// is a hard error, never a silent downgrade.
func buildTranscriber(o *options) (cloudsync.Transcriber, error) {
	switch o.asr {
	case "off":
		return nil, nil
	case "fixture":
		return cloudsync.FixtureTranscriber{}, nil
	case "doubao":
		key := os.Getenv(o.keyEnv)
		if key == "" {
			return nil, fmt.Errorf("--asr doubao requires env %s", o.keyEnv)
		}
		return cloudsync.DoubaoTranscriber{
			APIKey:        key,
			Endpoint:      o.endpoint,
			ResourceID:    o.resourceID,
			ModelName:     o.modelName,
			SegmentMS:     o.segmentMS,
			OverlapMS:     o.overlapMS,
			MaxConcurrent: o.concurrency,
		}, nil
	default:
		return nil, fmt.Errorf("--asr must be off|fixture|doubao, got %q", o.asr)
	}
}

// printStatus dumps the local sync state for operators and CI.
func printStatus(dbPath string) error {
	store, err := memory.Open(dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	st, err := cloudsync.Snapshot(context.Background(), store.DB())
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(st)
}
