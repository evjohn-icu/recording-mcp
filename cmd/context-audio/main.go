// Command context-audio enqueues explicit audio-analysis runs (VAD, tagging,
// diarization, speaker mapping) for an existing recording. It never runs
// inference itself: the job is claimed by a configured context-worker.
package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"recordingmcp/internal/audioanalysis"
	"recordingmcp/internal/memory"
	"recordingmcp/internal/profile"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		writeError(err)
		os.Exit(1)
	}
}

func run(args []string) error {
	command := "enqueue"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command = args[0]
		args = args[1:]
	}
	if command != "enqueue" {
		return errors.New("usage: context-audio enqueue --recording-id ID --capabilities vad,tagging --request-id ID --config FILE")
	}
	fs := flag.NewFlagSet("context-audio enqueue", flag.ContinueOnError)
	dbPath := fs.String("db", "data/context-memory.db", "SQLite database (legacy mode)")
	profileCLI := profile.RegisterCLI(fs)
	audioDir := fs.String("audio-dir", "", "managed audio directory (legacy mode)")
	recordingID := fs.String("recording-id", "", "memory recording id")
	capabilities := fs.String("capabilities", "", "comma-separated capabilities: vad,tagging,diarization,speaker")
	requestID := fs.String("request-id", "", "generation: a new value forces a new run")
	configPath := fs.String("config", "", "audio-analysis-config-v1 JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	if strings.TrimSpace(*recordingID) == "" || strings.TrimSpace(*requestID) == "" || strings.TrimSpace(*configPath) == "" {
		return errors.New("--recording-id, --request-id and --config are required")
	}
	resolvedDB, resolvedAudio, err := profileCLI.Resolve(*dbPath, *audioDir)
	if err != nil {
		return err
	}
	if resolvedDB != "" {
		*dbPath = resolvedDB
	}
	if *audioDir == "" {
		*audioDir = resolvedAudio
	}
	cfg, err := audioanalysis.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	store, err := memory.Open(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	audioStore := audioanalysis.NewStore(store.DB())
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	resolved, err := resolveRecording(ctx, store.DB(), *audioDir, *recordingID)
	if err != nil {
		return err
	}
	results := []audioanalysis.EnqueueOutput{}
	for _, capability := range splitCapabilities(*capabilities) {
		resolvedCapability, err := cfg.Resolve(capability)
		if err != nil {
			return err
		}
		req := resolved.request(resolvedCapability, capability)
		dependencies, err := resolveDependencies(ctx, store.DB(), *recordingID, capability)
		if err != nil {
			return err
		}
		out, err := audioStore.Enqueue(ctx, audioanalysis.EnqueueInput{
			RecordingID:  *recordingID,
			Capability:   capability,
			Generation:   strings.TrimSpace(*requestID),
			Request:      req,
			Dependencies: dependencies,
			MaxAttempts:  3,
		})
		if err != nil {
			return err
		}
		results = append(results, out)
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", " ")
	return encoder.Encode(map[string]any{"recording_id": *recordingID, "generation": *requestID, "jobs": results})
}

// resolvedRecording is the auditable input of one explicit request.
type resolvedRecording struct {
	recordingID string
	sourceHash  string
	inputHash   string
	durationMS  int64
	attachments []audioanalysis.AttachmentRef
}

func (r resolvedRecording) request(resolved audioanalysis.Resolved, capability string) audioanalysis.Request {
	files := make(map[string]string, len(resolved.Files))
	for _, file := range resolved.Files {
		files[file.Path] = file.SHA256
	}
	snapshotHash, _ := audioanalysis.SnapshotHash(r.attachments)
	ids := make([]string, 0, len(r.attachments))
	for _, ref := range r.attachments {
		ids = append(ids, ref.ID)
	}
	req := audioanalysis.Request{
		SchemaVersion:        audioanalysis.ContractSchema,
		RecordingID:          r.recordingID,
		SourceAudioSHA256:    r.sourceHash,
		InputSHA256:          r.inputHash,
		ModelID:              resolved.ModelID,
		ModelRevision:        resolved.ModelRevision,
		ModelFilesSHA256:     files,
		Capability:           capability,
		StartMS:              0,
		EndMS:                r.durationMS,
		ConfigSHA256:         resolved.ConfigSHA256,
		SourceAttachmentIDs:  ids,
		SourceSnapshotSHA256: snapshotHash,
	}
	if capability != audioanalysis.CapabilitySpeaker {
		req.SourceAttachmentIDs = []string{}
		req.SourceSnapshotSHA256 = ""
	} else {
		req.TranscriptRevision = audioanalysis.UniqueRevision(r.attachments)
	}
	return req
}

// resolveRecording reads the managed PCM window and its provenance.
func resolveRecording(ctx context.Context, db *sql.DB, audioRoot, recordingID string) (resolvedRecording, error) {
	out := resolvedRecording{recordingID: recordingID}
	var durationMS int64
	if err := db.QueryRowContext(ctx, `SELECT duration_ms,source_sha256 FROM recordings WHERE id=?`, recordingID).Scan(&durationMS, &out.sourceHash); err != nil {
		return out, fmt.Errorf("recording not found: %w", err)
	}
	out.durationMS = durationMS
	path, err := stitchedPath(ctx, db, audioRoot, recordingID)
	if err != nil {
		return out, err
	}
	hash, err := hashFile(path)
	if err != nil {
		return out, err
	}
	out.inputHash = hash
	if len(out.sourceHash) != 64 {
		out.sourceHash = hash
	}
	rows, err := db.QueryContext(ctx, `SELECT id,transcript_revision FROM source_attachments WHERE recording_id=? AND active=1 ORDER BY id`, recordingID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var ref audioanalysis.AttachmentRef
		if err := rows.Scan(&ref.ID, &ref.Revision); err != nil {
			return out, err
		}
		out.attachments = append(out.attachments, ref)
	}
	return out, rows.Err()
}

// resolveDependencies links a speaker mapping run to the desired diarization
// and tagging jobs. A failed dependency is reported instead of silently
// running without the segment evidence.
func resolveDependencies(ctx context.Context, db *sql.DB, recordingID, capability string) ([]string, error) {
	if capability != audioanalysis.CapabilitySpeaker {
		return nil, nil
	}
	dependencies := []string{}
	for _, parent := range []string{audioanalysis.CapabilityDiarization, audioanalysis.CapabilityTagging} {
		var jobID, status string
		err := db.QueryRowContext(ctx,
			`SELECT j.id,j.status FROM audio_analysis_heads h JOIN processing_jobs j ON j.id=h.desired_job_id
			 WHERE h.recording_id=? AND h.capability=?`, recordingID, parent).Scan(&jobID, &status)
		if errors.Is(err, sql.ErrNoRows) {
			if parent == audioanalysis.CapabilityDiarization {
				return nil, audioanalysis.NewError(audioanalysis.CodeDependencyUnavailable, false, "diarization has no desired run; enqueue it first")
			}
			continue
		}
		if err != nil {
			return nil, err
		}
		if status == "failed" || status == "cancelled" {
			return nil, audioanalysis.NewError(audioanalysis.CodeDependencyUnavailable, false, "the diarization dependency failed")
		}
		dependencies = append(dependencies, jobID)
	}
	return dependencies, nil
}

// stitchedPath mirrors the app's provenance resolution.
func stitchedPath(ctx context.Context, db *sql.DB, audioRoot, recordingID string) (string, error) {
	if strings.TrimSpace(audioRoot) == "" {
		return "", errors.New("audio directory is required")
	}
	var device, provider, stitched sql.NullString
	err := db.QueryRowContext(ctx,
		`SELECT json_extract(a.source_json,'$.device_id'), json_extract(a.source_json,'$.recording_id'), s.stitched_path
		 FROM source_attachments a
		 JOIN sync_recordings s
		   ON s.recording_id = json_extract(a.source_json,'$.recording_id')
		  AND s.device_id   = json_extract(a.source_json,'$.device_id')
		 WHERE a.recording_id=? AND s.state!='deleted' AND s.stitched_path IS NOT NULL AND s.stitched_path!=''
		 ORDER BY a.id LIMIT 1`, recordingID).Scan(&device, &provider, &stitched)
	if err != nil {
		return "", fmt.Errorf("managed audio not found: %w", err)
	}
	if !device.Valid || !provider.Valid || !stitched.Valid {
		return "", errors.New("audio provenance is incomplete")
	}
	if memory.RecordingID(device.String, provider.String) != recordingID {
		return "", errors.New("audio provenance does not match the recording")
	}
	root, err := filepath.Abs(audioRoot)
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(stitched.String)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == "." || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", errors.New("audio path escapes the managed root")
	}
	return abs, nil
}

func hashFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func splitCapabilities(raw string) []string {
	out := []string{}
	for _, part := range strings.Split(raw, ",") {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			continue
		}
		if audioanalysis.IsCapability(trimmed) {
			out = append(out, trimmed)
		}
	}
	return out
}

func writeError(err error) {
	code := audioanalysis.ErrorCodeOf(err)
	if code == "" {
		code = "error"
	}
	_ = json.NewEncoder(os.Stderr).Encode(map[string]any{"error": map[string]string{"code": code, "detail": err.Error()}})
}
