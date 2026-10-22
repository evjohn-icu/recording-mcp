package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"time"
)

// sessionCookieName is the only cookie the server sets. The session id is
// opaque, HttpOnly and SameSite=Strict; Secure is off because the server is
// loopback http by design.
const sessionCookieName = "ctxweb_session"

// session is one authenticated browser session. csrf is issued with the cookie
// and must be echoed in X-CSRF-Token on every write.
type session struct {
	id      string
	csrf    string
	profile string
}

// sessionStore keeps sessions and the single-use bootstrap token in memory, so
// restarting the process logs every browser out. The bootstrap token is
// consumed by the first successful POST /api/session.
type sessionStore struct {
	mu        sync.Mutex
	bootstrap string
	expires   time.Time
	sessions  map[string]*session
	now       func() time.Time
}

func newSessionStore(bootstrap string, ttl time.Duration) *sessionStore {
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	return &sessionStore{
		bootstrap: bootstrap,
		expires:   time.Now().Add(ttl),
		sessions:  map[string]*session{},
		now:       time.Now,
	}
}

func randomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// redeem exchanges the bootstrap token for a session and invalidates the
// token, so a leaked URL cannot be replayed.
func (s *sessionStore) redeem(token string) (*session, bool) {
	if strings.TrimSpace(token) == "" {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bootstrap == "" || s.now().After(s.expires) {
		return nil, false
	}
	if subtle.ConstantTimeCompare([]byte(token), []byte(s.bootstrap)) != 1 {
		return nil, false
	}
	id, err := randomToken()
	if err != nil {
		return nil, false
	}
	csrf, err := randomToken()
	if err != nil {
		return nil, false
	}
	s.bootstrap = ""
	s.sessions[id] = &session{id: id, csrf: csrf}
	return s.sessions[id], true
}

// createProfileSession issues a session already bound to the process-selected
// profile. The profile value is immutable for the lifetime of that session.
func (s *sessionStore) createProfileSession(profile string) (*session, bool) {
	if strings.TrimSpace(profile) == "" {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id, err := randomToken()
	if err != nil {
		return nil, false
	}
	csrf, err := randomToken()
	if err != nil {
		return nil, false
	}
	sess := &session{id: id, csrf: csrf, profile: profile}
	s.sessions[id] = sess
	return sess, true
}

func (s *sessionStore) revoke(id string) {
	s.mu.Lock()
	delete(s.sessions, id)
	s.mu.Unlock()
}

func (s *sessionStore) revokeAll() {
	s.mu.Lock()
	clear(s.sessions)
	s.mu.Unlock()
}

func (s *sessionStore) lookup(id string) (*session, bool) {
	if strings.TrimSpace(id) == "" {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	return sess, ok
}

// bootstrapExpiry reports when the unused bootstrap token stops working.
func (s *sessionStore) bootstrapExpiry() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.expires
}

func (s *sessionStore) bootstrapUnused() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bootstrap != "" && s.now().Before(s.expires)
}

func setSessionCookie(w http.ResponseWriter, sess *session) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    sess.id,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
}

func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: "", Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, MaxAge: -1,
	})
}

// fromRequest resolves the session named by the request cookie.
func (s *sessionStore) fromRequest(r *http.Request) (*session, bool) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return nil, false
	}
	return s.lookup(c.Value)
}
