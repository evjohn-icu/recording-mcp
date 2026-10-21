package web

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"recordingmcp/internal/app"
	"recordingmcp/internal/memory"
)

type errorDetail struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

type errorBody struct {
	Error errorDetail `json:"error"`
}

// writeJSON writes one JSON response. API responses are never cacheable.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The status line is already sent; nothing else can be reported to the
		// client without corrupting the body.
		return
	}
}

// conflictDetails is the 409 payload shape: the stored version, the whole
// current logical view, the counterpart of the edited note and the caller's
// input, so the UI can show both sides without guessing.
func conflictDetails(e *app.Error, submitted map[string]any) map[string]any {
	details := map[string]any{
		"current_version": e.CurrentVersion,
		"current":         e.Current,
		"current_note":    e.CurrentNote,
	}
	for k, v := range submitted {
		details[k] = v
	}
	return details
}

// writeError maps an error onto the JSON error envelope. App errors carry
// their own code and status; a missing memory row is 404; anything else is an
// internal failure whose detail stays in the log.
func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error, details map[string]any) {
	if err == nil {
		return
	}
	var appErr *app.Error
	switch {
	case errors.As(err, &appErr):
		if details == nil && appErr.Code == app.CodeAnnotationVersionConflict {
			details = conflictDetails(appErr, nil)
		}
		writeJSON(w, appErr.Status, errorBody{Error: errorDetail{Code: appErr.Code, Message: appErr.Message, Details: details}})
	case errors.Is(err, memory.ErrNotFound):
		writeJSON(w, http.StatusNotFound, errorBody{Error: errorDetail{Code: app.CodeNotFound, Message: err.Error()}})
	default:
		s.logger.Printf("web: %s %s: %v", r.Method, r.URL.Path, err)
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: errorDetail{Code: app.CodeInternal, Message: "internal error"}})
	}
}

func writeBadRequest(w http.ResponseWriter, msg string) {
	writeJSON(w, http.StatusBadRequest, errorBody{Error: errorDetail{Code: app.CodeValidationFailed, Message: msg}})
}

func writeForbidden(w http.ResponseWriter, code, msg string) {
	writeJSON(w, http.StatusForbidden, errorBody{Error: errorDetail{Code: code, Message: msg}})
}

// contextKey is the private context key type for the authenticated session.
type contextKey struct{ name string }

var sessionKey = contextKey{name: "session"}

func withSession(ctx context.Context, sess *session) context.Context {
	return context.WithValue(ctx, sessionKey, sess)
}

func sessionOf(r *http.Request) (*session, bool) {
	sess, ok := r.Context().Value(sessionKey).(*session)
	return sess, ok
}

// originAllowed reports whether the request's Origin header names this server
// (same-origin) or an explicitly allowed development origin. A missing or
// null Origin is never allowed: it means a non-browser or sandboxed caller
// that cannot be trusted with cookie-authenticated writes.
func (s *Server) originAllowed(r *http.Request) bool {
	origin, ok := canonicalOrigin(r.Header.Get("Origin"))
	if !ok {
		return false
	}
	if origin == "http://"+canonicalAuthority(r.Host) {
		return true
	}
	return s.devOrigins[origin]
}

func canonicalOrigin(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u == nil || u.Scheme != "http" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return "", false
	}
	if !validRequestHost(u.Host) {
		return "", false
	}
	return "http://" + canonicalAuthority(u.Host), true
}

func validRequestHost(host string) bool {
	authority := canonicalAuthority(host)
	if authority == "" || authority != strings.ToLower(host) || strings.ContainsAny(host, "\\\\/@?#") {
		return false
	}
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		h = strings.Trim(host, "[]")
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback()
	}
	return strings.EqualFold(h, "localhost")
}

func canonicalAuthority(host string) string {
	if host == "" || strings.ContainsAny(host, "\\\\/@?#") {
		return ""
	}
	h, port, err := net.SplitHostPort(host)
	if err == nil {
		p, parseErr := strconv.Atoi(port)
		if h == "" || parseErr != nil || p < 1 || p > 65535 {
			return ""
		}
		return strings.ToLower(net.JoinHostPort(h, strconv.Itoa(p)))
	}
	if strings.Contains(host, ":") {
		h = strings.Trim(host, "[]")
		if net.ParseIP(h) == nil {
			return ""
		}
		return "[" + strings.ToLower(h) + "]"
	}
	return strings.ToLower(host)
}
