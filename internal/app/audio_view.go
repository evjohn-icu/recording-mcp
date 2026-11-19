package app

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"recordingmcp/internal/audioanalysis"
	"recordingmcp/internal/memory"
	"recordingmcp/internal/retention"
)

// AudioAnalysisView is the shared, vector-free audio-analysis read projection
// used by Web and MCP. It reports each configured capability's status and the
// current run of every capability that has one.
type AudioAnalysisView struct {
	RecordingID  string                  `json:"recording_id"`
	Capabilities []AudioCapabilityView   `json:"capabilities"`
	Runs         []audioanalysis.RunView `json:"runs"`
}

// AudioCapabilityView is one capability's effective status.
type AudioCapabilityView struct {
	Capability string `json:"capability"`
	Status     string `json:"status"` // disabled|unavailable|not_requested|queued|running|succeeded|failed|invalidated
	Reason     string `json:"reason,omitempty"`
	Action     string `json:"action,omitempty"`
}

// AudioAnalysis returns the read projection for one recording. A service
// without an audio store returns an empty view instead of an error.
func (s *Service) AudioAnalysis(ctx context.Context, recordingID string) (AudioAnalysisView, error) {
	view := AudioAnalysisView{RecordingID: recordingID, Capabilities: []AudioCapabilityView{}, Runs: []audioanalysis.RunView{}}
	if strings.TrimSpace(recordingID) == "" {
		return view, validationError("recording_id is required")
	}
	if s.AudioRuns == nil {
		for _, capability := range audioanalysis.Capabilities {
			view.Capabilities = append(view.Capabilities, AudioCapabilityView{Capability: capability, Status: "disabled", Reason: audioanalysis.CodeDisabled})
		}
		return view, nil
	}
	runs, err := s.AudioRuns.CurrentRuns(ctx, recordingID)
	if err != nil {
		return view, err
	}
	view.Runs = runs
	current := map[string]audioanalysis.RunView{}
	for _, run := range runs {
		current[run.Capability] = run
	}
	for _, capability := range audioanalysis.Capabilities {
		entry := AudioCapabilityView{Capability: capability}
		if run, ok := current[capability]; ok {
			entry.Status = run.Status
			view.Capabilities = append(view.Capabilities, entry)
			continue
		}
		status, reason, action := s.audioCapabilityStatus(ctx, recordingID, capability)
		entry.Status, entry.Reason, entry.Action = status, reason, action
		view.Capabilities = append(view.Capabilities, entry)
	}
	return view, nil
}

// audioCapabilityStatus resolves a capability without a current run.
func (s *Service) audioCapabilityStatus(ctx context.Context, recordingID, capability string) (string, string, string) {
	if s.AudioConfig == nil {
		return "disabled", audioanalysis.CodeDisabled, "configure --audio-analysis-config to enable local audio analysis"
	}
	if _, err := s.AudioConfig.Resolve(capability); err != nil {
		code := audioanalysis.ErrorCodeOf(err)
		if code == audioanalysis.CodeDisabled {
			return "disabled", code, "enable the capability in the audio analysis config"
		}
		return "unavailable", code, "provide the pinned local model files"
	}
	var jobID, status string
	err := s.Store.DB().QueryRowContext(ctx,
		`SELECT j.id,j.status FROM audio_analysis_heads h JOIN processing_jobs j ON j.id=h.desired_job_id
		 WHERE h.recording_id=? AND h.capability=?`, recordingID, capability).Scan(&jobID, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return "not_requested", "", "enqueue the capability with context-audio"
	}
	if err != nil {
		return "unavailable", "audio_status_unavailable", "retry after the database is readable"
	}
	switch status {
	case "queued", "retry_wait":
		return "queued", "", ""
	case "running":
		return "running", "", ""
	case "failed", "cancelled":
		return "failed", "audio_run_failed", "inspect the safe error and retry with a new request id"
	case "succeeded":
		return "invalidated", "audio_result_invalidated", "the source changed; enqueue a new run"
	default:
		return "unavailable", "audio_status_unknown", ""
	}
}

// ItemDetail is the MCP get_context_item response: the unchanged item detail
// plus an optional audio-analysis projection for its recording.
type ItemDetail struct {
	memory.ItemDetail
	AudioAnalysis *AudioAnalysisView `json:"audio_analysis,omitempty"`
}

// ItemWithAudio reads one item and, when it belongs to a recording with audio
// analysis state, attaches the shared projection.
func (s *Service) ItemWithAudio(ctx context.Context, id string, includeStale bool) (ItemDetail, error) {
	detail, err := s.GetItem(ctx, id, includeStale)
	if err != nil {
		return ItemDetail{}, err
	}
	out := ItemDetail{ItemDetail: detail}
	if s.AudioRuns == nil || detail.Item.RecordingID == "" {
		return out, nil
	}
	view, err := s.AudioAnalysis(ctx, detail.Item.RecordingID)
	if err != nil {
		return ItemDetail{}, err
	}
	out.AudioAnalysis = &view
	return out, nil
}

// EventAudioClip returns one playable audio-analysis event slice. The event
// must be on the current, active run of a recording that is not forgotten.
func (s *Service) EventAudioClip(ctx context.Context, eventID string) (*AudioClip, error) {
	if s.AudioRuns == nil {
		return nil, audioError(CodeAudioUnavailable, "此安装未启用音频分析", 404)
	}
	if strings.TrimSpace(eventID) == "" {
		return nil, validationError("event_id is required")
	}
	event, err := s.AudioRuns.EventForAudio(ctx, eventID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, audioError(CodeAudioMissing, "该片段不可用", 404)
		}
		return nil, err
	}
	var conversationID string
	if err := s.Store.DB().QueryRowContext(ctx,
		`SELECT conversation_id FROM recordings WHERE id=?`, event.RecordingID).Scan(&conversationID); err != nil {
		return nil, audioError(CodeAudioMissing, "本机找不到音频文件", 404)
	}
	release, lockErr := retention.Lock(ctx, s.audioRoot, false)
	if lockErr != nil {
		return nil, lockErr
	}
	handedOff := false
	defer func() {
		if !handedOff {
			release()
		}
	}()
	src, err := s.newAudioSource(ctx, conversationID, event.RecordingID)
	if err != nil {
		return nil, err
	}
	switch src.state {
	case AudioDeleted:
		return nil, audioError(CodeAudioDeleted, "录音已删除（转写与归纳保留）", 410)
	case AudioMissing:
		return nil, audioError(CodeAudioMissing, "本机找不到音频文件", 404)
	case AudioUnavailable:
		return nil, audioError(CodeAudioUnavailable, "此录音没有本地音频", 404)
	}
	start := event.StartMS + event.RunStartMS
	end := event.EndMS + event.RunStartMS
	from, to, ok := src.clipBounds(&start, &end)
	if !ok {
		return nil, audioError(CodeAudioOffsetOutOfRange, "音频时间超出录音范围", 409)
	}
	root, err := os.OpenRoot(s.audioRoot)
	if err != nil {
		return nil, audioError(CodeAudioMissing, "本机找不到音频文件", 404)
	}
	defer root.Close()
	rel, err := filepath.Rel(s.audioRoot, src.path)
	if err != nil || filepath.IsAbs(rel) || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return nil, audioError(CodeAudioMissing, "本机找不到音频文件", 404)
	}
	file, err := root.Open(rel)
	if err != nil {
		return nil, audioError(CodeAudioMissing, "本机找不到音频文件", 404)
	}
	if _, err := file.Seek(from, io.SeekStart); err != nil {
		file.Close()
		return nil, audioError(CodeAudioMissing, "本机找不到音频文件", 404)
	}
	handedOff = true
	return &AudioClip{release: release, RecordingID: event.RecordingID, StartMS: start, EndMS: end, PCM: file, Size: to - from}, nil
}
