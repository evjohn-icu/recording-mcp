package main

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"recordingmcp/internal/migrations"
	"recordingmcp/internal/process"
)

func TestWorkerHealthHandler(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "health.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := migrations.Apply(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := process.WriteHeartbeat(context.Background(), db, process.Heartbeat{WorkerID: "w1", PID: 1, Stages: []string{"asr"}}, now); err != nil {
		t.Fatal(err)
	}
	handler := workerHealthHandler(db, "w1")
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("fresh heartbeat: %d %s", rec.Code, rec.Body.String())
	}

	// A heartbeat older than the 90s window must report degraded.
	old := time.Now().Add(-2 * time.Minute)
	if err := process.WriteHeartbeat(context.Background(), db, process.Heartbeat{WorkerID: "w1", PID: 1}, old); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("stale heartbeat: %d %s", rec.Code, rec.Body.String())
	}
}
