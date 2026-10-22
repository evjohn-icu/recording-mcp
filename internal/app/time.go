package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"recordingmcp/internal/memory"
	"recordingmcp/internal/plane"
)

// TimeService is the additive temporal facade. The parent wiring must provide
// it alongside app.Service until the shared service/server files are updated.
type TimeService struct {
	Store  *memory.Store
	Events *plane.Service
}

type EvidenceReference struct {
	TurnID   string `json:"turn_id"`
	Revision string `json:"revision"`
}
type CorrectionRequest struct {
	plane.Correction
	Evidence []EvidenceReference `json:"evidence"`
}

type FactRequest struct {
	Event    plane.Event         `json:"event"`
	Evidence []EvidenceReference `json:"evidence"`
}

func (s *TimeService) History(ctx context.Context, scope string) ([]plane.Event, error) {
	if s == nil || s.Events == nil {
		return nil, errors.New("temporal event service unavailable")
	}
	rows, err := s.Events.History(ctx, scope)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]plane.Event, len(rows))
	for _, e := range rows {
		byID[e.ID] = e
	}
	for i := range rows {
		status, err := s.eventSourceStatus(ctx, rows[i], byID, map[string]bool{})
		if err != nil {
			return nil, err
		}
		rows[i].SourceStatus = status
	}
	return rows, nil
}
func (s *TimeService) State(ctx context.Context, scope string, q plane.TemporalQuery) ([]plane.State, error) {
	if s == nil || s.Events == nil {
		return nil, errors.New("temporal event service unavailable")
	}
	q.Scope = scope
	rows, err := s.Events.History(ctx, scope)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]plane.Event, len(rows))
	for _, e := range rows {
		byID[e.ID] = e
	}
	for _, e := range rows {
		status, err := s.eventSourceStatus(ctx, e, byID, map[string]bool{})
		if err != nil {
			return nil, err
		}
		if status == "source_stale" {
			q.ExcludedIDs = append(q.ExcludedIDs, e.ID)
		}
	}
	return s.Events.TemporalState(ctx, q)
}

func (s *TimeService) eventSourceStatus(ctx context.Context, e plane.Event, byID map[string]plane.Event, visiting map[string]bool) (string, error) {
	if visiting[e.ID] {
		return "unknown", nil
	}
	visiting[e.ID] = true
	defer delete(visiting, e.ID)
	refs, malformed := sourceReferences(e)
	status := "current"
	if malformed {
		status = "unknown"
	}
	if len(refs) == 0 {
		status = "unknown"
	}
	if len(refs) > 0 {
		for _, ref := range refs {
			at := strings.LastIndex(ref, "@")
			if at < 1 || at == len(ref)-1 {
				status = "unknown"
				continue
			}
			turnID, revision := ref[:at], ref[at+1:]
			if s.Store == nil {
				status = "unknown"
				continue
			}
			var currentRevision string
			err := s.Store.DB().QueryRowContext(ctx, `SELECT a.transcript_revision
				FROM turns t JOIN attachment_turns at ON at.turn_id=t.id
				JOIN source_attachments a ON a.id=at.attachment_id
				WHERE t.id=? AND a.active=1
				AND NOT EXISTS(SELECT 1 FROM turn_classes tc WHERE tc.turn_id=t.id AND tc.class='ambient')`, turnID).Scan(&currentRevision)
			if errors.Is(err, sql.ErrNoRows) || err == nil && currentRevision != revision {
				status = "source_stale"
			} else if err != nil {
				return "", err
			}
		}
	}
	if targetID, _ := e.StructuredData["target_event_id"].(string); targetID != "" {
		target, ok := byID[targetID]
		if !ok {
			return "source_stale", nil
		}
		targetStatus, err := s.eventSourceStatus(ctx, target, byID, visiting)
		if err != nil {
			return "", err
		}
		if targetStatus == "source_stale" {
			status = "source_stale"
		} else if targetStatus == "unknown" && status == "current" {
			status = "unknown"
		}
	}
	return status, nil
}

func sourceReferences(e plane.Event) ([]string, bool) {
	if e.StructuredData == nil {
		return nil, false
	}
	refs := []string{}
	malformed := false
	for _, key := range []string{"evidence", "references"} {
		switch v := e.StructuredData[key].(type) {
		case []string:
			refs = append(refs, v...)
		case []any:
			for _, item := range v {
				if value, ok := item.(string); ok {
					refs = append(refs, value)
				} else {
					malformed = true
				}
			}
		case nil:
		default:
			malformed = true
		}
	}
	return refs, malformed
}

// Correct validates each turn/revision against the application's authoritative
// Store before appending feedback to the temporal event log.
func (s *TimeService) Correct(ctx context.Context, req CorrectionRequest) (int, error) {
	if s == nil || s.Store == nil || s.Events == nil {
		return 0, errors.New("temporal service unavailable")
	}
	if len(req.Evidence) == 0 {
		return 0, errors.New("at least one source turn/revision reference required")
	}
	refs, conversations, err := s.validateEvidence(ctx, req.Evidence)
	if err != nil {
		return 0, err
	}
	references := refs
	req.Correction.References = references
	req.Correction.Source = "local-web"
	if req.Correction.Scope == "" {
		return 0, errors.New("scope is required")
	}
	rows, err := s.Events.History(ctx, req.Correction.Scope)
	if err != nil {
		return 0, err
	}
	var target *plane.Event
	for i := range rows {
		if rows[i].ID == req.Correction.TargetID {
			target = &rows[i]
			break
		}
	}
	if target == nil || (target.Type != "fact.observed" && target.Type != "topic.fact" && target.Type != "fact.corrected") {
		return 0, errors.New("only source-backed reported fact events can be amended")
	}
	if target.ConversationID == "" || len(conversations) != 1 || conversations[0] != target.ConversationID {
		return 0, errors.New("amendment evidence must be current and belong to the target conversation")
	}
	return s.Events.Correct(ctx, req.Correction)
}

// ObserveFact stores an audio-sourced report with a current transcript
// provenance link. The projection status is always "reported"; this endpoint
// does not confirm that a statement is true.
func (s *TimeService) ObserveFact(ctx context.Context, scope string, req FactRequest) (int, error) {
	if s == nil || s.Store == nil || s.Events == nil {
		return 0, errors.New("temporal service unavailable")
	}
	if strings.TrimSpace(scope) == "" || req.Event.ID == "" || strings.TrimSpace(req.Event.Content) == "" || req.Event.Timestamp.IsZero() || strings.TrimSpace(req.Event.Source) == "" {
		return 0, errors.New("scope, event id, content, timestamp and source are required")
	}
	if req.Event.Type != "fact.observed" {
		return 0, errors.New("GUI fact entry only accepts fact.observed events")
	}
	if req.Event.StructuredData == nil {
		req.Event.StructuredData = map[string]any{}
	}
	dimension, _ := req.Event.StructuredData["dimension"].(string)
	if dimension == "" || (dimension != "person" && dimension != "task" && dimension != "topic" && dimension != "commitment") {
		return 0, errors.New("fact dimension must be person, task, topic or commitment")
	}
	refs, conversations, err := s.validateEvidence(ctx, req.Evidence)
	if err != nil {
		return 0, err
	}
	if len(conversations) != 1 {
		return 0, errors.New("fact evidence must belong to one conversation")
	}
	req.Event.ConversationID = conversations[0]
	req.Event.Source = "local-web"
	req.Event.StructuredData["scope"] = scope
	req.Event.StructuredData["origin"] = "audio"
	req.Event.StructuredData["claim_mode"] = "reported_statement"
	req.Event.StructuredData["status"] = "reported"
	req.Event.StructuredData["state_id"] = req.Event.ID
	req.Event.StructuredData["evidence"] = refs
	if req.Event.ObservedAt.IsZero() {
		req.Event.ObservedAt = req.Event.Timestamp
	}
	return s.Events.Ingest(ctx, []plane.Event{req.Event})
}

func (s *TimeService) validateEvidence(ctx context.Context, evidence []EvidenceReference) ([]string, []string, error) {
	if len(evidence) == 0 {
		return nil, nil, errors.New("at least one source turn/revision reference required")
	}
	refs := make([]string, 0, len(evidence))
	conversations := map[string]bool{}
	for _, ref := range evidence {
		if strings.TrimSpace(ref.TurnID) == "" || strings.TrimSpace(ref.Revision) == "" {
			return nil, nil, errors.New("turn_id and revision required")
		}
		var conversationID, revision string
		err := s.Store.DB().QueryRowContext(ctx, `SELECT t.conversation_id,a.transcript_revision
			FROM turns t JOIN attachment_turns at ON at.turn_id=t.id
			JOIN source_attachments a ON a.id=at.attachment_id
			WHERE t.id=? AND a.active=1
			AND NOT EXISTS(SELECT 1 FROM turn_classes tc WHERE tc.turn_id=t.id AND tc.class='ambient')`, ref.TurnID).
			Scan(&conversationID, &revision)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, fmt.Errorf("source turn %q is missing, superseded or ambient", ref.TurnID)
		}
		if err != nil {
			return nil, nil, err
		}
		if revision != ref.Revision {
			return nil, nil, fmt.Errorf("source revision does not match current turn %q", ref.TurnID)
		}
		conversations[conversationID] = true
		refs = append(refs, ref.TurnID+"@"+ref.Revision)
	}
	ids := make([]string, 0, len(conversations))
	for id := range conversations {
		ids = append(ids, id)
	}
	return refs, ids, nil
}
func (s *TimeService) PurgeContent(ctx context.Context, ids []string) error {
	if s == nil || s.Events == nil {
		return errors.New("temporal service unavailable")
	}
	return s.Events.PurgeContent(ctx, ids)
}

// PurgeSources redacts temporal-event payloads tied to forgotten transcript
// sources, then follows correction links so amended copies cannot retain them.
func (s *TimeService) PurgeSources(ctx context.Context, scope string, conversations, turnIDs []string) error {
	if s == nil || s.Events == nil || strings.TrimSpace(scope) == "" {
		return errors.New("temporal service and scope required")
	}
	rows, err := s.Events.History(ctx, scope)
	if err != nil {
		return err
	}
	conversationSet, turnSet := map[string]bool{}, map[string]bool{}
	for _, id := range conversations {
		conversationSet[id] = true
	}
	for _, id := range turnIDs {
		turnSet[id] = true
	}
	chosen := map[string]bool{}
	for _, e := range rows {
		if conversationSet[e.ConversationID] {
			chosen[e.ID] = true
			continue
		}
		refs, _ := sourceReferences(e)
		for _, ref := range refs {
			if at := strings.LastIndex(ref, "@"); at > 0 && turnSet[ref[:at]] {
				chosen[e.ID] = true
				break
			}
		}
	}
	changed := true
	for changed {
		changed = false
		for _, e := range rows {
			if chosen[e.ID] {
				continue
			}
			if target, _ := e.StructuredData["target_event_id"].(string); chosen[target] && target != "" {
				chosen[e.ID] = true
				changed = true
			}
		}
	}
	ids := make([]string, 0, len(chosen))
	for id := range chosen {
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil
	}
	return s.Events.PurgeContent(ctx, ids)
}

// TemporalCutoffs creates a validated two-axis request without deriving either
// timestamp from the other.
func TemporalCutoffs(validAt, knownAt time.Time, dimensions []string) plane.TemporalQuery {
	return plane.TemporalQuery{Scope: "", ValidAt: validAt, KnownAt: knownAt, Dimensions: dimensions}
}
