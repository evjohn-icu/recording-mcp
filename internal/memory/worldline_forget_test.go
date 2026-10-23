package memory

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"recordingmcp/internal/process"
)

func TestForgetCancelsCrossConversationWorldlineSnapshot(t *testing.T) {
	ctx := context.Background()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tr, annotations := synthetic()
	if _, err = s.Import(ctx, tr, annotations, "synthetic.wav", strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	turn := turnKey(annotations.RecordingID, "seg01-turn0002")
	payload, _ := json.Marshal(map[string]any{"scope": "synthetic", "request_id": "analysis-1", "references": []map[string]string{{"turn_id": turn, "revision": "rev-1"}}})
	jobID, _, err := process.Enqueue(ctx, s.DB(), process.Spec{Stage: "worldline", InputSnapshotID: "whole-profile-snapshot", IdempotencyKey: "worldline-forget-test", PayloadJSON: string(payload)}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	req := ForgetRequest{Scope: "turn", TurnID: turn}
	preview, err := s.PreviewForget(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	req.ExpectedState = preview.ExpectedState
	if _, err = s.CommitForget(ctx, req); err != nil {
		t.Fatal(err)
	}
	job, err := process.Get(ctx, s.DB(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != "cancelled" || job.PayloadJSON != "{}" {
		t.Fatalf("worldline job retained forgotten source: status=%s payload=%s", job.Status, job.PayloadJSON)
	}
}
