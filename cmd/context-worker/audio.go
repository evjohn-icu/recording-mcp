package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"recordingmcp/internal/audioanalysis"
	"recordingmcp/internal/cloudsync"
	"recordingmcp/internal/memory"
	"recordingmcp/internal/process"
	"recordingmcp/internal/worker"
)

// audioStage adapts one audio-analysis capability to the worker Executor
// boundary. RunJob commits or fails the run and the job atomically, so the
// stage reports ErrFinalized and the worker skips its own Complete/Fail.
type audioStage struct {
	runner     *audioanalysis.Runner
	capability string
}

func (s *audioStage) Execute(ctx context.Context, job process.Job) (string, error) {
	if err := s.runner.RunJob(ctx, job); err != nil {
		return "", err
	}
	return "audio:" + s.capability, fmt.Errorf("%w", worker.ErrFinalized)
}

// buildAudioStages loads the optional audio configuration and returns the
// enabled stages plus the reason code of every configured-but-unavailable
// capability. A missing config file disables audio entirely.
func buildAudioStages(configPath string, db *sql.DB, store *memory.Store, audioDir string, logger *log.Logger) (*audioanalysis.Config, string, map[string]*audioStage, map[string]string, *audioanalysis.Store, error) {
	stages := map[string]*audioStage{}
	unavailable := map[string]string{}
	trimmed := strings.TrimSpace(configPath)
	if trimmed == "" {
		return nil, "", stages, unavailable, audioanalysis.NewStore(db), nil
	}
	abs, err := filepath.Abs(trimmed)
	if err != nil {
		return nil, "", nil, nil, nil, err
	}
	cfg, err := audioanalysis.LoadConfig(abs)
	if err != nil {
		return nil, "", nil, nil, nil, err
	}
	audioStore := audioanalysis.NewStore(db)
	resolver := &recordingAudioResolver{db: db, audioRoot: audioDir}
	for _, capability := range audioanalysis.Capabilities {
		resolved, err := cfg.Resolve(capability)
		if err != nil {
			code := audioanalysis.ErrorCodeOf(err)
			if code == "" {
				code = "audio_unavailable"
			}
			logger.Printf("context-worker: audio capability %s unavailable (%s)", capability, code)
			if _, configured := cfg.Capabilities[capability]; configured {
				unavailable["audio_"+capability] = code
			}
			continue
		}
		analyzer := audioanalysis.SubprocessAnalyzer{
			Config:     cfg,
			ConfigPath: abs,
			Resolved:   resolved,
		}
		if strings.HasPrefix(strings.ToLower(resolved.Device), "cuda") {
			analyzer.VRAMFreeMiB = func(context.Context) (int, error) {
				_, _, free := cloudsync.GPUInfo()
				return free, nil
			}
		}
		stages[capability] = &audioStage{
			capability: capability,
			runner: &audioanalysis.Runner{
				Store:     audioStore,
				Analyzer:  analyzer,
				Resolver:  resolver,
				AudioRoot: audioDir,
			},
		}
	}
	_ = store
	return cfg, abs, stages, unavailable, audioStore, nil
}

// recoverAudioStages terminates claimable audio jobs of stages nobody claims,
// so a disabled or unavailable model cannot block ASR dependents. It is driven
// by the pipeline worker after its first successful heartbeat and on every 5s
// heartbeat tick (--once included).
//
// The claim set is the live valid-stage union: the stages live workers
// (heartbeat at most 30 seconds old, inclusive) still claim, plus this
// worker's own configured stages — a worker never terminates jobs of a stage
// it would claim itself. Recovery only fires when a live worker actually
// claims ingest or ASR: those are the dependents it unblocks, and an audio
// stage without a claimant that also has no ingest/ASR claimant anywhere has
// no dependent to unblock.
func recoverAudioStages(ctx context.Context, audioStore *audioanalysis.Store, enabled []string, unavailable map[string]string, now time.Time) error {
	live, err := liveWorkerStages(ctx, audioStore.DB, now)
	if err != nil {
		return err
	}
	if !live["ingest"] && !live["asr"] {
		return nil
	}
	protected := map[string]bool{}
	for _, stage := range enabled {
		protected[stage] = true
	}
	for stage := range live {
		protected[stage] = true
	}
	rows, err := audioStore.DB.QueryContext(ctx,
		`SELECT DISTINCT stage FROM processing_jobs WHERE stage LIKE 'audio\_%' ESCAPE '\' AND (status IN ('queued','retry_wait') OR (status='running' AND lease_expires_at IS NOT NULL AND lease_expires_at<=?))`,
		now.UnixMilli())
	if err != nil {
		return err
	}
	pending := []string{}
	for rows.Next() {
		var stage string
		if err := rows.Scan(&stage); err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, stage)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, stage := range pending {
		if protected[stage] {
			continue
		}
		code := audioanalysis.CodeDisabled
		if reason, ok := unavailable[stage]; ok {
			code = reason
		}
		if err := audioStore.FailUnclaimed(ctx, stage, code, now); err != nil {
			return err
		}
	}
	return nil
}

// liveWorkerStages returns the union of stages that live workers — heartbeats
// at most 30 seconds old, inclusive — still claim.
func liveWorkerStages(ctx context.Context, db *sql.DB, now time.Time) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT stages_json FROM worker_heartbeats WHERE last_seen_at >= ?`,
		now.Add(-30*time.Second).UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	valid := map[string]bool{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var stages []string
		if err := json.Unmarshal([]byte(raw), &stages); err != nil {
			continue
		}
		for _, stage := range stages {
			valid[stage] = true
		}
	}
	return valid, rows.Err()
}

// recordingAudioResolver serves the requested PCM window from the managed
// audio directory. It never accepts a caller-supplied path.
type recordingAudioResolver struct {
	db        *sql.DB
	audioRoot string
}

func (r *recordingAudioResolver) RecordingWindow(ctx context.Context, recordingID string, startMS, endMS int64) (io.ReadCloser, error) {
	rel, err := audioanalysis.ResolveRecordingPCM(ctx, r.db, r.audioRoot, recordingID)
	if err != nil {
		return nil, err
	}
	if startMS < 0 || endMS <= startMS || endMS > int64(^uint64(0)>>1)/32 {
		return nil, audioanalysis.NewError(audioanalysis.CodeInputInvalid, false, "invalid window")
	}
	root, err := os.OpenRoot(r.audioRoot)
	if err != nil {
		return nil, audioanalysis.NewError(audioanalysis.CodeModelUnavailable, false, "audio directory is not readable")
	}
	defer root.Close()
	info, err := root.Stat(rel)
	if err != nil || !info.Mode().IsRegular() {
		return nil, audioanalysis.NewError(audioanalysis.CodeModelUnavailable, false, "audio file is missing")
	}
	from := startMS * 32
	to := endMS * 32
	if from >= info.Size() || to > info.Size() {
		return nil, audioanalysis.NewError(audioanalysis.CodeInputInvalid, false, "window exceeds the audio file")
	}
	file, err := root.Open(rel)
	if err != nil {
		return nil, audioanalysis.NewError(audioanalysis.CodeModelUnavailable, false, "audio file is unreadable")
	}
	return &sectionReadCloser{file: file, reader: io.NewSectionReader(file, from, to-from)}, nil
}

func (r *recordingAudioResolver) SourceAudioSHA256(ctx context.Context, recordingID string) (string, error) {
	var hash string
	if err := r.db.QueryRowContext(ctx, `SELECT source_sha256 FROM recordings WHERE id=?`, recordingID).Scan(&hash); err != nil {
		return "", err
	}
	return hash, nil
}

// sectionReadCloser keeps the underlying file open for the section reader.
type sectionReadCloser struct {
	file   *os.File
	reader *io.SectionReader
}

func (s *sectionReadCloser) Read(p []byte) (int, error) { return s.reader.Read(p) }

func (s *sectionReadCloser) Close() error { return s.file.Close() }
