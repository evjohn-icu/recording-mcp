package web

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"recordingmcp/internal/app"
	"recordingmcp/internal/plane"
)

// RegisterTimeRoutes registers only the temporal API. scope must be resolved by
// the caller from authenticated profile context, never copied from user input.
func RegisterTimeRoutes(mux *http.ServeMux, service *app.TimeService, scope func(*http.Request) string) {
	mux.HandleFunc("GET /api/time/history", func(w http.ResponseWriter, r *http.Request) {
		profile := scope(r)
		if strings.TrimSpace(profile) == "" {
			writeForbidden(w, "profile_scope_required", "profile scope required")
			return
		}
		rows, err := service.History(r.Context(), profile)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody{Error: errorDetail{Code: "time_history_failed", Message: err.Error()}})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"events": rows})
	})
	mux.HandleFunc("GET /api/time/state", func(w http.ResponseWriter, r *http.Request) {
		profile := scope(r)
		if strings.TrimSpace(profile) == "" {
			writeForbidden(w, "profile_scope_required", "profile scope required")
			return
		}
		validAt, err := time.Parse(time.RFC3339, r.URL.Query().Get("valid_at"))
		if err != nil {
			writeBadRequest(w, "valid_at must be RFC3339")
			return
		}
		knownAt, err := time.Parse(time.RFC3339, r.URL.Query().Get("known_at"))
		if err != nil {
			writeBadRequest(w, "known_at must be RFC3339")
			return
		}
		states, err := service.State(r.Context(), profile, plane.TemporalQuery{ValidAt: validAt, KnownAt: knownAt})
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody{Error: errorDetail{Code: "time_state_failed", Message: err.Error()}})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"states": states, "scope": profile})
	})
	mux.HandleFunc("POST /api/time/corrections", func(w http.ResponseWriter, r *http.Request) {
		profile := scope(r)
		if strings.TrimSpace(profile) == "" {
			writeForbidden(w, "profile_scope_required", "profile scope required")
			return
		}
		var req app.CorrectionRequest
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeBadRequest(w, "invalid correction")
			return
		}
		if req.Scope != "" && req.Scope != profile {
			writeForbidden(w, "profile_scope_mismatch", "scope mismatch")
			return
		}
		req.Scope = profile
		n, err := service.Correct(r.Context(), req)
		if err != nil {
			writeJSON(w, http.StatusConflict, errorBody{Error: errorDetail{Code: "time_correction_conflict", Message: err.Error()}})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]int{"inserted": n})
	})
	mux.HandleFunc("POST /api/time/facts", func(w http.ResponseWriter, r *http.Request) {
		profile := scope(r)
		if strings.TrimSpace(profile) == "" {
			writeForbidden(w, "profile_scope_required", "profile scope required")
			return
		}
		if !requireJSON(w, r) {
			return
		}
		var req app.FactRequest
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeBadRequest(w, "invalid fact report: "+err.Error())
			return
		}
		n, err := service.ObserveFact(r.Context(), profile, req)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody{Error: errorDetail{Code: "invalid_fact_report", Message: err.Error()}})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]int{"inserted": n})
	})
}
