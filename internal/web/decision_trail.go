package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"recordingmcp/internal/app"
)

// registerDecisionRoutes wires the read model and the human review write.
func (s *Server) registerDecisionRoutes(mux *http.ServeMux, scope func(*http.Request) string) {
	mux.HandleFunc("GET /api/decisions", func(w http.ResponseWriter, r *http.Request) {
		if s.decisionTrail == nil {
			writeJSON(w, http.StatusServiceUnavailable, errorBody{Error: errorDetail{Code: "decisions_unavailable", Message: "decision trail is not configured"}})
			return
		}
		profile := scope(r)
		if strings.TrimSpace(profile) == "" {
			writeForbidden(w, "profile_scope_required", "profile scope required")
			return
		}
		q, err := decisionQuery(r)
		if err != nil {
			writeBadRequest(w, err.Error())
			return
		}
		trail, err := s.decisionTrail.Build(r.Context(), profile, q)
		if err != nil {
			s.writeDecisionError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, trail)
	})
	mux.HandleFunc("POST /api/decisions/review", func(w http.ResponseWriter, r *http.Request) {
		if s.decisionTrail == nil {
			writeJSON(w, http.StatusServiceUnavailable, errorBody{Error: errorDetail{Code: "decisions_unavailable", Message: "decision trail is not configured"}})
			return
		}
		profile := scope(r)
		if strings.TrimSpace(profile) == "" {
			writeForbidden(w, "profile_scope_required", "profile scope required")
			return
		}
		if !requireJSON(w, r) {
			return
		}
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		var req app.DecisionReview
		if err := decoder.Decode(&req); err != nil {
			writeBadRequest(w, "invalid decision review body: "+err.Error())
			return
		}
		trail, err := s.decisionTrail.Review(r.Context(), profile, req)
		if err != nil {
			s.writeDecisionError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, trail)
	})
}

func decisionQuery(r *http.Request) (app.DecisionQuery, error) {
	values := r.URL.Query()
	q := app.DecisionQuery{TopicID: strings.TrimSpace(values.Get("topic_id")), IncludeHistory: true}
	if raw := strings.TrimSpace(values.Get("known_at")); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return q, errors.New("known_at must be an RFC3339 timestamp")
		}
		q.KnownAt = &parsed
	}
	if raw := strings.TrimSpace(values.Get("include_history")); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			return q, errors.New("include_history must be a boolean")
		}
		q.IncludeHistory = parsed
	}
	for key, target := range map[string]*int{"limit": &q.Limit, "max_bytes": &q.MaxBytes} {
		raw := strings.TrimSpace(values.Get(key))
		if raw == "" {
			continue
		}
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 {
			return q, errors.New(key + " must be a non-negative integer")
		}
		*target = parsed
	}
	return q, nil
}

// writeDecisionError maps decision failures onto the shared envelope.
func (s *Server) writeDecisionError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, app.ErrDecisionVersionConflict):
		writeJSON(w, http.StatusConflict, errorBody{Error: errorDetail{
			Code: "decision_version_conflict", Message: "轨迹已更新，请刷新后重试",
			Details: map[string]any{"action": "刷新决策面板后重新提交"},
		}})
	case errors.Is(err, app.ErrDecisionRequestConflict):
		writeJSON(w, http.StatusConflict, errorBody{Error: errorDetail{
			Code: "decision_request_conflict", Message: "同一请求编号已用于不同内容",
			Details: map[string]any{"action": "使用新的 request_id 重试"},
		}})
	case errors.Is(err, app.ErrDecisionNotFound):
		writeJSON(w, http.StatusNotFound, errorBody{Error: errorDetail{
			Code: "decision_not_found", Message: "找不到该候选决定",
			Details: map[string]any{"action": "刷新决策面板"},
		}})
	case errors.Is(err, app.ErrDecisionValidation):
		writeJSON(w, http.StatusBadRequest, errorBody{Error: errorDetail{
			Code: "validation_failed", Message: err.Error(),
			Details: map[string]any{"action": "确认候选、生效时间和来源证据后重试"},
		}})
	default:
		s.logger.Printf("web: %s %s: %v", r.Method, r.URL.Path, err)
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: errorDetail{Code: app.CodeInternal, Message: "internal error"}})
	}
}
