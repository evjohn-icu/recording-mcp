package web

import (
	"net/http"
	"strings"

	"recordingmcp/internal/app"
)

// handleProfileLogin authenticates against the profile fixed to this server
// instance. A profile dispatcher can create one child server per profile and
// must route only to the child selected by the trusted profile registry.
func (s *Server) handleProfileLogin(w http.ResponseWriter, r *http.Request) {
	if s.profileName == "" || s.verifyProfilePassword == nil {
		writeJSON(w, http.StatusNotFound, errorBody{Error: errorDetail{Code: app.CodeNotFound, Message: "profile login is unavailable"}})
		return
	}
	if !requireJSON(w, r) {
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	ok, admitted := s.loginGate.verify(r, func() bool {
		return strings.TrimSpace(body.Password) != "" && s.verifyProfilePassword(body.Password)
	})
	if !admitted {
		writeJSON(w, http.StatusTooManyRequests, errorBody{Error: errorDetail{Code: app.CodeUnauthenticated, Message: "too many profile login attempts; try again later"}})
		return
	}
	if !ok {
		writeJSON(w, http.StatusUnauthorized, errorBody{Error: errorDetail{Code: app.CodeUnauthenticated, Message: "invalid profile password"}})
		return
	}
	sess, ok := s.sessions.createProfileSession(s.profileName)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: errorDetail{Code: app.CodeInternal, Message: "could not create session"}})
		return
	}
	setSessionCookie(w, sess)
	writeJSON(w, http.StatusOK, map[string]string{"csrf_token": sess.csrf, "profile": sess.profile})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	sess, ok := sessionOf(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, errorBody{Error: errorDetail{Code: app.CodeUnauthenticated, Message: "session required"}})
		return
	}
	s.sessions.revoke(sess.id)
	clearSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleProfilePasswordChange(w http.ResponseWriter, r *http.Request) {
	if s.changeProfilePassword == nil {
		writeJSON(w, http.StatusNotFound, errorBody{Error: errorDetail{Code: app.CodeNotFound, Message: "password change is unavailable"}})
		return
	}
	if !requireJSON(w, r) {
		return
	}
	var body struct {
		Current string `json:"current_password"`
		Next    string `json:"new_password"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	if err := s.changeProfilePassword(body.Current, body.Next); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: errorDetail{Code: app.CodeValidationFailed, Message: err.Error()}})
		return
	}
	s.sessions.revokeAll()
	clearSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
}
