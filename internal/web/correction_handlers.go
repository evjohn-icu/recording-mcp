package web

import (
	"net/http"
	"strings"
)

// registerCorrectionRoutes must be called by Server.New when wiring the
// correction and speaker-review API.
func (s *Server) registerCorrectionRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/turns/{id}/transcript-source", s.handleTranscriptSourceForTurn)
	mux.HandleFunc("GET /api/conversations/{id}/recordings/{recordingID}/segments/{segmentID}", s.handleTranscriptSegment)
	mux.HandleFunc("POST /api/conversations/{id}/recordings/{recordingID}/segments/{segmentID}/correction", s.handleTranscriptCorrection)
	mux.HandleFunc("GET /api/recordings/{recordingID}/speakers", s.handleSpeakerBindings)
	mux.HandleFunc("POST /api/recordings/{recordingID}/speakers/proposals", s.handleSpeakerProposal)
	mux.HandleFunc("PUT /api/speaker-bindings/{bindingID}", s.handleSpeakerReview)
}

func (s *Server) handleTranscriptSourceForTurn(w http.ResponseWriter, r *http.Request) {
	source, err := s.svc.TranscriptSourceForTurn(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err, nil)
		return
	}
	writeJSON(w, http.StatusOK, source)
}

func (s *Server) handleTranscriptSegment(w http.ResponseWriter, r *http.Request) {
	segment, err := s.svc.TranscriptSegment(r.Context(), r.PathValue("id"), r.PathValue("recordingID"), r.PathValue("segmentID"))
	if err != nil {
		s.writeError(w, r, err, nil)
		return
	}
	writeJSON(w, http.StatusOK, segment)
}

func (s *Server) handleTranscriptCorrection(w http.ResponseWriter, r *http.Request) {
	if !requireJSON(w, r) {
		return
	}
	var body struct {
		ExpectedRevision string            `json:"expected_revision"`
		NewRevision      string            `json:"new_revision"`
		Texts            map[string]string `json:"texts"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	report, err := s.svc.CorrectTranscriptSegment(r.Context(), r.PathValue("id"), r.PathValue("recordingID"), r.PathValue("segmentID"), body.ExpectedRevision, body.NewRevision, body.Texts)
	if err != nil {
		s.writeError(w, r, err, map[string]any{"expected_revision": body.ExpectedRevision, "new_revision": body.NewRevision})
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func (s *Server) handleSpeakerBindings(w http.ResponseWriter, r *http.Request) {
	bindings, err := s.svc.ListSpeakerBindings(r.Context(), r.PathValue("recordingID"), strings.TrimSpace(r.URL.Query().Get("speaker_scoped_id")))
	if err != nil {
		s.writeError(w, r, err, nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"bindings": bindings})
}

func (s *Server) handleSpeakerProposal(w http.ResponseWriter, r *http.Request) {
	if !requireJSON(w, r) {
		return
	}
	var body struct {
		SpeakerScopedID    string   `json:"speaker_scoped_id"`
		TranscriptRevision string   `json:"transcript_revision"`
		EvidenceTurnIDs    []string `json:"evidence_turn_ids"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	actor := sessionActor(r)
	binding, err := s.svc.ProposeSpeakerBinding(r.Context(), r.PathValue("recordingID"), body.SpeakerScopedID, body.TranscriptRevision, body.EvidenceTurnIDs, actor)
	if err != nil {
		s.writeError(w, r, err, nil)
		return
	}
	writeJSON(w, http.StatusOK, binding)
}

func (s *Server) handleSpeakerReview(w http.ResponseWriter, r *http.Request) {
	if !requireJSON(w, r) {
		return
	}
	var body struct {
		Action   string `json:"action"`
		PersonID string `json:"person_id"`
		Note     string `json:"note"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	reviewer := sessionActor(r)
	var (
		binding any
		err     error
	)
	switch body.Action {
	case "confirm":
		binding, err = s.svc.ConfirmSpeakerBinding(r.Context(), r.PathValue("bindingID"), body.PersonID, reviewer)
	case "reject":
		binding, err = s.svc.RejectSpeakerBinding(r.Context(), r.PathValue("bindingID"), reviewer, body.Note)
	case "revoke":
		binding, err = s.svc.RevokeSpeakerBinding(r.Context(), r.PathValue("bindingID"), reviewer, body.Note)
	default:
		writeBadRequest(w, "action must be confirm, reject or revoke")
		return
	}
	if err != nil {
		s.writeError(w, r, err, nil)
		return
	}
	writeJSON(w, http.StatusOK, binding)
}

func sessionActor(r *http.Request) string {
	if sess, ok := sessionOf(r); ok && strings.TrimSpace(sess.profile) != "" {
		return sess.profile
	}
	return "local-web"
}
