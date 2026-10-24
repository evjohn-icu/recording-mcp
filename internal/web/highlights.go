package web

import (
	"encoding/json"
	"io"
	"net/http"
	"recordingmcp/internal/contextbrief"
	"recordingmcp/internal/memory"
	"strconv"
)

func (s *Server) registerHighlightRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/recordings/{id}/highlights", s.handleHighlightCreate)
	mux.HandleFunc("GET /api/recordings/{id}/highlights", s.handleHighlights)
	mux.HandleFunc("GET /api/highlights/{id}", s.handleHighlight)
	mux.HandleFunc("PATCH /api/highlights/{id}", s.handleHighlightUpdate)
	mux.HandleFunc("DELETE /api/highlights/{id}", s.handleHighlightDelete)
	mux.HandleFunc("GET /api/highlights/{id}/audio", s.handleHighlightAudio)
	mux.HandleFunc("POST /api/briefs/preview", s.handleBrief)
}
func decodeHighlightBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if !requireJSON(w, r) {
		return false
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		writeBadRequest(w, "invalid request body")
		return false
	}
	if d.Decode(new(any)) != io.EOF {
		writeBadRequest(w, "one JSON object required")
		return false
	}
	return true
}
func (s *Server) handleHighlightCreate(w http.ResponseWriter, r *http.Request) {
	var in memory.HighlightCreate
	if !decodeHighlightBody(w, r, &in) {
		return
	}
	if in.RecordingID != "" && in.RecordingID != r.PathValue("id") {
		writeBadRequest(w, "recording_id does not match route")
		return
	}
	in.RecordingID = r.PathValue("id")
	h, e := s.svc.CreateHighlight(r.Context(), in)
	if e != nil {
		s.writeError(w, r, e, nil)
		return
	}
	writeJSON(w, 200, h)
}
func (s *Server) handleHighlights(w http.ResponseWriter, r *http.Request) {
	h, e := s.svc.ListHighlights(r.Context(), r.PathValue("id"))
	if e != nil {
		s.writeError(w, r, e, nil)
		return
	}
	writeJSON(w, 200, h)
}
func (s *Server) handleHighlight(w http.ResponseWriter, r *http.Request) {
	h, e := s.svc.GetHighlight(r.Context(), r.PathValue("id"))
	if e != nil {
		s.writeError(w, r, e, nil)
		return
	}
	writeJSON(w, 200, h)
}
func (s *Server) handleHighlightUpdate(w http.ResponseWriter, r *http.Request) {
	var in memory.HighlightUpdate
	if !decodeHighlightBody(w, r, &in) {
		return
	}
	h, e := s.svc.UpdateHighlight(r.Context(), r.PathValue("id"), in)
	if e != nil {
		s.writeError(w, r, e, nil)
		return
	}
	writeJSON(w, 200, h)
}
func (s *Server) handleHighlightDelete(w http.ResponseWriter, r *http.Request) {
	v, e := strconv.ParseInt(r.URL.Query().Get("expected_version"), 10, 64)
	if e != nil || v < 1 {
		writeBadRequest(w, "expected_version required")
		return
	}
	if e = s.svc.DeleteHighlight(r.Context(), r.PathValue("id"), v); e != nil {
		s.writeError(w, r, e, nil)
		return
	}
	w.WriteHeader(204)
}
func (s *Server) handleHighlightAudio(w http.ResponseWriter, r *http.Request) {
	clip, e := s.svc.HighlightAudioClip(r.Context(), r.PathValue("id"))
	if e != nil {
		s.writeError(w, r, e, nil)
		return
	}
	defer clip.Close()
	writeAudioClip(w, r, clip)
}
func (s *Server) handleBrief(w http.ResponseWriter, r *http.Request) {
	var in contextbrief.Request
	if !decodeHighlightBody(w, r, &in) {
		return
	}
	b, e := s.svc.BuildContextBrief(r.Context(), in)
	if e != nil {
		s.writeError(w, r, e, nil)
		return
	}
	writeJSON(w, 200, b)
}
