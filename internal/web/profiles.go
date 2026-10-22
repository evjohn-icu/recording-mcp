package web

import (
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"

	"recordingmcp/internal/app"
	"recordingmcp/internal/profile"
)

const profileSessionCookieName = "ctxweb_profile_session"

// ProfileServerFactory opens the service for one trusted profile. The child
// server must use the supplied verifier and profile name in its web.Config.
type ProfileServerFactory func(profile.Paths, func(string) bool) (*Server, error)

type profileSession struct {
	id      string
	profile string
	childID string
}

// ProfilesConfig creates a local dispatcher that selects a child server only
// after successful registry password verification.
type ProfilesConfig struct {
	Manager    *profile.Manager
	Factory    ProfileServerFactory
	WebDir     string
	DevOrigins []string
	Logger     *log.Logger
}

// Profiles serves the shared SPA and dispatches authenticated requests to
// exactly one password-verified profile server. The opaque outer cookie maps
// to a server-side profile and child session; request parameters never select
// a data directory.
type Profiles struct {
	manager    *profile.Manager
	factory    ProfileServerFactory
	webDir     string
	logger     *log.Logger
	devOrigins map[string]bool
	mu         sync.Mutex
	children   map[string]*Server
	sessions   map[string]*profileSession
	handler    http.Handler
	loginGate  *loginGate
}

// NewProfiles creates a profile dispatcher.
func NewProfiles(cfg ProfilesConfig) (*Profiles, error) {
	if cfg.Manager == nil || cfg.Factory == nil {
		return nil, errors.New("web: profile manager and server factory are required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = log.Default()
	}
	p := &Profiles{
		manager: cfg.Manager, factory: cfg.Factory, webDir: cfg.WebDir, logger: logger,
		devOrigins: map[string]bool{}, children: map[string]*Server{}, sessions: map[string]*profileSession{}, loginGate: newLoginGate(),
	}
	for _, origin := range cfg.DevOrigins {
		canonical, ok := canonicalOrigin(strings.TrimSpace(origin))
		if !ok {
			return nil, errors.New("web: invalid development origin")
		}
		p.devOrigins[canonical] = true
	}
	p.handler = http.HandlerFunc(p.serveHTTP)
	return p, nil
}

func (p *Profiles) Handler() http.Handler { return p.handler }

func (p *Profiles) serveHTTP(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	if !validRequestHost(r.Host) {
		writeForbidden(w, app.CodeOriginRejected, "host rejected")
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	}
	if r.URL.Path == "/api/auth-info" && r.Method == http.MethodGet {
		names, err := p.manager.List()
		if err != nil {
			p.logger.Printf("web: list profiles: %v", err)
			writeJSON(w, http.StatusInternalServerError, errorBody{Error: errorDetail{Code: app.CodeInternal, Message: "could not list profiles"}})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"mode": "profiles", "profiles": names})
		return
	}
	if r.URL.Path == "/api/profiles/login" && r.Method == http.MethodPost {
		if !p.originAllowed(r) {
			writeForbidden(w, app.CodeOriginRejected, "origin rejected")
			return
		}
		p.handleLogin(w, r)
		return
	}
	if r.URL.Path == "/api/profiles/retire" && r.Method == http.MethodPost {
		p.handleRetireProfile(w, r)
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/api/") {
		(&Server{webDir: p.webDir}).serveStatic(w, r)
		return
	}
	outer, ok := p.sessionFromRequest(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, errorBody{Error: errorDetail{Code: app.CodeUnauthenticated, Message: "profile session required"}})
		return
	}
	child, err := p.child(outer.profile)
	if err != nil {
		p.logger.Printf("web: open profile server: %v", err)
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: errorDetail{Code: app.CodeInternal, Message: "profile unavailable"}})
		return
	}
	if r.URL.Path == "/api/logout" && r.Method == http.MethodPost {
		recorder := httptest.NewRecorder()
		child.handler.ServeHTTP(recorder, p.childRequest(r, outer.childID))
		if recorder.Code == http.StatusNoContent {
			clearProfileSessionCookie(w)
		}
		copyResponse(w, recorder)
		if recorder.Code == http.StatusNoContent {
			p.mu.Lock()
			delete(p.sessions, outer.id)
			p.mu.Unlock()
		}
		return
	}
	if r.URL.Path == "/api/profile/password" && r.Method == http.MethodPost {
		recorder := httptest.NewRecorder()
		child.handler.ServeHTTP(recorder, p.childRequest(r, outer.childID))
		if recorder.Code == http.StatusNoContent {
			clearProfileSessionCookie(w)
			p.mu.Lock()
			delete(p.sessions, outer.id)
			p.mu.Unlock()
		}
		copyResponse(w, recorder)
		return
	}
	child.handler.ServeHTTP(w, p.childRequest(r, outer.childID))
}

func (p *Profiles) childRequest(r *http.Request, childID string) *http.Request {
	childReq := r.Clone(r.Context())
	childReq.Header = r.Header.Clone()
	childReq.Header.Del("Cookie")
	childReq.AddCookie(&http.Cookie{Name: sessionCookieName, Value: childID})
	return childReq
}

func copyResponse(w http.ResponseWriter, resp *httptest.ResponseRecorder) {
	for key, values := range resp.Header() {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(resp.Code)
	_, _ = w.Write(resp.Body.Bytes())
}

func (p *Profiles) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !requireJSON(w, r) {
		return
	}
	var body struct {
		Profile  string `json:"profile"`
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	ok, admitted := p.loginGate.verify(r, func() bool { return p.manager.Verify(body.Profile, body.Password) })
	if !admitted {
		writeJSON(w, http.StatusTooManyRequests, errorBody{Error: errorDetail{Code: app.CodeUnauthenticated, Message: "too many profile login attempts; try again later"}})
		return
	}
	if !ok {
		writeJSON(w, http.StatusUnauthorized, errorBody{Error: errorDetail{Code: app.CodeUnauthenticated, Message: "invalid profile or password"}})
		return
	}
	child, err := p.child(body.Profile)
	if err == nil {
		_, retired, e := child.svc.Store.ProfileRetirement(r.Context())
		if e != nil || retired {
			writeJSON(w, http.StatusGone, errorBody{Error: errorDetail{Code: "profile_retired", Message: "this local profile has been permanently retired"}})
			return
		}
	}
	if err != nil {
		p.logger.Printf("web: open profile server: %v", err)
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: errorDetail{Code: app.CodeInternal, Message: "profile unavailable"}})
		return
	}
	childSession, ok := child.sessions.createProfileSession(body.Profile)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: errorDetail{Code: app.CodeInternal, Message: "could not create session"}})
		return
	}
	outerID, err := randomToken()
	if err != nil {
		child.sessions.revoke(childSession.id)
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: errorDetail{Code: app.CodeInternal, Message: "could not create session"}})
		return
	}
	if old, ok := p.sessionFromRequest(r); ok {
		if oldChild, e := p.child(old.profile); e == nil {
			oldChild.sessions.revoke(old.childID)
		}
		p.mu.Lock()
		delete(p.sessions, old.id)
		p.mu.Unlock()
	}
	p.mu.Lock()
	p.sessions[outerID] = &profileSession{id: outerID, profile: body.Profile, childID: childSession.id}
	p.mu.Unlock()
	setProfileSessionCookie(w, outerID)
	setSessionCookie(w, childSession)
	writeJSON(w, http.StatusOK, map[string]string{"csrf_token": childSession.csrf, "profile": body.Profile})
}

func (p *Profiles) child(name string) (*Server, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if child := p.children[name]; child != nil {
		return child, nil
	}
	paths, err := p.manager.Resolve(name)
	if err != nil {
		return nil, err
	}
	verifier := func(password string) bool { return p.manager.Verify(name, password) }
	child, err := p.factory(paths, verifier)
	if err != nil {
		return nil, err
	}
	if child == nil || child.profileName != name || child.BootstrapToken() != "" {
		return nil, errors.New("web: profile factory returned a server with the wrong profile binding")
	}
	p.children[name] = child
	return child, nil
}

func (p *Profiles) sessionFromRequest(r *http.Request) (*profileSession, bool) {
	cookie, err := r.Cookie(profileSessionCookieName)
	if err != nil || cookie.Value == "" {
		return nil, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	sess, ok := p.sessions[cookie.Value]
	return sess, ok
}

func (p *Profiles) originAllowed(r *http.Request) bool {
	origin, ok := canonicalOrigin(r.Header.Get("Origin"))
	if !ok {
		return false
	}
	return origin == "http://"+canonicalAuthority(r.Host) || p.devOrigins[origin]
}

func setProfileSessionCookie(w http.ResponseWriter, id string) {
	http.SetCookie(w, &http.Cookie{Name: profileSessionCookieName, Value: id, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
}

func clearProfileSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: profileSessionCookieName, Value: "", Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: -1})
}

func setSecurityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy", contentSecurityPolicy)
}

// serveProfileAsset is kept separate from profile storage; all paths are
// resolved beneath the configured static root and directories are not listed.
