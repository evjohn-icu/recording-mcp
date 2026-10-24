package app

import (
	"context"
	"database/sql"
	"errors"
	"recordingmcp/internal/contextbrief"
	"recordingmcp/internal/memory"
	"strings"
)

const HighlightsContract = "highlights/0.1"

type HighlightList struct {
	ContractVersion string             `json:"contract_version"`
	Highlights      []memory.Highlight `json:"highlights"`
}

func highlightError(err error) error {
	if err == nil {
		return nil
	}
	for _, v := range []struct {
		target error
		code   string
		status int
	}{
		{memory.ErrHighlightInvalid, "validation_failed", 400},
		{memory.ErrHighlightVersionConflict, "version_conflict", 409},
		{memory.ErrHighlightRequestConflict, "request_conflict", 409},
		{memory.ErrHighlightSourceMismatch, "source_mismatch", 409},
		{memory.ErrHighlightSourceForgotten, "source_forgotten", 410},
		{memory.ErrHighlightGone, "highlight_gone", 410},
		{memory.ErrProfileRetired, "profile_retired", 410},
		{memory.ErrNotFound, "not_found", 404},
		{memory.ErrHighlightNotFound, "not_found", 404},
		{sql.ErrNoRows, "not_found", 404},
	} {
		if errors.Is(err, v.target) {
			return &Error{Code: v.code, Status: v.status, Message: v.target.Error()}
		}
	}
	return &Error{Code: "internal", Status: 500, Message: "highlight operation failed"}
}
func (s *Service) CreateHighlight(ctx context.Context, in memory.HighlightCreate) (memory.Highlight, error) {
	h, e := s.Store.CreateHighlight(ctx, in)
	return h, highlightError(e)
}
func (s *Service) GetHighlight(ctx context.Context, id string) (memory.Highlight, error) {
	h, e := s.Store.GetHighlight(ctx, id)
	return h, highlightError(e)
}
func (s *Service) ListHighlights(ctx context.Context, id string) (HighlightList, error) {
	h, e := s.Store.ListHighlights(ctx, id)
	if h == nil {
		h = []memory.Highlight{}
	}
	return HighlightList{HighlightsContract, h}, highlightError(e)
}
func (s *Service) UpdateHighlight(ctx context.Context, id string, in memory.HighlightUpdate) (memory.Highlight, error) {
	h, e := s.Store.UpdateHighlight(ctx, id, in)
	return h, highlightError(e)
}
func (s *Service) DeleteHighlight(ctx context.Context, id string, v int64) error {
	return highlightError(s.Store.DeleteHighlight(ctx, id, v))
}
func (s *Service) BuildContextBrief(ctx context.Context, in contextbrief.Request) (contextbrief.Result, error) {
	out, err := contextbrief.Build(ctx, s.Store, in)
	if err == nil {
		return out, nil
	}
	if errors.Is(err, memory.ErrProfileRetired) {
		return out, highlightError(err)
	}
	if strings.HasPrefix(err.Error(), "context brief:") || strings.Contains(err.Error(), "scope only accepts") {
		return out, validationError("invalid brief scope or resource budget")
	}
	return out, &Error{Code: CodeInternal, Status: 500, Message: "brief source unavailable"}
}
