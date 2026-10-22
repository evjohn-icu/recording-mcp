package web

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// loginGate bounds expensive Argon2 verification and slows repeated password
// guessing from one local client address.
type loginGate struct {
	mu       sync.Mutex
	failures map[string][]time.Time
	parallel chan struct{}
	now      func() time.Time
}

func newLoginGate() *loginGate {
	return &loginGate{failures: map[string][]time.Time{}, parallel: make(chan struct{}, 2), now: time.Now}
}

func clientKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

func (g *loginGate) verify(r *http.Request, check func() bool) (bool, bool) {
	key := clientKey(r)
	now := g.now()
	g.mu.Lock()
	cut := now.Add(-10 * time.Minute)
	old := g.failures[key]
	fresh := old[:0]
	for _, at := range old {
		if at.After(cut) {
			fresh = append(fresh, at)
		}
	}
	if len(fresh) >= 5 {
		g.failures[key] = fresh
		g.mu.Unlock()
		return false, false
	}
	g.failures[key] = fresh
	g.mu.Unlock()
	select {
	case g.parallel <- struct{}{}:
	default:
		return false, false
	}
	ok := check()
	<-g.parallel
	g.mu.Lock()
	if ok {
		delete(g.failures, key)
	} else {
		g.failures[key] = append(g.failures[key], now)
	}
	g.mu.Unlock()
	return ok, true
}
