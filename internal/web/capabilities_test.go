package web

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"recordingmcp/internal/app"
	"recordingmcp/internal/process"
)

func TestCapabilitiesRequiresSessionAndReportsWorkerState(t *testing.T) {
	env := newTestEnv(t, "")
	resp := env.raw(t, http.MethodGet, "/api/capabilities", "", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated capabilities: %d", resp.StatusCode)
	}

	// Without a configured ASR provider the answer explains why uploads would
	// be rejected instead of pretending they are available.
	status, data := env.do(t, http.MethodGet, "/api/capabilities", "", nil)
	if status != http.StatusOK {
		t.Fatalf("capabilities: %d %s", status, data)
	}
	var caps app.Capabilities
	if err := json.Unmarshal(data, &caps); err != nil {
		t.Fatal(err)
	}
	if caps.ASR.Configured || caps.ASR.Available || caps.ASR.Reason != "asr_not_configured" {
		t.Fatalf("unconfigured ASR: %+v", caps.ASR)
	}
	if caps.Retrieval.DefaultVersion != "v1" || caps.Limits.UploadBytes != app.MaxUploadBytes {
		t.Fatalf("defaults: %+v", caps)
	}

	// With a matching live worker the capability flips to available.
	env.server.svc.ASRProvider = "fixture"
	if err := process.WriteHeartbeat(context.Background(), env.store.DB(), process.Heartbeat{
		WorkerID: "w-asr", PID: 1, Stages: []string{"asr"}, ASRProvider: "fixture", ASRAvailable: true, EmbedProvider: "hash",
	}, time.Now()); err != nil {
		t.Fatal(err)
	}
	status, data = env.do(t, http.MethodGet, "/api/capabilities", "", nil)
	if status != http.StatusOK {
		t.Fatalf("capabilities: %d %s", status, data)
	}
	caps = app.Capabilities{}
	if err := json.Unmarshal(data, &caps); err != nil {
		t.Fatal(err)
	}
	if !caps.ASR.Available || !caps.Worker.Alive || caps.Worker.Count != 1 || caps.ASR.Reason != "" {
		t.Fatalf("available ASR: %+v worker=%+v", caps.ASR, caps.Worker)
	}
}
