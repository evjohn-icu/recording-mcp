package app

import (
	"context"
	"strings"
	"time"

	"recordingmcp/internal/process"
)

// ASRCapability describes whether an upload can be transcribed right now.
// GPU diagnostics come from the freshest live worker so the UI can explain a
// degraded local runtime instead of only reporting a boolean.
type ASRCapability struct {
	Provider        string `json:"provider"`
	Configured      bool   `json:"configured"`
	WorkerAvailable bool   `json:"worker_available"`
	Available       bool   `json:"available"`
	Reason          string `json:"reason,omitempty"`
	GPUName         string `json:"gpu_name,omitempty"`
	VRAMTotalMB     int    `json:"vram_total_mb"`
	VRAMFreeMB      int    `json:"vram_free_mb"`
	Device          string `json:"device,omitempty"`
	Dtype           string `json:"dtype,omitempty"`
	Error           string `json:"error,omitempty"`
}

// GeneratorCapability reports whether candidate extraction is enabled.
type GeneratorCapability struct {
	Enabled bool `json:"enabled"`
}

// RetrievalCapability reports which retrieval versions can be selected. The
// default never changes: v2 requires an explicit retrieval_version=v2.
type RetrievalCapability struct {
	DefaultVersion string `json:"default_version"`
	V2Available    bool   `json:"v2_available"`
}

// WorkerCapability describes the live worker pool.
type WorkerCapability struct {
	Alive bool `json:"alive"`
	Count int  `json:"count"`
}

// Limits carries request limits the UI needs to enforce before uploading.
type Limits struct {
	UploadBytes int64 `json:"upload_bytes"`
}

// Capabilities is the single startup answer the UI uses to explain what this
// installation can do instead of letting a user queue work nothing can run.
type Capabilities struct {
	Version   string              `json:"version"`
	ASR       ASRCapability       `json:"asr"`
	Generator GeneratorCapability `json:"generator"`
	Retrieval RetrievalCapability `json:"retrieval"`
	Worker    WorkerCapability    `json:"worker"`
	Limits    Limits              `json:"limits"`
}

// MaxUploadBytes is the hard multipart upload bound shared by the importer and
// the capabilities response.
const MaxUploadBytes int64 = 2 << 30

// Capabilities derives the answer from the configured ASR provider and the
// fresh worker heartbeats.
func (s *Service) Capabilities(ctx context.Context) (Capabilities, error) {
	out := Capabilities{
		Version:   "beta",
		Retrieval: RetrievalCapability{DefaultVersion: "v1", V2Available: true},
		Limits:    Limits{UploadBytes: MaxUploadBytes},
	}
	provider := strings.TrimSpace(s.ASRProvider)
	out.ASR.Provider = provider
	out.ASR.Configured = provider != "" && provider != "off"

	workers, err := process.ReadHeartbeats(ctx, s.Store.DB(), time.Now(), 30*time.Second)
	if err != nil {
		return Capabilities{}, err
	}
	var freshest *process.Heartbeat
	for i, w := range workers {
		if !w.Alive {
			continue
		}
		out.Worker.Alive = true
		out.Worker.Count++
		if w.GeneratorEnabled {
			out.Generator.Enabled = true
		}
		if w.ASRAvailable {
			out.ASR.WorkerAvailable = true
		}
		if freshest == nil || w.LastSeenAt.After(freshest.LastSeenAt) {
			freshest = &workers[i]
		}
	}
	if freshest != nil {
		out.ASR.GPUName = freshest.GPUName
		out.ASR.VRAMTotalMB = freshest.VRAMTotalMB
		out.ASR.VRAMFreeMB = freshest.VRAMFreeMB
		out.ASR.Device = freshest.ASRDevice
		out.ASR.Dtype = freshest.ASRDtype
		out.ASR.Error = freshest.ASRError
	}
	asrReason := strings.TrimSpace(s.ASRInitError)
	if asrReason == "" && freshest != nil {
		asrReason = strings.TrimSpace(freshest.ASRError)
	}

	out.ASR.Available = out.ASR.Configured && out.Worker.Alive && out.ASR.WorkerAvailable && asrReason == ""
	switch {
	case !out.ASR.Configured:
		out.ASR.Reason = "asr_not_configured"
	case !out.Worker.Alive:
		out.ASR.Reason = "worker_offline"
	case strings.HasPrefix(asrReason, "gpu_unavailable"):
		out.ASR.Reason = "gpu_unavailable"
	case asrReason != "":
		out.ASR.Reason = "model_probe_failed"
	case !out.ASR.WorkerAvailable:
		out.ASR.Reason = "worker_asr_mismatch"
	}
	return out, nil
}
