package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"recordingmcp/internal/process"
)

func TestProcessingStatusAndExplicitRetry(t *testing.T) {
	e := newTestEnv(t, "")
	jobID, _, err := process.Enqueue(context.Background(), e.store.DB(), process.Spec{Stage: "embed", InputSnapshotID: e.convID, IdempotencyKey: "retry-web", PayloadJSON: `{"conversation_id":"` + e.convID + `"}`, MaxAttempts: 3}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.DB().Exec(`UPDATE processing_jobs SET status='failed',attempt_count=3,max_attempts=3 WHERE id=?`, jobID); err != nil {
		t.Fatal(err)
	}
	status, body := e.do(t, http.MethodGet, fmt.Sprintf("/api/conversations/%s/processing", e.convID), "", nil)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	var got struct {
		Jobs []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"jobs"`
		Workers []any `json:"workers"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, j := range got.Jobs {
		if j.ID == jobID && j.Status == "failed" {
			found = true
		}
	}
	if !found {
		t.Fatalf("extract job missing from conversation processing response: %s", body)
	}
	status, body = e.do(t, http.MethodPost, fmt.Sprintf("/api/conversations/%s/processing/retry", e.convID), fmt.Sprintf(`{"job_id":%q,"expected_attempt":3,"request_id":"request-1"}`, jobID), nil)
	if status != http.StatusOK {
		t.Fatalf("retry status=%d body=%s", status, body)
	}
	var job struct {
		Status      string `json:"status"`
		MaxAttempts int    `json:"max_attempts"`
	}
	if err := json.Unmarshal(body, &struct {
		Job *struct {
			Status      string `json:"status"`
			MaxAttempts int    `json:"max_attempts"`
		} `json:"job"`
	}{Job: &job}); err != nil {
		t.Fatal(err)
	}
	if job.Status != "queued" || job.MaxAttempts < 4 {
		t.Fatalf("retry did not requeue exhausted job: %+v", job)
	}
}

func TestProcessingSnapshotUsesEmptyArraysWhenIdle(t *testing.T) {
	e := newTestEnv(t, "")
	status, body := e.do(t, http.MethodGet, fmt.Sprintf("/api/conversations/%s/processing", e.convID), "", nil)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	var got struct {
		Jobs    []json.RawMessage `json:"jobs"`
		Workers []json.RawMessage `json:"workers"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Jobs == nil || got.Workers == nil {
		t.Fatalf("idle arrays must be JSON arrays: %s", body)
	}
}

func TestProcessingRetryInternalFailureUsesGenericServerError(t *testing.T) {
	e := newTestEnv(t, "")
	if err := e.store.DB().Close(); err != nil {
		t.Fatal(err)
	}
	status, body := e.do(t, http.MethodPost,
		fmt.Sprintf("/api/conversations/%s/processing/retry", e.convID),
		`{"job_id":"job-id","expected_attempt":1,"request_id":"retry-request"}`, nil)
	if status != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s, want 500", status, body)
	}
	if !strings.Contains(string(body), `"message":"internal error"`) || strings.Contains(string(body), "database is closed") {
		t.Fatalf("internal retry error was exposed to client: %s", body)
	}
}
