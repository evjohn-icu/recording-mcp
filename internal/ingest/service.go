package ingest

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"recordingmcp/internal/cloudsync"
	"recordingmcp/internal/memory"
	"recordingmcp/internal/process"
)

// allowedExtensions is the pre-filter. ffprobe still has to prove the bytes are
// audio: a whitelisted extension alone is not evidence.
var allowedExtensions = map[string]bool{
	".wav": true, ".m4a": true, ".mp3": true, ".aac": true,
	".flac": true, ".ogg": true, ".opus": true,
}

// Service imports audio and runs the ingest/asr worker stages.
type Service struct {
	store     *memory.Store
	db        *sql.DB
	audioDir  string
	decoder   Decoder
	trans     cloudsync.Transcriber
	provider  string
	model     string
	resource  string
	revision  string
	now       func() time.Time
	importJID string
}

// New builds the importer. The audio directory is resolved once so a later
// working-directory change cannot move the containment root.
func New(cfg Config) (*Service, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	audioDir, err := filepath.Abs(cfg.AudioDir)
	if err != nil {
		return nil, fmt.Errorf("ingest: resolve audio dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(audioDir, "imports"), 0o700); err != nil {
		return nil, fmt.Errorf("ingest: create imports dir: %w", err)
	}
	revision := strings.TrimSpace(cfg.TranscriptRevision)
	if revision == "" {
		revision = DefaultTranscriptRevision
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Service{
		store: cfg.Store, db: cfg.DB, audioDir: audioDir, decoder: cfg.Decoder,
		trans: cfg.Transcription, provider: strings.ToLower(strings.TrimSpace(cfg.ASRProvider)),
		model: cfg.ASRModel, resource: cfg.ASRResourceID, revision: revision, now: now,
	}, nil
}

// Decoder exposes the configured decoder for diagnostics.
func (s *Service) Decoder() Decoder { return s.decoder }

func (s *Service) decoderOrErr() (Decoder, error) {
	if s.decoder == nil {
		return nil, &Error{Code: "decoder_unavailable", Message: "audio decoder is not configured", Action: "install ffmpeg/ffprobe or restart with --ffmpeg/--ffprobe", Status: 503, FatalFlag: true}
	}
	return s.decoder, nil
}

type ingestPayload struct {
	ConversationID         string `json:"conversation_id"`
	RecordingID            string `json:"recording_id"`
	ProviderRecordingID    string `json:"provider_recording_id"`
	ProviderConversationID string `json:"provider_conversation_id"`
	DeviceID               string `json:"device_id"`
	AudioPath              string `json:"audio_path"`
	SourceSHA256           string `json:"source_sha256"`
	DurationMS             int64  `json:"duration_ms"`
	FileName               string `json:"file_name"`
}

type asrPayload struct {
	ConversationID         string `json:"conversation_id"`
	RecordingID            string `json:"recording_id"`
	ProviderRecordingID    string `json:"provider_recording_id"`
	ProviderConversationID string `json:"provider_conversation_id"`
	DeviceID               string `json:"device_id"`
	PCMPath                string `json:"pcm_path"`
	SourceSHA256           string `json:"source_sha256"`
	DurationMS             int64  `json:"duration_ms"`
}

// Import streams one upload into the private audio directory, probes it and
// queues exactly one ingest job. Unsupported or unreadable files write no row.
func (s *Service) Import(ctx context.Context, r io.Reader, filename string, size int64) (ImportResult, error) {
	if s.provider == "" || s.provider == "off" || s.trans == nil {
		return ImportResult{}, &Error{
			Code: "asr_unavailable", Message: "音频导入未启用：未配置 ASR", Status: 409,
			Action: "设置 ASR_PROVIDER=doubao 并提供 VOLC_API_KEY，或设置 ASR_PROVIDER=ark|qwen 使用本地 float16 模型，或使用明确的 fixture 演示模式",
		}
	}
	name := filepath.Base(strings.TrimSpace(filename))
	ext := strings.ToLower(filepath.Ext(name))
	if name == "" || name == "." || !allowedExtensions[ext] {
		return ImportResult{}, &Error{
			Code: "unsupported_format", Message: "不支持的音频格式", Status: 415,
			Action: "请上传 wav/m4a/mp3/aac/flac/ogg/opus 音频",
		}
	}
	decoder, err := s.decoderOrErr()
	if err != nil {
		return ImportResult{}, err
	}
	importsDir := filepath.Join(s.audioDir, "imports")
	tmp, err := os.CreateTemp(importsDir, ".tmp-upload-*")
	if err != nil {
		return ImportResult{}, fmt.Errorf("ingest: create upload: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(tmp, hash), io.LimitReader(r, MaxUploadBytes+1))
	closeErr := tmp.Close()
	if err != nil {
		return ImportResult{}, fmt.Errorf("ingest: write upload: %w", err)
	}
	if closeErr != nil {
		return ImportResult{}, fmt.Errorf("ingest: close upload: %w", closeErr)
	}
	if written > MaxUploadBytes || (size > 0 && size > MaxUploadBytes) {
		return ImportResult{}, &Error{
			Code: "file_too_large", Message: "音频文件超过 2 GiB", Status: 413,
			Action: "请先压缩或裁剪音频后再上传",
		}
	}
	if written == 0 {
		return ImportResult{}, &Error{Code: "empty_file", Message: "上传的音频为空", Status: 400, Action: "请选择包含音频内容的文件"}
	}
	info, err := decoder.Probe(ctx, tmpName)
	if err != nil {
		return ImportResult{}, &Error{
			Code: "invalid_audio", Message: "无法识别音频内容", Status: 422,
			Action: "请确认文件是完整的音频，或先用 ffmpeg 转码", Err: err,
		}
	}
	sum := hex.EncodeToString(hash.Sum(nil))
	providerID := "import:" + sum[:32]
	deviceID := DeviceID
	conversationID := memory.ConversationID(deviceID, providerID)
	recordingID := memory.RecordingID(deviceID, providerID)

	// Duplicate: the same bytes were already accepted. Return the existing
	// ids and job without writing anything.
	if existing, jobID, err := s.existingImport(ctx, conversationID, recordingID, sum); err != nil {
		return ImportResult{}, err
	} else if existing {
		return ImportResult{
			ConversationID: conversationID, RecordingID: recordingID, JobID: jobID,
			Duplicate: true, Audio: info, FileName: name, Bytes: written,
		}, nil
	}

	audioRel := filepath.Join("imports", providerID+ext)
	audioAbs := filepath.Join(s.audioDir, audioRel)
	if err := os.Rename(tmpName, audioAbs); err != nil {
		return ImportResult{}, fmt.Errorf("ingest: store upload: %w", err)
	}

	payload, err := json.Marshal(ingestPayload{
		ConversationID: conversationID, RecordingID: recordingID,
		ProviderRecordingID: providerID, ProviderConversationID: providerID,
		DeviceID: deviceID, AudioPath: audioRel, SourceSHA256: sum,
		DurationMS: info.DurationMS, FileName: name,
	})
	if err != nil {
		return ImportResult{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ImportResult{}, err
	}
	defer tx.Rollback()
	if _, err := s.store.EnsurePendingRecordingTx(ctx, tx, memory.PendingRecording{
		DeviceID: deviceID, ProviderConversationID: providerID, ProviderRecordingID: providerID,
		Title: name, SourceFile: name, SourceSHA256: sum, SourceBytes: written, DurationMS: info.DurationMS,
	}); err != nil {
		return ImportResult{}, err
	}
	jobID, _, err := process.EnqueueTx(ctx, tx, process.Spec{
		Stage:           "ingest",
		InputSnapshotID: conversationID,
		IdempotencyKey:  "ingest|" + providerID + "|" + sum,
		PayloadJSON:     string(payload),
		MaxAttempts:     3,
	}, s.now())
	if err != nil {
		return ImportResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return ImportResult{}, err
	}
	return ImportResult{
		ConversationID: conversationID, RecordingID: recordingID, JobID: jobID,
		Audio: info, FileName: name, Bytes: written,
	}, nil
}

// existingImport reports whether the stable conversation already exists and
// returns its queued ingest job id when one is present.
func (s *Service) existingImport(ctx context.Context, conversationID, recordingID, sha string) (bool, string, error) {
	var bound string
	err := s.db.QueryRowContext(ctx, `SELECT recording_id FROM conversations WHERE id=?`, conversationID).Scan(&bound)
	if errors.Is(err, sql.ErrNoRows) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	if bound != recordingID {
		return false, "", fmt.Errorf("%w: conversation %s is bound to %s", memory.ErrImportConflict, conversationID, bound)
	}
	var storedSHA string
	if err := s.db.QueryRowContext(ctx, `SELECT source_sha256 FROM recordings WHERE id=?`, recordingID).Scan(&storedSHA); err != nil {
		return false, "", err
	}
	if storedSHA != sha {
		return false, "", fmt.Errorf("%w: same identity with different bytes", memory.ErrImportConflict)
	}
	var jobID string
	err = s.db.QueryRowContext(ctx, `SELECT id FROM processing_jobs WHERE idempotency_key=?`, "ingest|import:"+sha[:32]+"|"+sha).Scan(&jobID)
	if errors.Is(err, sql.ErrNoRows) {
		return true, "", nil
	}
	if err != nil {
		return false, "", err
	}
	return true, jobID, nil
}

// RunIngestJob decodes the stored upload to PCM, records the sync_recording and
// queues the ASR stage. It is idempotent: replay overwrites the same row and
// reuses the same ASR job key.
func (s *Service) RunIngestJob(ctx context.Context, job process.Job) (string, error) {
	var p ingestPayload
	if err := json.Unmarshal([]byte(job.PayloadJSON), &p); err != nil {
		return "", &Error{Code: "invalid_job_payload", Message: "ingest job payload is not valid JSON", FatalFlag: true, Err: err}
	}
	if p.ConversationID == "" || p.ProviderRecordingID == "" || p.AudioPath == "" {
		return "", &Error{Code: "invalid_job_payload", Message: "ingest job payload is incomplete", FatalFlag: true}
	}
	audioAbs, err := s.resolve(p.AudioPath)
	if err != nil {
		return "", &Error{Code: "audio_source_missing", Message: "导入的音频文件不可用", FatalFlag: true, Err: err}
	}
	decoder, err := s.decoderOrErr()
	if err != nil {
		return "", err
	}
	pcmRel := filepath.Join("imports", p.ProviderRecordingID+".pcm")
	pcmAbs := filepath.Join(s.audioDir, pcmRel)
	if err := decoder.ToPCM(ctx, audioAbs, pcmAbs); err != nil {
		return "", &Error{Code: "decode_failed", Message: "音频解码失败", Action: "确认 ffmpeg 可读取该文件；损坏文件请重新上传", Err: err}
	}
	st, err := os.Stat(pcmAbs)
	if err != nil {
		return "", fmt.Errorf("ingest: stat pcm: %w", err)
	}
	durationMS := p.DurationMS
	if durationMS <= 0 {
		durationMS = st.Size() / 32
	}
	payload, err := json.Marshal(asrPayload{
		ConversationID: p.ConversationID, RecordingID: p.RecordingID,
		ProviderRecordingID: p.ProviderRecordingID, ProviderConversationID: p.ProviderConversationID,
		DeviceID: p.DeviceID, PCMPath: pcmRel, SourceSHA256: p.SourceSHA256, DurationMS: durationMS,
	})
	if err != nil {
		return "", err
	}
	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO sync_recordings(recording_id,owner,device_id,state,total_chunks,total_samples,stitched_path,transcript_revision,transcription_state,updated_at)
		VALUES(?,?,?,'downloaded',1,?,?,?,'pending',?)
		ON CONFLICT(recording_id) DO UPDATE SET state='downloaded', total_chunks=1, total_samples=excluded.total_samples,
			stitched_path=excluded.stitched_path, transcript_revision=excluded.transcript_revision,
			transcription_state='pending', last_error=NULL, updated_at=excluded.updated_at`,
		p.ProviderRecordingID, ImportOwner, p.DeviceID, durationMS*16, pcmAbs, s.revision, now.UnixMilli()); err != nil {
		return "", err
	}
	if _, _, err := process.EnqueueTx(ctx, tx, process.Spec{
		Stage:           "asr",
		InputSnapshotID: p.ConversationID,
		IdempotencyKey:  "asr|" + p.ProviderRecordingID + "|" + p.SourceSHA256,
		PayloadJSON:     string(payload),
		MaxAttempts:     3,
	}, now); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return "ingested:" + p.ConversationID, nil
}

// RunASRJob transcribes one PCM file and hands each result to the existing
// memory ingestion path. Partial results from an interrupted attempt are
// replayed idempotently by IngestTranscript.
func (s *Service) RunASRJob(ctx context.Context, job process.Job) (string, error) {
	var p asrPayload
	if err := json.Unmarshal([]byte(job.PayloadJSON), &p); err != nil {
		return "", &Error{Code: "invalid_job_payload", Message: "asr job payload is not valid JSON", FatalFlag: true, Err: err}
	}
	if p.ConversationID == "" || p.ProviderRecordingID == "" || p.PCMPath == "" {
		return "", &Error{Code: "invalid_job_payload", Message: "asr job payload is incomplete", FatalFlag: true}
	}
	if s.trans == nil {
		action := "设置 ASR_PROVIDER=doubao 并提供 VOLC_API_KEY"
		switch s.provider {
		case "ark", "qwen", "local":
			action = "确认 .venv-asr 可用、本地模型已下载且 GPU 显存足够后重试"
		}
		return "", &Error{Code: "asr_unavailable", Message: "ASR provider is not configured", Action: action, FatalFlag: true}
	}
	pcmAbs, err := s.resolve(p.PCMPath)
	if err != nil {
		return "", &Error{Code: "audio_source_missing", Message: "PCM 文件不可用", FatalFlag: true, Err: err}
	}
	meta := cloudsync.MetaBasis{
		DeviceID: p.DeviceID, RecordingID: p.ProviderRecordingID, ConversationID: p.ProviderConversationID,
		Revision: s.revision, SourceAudioSHA256: p.SourceSHA256,
	}
	if strings.TrimSpace(s.model) != "" {
		meta.Model = new(s.model)
	}
	if strings.TrimSpace(s.resource) != "" {
		meta.ResourceID = new(s.resource)
	}
	results, err := s.trans.Transcribe(ctx, pcmAbs, meta)
	if err != nil {
		s.markASRFailed(context.WithoutCancel(ctx), p.ProviderRecordingID, err)
		return "", &Error{Code: "asr_failed", Message: "语音转写失败", Action: "检查 ASR 凭据与网络后重试", Err: err}
	}
	for _, result := range results {
		if _, err := s.store.IngestTranscript(ctx, result); err != nil {
			s.markASRFailed(context.WithoutCancel(ctx), p.ProviderRecordingID, err)
			return "", &Error{Code: "ingest_failed", Message: "转写结果写入失败", Err: err}
		}
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE sync_recordings SET state='ingested', transcription_state='done', last_error=NULL, updated_at=? WHERE recording_id=?`, s.now().UnixMilli(), p.ProviderRecordingID); err != nil {
		return "", err
	}
	return fmt.Sprintf("asr:%d", len(results)), nil
}

func (s *Service) markASRFailed(ctx context.Context, providerRecordingID string, cause error) {
	_, _ = s.db.ExecContext(ctx, `UPDATE sync_recordings SET transcription_state='failed', last_error=?, updated_at=? WHERE recording_id=?`,
		cause.Error(), s.now().UnixMilli(), providerRecordingID)
}

// resolve maps a stored relative audio path to an absolute file inside the
// audio root, refusing traversal and symlinks.
func (s *Service) resolve(rel string) (string, error) {
	clean := filepath.Clean(rel)
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("ingest: audio path %q escapes the audio root", rel)
	}
	full := filepath.Join(s.audioDir, clean)
	if !strings.HasPrefix(full, s.audioDir+string(filepath.Separator)) {
		return "", fmt.Errorf("ingest: audio path %q escapes the audio root", rel)
	}
	info, err := os.Lstat(full)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("ingest: audio path %q is not a regular file", rel)
	}
	return full, nil
}
