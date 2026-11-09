package app

import (
	"context"
	"testing"
	"time"

	"recordingmcp/internal/memory"
	"recordingmcp/internal/process"
)

func heartbeatEnv(t *testing.T) (*Service, *memory.Store) {
	t.Helper()
	fx := newFixture(t)
	return fx.svc, fx.store
}

// TestCapabilitiesReportsGPUUnavailable pins that a live worker with a failed
// CUDA probe degrades the ASR capability with a reason instead of claiming the
// product cannot start.
func TestCapabilitiesReportsGPUUnavailable(t *testing.T) {
	svc, store := heartbeatEnv(t)
	svc.ASRProvider = "qwen"
	now := time.Now()
	if err := process.WriteHeartbeat(context.Background(), store.DB(), process.Heartbeat{
		WorkerID: "w-gpu", PID: 7, Stages: []string{"ingest", "asr"}, ASRProvider: "qwen",
		ASRAvailable: false, GPUName: "NVIDIA GeForce RTX 2070", VRAMTotalMB: 8192, VRAMFreeMB: 7000,
		ASRDevice: "cuda:0", ASRDtype: "float16", ASRError: "gpu_unavailable: torch.cuda.is_available() is false",
	}, now); err != nil {
		t.Fatal(err)
	}
	caps, err := svc.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if caps.ASR.Available {
		t.Fatal("ASR must not be available when the CUDA probe failed")
	}
	if caps.ASR.Reason != "gpu_unavailable" {
		t.Fatalf("reason = %q, want gpu_unavailable", caps.ASR.Reason)
	}
	if caps.ASR.GPUName != "NVIDIA GeForce RTX 2070" || caps.ASR.VRAMTotalMB != 8192 || caps.ASR.VRAMFreeMB != 7000 {
		t.Fatalf("gpu diagnostics = %+v", caps.ASR)
	}
	if caps.ASR.Device != "cuda:0" || caps.ASR.Dtype != "float16" {
		t.Fatalf("device/dtype = %q/%q", caps.ASR.Device, caps.ASR.Dtype)
	}
	if caps.ASR.Error == "" {
		t.Fatal("capability must expose the probe error")
	}
	if !caps.Worker.Alive {
		t.Fatal("worker must still count as alive; only ASR is degraded")
	}
}

func TestCapabilitiesReportsInitFailureWithoutWorker(t *testing.T) {
	svc, _ := heartbeatEnv(t)
	svc.ASRProvider = "ark"
	svc.ASRInitError = "model_probe_failed: local ASR runner not found"
	caps, err := svc.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if caps.ASR.Available || caps.ASR.Reason != "worker_offline" {
		t.Fatalf("capability = %+v, want worker_offline while no worker is alive", caps.ASR)
	}
}

func TestCapabilitiesHealthyLocalWorker(t *testing.T) {
	svc, store := heartbeatEnv(t)
	svc.ASRProvider = "qwen"
	if err := process.WriteHeartbeat(context.Background(), store.DB(), process.Heartbeat{
		WorkerID: "w-ok", PID: 8, Stages: []string{"asr"}, ASRProvider: "qwen", ASRAvailable: true,
		GPUName: "NVIDIA GeForce RTX 2070", VRAMTotalMB: 8192, VRAMFreeMB: 7000,
		ASRDevice: "cuda:0", ASRDtype: "float16",
	}, time.Now()); err != nil {
		t.Fatal(err)
	}
	caps, err := svc.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !caps.ASR.Available || caps.ASR.Reason != "" {
		t.Fatalf("capability = %+v, want available without a reason", caps.ASR)
	}
}
