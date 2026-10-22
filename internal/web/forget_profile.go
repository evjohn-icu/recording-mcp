package web

import (
	"errors"
	"net/http"
	"strings"

	"recordingmcp/internal/app"
)

type retireProfileRequest struct {
	Profile        string `json:"profile"`
	ConfirmProfile string `json:"confirm_profile"`
	Password       string `json:"password"`
}

func (p *Profiles) handleRetireProfile(w http.ResponseWriter, r *http.Request) {
	if !p.originAllowed(r) {
		writeForbidden(w, app.CodeOriginRejected, "origin rejected")
		return
	}
	if !requireJSON(w, r) {
		return
	}
	var body retireProfileRequest
	if err := decodeJSON(r, &body); err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	body.Profile = strings.TrimSpace(body.Profile)
	if body.Profile == "" || body.ConfirmProfile != body.Profile || len(body.Password) < 12 {
		writeBadRequest(w, "exact profile name and current password are required")
		return
	}
	ok, admitted := p.loginGate.verify(r, func() bool { return p.manager.VerifyForRetirement(body.Profile, body.Password) })
	if !admitted {
		writeJSON(w, http.StatusTooManyRequests, errorBody{Error: errorDetail{Code: app.CodeUnauthenticated, Message: "too many profile retirement attempts; try again later"}})
		return
	}
	if !ok {
		writeJSON(w, http.StatusUnauthorized, errorBody{Error: errorDetail{Code: app.CodeUnauthenticated, Message: "invalid profile or password"}})
		return
	}
	child, err := p.childForRetirement(body.Profile)
	if err != nil {
		p.logger.Printf("web: load retiring profile: %v", err)
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: errorDetail{Code: app.CodeInternal, Message: "profile cleanup could not start"}})
		return
	}
	if err = child.svc.BeginProfileRetirement(r.Context()); err != nil {
		p.logger.Printf("web: fence retiring profile: %v", err)
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: errorDetail{Code: app.CodeInternal, Message: "profile cleanup could not start"}})
		return
	}
	// The external catalog is persisted before cleanup so the profile cannot be
	// listed, opened, or recreated after a crash. Password-gated retry remains.
	if err = p.manager.Retire(body.Profile); err != nil {
		p.logger.Printf("web: retire profile catalog: %v", err)
		p.revokeProfileSessions(body.Profile)
		clearProfileSessionCookie(w)
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "pending", "message": "Deletion is fenced and can be retried with the profile name and password."})
		return
	}
	p.revokeProfileSessions(body.Profile)
	clearProfileSessionCookie(w)
	paths, err := p.manager.ResolveForRetirement(body.Profile)
	if err == nil {
		err = child.svc.CleanupRetiredProfile(r.Context(), paths.Tasks)
	}
	if err != nil {
		p.logger.Printf("web: profile cleanup pending: %v", err)
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "pending", "message": "Deletion is fenced and can be retried with the profile name and password."})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "complete"})
}

func (p *Profiles) childForRetirement(name string) (*Server, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if child := p.children[name]; child != nil {
		return child, nil
	}
	paths, err := p.manager.ResolveForRetirement(name)
	if err != nil {
		return nil, err
	}
	verifier := func(password string) bool { return p.manager.VerifyForRetirement(name, password) }
	child, err := p.factory(paths, verifier)
	if err != nil {
		return nil, err
	}
	if child == nil || child.profileName != name || child.BootstrapToken() != "" {
		return nil, errors.New("web: invalid retiring profile server")
	}
	p.children[name] = child
	return child, nil
}

func (p *Profiles) revokeProfileSessions(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, sess := range p.sessions {
		if sess.profile == name {
			if child := p.children[name]; child != nil {
				child.sessions.revoke(sess.childID)
			}
			delete(p.sessions, id)
		}
	}
}
