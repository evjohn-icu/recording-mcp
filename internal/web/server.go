package web

import (
	"crypto/subtle"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"recordingmcp/internal/app"
	"recordingmcp/internal/decision"
)

// contentSecurityPolicy is the exact policy the frontend is built for: one
// origin, no inline script, no external asset.
const contentSecurityPolicy = "default-src 'self'; img-src 'self' data:; media-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'"

// maxRequestBody bounds every JSON request body.
const maxRequestBody = 1 << 20

// Config assembles the HTTP server.
type Config struct {
	// Service is the shared business facade; required.
	Service *app.Service
	// TimeService is optional and uses the authenticated session profile as its
	// event scope (or "local" in legacy bootstrap mode).
	TimeService       *app.TimeService
	WorldlineDecision decision.Engine
	// WebDir is the built frontend directory (web/dist). Empty disables static
	// serving while the API keeps working.
	WebDir string
	// DevOrigins lists additional allowed Origin header values, for a Vite dev
	// server on another port. Same-origin requests are always allowed.
	DevOrigins []string
	// BootstrapToken is the single-use token printed at startup. Empty means a
	// random token is generated.
	BootstrapToken string
	// TokenTTL is how long the bootstrap token stays valid. Zero means 10m.
	TokenTTL time.Duration
	// ProfileName binds every browser session to the profile selected when this
	// server instance was created. Empty preserves the legacy bootstrap-token
	// flow. Multi-profile dispatch should create one server per profile.
	ProfileName string
	// VerifyProfilePassword validates a password for ProfileName. Required when
	// ProfileName is set; it must be backed by the trusted profile registry.
	VerifyProfilePassword func(password string) bool
	ChangeProfilePassword func(oldPassword, newPassword string) error
	Logger                *log.Logger
}

// Server is the local web application: one JSON API, one static SPA and one
// audio endpoint over the shared app service.
type Server struct {
	svc                   *app.Service
	sessions              *sessionStore
	webDir                string
	devOrigins            map[string]bool
	logger                *log.Logger
	handler               http.Handler
	profileName           string
	verifyProfilePassword func(string) bool
	changeProfilePassword func(string, string) error
	timeService           *app.TimeService
	loginGate             *loginGate

	bootstrap       string
	bootstrapExpiry time.Time
}

// New builds the server and its route table.
func New(cfg Config) (*Server, error) {
	if cfg.Service == nil {
		return nil, errors.New("web: app service is required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = log.Default()
	}
	token := strings.TrimSpace(cfg.BootstrapToken)
	profileName := strings.TrimSpace(cfg.ProfileName)
	if profileName != "" && cfg.VerifyProfilePassword == nil {
		return nil, errors.New("web: profile password verifier is required")
	}
	if token == "" && profileName == "" {
		generated, err := randomToken()
		if err != nil {
			return nil, err
		}
		token = generated
	}
	s := &Server{
		svc:                   cfg.Service,
		timeService:           cfg.TimeService,
		sessions:              newSessionStore(token, cfg.TokenTTL),
		webDir:                cfg.WebDir,
		devOrigins:            map[string]bool{},
		logger:                logger,
		bootstrap:             token,
		profileName:           profileName,
		verifyProfilePassword: cfg.VerifyProfilePassword,
		changeProfilePassword: cfg.ChangeProfilePassword,
		loginGate:             newLoginGate(),
	}
	for _, origin := range cfg.DevOrigins {
		if origin = strings.TrimSpace(origin); origin != "" {
			canonical, ok := canonicalOrigin(origin)
			if !ok {
				return nil, errors.New("web: invalid development origin")
			}
			s.devOrigins[canonical] = true
		}
	}
	s.bootstrapExpiry = s.sessions.bootstrapExpiry()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/session", s.handleSessionCreate)
	mux.HandleFunc("GET /api/session", s.handleSessionRead)
	mux.HandleFunc("POST /api/login", s.handleProfileLogin)
	mux.HandleFunc("POST /api/logout", s.handleLogout)
	mux.HandleFunc("POST /api/profile/password", s.handleProfilePasswordChange)
	mux.HandleFunc("GET /api/conversations", s.handleConversations)
	mux.HandleFunc("GET /api/conversations/{id}", s.handleConversation)
	mux.HandleFunc("GET /api/conversations/{id}/processing", s.handleProcessing)
	mux.HandleFunc("POST /api/conversations/{id}/processing/retry", s.handleProcessingRetry)
	mux.HandleFunc("GET /api/conversations/{id}/turns", s.handleTurns)
	mux.HandleFunc("GET /api/conversations/{id}/annotations/history", s.handleHistory)
	mux.HandleFunc("PUT /api/conversations/{id}/notes/{rowID}", s.handleEditNote)
	mux.HandleFunc("GET /api/items/{id}", s.handleItem)
	mux.HandleFunc("GET /api/turns/{id}/evidence", s.handleEvidence)
	mux.HandleFunc("GET /api/turns/{id}/audio", s.handleAudio)
	mux.HandleFunc("GET /api/search", s.handleSearch)
	mux.HandleFunc("GET /api/search/hybrid", s.handleHybrid)
	mux.HandleFunc("GET /api/topics", s.handleTopics)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /api/auth-info", s.handleAuthInfo)
	if cfg.TimeService != nil {
		profileScope := func(r *http.Request) string {
			if sess, ok := sessionOf(r); ok && strings.TrimSpace(sess.profile) != "" {
				return sess.profile
			}
			return "local"
		}
		RegisterTimeRoutes(mux, cfg.TimeService, profileScope)
		registerWorldlineRoutes(mux, &app.WorldlineService{Time: cfg.TimeService, DecisionEngine: cfg.WorldlineDecision}, profileScope)
	}
	s.registerCorrectionRoutes(mux)
	s.registerForgetRoutes(mux)
	mux.HandleFunc("/", s.handleCatchAll)
	s.handler = s.wrap(mux)
	return s, nil
}

// Handler returns the root handler, including security headers and auth.
func (s *Server) Handler() http.Handler { return s.handler }

// BootstrapToken is the single-use token the startup log prints.
func (s *Server) BootstrapToken() string { return s.bootstrap }

// BootstrapExpiry reports when the unused bootstrap token expires.
func (s *Server) BootstrapExpiry() time.Time { return s.bootstrapExpiry }

// BootstrapUnused reports whether the printed token can still be exchanged.
func (s *Server) BootstrapUnused() bool { return s.sessions.bootstrapUnused() }

// wrap applies the response headers, authentication, write protection and body
// limit before the route table runs.
func (s *Server) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", contentSecurityPolicy)

		if !validRequestHost(r.Host) {
			writeForbidden(w, app.CodeOriginRejected, "host rejected")
			return
		}
		if s.profileName != "" && strings.HasPrefix(r.URL.Path, "/api/") {
			_, retired, err := s.svc.Store.ProfileRetirement(r.Context())
			if err != nil || retired {
				writeJSON(w, http.StatusGone, errorBody{Error: errorDetail{Code: "profile_retired", Message: "this local profile has been permanently retired"}})
				return
			}
		}
		api := strings.HasPrefix(r.URL.Path, "/api/")
		if api {
			r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
			sessionEndpoint := r.URL.Path == "/api/session"
			bootstrapPost := sessionEndpoint && r.Method == http.MethodPost
			profileLogin := r.URL.Path == "/api/login" && r.Method == http.MethodPost
			authInfo := r.URL.Path == "/api/auth-info" && r.Method == http.MethodGet
			if bootstrapPost || profileLogin || authInfo {
				if !s.originAllowed(r) {
					if authInfo {
						next.ServeHTTP(w, r)
						return
					}
					writeForbidden(w, app.CodeOriginRejected, "origin rejected")
					return
				}
				if profileLogin || authInfo {
					next.ServeHTTP(w, r)
					return
				}
			} else {
				sess, ok := s.sessions.fromRequest(r)
				if !ok {
					writeJSON(w, http.StatusUnauthorized, errorBody{Error: errorDetail{Code: app.CodeUnauthenticated, Message: "session required"}})
					return
				}
				if r.Method != http.MethodGet && r.Method != http.MethodHead {
					if !s.originAllowed(r) {
						writeForbidden(w, app.CodeOriginRejected, "origin rejected")
						return
					}
					token := r.Header.Get("X-CSRF-Token")
					if subtle.ConstantTimeCompare([]byte(token), []byte(sess.csrf)) != 1 {
						writeForbidden(w, app.CodeCSRFRejected, "csrf token rejected")
						return
					}
				}
				r = r.WithContext(withSession(r.Context(), sess))
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleAuthInfo(w http.ResponseWriter, _ *http.Request) {
	mode := "token"
	info := map[string]string{"mode": mode}
	if s.profileName != "" {
		info["mode"] = "profile"
		info["profile"] = s.profileName
	}
	writeJSON(w, http.StatusOK, info)
}

// handleCatchAll serves the SPA for non-API paths and a JSON 404 for unmatched
// API paths, so a typo in a fetch URL never returns index.html.
func (s *Server) handleCatchAll(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/api" {
		writeJSON(w, http.StatusNotFound, errorBody{Error: errorDetail{Code: app.CodeNotFound, Message: "no such endpoint"}})
		return
	}
	s.serveStatic(w, r)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleSessionCreate(w http.ResponseWriter, r *http.Request) {
	if !requireJSON(w, r) {
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	sess, ok := s.sessions.redeem(body.Token)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, errorBody{Error: errorDetail{Code: app.CodeUnauthenticated, Message: "invalid or expired bootstrap token"}})
		return
	}
	setSessionCookie(w, sess)
	writeJSON(w, http.StatusOK, map[string]string{"csrf_token": sess.csrf})
}

func (s *Server) handleSessionRead(w http.ResponseWriter, r *http.Request) {
	sess, ok := sessionOf(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, errorBody{Error: errorDetail{Code: app.CodeUnauthenticated, Message: "session required"}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "csrf_token": sess.csrf, "profile": sess.profile})
}
