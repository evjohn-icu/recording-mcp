package main

import (
	"context"
	"database/sql"
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
				Store:    audioStore,
				Analyzer: analyzer,
				Resolver: resolver,
			},
		}
	}
	_ = store
	return cfg, abs, stages, unavailable, audioStore, nil
}

// recoverAudioStages terminates claimable audio jobs of stages this worker does
// not run, so a disabled or unavailable model cannot block ASR dependents.
func recoverAudioStages(ctx context.Context, audioStore *audioanalysis.Store, enabled []string, unavailable map[string]string, now time.Time) error {
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
		enabledStage := false
		for _, candidate := range enabled {
			if candidate == stage {
				enabledStage = true
				break
			}
		}
		if enabledStage {
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

// recordingAudioResolver serves the requested PCM window from the managed
// audio directory. It never accepts a caller-supplied path.
type recordingAudioResolver struct {
	db        *sql.DB
	audioRoot string
}

func (r *recordingAudioResolver) RecordingWindow(ctx context.Context, recordingID string, startMS, endMS int64) (io.ReadCloser, error) {
	path, err := r.stitchedPath(ctx, recordingID)
	if err != nil {
		return nil, err
	}
	if startMS < 0 || endMS <= startMS || endMS > int64(^uint64(0)>>1)/32 {
		return nil, audioanalysis.NewError(audioanalysis.CodeInputInvalid, false, "invalid window")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, audioanalysis.NewError(audioanalysis.CodeModelUnavailable, false, "audio file is missing")
	}
	from := startMS * 32
	to := endMS * 32
	if from >= info.Size() || to > info.Size() {
		return nil, audioanalysis.NewError(audioanalysis.CodeInputInvalid, false, "window exceeds the audio file")
	}
	file, err := os.Open(path)
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

// stitchedPath resolves the managed audio file through the provenance bridge.
func (r *recordingAudioResolver) stitchedPath(ctx context.Context, recordingID string) (string, error) {
	if strings.TrimSpace(r.audioRoot) == "" {
		return "", audioanalysis.NewError(audioanalysis.CodeModelUnavailable, false, "no audio directory configured")
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT json_extract(source_json,'$.device_id'), json_extract(source_json,'$.recording_id'), s.stitched_path
		 FROM source_attachments a
		 JOIN sync_recordings s
		   ON s.recording_id = json_extract(a.source_json,'$.recording_id')
		  AND s.device_id   = json_extract(a.source_json,'$.device_id')
		 WHERE a.recording_id=? AND s.state!='deleted' AND s.stitched_path IS NOT NULL AND s.stitched_path!=''
		 ORDER BY a.id LIMIT 1`, recordingID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	if !rows.Next() {
		return "", audioanalysis.NewError(audioanalysis.CodeModelUnavailable, false, "no managed audio for this recording")
	}
	var device, provider, stitched sql.NullString
	if err := rows.Scan(&device, &provider, &stitched); err != nil {
		return "", err
	}
	if !device.Valid || !provider.Valid || !stitched.Valid {
		return "", audioanalysis.NewError(audioanalysis.CodeModelUnavailable, false, "audio provenance is incomplete")
	}
	if memory.RecordingID(device.String, provider.String) != recordingID {
		return "", audioanalysis.NewError(audioanalysis.CodeModelUnavailable, false, "audio provenance does not match the recording")
	}
	root, err := filepath.Abs(r.audioRoot)
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(stitched.String)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == "." || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", audioanalysis.NewError(audioanalysis.CodeModelUnavailable, false, "audio path escapes the managed root")
	}
	return abs, nil
}

// sectionReadCloser keeps the underlying file open for the section reader.
type sectionReadCloser struct {
	file   *os.File
	reader *io.SectionReader
}

func (s *sectionReadCloser) Read(p []byte) (int, error) { return s.reader.Read(p) }

func (s *sectionReadCloser) Close() error { return s.file.Close() }
