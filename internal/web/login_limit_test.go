package web

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestLoginGateThrottlesAndExpiresFailedAttempts(t *testing.T) {
	g := newLoginGate()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	g.now = func() time.Time { return now }
	r := httptest.NewRequest("POST", "/api/login", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	for i := 0; i < 5; i++ {
		ok, admitted := g.verify(r, func() bool { return false })
		if ok || !admitted {
			t.Fatalf("failure %d: ok=%v admitted=%v", i, ok, admitted)
		}
	}
	called := false
	if _, admitted := g.verify(r, func() bool { called = true; return true }); admitted || called {
		t.Fatal("throttled request ran password verification")
	}
	now = now.Add(10*time.Minute + time.Second)
	if ok, admitted := g.verify(r, func() bool { return true }); !ok || !admitted {
		t.Fatalf("expired rate limit: ok=%v admitted=%v", ok, admitted)
	}
}
