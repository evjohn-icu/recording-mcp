package app

import (
	"context"
	"encoding/json"
	"time"

	"recordingmcp/internal/contextbrief"
)

// BriefPolishRequest carries selectors, never client-supplied private source text.
type BriefPolishRequest struct {
	Request    contextbrief.Request `json:"request"`
	SnapshotID string               `json:"snapshot_id"`
	Consent    bool                 `json:"consent"`
}

// PolishContextBrief leaves deterministic data intact. Candidates are ephemeral
// suggestions and cannot update memory or any source/assertion state.
func (s *Service) PolishContextBrief(ctx context.Context, in BriefPolishRequest) (contextbrief.PolishResult, error) {
	out := contextbrief.PolishResult{SnapshotID: in.SnapshotID, Candidates: []contextbrief.PolishCandidate{}}
	if !in.Consent || in.SnapshotID == "" {
		return out, validationError("explicit consent and snapshot_id are required")
	}
	if s.BriefPolisher == nil {
		return out, &Error{Code: "polish_disabled", Status: 503, Message: "optional brief polishing is disabled"}
	}
	base, err := s.BuildContextBrief(ctx, in.Request)
	if err != nil {
		return out, err
	}
	changed := func() error {
		return &Error{Code: "snapshot_changed", Status: 409, Message: "brief sources changed; regenerate the deterministic preview"}
	}
	if base.SnapshotID != in.SnapshotID {
		return out, changed()
	}
	// Defend the baseline against accidental adapter mutation of shared slices.
	raw, _ := json.Marshal(base)
	var input contextbrief.Result
	if err = json.Unmarshal(raw, &input); err != nil {
		return out, &Error{Code: CodeInternal, Status: 500, Message: "brief unavailable"}
	}
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	candidates, callErr := s.BriefPolisher.Polish(callCtx, input)
	if callCtx.Err() != nil {
		callErr = callCtx.Err()
	}
	cancel()
	// A source revision, forget, retirement or binding change during the model
	// call invalidates the candidate, even when the model reported failure.
	current, err := s.BuildContextBrief(ctx, in.Request)
	if err != nil {
		return out, err
	}
	if current.SnapshotID != base.SnapshotID {
		return out, changed()
	}
	if callErr != nil || contextbrief.ValidatePolishCandidates(base, candidates) != nil {
		out.Degraded = true
		out.Reason = "polish_unavailable"
		return out, nil
	}
	if candidates != nil {
		out.Candidates = candidates
	}
	return out, nil
}
