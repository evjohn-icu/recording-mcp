package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTimeRouteErrorsUseJSONEnvelope(t *testing.T) {
	mux := http.NewServeMux()
	RegisterTimeRoutes(mux, nil, func(*http.Request) string { return "profile-a" })
	for _, tc := range []struct {
		path string
		want int
	}{
		{"GET /api/time/state?valid_at=bad&known_at=bad", http.StatusBadRequest},
		{"GET /api/time/history", http.StatusBadRequest},
	} {
		method, path, _ := strings.Cut(tc.path, " ")
		req := httptest.NewRequest(method, path, nil)
		resp := httptest.NewRecorder()
		mux.ServeHTTP(resp, req)
		if resp.Code != tc.want || !strings.HasPrefix(resp.Header().Get("Content-Type"), "application/json") || !strings.Contains(resp.Body.String(), `"error"`) {
			t.Fatalf("%s response = %d %q %q", tc.path, resp.Code, resp.Header().Get("Content-Type"), resp.Body.String())
		}
	}
}
