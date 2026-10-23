package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"recordingmcp/internal/process"
)

func (s *Server) handleProcessing(w http.ResponseWriter, r *http.Request) {
	snap, err := s.svc.ProcessingSnapshot(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err, nil)
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

func (s *Server) handleProcessingRetry(w http.ResponseWriter, r *http.Request) {
	if !requireJSON(w, r) {
		return
	}
	var body struct {
		JobID           string `json:"job_id"`
		ExpectedAttempt int    `json:"expected_attempt"`
		RequestID       string `json:"request_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody)).Decode(&body); err != nil {
		writeBadRequest(w, "invalid retry request")
		return
	}
	body.JobID = strings.TrimSpace(body.JobID)
	body.RequestID = strings.TrimSpace(body.RequestID)
	if body.JobID == "" || body.RequestID == "" || body.ExpectedAttempt < 0 {
		writeBadRequest(w, "job_id, request_id and a non-negative expected_attempt are required")
		return
	}
	job, err := s.svc.RetryProcessing(r.Context(), r.PathValue("id"), body.JobID, body.ExpectedAttempt, body.RequestID)
	if err != nil {
		if errors.Is(err, process.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, errorBody{Error: errorDetail{Code: "not_found", Message: "processing job not found"}})
			return
		}
		if errors.Is(err, process.ErrRetryConflict) || errors.Is(err, process.ErrStaleSource) {
			writeJSON(w, http.StatusConflict, errorBody{Error: errorDetail{Code: "processing_conflict", Message: err.Error()}})
			return
		}
		s.writeError(w, r, err, nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": job})
}
