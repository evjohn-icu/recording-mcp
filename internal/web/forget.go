package web

import (
	"net/http"
	"recordingmcp/internal/memory"
)

// handleForgetPreview and handleForgetCommit are transport handlers for
// coordinator/router integration; this package intentionally does not register
// routes here, so deployments must opt in explicitly.
func (s *Server) handleForgetPreview(w http.ResponseWriter, r *http.Request) {
	if !requireJSON(w, r) {
		return
	}
	var req memory.ForgetRequest
	if err := decodeJSON(r, &req); err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	p, err := s.svc.PreviewForget(r.Context(), req)
	if err != nil {
		s.writeError(w, r, err, nil)
		return
	}
	writeJSON(w, http.StatusOK, p)
}
func (s *Server) handleForgetCommit(w http.ResponseWriter, r *http.Request) {
	if !requireJSON(w, r) {
		return
	}
	var req memory.ForgetRequest
	if err := decodeJSON(r, &req); err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	result, err := s.svc.CommitForget(r.Context(), req)
	if err != nil {
		s.writeError(w, r, err, nil)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) registerForgetRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/conversations/{id}/notes/{rowID}/suppression-preview", func(w http.ResponseWriter, r *http.Request) {
		item, err := s.svc.GetWebItem(r.Context(), r.PathValue("rowID"))
		if err != nil {
			s.writeError(w, r, err, nil)
			return
		}
		if item.Item.ConversationID != r.PathValue("id") {
			writeBadRequest(w, "note does not belong to conversation")
			return
		}
		n, notice, err := s.svc.ManagedRetentionNotice()
		if err != nil {
			s.writeError(w, r, err, nil)
			return
		}
		writeJSON(w, 200, map[string]any{"managed_backup_count": n, "retention_notice": notice, "source_policy": "same-source-same-kind", "source_retained": true})
	})
	mux.HandleFunc("POST /api/conversations/{id}/notes/{rowID}/suppress", func(w http.ResponseWriter, r *http.Request) {
		if !requireJSON(w, r) {
			return
		}
		var req struct {
			ExpectedVersion string `json:"expected_version"`
		}
		if err := decodeJSON(r, &req); err != nil {
			writeBadRequest(w, err.Error())
			return
		}
		result, err := s.svc.SuppressNote(r.Context(), r.PathValue("id"), r.PathValue("rowID"), req.ExpectedVersion)
		if err != nil {
			s.writeError(w, r, err, nil)
			return
		}
		writeJSON(w, 200, result)
	})
	mux.HandleFunc("GET /api/suppressions/{id}", func(w http.ResponseWriter, r *http.Request) {
		status, err := s.svc.SuppressionCleanupStatus(r.Context(), r.PathValue("id"))
		if err != nil {
			s.writeError(w, r, err, nil)
			return
		}
		writeJSON(w, 200, status)
	})
	mux.HandleFunc("GET /api/suppressions", func(w http.ResponseWriter, r *http.Request) {
		pending, err := s.svc.PendingSuppressionCleanups(r.Context())
		if err != nil {
			s.writeError(w, r, err, nil)
			return
		}
		writeJSON(w, 200, map[string]any{"operations": pending})
	})
	mux.HandleFunc("POST /api/suppressions/{id}/retry", func(w http.ResponseWriter, r *http.Request) {
		status, err := s.svc.RetrySuppressionCleanup(r.Context(), r.PathValue("id"))
		if err != nil {
			s.writeError(w, r, err, nil)
			return
		}
		writeJSON(w, 200, status)
	})
	mux.HandleFunc("POST /api/forget/preview", s.handleForgetPreview)
	mux.HandleFunc("POST /api/forget", s.handleForgetCommit)
	mux.HandleFunc("GET /api/forget", func(w http.ResponseWriter, r *http.Request) {
		pending, err := s.svc.PendingForgets(r.Context())
		if err != nil {
			s.writeError(w, r, err, nil)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"operations": pending})
	})
	mux.HandleFunc("GET /api/forget/{id}", func(w http.ResponseWriter, r *http.Request) {
		status, err := s.svc.Store.ForgetStatus(r.Context(), r.PathValue("id"))
		if err != nil {
			s.writeError(w, r, err, nil)
			return
		}
		writeJSON(w, 200, status)
	})
	mux.HandleFunc("POST /api/forget/{id}/retry", func(w http.ResponseWriter, r *http.Request) {
		status, err := s.svc.RetryForget(r.Context(), r.PathValue("id"))
		if err != nil {
			s.writeError(w, r, err, nil)
			return
		}
		writeJSON(w, 200, status)
	})
}
