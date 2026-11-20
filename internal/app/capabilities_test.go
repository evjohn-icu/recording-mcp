package app

import (
	"context"
	"testing"
	"time"

	"recordingmcp/internal/contextbuild"
	"recordingmcp/internal/memory"
	"recordingmcp/internal/process"
	"recordingmcp/internal/semantic"
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

// capabilitiesSemanticStub pins Describe() so V3Available identity matching can
// be tested without a real embedding backend.
type capabilitiesSemanticStub struct {
	describe string
}

func (s *capabilitiesSemanticStub) Search(_ context.Context, _ string, _ semantic.SearchOptions) ([]semantic.SemanticHit, error) {
	return nil, nil
}

func (s *capabilitiesSemanticStub) Ready(context.Context) (bool, error) { return true, nil }

func (s *capabilitiesSemanticStub) Describe() string { return s.describe }

// capabilitiesAssessorStub satisfies the v3 gate's non-nil assessor check.
type capabilitiesAssessorStub struct{}

func (capabilitiesAssessorStub) AssessCandidates(_ context.Context, _ string, _ []contextbuild.AdmissionCandidate) ([]contextbuild.AdmissionDecision, error) {
	return nil, nil
}

func v3CapabilityPolicy(providerID, modelID string) *contextbuild.AdmissionPolicy {
	return &contextbuild.AdmissionPolicy{
		SchemaVersion:    contextbuild.AdmissionPolicySchema,
		RetrievalVersion: "v3",
		ProviderID:       providerID,
		ModelID:          modelID,
		ModelRevision:    "rev-1",
		AssessorModel:    "assessor-x",
		FixtureSHA256:    "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Threshold:        0.5,
		Phase:            "selected",
	}
}

// TestCapabilitiesV3AvailableRequiresMatchingAdmissionIdentity pins that v3 is
// only advertised when the admission policy's embedding identity matches the
// configured semantic retriever. A policy pinned to another provider/model must
// keep v3 unavailable so the UI never lets users queue v3 searches that
// admission would have to reject; a nil policy or nil semantic short-circuits
// to unavailable.
func TestCapabilitiesV3AvailableRequiresMatchingAdmissionIdentity(t *testing.T) {
	svc, _ := heartbeatEnv(t)
	svc.Semantic = &capabilitiesSemanticStub{describe: "openai-compat/text-embedding@768"}
	svc.CandidateAssessor = capabilitiesAssessorStub{}

	svc.AdmissionPolicy = v3CapabilityPolicy("openai-compat", "text-embedding")
	caps, err := svc.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !caps.Retrieval.V3Available {
		t.Fatalf("v3 = %+v, want available when the policy matches the configured embedding provider", caps.Retrieval)
	}

	svc.AdmissionPolicy = v3CapabilityPolicy("other-provider", "other-model")
	caps, err = svc.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if caps.Retrieval.V3Available {
		t.Fatalf("v3 = %+v, want unavailable when the policy does not match the configured embedding provider", caps.Retrieval)
	}
	if !caps.Retrieval.V2Available || caps.Retrieval.DefaultVersion != "v1" {
		t.Fatalf("v2/default = %+v, want v2 available and default v1", caps.Retrieval)
	}

	svc.AdmissionPolicy = nil
	caps, err = svc.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if caps.Retrieval.V3Available {
		t.Fatalf("v3 = %+v, want unavailable without an admission policy", caps.Retrieval)
	}
	svc.Semantic = nil
	caps, err = svc.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if caps.Retrieval.V3Available {
		t.Fatalf("v3 = %+v, want unavailable without a semantic backend", caps.Retrieval)
	}
}
