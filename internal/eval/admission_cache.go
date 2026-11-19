package eval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"recordingmcp/internal/contextbuild"
)

// CachedCandidateAssessor memoizes admission decisions for calibration only.
// The cache key covers the assessor model and the complete canonical candidate
// payload, so a changed query, evidence or model can never reuse a stale
// probability. The cache is not valid evidence for a production latency gate:
// the selected candidate must be re-run uncached.
type CachedCandidateAssessor struct {
	inner contextbuild.CandidateAssessor
	path  string
	model string

	mu      sync.Mutex
	entries map[string][]contextbuild.AdmissionDecision
}

// NewCachedCandidateAssessor loads or creates the cache file.
func NewCachedCandidateAssessor(inner contextbuild.CandidateAssessor, path, model string) (*CachedCandidateAssessor, error) {
	cache := &CachedCandidateAssessor{inner: inner, path: path, model: model, entries: map[string][]contextbuild.AdmissionDecision{}}
	data, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(data, &cache.entries); err != nil {
			return nil, fmt.Errorf("eval: decode admission probability cache: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return cache, nil
}

// AssessCandidates serves cached decisions or evaluates and stores them.
func (c *CachedCandidateAssessor) AssessCandidates(ctx context.Context, query string, candidates []contextbuild.AdmissionCandidate) ([]contextbuild.AdmissionDecision, error) {
	key, err := admissionCacheKey(c.model, query, candidates)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	cached, ok := c.entries[key]
	c.mu.Unlock()
	if ok {
		return cached, nil
	}
	decisions, err := c.inner.AssessCandidates(ctx, query, candidates)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.entries[key] = decisions
	err = c.saveLocked()
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return decisions, nil
}

func (c *CachedCandidateAssessor) saveLocked() error {
	data, err := json.Marshal(c.entries)
	if err != nil {
		return err
	}
	dir := filepath.Dir(c.path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
}

// admissionCacheKey hashes the model, query and full candidate payload.
func admissionCacheKey(model, query string, candidates []contextbuild.AdmissionCandidate) (string, error) {
	payload, err := json.Marshal(struct {
		Model      string                            `json:"model"`
		Query      string                            `json:"query"`
		Candidates []contextbuild.AdmissionCandidate `json:"candidates"`
	}{Model: model, Query: query, Candidates: candidates})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}
