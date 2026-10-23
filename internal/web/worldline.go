package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"recordingmcp/internal/app"
)

func registerWorldlineRoutes(mux *http.ServeMux, service *app.WorldlineService, scope func(*http.Request) string) {
	mux.HandleFunc("GET /api/worldline/threads", func(w http.ResponseWriter, r *http.Request) {
		profile := scope(r)
		if strings.TrimSpace(profile) == "" {
			writeForbidden(w, "profile_scope_required", "profile scope required")
			return
		}
		q, err := worldlineThreadQuery(r, 8, 100)
		if err != nil {
			writeBadRequest(w, err.Error())
			return
		}
		result, err := service.Threads(r.Context(), profile, q)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody{Error: errorDetail{Code: "worldline_threads_failed", Message: err.Error()}})
			return
		}
		writeJSON(w, http.StatusOK, result)
	})
	mux.HandleFunc("GET /api/worldline/threads/{id}", func(w http.ResponseWriter, r *http.Request) {
		profile := scope(r)
		if strings.TrimSpace(profile) == "" {
			writeForbidden(w, "profile_scope_required", "profile scope required")
			return
		}
		q, err := worldlineThreadQuery(r, 25, 500)
		if err != nil {
			writeBadRequest(w, err.Error())
			return
		}
		entriesOffset, err := optionalWorldlineInt(r.URL.Query().Get("entries_offset"), 0)
		if err != nil {
			writeBadRequest(w, "entries_offset must be a non-negative integer")
			return
		}
		unknownOffset, err := optionalWorldlineInt(r.URL.Query().Get("unknown_offset"), 0)
		if err != nil {
			writeBadRequest(w, "unknown_offset must be a non-negative integer")
			return
		}
		result, err := service.Thread(r.Context(), profile, r.PathValue("id"), q, entriesOffset, unknownOffset, q.Limit)
		if err != nil {
			writeJSON(w, http.StatusNotFound, errorBody{Error: errorDetail{Code: "worldline_thread_not_found", Message: err.Error()}})
			return
		}
		writeJSON(w, http.StatusOK, result)
	})
	mux.HandleFunc("GET /api/worldline", func(w http.ResponseWriter, r *http.Request) {
		profile := scope(r)
		if strings.TrimSpace(profile) == "" {
			writeForbidden(w, "profile_scope_required", "profile scope required")
			return
		}
		q := app.WorldlineQuery{Query: r.URL.Query().Get("q"), Status: r.URL.Query().Get("status")}
		var err error
		if q.From, err = optionalWorldlineTime(r.URL.Query().Get("from")); err != nil {
			writeBadRequest(w, "from must be RFC3339")
			return
		}
		if q.To, err = optionalWorldlineTime(r.URL.Query().Get("to")); err != nil {
			writeBadRequest(w, "to must be RFC3339")
			return
		}
		if q.Offset, err = optionalWorldlineInt(r.URL.Query().Get("offset"), 0); err != nil {
			writeBadRequest(w, "offset must be a non-negative integer")
			return
		}
		if q.Limit, err = optionalWorldlineInt(r.URL.Query().Get("limit"), 100); err != nil {
			writeBadRequest(w, "limit must be between 1 and 500")
			return
		}
		result, err := service.List(r.Context(), profile, q)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody{Error: errorDetail{Code: "worldline_query_failed", Message: err.Error()}})
			return
		}
		writeJSON(w, http.StatusOK, result)
	})
	mux.HandleFunc("GET /api/worldline/topics/{id}", func(w http.ResponseWriter, r *http.Request) {
		profile := scope(r)
		if strings.TrimSpace(profile) == "" {
			writeForbidden(w, "profile_scope_required", "profile scope required")
			return
		}
		offset, err := optionalWorldlineInt(r.URL.Query().Get("offset"), 0)
		if err != nil {
			writeBadRequest(w, "offset must be a non-negative integer")
			return
		}
		limit, err := optionalWorldlineInt(r.URL.Query().Get("limit"), 100)
		if err != nil {
			writeBadRequest(w, "limit must be between 1 and 500")
			return
		}
		filters := app.WorldlineQuery{Query: r.URL.Query().Get("q"), Status: r.URL.Query().Get("status")}
		if filters.From, err = optionalWorldlineTime(r.URL.Query().Get("from")); err != nil {
			writeBadRequest(w, "from must be RFC3339")
			return
		}
		if filters.To, err = optionalWorldlineTime(r.URL.Query().Get("to")); err != nil {
			writeBadRequest(w, "to must be RFC3339")
			return
		}
		if filters.Status == "" {
			filters.Status = "all"
		}
		if filters.Status != "all" && filters.Status != "unconfirmed" && filters.Status != "reviewed" && filters.Status != "conflict" {
			writeBadRequest(w, "status must be all, unconfirmed, reviewed, or conflict")
			return
		}
		if filters.From != nil && filters.To != nil && filters.From.After(*filters.To) {
			writeBadRequest(w, "from must not follow to")
			return
		}
		result, err := service.Topic(r.Context(), profile, r.PathValue("id"), offset, limit, filters)
		if err != nil {
			writeJSON(w, http.StatusNotFound, errorBody{Error: errorDetail{Code: "worldline_topic_not_found", Message: err.Error()}})
			return
		}
		writeJSON(w, http.StatusOK, result)
	})
	mux.HandleFunc("POST /api/worldline/feedback", func(w http.ResponseWriter, r *http.Request) {
		profile := scope(r)
		if strings.TrimSpace(profile) == "" {
			writeForbidden(w, "profile_scope_required", "profile scope required")
			return
		}
		if !requireJSON(w, r) {
			return
		}
		var req app.WorldlineFeedback
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeBadRequest(w, "invalid worldline feedback")
			return
		}
		version, err := service.Feedback(r.Context(), profile, req)
		if err != nil {
			status, code := http.StatusBadRequest, "invalid_worldline_feedback"
			if strings.Contains(err.Error(), "version conflict") || strings.Contains(err.Error(), "conflicting event ID") {
				status, code = http.StatusConflict, "worldline_version_conflict"
			}
			writeJSON(w, status, errorBody{Error: errorDetail{Code: code, Message: err.Error()}})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"version": version})
	})
	mux.HandleFunc("POST /api/worldline/threads/{id}/rename", func(w http.ResponseWriter, r *http.Request) {
		profile := scope(r)
		if strings.TrimSpace(profile) == "" {
			writeForbidden(w, "profile_scope_required", "profile scope required")
			return
		}
		if !requireJSON(w, r) {
			return
		}
		var req struct {
			Title           string `json:"title"`
			ExpectedVersion string `json:"expected_version"`
			RequestID       string `json:"request_id"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeBadRequest(w, "invalid worldline rename request")
			return
		}
		version, err := service.RenameThread(r.Context(), profile, r.PathValue("id"), req.Title, req.ExpectedVersion, req.RequestID)
		if err != nil {
			status, code := http.StatusBadRequest, "worldline_rename_failed"
			if strings.Contains(err.Error(), "version conflict") || strings.Contains(err.Error(), "conflicting event ID") {
				status, code = http.StatusConflict, "worldline_version_conflict"
			}
			writeJSON(w, status, errorBody{Error: errorDetail{Code: code, Message: err.Error()}})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"version": version})
	})
	mux.HandleFunc("POST /api/worldline/threads/group", func(w http.ResponseWriter, r *http.Request) {
		profile := scope(r)
		if strings.TrimSpace(profile) == "" {
			writeForbidden(w, "profile_scope_required", "profile scope required")
			return
		}
		if !requireJSON(w, r) {
			return
		}
		var req struct {
			ThreadIDs       []string `json:"thread_ids"`
			Title           string   `json:"title"`
			Action          string   `json:"action"`
			GroupID         string   `json:"group_id"`
			ExpectedVersion string   `json:"expected_version"`
			RequestID       string   `json:"request_id"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeBadRequest(w, "invalid worldline grouping request")
			return
		}
		version, err := service.GroupThreads(r.Context(), profile, req.ThreadIDs, req.Title, req.Action, req.GroupID, req.ExpectedVersion, req.RequestID)
		if err != nil {
			status, code := http.StatusBadRequest, "worldline_group_failed"
			if strings.Contains(err.Error(), "version conflict") || strings.Contains(err.Error(), "conflicting event ID") {
				status, code = http.StatusConflict, "worldline_version_conflict"
			}
			writeJSON(w, status, errorBody{Error: errorDetail{Code: code, Message: err.Error()}})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"version": version})
	})
	mux.HandleFunc("POST /api/worldline/semantic-review", func(w http.ResponseWriter, r *http.Request) {
		profile := scope(r)
		if strings.TrimSpace(profile) == "" {
			writeForbidden(w, "profile_scope_required", "profile scope required")
			return
		}
		if !requireJSON(w, r) {
			return
		}
		var req app.WorldlineSemanticReview
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeBadRequest(w, "invalid semantic review")
			return
		}
		version, err := service.ReviewSemanticRelation(r.Context(), profile, req)
		if err != nil {
			status, code := http.StatusBadRequest, "worldline_semantic_review_failed"
			if strings.Contains(err.Error(), "version conflict") {
				status, code = http.StatusConflict, "worldline_version_conflict"
			}
			writeJSON(w, status, errorBody{Error: errorDetail{Code: code, Message: err.Error()}})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"version": version})
	})
	mux.HandleFunc("POST /api/worldline/generate", func(w http.ResponseWriter, r *http.Request) {
		profile := scope(r)
		if strings.TrimSpace(profile) == "" {
			writeForbidden(w, "profile_scope_required", "profile scope required")
			return
		}
		if !requireJSON(w, r) {
			return
		}
		var req struct {
			RequestID string `json:"request_id"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeBadRequest(w, "invalid worldline generation request")
			return
		}
		result, err := service.Generate(r.Context(), profile, req.RequestID)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody{Error: errorDetail{Code: "worldline_generation_failed", Message: err.Error()}})
			return
		}
		writeJSON(w, http.StatusOK, result)
	})
	mux.HandleFunc("POST /api/worldline/analysis", func(w http.ResponseWriter, r *http.Request) {
		profile := scope(r)
		if strings.TrimSpace(profile) == "" {
			writeForbidden(w, "profile_scope_required", "profile scope required")
			return
		}
		if !requireJSON(w, r) {
			return
		}
		var req struct {
			RequestID string `json:"request_id"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeBadRequest(w, "invalid worldline analysis request")
			return
		}
		job, err := service.EnqueueAnalysis(r.Context(), profile, req.RequestID)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody{Error: errorDetail{Code: "worldline_analysis_enqueue_failed", Message: err.Error()}})
			return
		}
		writeJSON(w, http.StatusAccepted, job)
	})
	mux.HandleFunc("GET /api/worldline/analysis/{id}", func(w http.ResponseWriter, r *http.Request) {
		profile := scope(r)
		if strings.TrimSpace(profile) == "" {
			writeForbidden(w, "profile_scope_required", "profile scope required")
			return
		}
		job, err := service.AnalysisStatus(r.Context(), profile, r.PathValue("id"))
		if err != nil {
			writeJSON(w, http.StatusNotFound, errorBody{Error: errorDetail{Code: "worldline_job_not_found", Message: err.Error()}})
			return
		}
		writeJSON(w, http.StatusOK, job)
	})
}

func worldlineThreadQuery(r *http.Request, defaultLimit, maxLimit int) (app.WorldlineQuery, error) {
	query := app.WorldlineQuery{Query: r.URL.Query().Get("q"), Status: r.URL.Query().Get("status")}
	var err error
	if query.From, err = optionalWorldlineTime(r.URL.Query().Get("from")); err != nil {
		return query, errors.New("from must be RFC3339")
	}
	if query.To, err = optionalWorldlineTime(r.URL.Query().Get("to")); err != nil {
		return query, errors.New("to must be RFC3339")
	}
	if query.Offset, err = optionalWorldlineInt(r.URL.Query().Get("offset"), 0); err != nil {
		return query, errors.New("offset must be a non-negative integer")
	}
	if query.Limit, err = optionalWorldlineInt(r.URL.Query().Get("limit"), defaultLimit); err != nil || query.Limit < 1 || query.Limit > maxLimit {
		return query, fmt.Errorf("limit must be between 1 and %d", maxLimit)
	}
	return query, nil
}

func optionalWorldlineTime(raw string) (*time.Time, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	value, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil, err
	}
	return &value, nil
}

func optionalWorldlineInt(raw string, defaultValue int) (int, error) {
	if strings.TrimSpace(raw) == "" {
		return defaultValue, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		return 0, errors.New("invalid integer")
	}
	return value, nil
}
