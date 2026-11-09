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
	"sort"
	"strings"
	"time"

	"recordingmcp/internal/asrinput"
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
	chunker, chunked := s.trans.(cloudsync.ChunkTranscriber)
	if !chunked {
		return s.runASRWholeFile(ctx, p, meta, pcmAbs)
	}
	durationMS := p.DurationMS
	if info, statErr := os.Stat(pcmAbs); statErr == nil && info.Size() > 0 {
		if computed := info.Size() / 32; computed > 0 {
			durationMS = computed
		}
	}
	windowMS, overlapMS := int64(30000), int64(0)
	if planner, ok := s.trans.(interface{ WindowPlan() (int64, int64) }); ok {
		windowMS, overlapMS = planner.WindowPlan()
	}
	plan, err := cloudsync.PlanWindows(durationMS, windowMS, overlapMS)
	if err != nil {
		return "", &Error{Code: "asr_plan_failed", Message: "无法规划转写窗口", Action: "检查 --asr-window-ms/--asr-overlap-ms 配置", FatalFlag: true, Err: err}
	}
	// source_attachments is the checkpoint: an active attachment for the
	// current revision means that window is already paid for.
	existing, intervals, err := s.loadASRWindows(ctx, memory.RecordingID(p.DeviceID, p.ProviderRecordingID), s.revision)
	if err != nil {
		return "", &Error{Code: "ingest_failed", Message: "无法读取已提交窗口", Err: err}
	}
	// The skip set is keyed by segment index, so the stored window bounds must
	// match the current plan. A changed --asr-window-ms/--asr-overlap-ms would
	// otherwise skip audio that was never decoded and still report done.
	planned := make(map[int][2]int64, len(plan))
	for _, window := range plan {
		planned[window.Index] = [2]int64{window.StartMS, window.EndMS}
	}
	for index, bounds := range existing {
		if want, ok := planned[index]; !ok || want != bounds {
			return "", &Error{
				Code: "asr_plan_mismatch", Message: "已提交窗口与当前窗口配置不一致",
				Action:    "恢复原来的 --asr-window-ms/--asr-overlap-ms 后重试，或删除该录音后重新导入",
				FatalFlag: true,
			}
		}
	}
	completed := 0
	skip := make([]int, 0, len(plan))
	for _, window := range plan {
		if bounds, ok := existing[window.Index]; ok && bounds == planned[window.Index] {
			completed++
			skip = append(skip, window.Index)
		}
	}
	coverage := unionCoverage(intervals)
	if completed == len(plan) && len(plan) > 0 {
		if _, err := s.db.ExecContext(ctx, `UPDATE sync_recordings SET total_chunks=?, completed_chunks=?, coverage_ms=?, state='ingested', transcription_state='done', last_error=NULL, updated_at=? WHERE recording_id=?`,
			len(plan), completed, coverage, s.now().UnixMilli(), p.ProviderRecordingID); err != nil {
			return "", err
		}
		return fmt.Sprintf("asr:%d", completed), nil
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE sync_recordings SET total_chunks=?, completed_chunks=?, coverage_ms=?, transcription_state='partial', last_error=NULL, updated_at=? WHERE recording_id=?`,
		len(plan), completed, coverage, s.now().UnixMilli(), p.ProviderRecordingID); err != nil {
		return "", err
	}
	err = chunker.TranscribeChunks(ctx, pcmAbs, meta, skip, func(result asrinput.Result) error {
		if _, err := s.store.IngestTranscript(ctx, result); err != nil {
			return err
		}
		if index := windowIndex(result.Source.SegmentID); index > 0 {
			existing[index] = [2]int64{result.Source.SegmentOffsetMS, result.Source.SegmentOffsetMS + result.Source.SegmentDurationMS}
		}
		intervals = append(intervals, [2]int64{result.Source.SegmentOffsetMS, result.Source.SegmentOffsetMS + result.Source.SegmentDurationMS})
		completed++
		coverage = unionCoverage(intervals)
		s.updateASRProgress(context.WithoutCancel(ctx), p.ProviderRecordingID, completed, coverage)
		return nil
	})
	if err != nil {
		if ctx.Err() != nil {
			// Cancellation keeps transcription_state='partial' so a restart
			// resumes instead of treating committed windows as failed work.
			return "", err
		}
		s.markASRFailedWithProgress(context.WithoutCancel(ctx), p.ProviderRecordingID, completed, err)
		code, message, action := s.asrFailureDetails(err)
		return "", &Error{Code: code, Message: message, Action: action, Err: err}
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE sync_recordings SET total_chunks=?, completed_chunks=?, coverage_ms=?, state='ingested', transcription_state='done', last_error=NULL, updated_at=? WHERE recording_id=?`,
		len(plan), completed, coverage, s.now().UnixMilli(), p.ProviderRecordingID); err != nil {
		return "", err
	}
	return fmt.Sprintf("asr:%d", completed), nil
}

// runASRWholeFile keeps the all-at-once path for providers without window
// callbacks (doubao, fixture) and still records the progress projection.
func (s *Service) runASRWholeFile(ctx context.Context, p asrPayload, meta cloudsync.MetaBasis, pcmAbs string) (string, error) {
	results, err := s.trans.Transcribe(ctx, pcmAbs, meta)
	if err != nil {
		s.markASRFailed(context.WithoutCancel(ctx), p.ProviderRecordingID, err)
		code, message, action := s.asrFailureDetails(err)
		return "", &Error{Code: code, Message: message, Action: action, Err: err}
	}
	intervals := make([][2]int64, 0, len(results))
	for _, result := range results {
		if _, err := s.store.IngestTranscript(ctx, result); err != nil {
			s.markASRFailed(context.WithoutCancel(ctx), p.ProviderRecordingID, err)
			return "", &Error{Code: "ingest_failed", Message: "转写结果写入失败", Err: err}
		}
		intervals = append(intervals, [2]int64{result.Source.SegmentOffsetMS, result.Source.SegmentOffsetMS + result.Source.SegmentDurationMS})
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE sync_recordings SET state='ingested', transcription_state='done', total_chunks=?, completed_chunks=?, failed_chunks=0, coverage_ms=?, last_error=NULL, updated_at=? WHERE recording_id=?`,
		len(results), len(results), unionCoverage(intervals), s.now().UnixMilli(), p.ProviderRecordingID); err != nil {
		return "", err
	}
	return fmt.Sprintf("asr:%d", len(results)), nil
}

// asrFailureDetails maps a typed runner error onto an actionable message. The
// code lands in processing_attempts.error_json and the UI action line.
func (s *Service) asrFailureDetails(err error) (code, message, action string) {
	var runner *cloudsync.RunnerError
	if errors.As(err, &runner) {
		switch runner.Code {
		case "asr_oom":
			return "asr_oom", "GPU 显存不足", "显存不足：关闭其他 GPU 进程或改用 ARK/更小窗口后重试"
		case "asr_model_load_failed":
			return "asr_model_load_failed", "本机 ASR 模型加载失败", "确认钉住模型已下载（scripts/fetch-local-models.sh）后重试"
		case "asr_timeout":
			return "asr_timeout", "本机 ASR 超时", "检查 GPU 状态或调大 ASR_WINDOW_TIMEOUT 后重试"
		}
	}
	action = "检查 ASR 凭据与网络后重试"
	if cloudsync.IsLocalProvider(s.provider) {
		action = "确认 .venv-asr 可用、本地模型已下载且 GPU 显存足够后重试"
	}
	return "asr_failed", "语音转写失败", action
}

func (s *Service) loadASRWindows(ctx context.Context, attachmentRecordingID, revision string) (map[int][2]int64, [][2]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT segment_id, result_json FROM source_attachments WHERE recording_id=? AND transcript_revision=? AND active=1`, attachmentRecordingID, revision)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	windows := map[int][2]int64{}
	var intervals [][2]int64
	for rows.Next() {
		var segmentID, resultJSON string
		if err := rows.Scan(&segmentID, &resultJSON); err != nil {
			return nil, nil, err
		}
		var result asrinput.Result
		if err := json.Unmarshal([]byte(resultJSON), &result); err != nil {
			return nil, nil, fmt.Errorf("ingest: decode stored ASR window %s: %w", segmentID, err)
		}
		start, end := result.Source.SegmentOffsetMS, result.Source.SegmentOffsetMS+result.Source.SegmentDurationMS
		if index := windowIndex(segmentID); index > 0 {
			windows[index] = [2]int64{start, end}
		}
		if end > start {
			intervals = append(intervals, [2]int64{start, end})
		}
	}
	return windows, intervals, rows.Err()
}

func windowIndex(segmentID string) int {
	var index int
	if _, err := fmt.Sscanf(segmentID, "segment-%d", &index); err != nil {
		return 0
	}
	return index
}

// unionCoverage returns the length covered by at least one interval. Overlapping
// windows must not be summed: coverage is a claim about audio, not work.
func unionCoverage(intervals [][2]int64) int64 {
	if len(intervals) == 0 {
		return 0
	}
	ordered := append([][2]int64(nil), intervals...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i][0] == ordered[j][0] {
			return ordered[i][1] < ordered[j][1]
		}
		return ordered[i][0] < ordered[j][0]
	})
	var total int64
	current := ordered[0]
	for _, interval := range ordered[1:] {
		if interval[0] > current[1] {
			total += current[1] - current[0]
			current = interval
			continue
		}
		if interval[1] > current[1] {
			current[1] = interval[1]
		}
	}
	return total + current[1] - current[0]
}

func (s *Service) updateASRProgress(ctx context.Context, providerRecordingID string, completed int, coverage int64) {
	_, _ = s.db.ExecContext(ctx, `UPDATE sync_recordings SET completed_chunks=?, coverage_ms=?, updated_at=? WHERE recording_id=?`,
		completed, coverage, s.now().UnixMilli(), providerRecordingID)
}

// markASRFailedWithProgress records a failed attempt without destroying the
// committed progress: any committed window means the recording is partial and
// resumable, not failed. failed_chunks counts the window that broke the run.
func (s *Service) markASRFailedWithProgress(ctx context.Context, providerRecordingID string, completed int, cause error) {
	state := "failed"
	if completed > 0 {
		state = "partial"
	}
	failedInc := ""
	var runner *cloudsync.RunnerError
	if errors.As(cause, &runner) && runner.Index > 0 {
		failedInc = ", failed_chunks=failed_chunks+1"
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE sync_recordings SET transcription_state=?`+failedInc+`, last_error=?, updated_at=? WHERE recording_id=?`,
		state, cause.Error(), s.now().UnixMilli(), providerRecordingID)
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
