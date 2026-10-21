package web

import (
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// serveStatic serves the built frontend with history-API fallback: an existing
// file wins, everything else gets index.html so client routes work on reload.
// Directory listings are never produced.
func (s *Server) serveStatic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeJSON(w, http.StatusMethodNotAllowed, errorBody{Error: errorDetail{Code: "method_not_allowed", Message: "method not allowed"}})
		return
	}
	if strings.TrimSpace(s.webDir) == "" {
		s.frontendNotBuilt(w)
		return
	}
	root, err := filepath.Abs(s.webDir)
	if err != nil {
		s.frontendNotBuilt(w)
		return
	}
	clean := path.Clean("/" + strings.TrimPrefix(r.URL.Path, "/"))
	target := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(clean, "/")))
	abs, err := filepath.Abs(target)
	if err != nil {
		s.frontendNotBuilt(w)
		return
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		writeJSON(w, http.StatusNotFound, errorBody{Error: errorDetail{Code: "not_found", Message: "not found"}})
		return
	}
	if info, statErr := os.Stat(abs); statErr == nil && !info.IsDir() {
		if strings.HasPrefix(clean, "/assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		http.ServeFile(w, r, abs)
		return
	}
	index := filepath.Join(root, "index.html")
	if info, statErr := os.Stat(index); statErr != nil || info.IsDir() {
		s.frontendNotBuilt(w)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeFile(w, r, index)
}

// frontendNotBuilt explains a missing web/dist instead of failing opaquely.
func (s *Server) frontendNotBuilt(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = io.WriteString(w, "frontend not built: run make web\n")
}
