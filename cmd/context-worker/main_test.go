package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"recordingmcp/internal/memory"
	"recordingmcp/internal/process"
	"testing"
)

func TestValidateRealMode(t *testing.T) {
	cases := []struct {
		name           string
		executor       string
		generator      string
		allowSynthetic bool
		wantErr        string
	}{
		{
			name:      "pipeline with synthetic generator is rejected without the flag",
			executor:  "pipeline",
			generator: "synthetic",
			wantErr:   "--generator synthetic produces placeholder candidates; pass --allow-synthetic for demos and tests only",
		},
		{
			name:           "pipeline with synthetic generator is allowed with the flag",
			executor:       "pipeline",
			generator:      "synthetic",
			allowSynthetic: true,
		},
		{
			name:      "pipeline with off generator needs no flag",
			executor:  "pipeline",
			generator: "off",
		},
		{
			name:      "pipeline with openai generator needs no flag",
			executor:  "pipeline",
			generator: "openai",
		},
		{
			name:      "synthetic executor stays demo-only but permitted",
			executor:  "synthetic",
			generator: "synthetic",
		},
		{
			name:      "other executor and generator names do not trip the guard",
			executor:  "pipeline",
			generator: "unknown-name",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateRealMode(tc.executor, tc.generator, tc.allowSynthetic)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validateRealMode(%q, %q, %v) = %v, want nil", tc.executor, tc.generator, tc.allowSynthetic, err)
				}
				return
			}
			if err == nil || err.Error() != tc.wantErr {
				t.Fatalf("validateRealMode(%q, %q, %v) = %v, want %q", tc.executor, tc.generator, tc.allowSynthetic, err, tc.wantErr)
			}
		})
	}
}

type recordingExecutor struct{ got string }

func (e *recordingExecutor) Execute(_ context.Context, j process.Job) (string, error) {
	e.got = j.Stage
	return j.Stage, nil
}

func TestWorldlineStageRouterUsesScopedWorldlineExecutor(t *testing.T) {
	next := &recordingExecutor{}
	world := &recordingExecutor{}
	r := routeWorldlineJobs(next, world)
	if _, err := r.Execute(context.Background(), process.Job{Stage: "worldline"}); err != nil {
		t.Fatal(err)
	}
	if world.got != "worldline" || next.got != "" {
		t.Fatalf("worldline route=%q base=%q", world.got, next.got)
	}
	if _, err := r.Execute(context.Background(), process.Job{Stage: "embed"}); err != nil {
		t.Fatal(err)
	}
	if next.got != "embed" {
		t.Fatalf("base executor did not receive embed stage: %q", next.got)
	}
}

func TestLegacyWorkerDefaultDoesNotEnableWorldlineWithoutPlane(t *testing.T) {
	if containsStage(workerStages("extract,publish,curate,embed", false, false), "worldline") {
		t.Fatal("legacy worker default unexpectedly requires an event-plane path")
	}
	if !containsStage(workerStages("extract,publish,curate,embed", true, false), "worldline") {
		t.Fatal("profile worker default omitted worldline")
	}
	if containsStage(workerStages("extract,publish,curate,embed", true, true), "worldline") {
		t.Fatal("explicit stage selection was unexpectedly changed")
	}
}

func TestWorldlineExecutorUsesConfiguredScopeAndPlane(t *testing.T) {
	store, err := memory.Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	events := filepath.Join(t.TempDir(), "events.db")
	executor := &WorldlineJobExecutor{Store: store, Scope: "local", EventsPath: events}
	payload, _ := json.Marshal(map[string]string{"scope": "other-profile", "request_id": "worker-test-2"})
	job := process.Job{Stage: "worldline", PayloadJSON: string(payload)}
	if _, err := executor.Execute(context.Background(), job); err == nil {
		t.Fatal("payload-selected profile was accepted")
	}
}
