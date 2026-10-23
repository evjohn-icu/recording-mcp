package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"recordingmcp/internal/plane"
)

type WorldlineSemanticReview struct {
	ExpectedVersion string `json:"expected_version"`
	SuggestionID    string `json:"suggestion_id"`
	Decision        string `json:"decision"`
	RequestID       string `json:"request_id"`
}

// ReviewSemanticRelation records a decision against one exact, source-versioned
// suggestion. Decisions are append-only and never modify factual claims.
func (s *WorldlineService) ReviewSemanticRelation(ctx context.Context, scope string, req WorldlineSemanticReview) (string, error) {
	if s == nil || s.Time == nil || strings.TrimSpace(scope) == "" {
		return "", errors.New("worldline service and profile scope required")
	}
	if req.Decision != "accept" && req.Decision != "reject" && req.Decision != "insufficient" && req.Decision != "reopen" {
		return "", errors.New("decision must be accept, reject, insufficient, or reopen")
	}
	if strings.TrimSpace(req.RequestID) == "" || strings.TrimSpace(req.SuggestionID) == "" || strings.TrimSpace(req.ExpectedVersion) == "" {
		return "", errors.New("suggestion_id, expected_version, and request_id required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	events, err := s.Time.History(ctx, scope)
	if err != nil {
		return "", err
	}
	var suggestion *plane.Event
	for i := range events {
		if events[i].ID == req.SuggestionID && events[i].Type == "worldline.semantic" {
			e := events[i]
			suggestion = &e
			break
		}
	}
	if suggestion == nil {
		return "", errors.New("semantic suggestion not found")
	}
	data, _ := json.Marshal(struct{ SuggestionID, Decision string }{req.SuggestionID, req.Decision})
	id := "wl-semantic-review-" + shortHash(scope+"\x00"+req.RequestID)
	for _, e := range events {
		if e.ID == id {
			if e.StructuredData["review_payload_hash"] == shortHash(string(data)) {
				_, _, v, err := s.project(ctx, scope)
				return v, err
			}
			return "", errors.New("request_id conflict: existing review payload differs")
		}
	}
	currentTopics, _, version, err := s.project(ctx, scope)
	if err != nil {
		return "", err
	}
	if version != req.ExpectedVersion {
		return version, fmt.Errorf("worldline version conflict: current version is %s", version)
	}
	sources, err := s.readSources(ctx)
	if err != nil {
		return "", err
	}
	currentNotes := map[string]string{}
	for _, src := range sources {
		if id, ok := src.claim.Metadata["source_note_id"].(string); ok {
			currentNotes[id] = src.claim.Version
		}
	}
	if !feedbackSourcesCurrent(*suggestion, currentNotes) {
		return version, errors.New("semantic suggestion source version is stale")
	}
	currentSubjects := map[string]string{}
	for _, topic := range currentTopics {
		for _, lane := range topic.Lanes {
			for _, claim := range lane.Claims {
				for _, evidence := range claim.Evidence {
					currentSubjects[evidence.TurnID+"@"+evidence.Revision] = evidence.SubjectID
				}
			}
		}
	}
	if !semanticSourceSubjectsCurrent(*suggestion, currentSubjects) {
		return version, errors.New("semantic suggestion speaker identity snapshot is stale")
	}
	entryID, _ := suggestion.StructuredData["entry_id"].(string)
	currentEntries := map[string]bool{}
	for _, thread := range buildWorldlineThreadDetails(currentTopics) {
		for _, entry := range append(append([]WorldlineThreadEntry{}, thread.entries...), thread.unknown...) {
			currentEntries[entry.ID] = true
		}
	}
	if !currentEntries[entryID] {
		return version, errors.New("worldline version conflict: semantic suggestion source is no longer current")
	}
	var latest *plane.Event
	for i := range events {
		event := &events[i]
		if event.Type != "worldline.semantic" || event.SourceStatus != "current" || !feedbackSourcesCurrent(*event, currentNotes) || !semanticSourceSubjectsCurrent(*event, currentSubjects) {
			continue
		}
		candidateEntryID, _ := event.StructuredData["entry_id"].(string)
		if candidateEntryID != entryID {
			continue
		}
		if latest == nil || eventRecordedAfter(*event, *latest) {
			latest = event
		}
	}
	if latest == nil || latest.ID != suggestion.ID {
		return version, errors.New("worldline version conflict: semantic suggestion was superseded")
	}
	if suggestion.StructuredData["model"] == nil || suggestion.StructuredData["weights_version"] == nil || suggestion.StructuredData["source_snapshot_version"] == nil {
		return version, errors.New("semantic suggestion provenance is incomplete")
	}
	now := time.Now().UTC()
	structured := map[string]any{"scope": scope, "origin": "worldline", "suggestion_id": req.SuggestionID, "decision": req.Decision, "request_id": req.RequestID, "review_payload_hash": shortHash(string(data)), "model": suggestion.StructuredData["model"], "weights_version": suggestion.StructuredData["weights_version"], "source_snapshot_version": suggestion.StructuredData["source_snapshot_version"], "source_note_ids": suggestion.StructuredData["source_note_ids"], "source_note_versions": suggestion.StructuredData["source_note_versions"], "references": suggestion.StructuredData["references"]}
	ev := plane.Event{ID: id, Type: "worldline.semantic_review", Timestamp: now, ObservedAt: now, RecordedAt: &now, Source: "local-web", Content: "worldline semantic relation " + req.Decision, Confidence: 1, StructuredData: structured}
	if err = s.appendWorldlineMutation(ctx, scope, req.ExpectedVersion, shortHash(string(data)), ev); err != nil {
		return version, err
	}
	_, _, v, err := s.project(ctx, scope)
	return v, err
}
