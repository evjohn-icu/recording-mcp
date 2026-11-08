package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"recordingmcp/internal/plane"
)

// Decision states. A decision is never confirmed by discussion: only a human
// review event can move a candidate to confirmed.
const (
	DecisionObservation       = "observation"
	DecisionProposal          = "proposal"
	DecisionCandidate         = "decision_candidate"
	DecisionConfirmed         = "confirmed"
	DecisionSuperseded        = "superseded"
	DecisionUnresolved        = "unresolved"
	DecisionSchemaVersion     = "decision-trail-v1"
	decisionCandidateSource   = "rule-v1"
	decisionDefaultMaxBytes   = 16000
	decisionMinMaxBytes       = 256
	decisionMaxMaxBytes       = 262144
	decisionDefaultLimit      = 50
	decisionMaxLimit          = 200
	decisionEvidenceTextLimit = 512
)

// DecisionTrailService projects decisions from the Worldline snapshot and the
// decision.review events in the event plane. The projection is deterministic:
// the same rows always serialise to the same bytes.
type DecisionTrailService struct {
	Worldline *WorldlineService
	Time      *TimeService
	mu        sync.Mutex
}

var (
	ErrDecisionVersionConflict = errors.New("app: decision trail version conflict")
	ErrDecisionRequestConflict = errors.New("app: decision review request conflict")
	ErrDecisionNotFound        = errors.New("app: decision candidate not found")
	ErrDecisionValidation      = errors.New("app: decision review validation failed")
)

// DecisionEvidence is one source reference with its own provenance.
type DecisionEvidence struct {
	TurnID         string `json:"turn_id"`
	Revision       string `json:"revision"`
	ConversationID string `json:"conversation_id,omitempty"`
	RecordingID    string `json:"recording_id,omitempty"`
	Text           string `json:"text,omitempty"`
	OffsetMS       *int64 `json:"offset_ms,omitempty"`
	SourceStatus   string `json:"source_status"`
}

// DecisionRecord is one state-bearing entry in a topic's decision history.
type DecisionRecord struct {
	ID                string             `json:"id"`
	CandidateID       string             `json:"candidate_id,omitempty"`
	TopicID           string             `json:"topic_id"`
	TopicTitle        string             `json:"topic_title"`
	State             string             `json:"state"`
	ReviewStatus      string             `json:"review_status,omitempty"`
	Text              string             `json:"text"`
	Summary           string             `json:"summary,omitempty"`
	SourceKind        string             `json:"source_kind"`
	SourceID          string             `json:"source_id,omitempty"`
	SourceVersion     string             `json:"source_version,omitempty"`
	ValidFrom         *time.Time         `json:"valid_from"`
	ValidUntil        *time.Time         `json:"valid_until"`
	TimeBasis         string             `json:"time_basis,omitempty"`
	MentionedAt       *time.Time         `json:"mentioned_at"`
	RecordedAt        *time.Time         `json:"recorded_at"`
	ConfirmedAt       *time.Time         `json:"confirmed_at"`
	Reviewer          string             `json:"reviewer,omitempty"`
	ReviewNote        string             `json:"note,omitempty"`
	Evidence          []DecisionEvidence `json:"evidence"`
	Supersedes        string             `json:"supersedes,omitempty"`
	SupersededBy      string             `json:"superseded_by,omitempty"`
	ConflictsWith     []string           `json:"conflicts_with,omitempty"`
	UnresolvedReasons []string           `json:"unresolved_reasons,omitempty"`
	CandidateSource   string             `json:"candidate_source,omitempty"`
}

// DecisionCounts summarises one topic.
type DecisionCounts struct {
	Observations int `json:"observations"`
	Proposals    int `json:"proposals"`
	Candidates   int `json:"candidates"`
	Confirmed    int `json:"confirmed"`
	Superseded   int `json:"superseded"`
	Unresolved   int `json:"unresolved"`
}

// DecisionTopic is one macro topic with its current slot and full history.
type DecisionTopic struct {
	ID                string           `json:"id"`
	Title             string           `json:"title"`
	CurrentState      string           `json:"current_state"`
	Current           *DecisionRecord  `json:"current"`
	History           []DecisionRecord `json:"history"`
	Unresolved        []DecisionRecord `json:"unresolved"`
	UnresolvedReasons []string         `json:"unresolved_reasons,omitempty"`
	Counts            DecisionCounts   `json:"counts"`
}

// DecisionTrail is the read model. Bytes is the measured serialised length and
// is never on the wire; there is no generated_at so repeats are byte-identical.
type DecisionTrail struct {
	SchemaVersion string          `json:"schema_version"`
	Version       string          `json:"version"`
	Topics        []DecisionTopic `json:"topics"`
	Truncated     bool            `json:"truncated"`
	Bytes         int             `json:"-"`
}

// DecisionQuery selects one topic or all decision-bearing topics.
type DecisionQuery struct {
	TopicID        string
	KnownAt        *time.Time
	IncludeHistory bool
	Limit          int
	MaxBytes       int
}

// DecisionReview is the write request.
type DecisionReview struct {
	ExpectedVersion string     `json:"expected_version"`
	CandidateID     string     `json:"candidate_id"`
	Action          string     `json:"action"`
	RequestID       string     `json:"request_id"`
	Text            string     `json:"text,omitempty"`
	Reviewer        string     `json:"reviewer,omitempty"`
	Note            string     `json:"note,omitempty"`
	ValidFrom       *time.Time `json:"valid_from,omitempty"`
	ValidUntil      *time.Time `json:"valid_until,omitempty"`
}

var decisionCue = regexp.MustCompile(`(?i)(决定|确定|定了|采用|选择|方案|结论|同意|批准|最终|敲定|拍板|decided|decision|chosen|approved|final)`)

// Snapshot returns every projected topic and the projection version in one
// call. It is the same deterministic projection List uses, without paging.
func (s *WorldlineService) Snapshot(ctx context.Context, scope string) ([]WorldlineTopic, string, error) {
	topics, _, version, err := s.project(ctx, scope)
	if err != nil {
		return nil, "", err
	}
	return topics, version, nil
}

// Build projects the trail. It never writes; known_at only filters history.
func (s *DecisionTrailService) Build(ctx context.Context, scope string, q DecisionQuery) (DecisionTrail, error) {
	if s == nil || s.Worldline == nil || s.Time == nil {
		return DecisionTrail{}, errors.New("app: decision trail service is not configured")
	}
	q = normalizedDecisionQuery(q)
	topics, version, err := s.Worldline.Snapshot(ctx, scope)
	if err != nil {
		return DecisionTrail{}, err
	}
	events, err := s.Time.History(ctx, scope)
	if err != nil {
		return DecisionTrail{}, err
	}
	reviews, err := s.decisionReviews(ctx, events, q.KnownAt)
	if err != nil {
		return DecisionTrail{}, err
	}
	trail := DecisionTrail{SchemaVersion: DecisionSchemaVersion, Version: version, Topics: []DecisionTopic{}}
	seen := map[string]bool{}
	for _, worldlineTopic := range topics {
		seen[worldlineTopic.ID] = true
		topic := s.buildTopic(ctx, worldlineTopic, reviews[worldlineTopic.ID], q)
		if q.TopicID == "" && !decisionTopicHasDecision(topic) {
			continue
		}
		trail.Topics = append(trail.Topics, topic)
		if q.TopicID != "" {
			break
		}
		if len(trail.Topics) >= q.Limit {
			trail.Truncated = true
			break
		}
	}
	if q.TopicID == "" && len(trail.Topics) < q.Limit {
		// A review can outlive its source notes (a superseded revision turns
		// the notes stale). Keep those decisions visible by attaching reviews
		// to a placeholder topic instead of dropping the history.
		orphanIDs := make([]string, 0, len(reviews))
		for topicID := range reviews {
			if !seen[topicID] {
				orphanIDs = append(orphanIDs, topicID)
			}
		}
		sort.Strings(orphanIDs)
		for _, topicID := range orphanIDs {
			title := ""
			for _, review := range reviews[topicID] {
				if review.record.TopicTitle != "" {
					title = review.record.TopicTitle
					break
				}
			}
			topic := s.buildTopic(ctx, WorldlineTopic{ID: topicID, Title: title, Lanes: []WorldlineLane{}}, reviews[topicID], q)
			if !decisionTopicHasDecision(topic) {
				continue
			}
			trail.Topics = append(trail.Topics, topic)
			if len(trail.Topics) >= q.Limit {
				trail.Truncated = true
				break
			}
		}
	}
	if q.TopicID != "" && len(trail.Topics) == 0 {
		// The source notes can be gone (stale revision or forgotten content)
		// while the review history must stay visible.
		if reviewsForTopic, ok := reviews[q.TopicID]; ok {
			topic := s.buildTopic(ctx, WorldlineTopic{ID: q.TopicID, Lanes: []WorldlineLane{}}, reviewsForTopic, q)
			if decisionTopicHasDecision(topic) {
				trail.Topics = append(trail.Topics, topic)
			}
		}
	}
	if q.TopicID != "" && len(trail.Topics) == 0 {
		return DecisionTrail{}, ErrDecisionNotFound
	}
	for i := range trail.Topics {
		if !q.IncludeHistory {
			trail.Topics[i].History = nil
		}
	}
	fitDecisionBudget(&trail, q.MaxBytes)
	return trail, nil
}

func normalizedDecisionQuery(q DecisionQuery) DecisionQuery {
	if q.Limit <= 0 {
		q.Limit = decisionDefaultLimit
	}
	if q.Limit > decisionMaxLimit {
		q.Limit = decisionMaxLimit
	}
	if q.MaxBytes == 0 {
		q.MaxBytes = decisionDefaultMaxBytes
	}
	if q.MaxBytes < decisionMinMaxBytes {
		q.MaxBytes = decisionMinMaxBytes
	}
	if q.MaxBytes > decisionMaxMaxBytes {
		q.MaxBytes = decisionMaxMaxBytes
	}
	return q
}

type decisionReviewEvent struct {
	record    DecisionRecord
	request   string
	candidate string
	action    string
	event     plane.Event
}

// decisionReviews parses decision.review events per topic and resolves their
// evidence against the current active attachments.
func (s *DecisionTrailService) decisionReviews(ctx context.Context, events []plane.Event, knownAt *time.Time) (map[string][]decisionReviewEvent, error) {
	out := map[string][]decisionReviewEvent{}
	for _, event := range events {
		if event.Type != "decision.review" {
			continue
		}
		if event.StructuredData["purged"] == true || event.Content == "[purged]" {
			continue
		}
		data := event.StructuredData
		if decisionString(data, "origin") != "decision-trail" {
			continue
		}
		recorded := eventRecordedAt(event)
		if knownAt != nil && recorded != nil && recorded.After(*knownAt) {
			continue
		}
		topicID := decisionString(data, "topic_id")
		candidateID := decisionString(data, "candidate_id")
		action := decisionString(data, "action")
		if topicID == "" || candidateID == "" || action == "" {
			continue
		}
		evidence := reviewEvidence(event)
		status, reasons := s.resolveEvidence(ctx, evidence)
		if event.SourceStatus == "source_stale" {
			status = "stale"
		}
		state := DecisionUnresolved
		reviewStatus := "rejected"
		switch action {
		case "confirm":
			reviewStatus = "accepted"
			if status == "current" {
				state = DecisionConfirmed
			}
		case "reject":
			reviewStatus = "rejected"
		case "reopen":
			reviewStatus = "reopened"
		}
		text := decisionString(data, "text")
		if text == "" {
			text = event.Content
		}
		record := DecisionRecord{
			ID: event.ID, CandidateID: candidateID, TopicID: topicID,
			TopicTitle: decisionString(data, "topic_title"), State: state, ReviewStatus: reviewStatus,
			Text: text, SourceKind: "review", SourceID: event.ID,
			ValidFrom: event.ValidFrom, ValidUntil: event.ValidUntil,
			MentionedAt: event.ValidAt, RecordedAt: recorded, ConfirmedAt: recorded,
			Reviewer: decisionString(data, "reviewer"), ReviewNote: decisionString(data, "note"),
			Evidence: evidence, UnresolvedReasons: reasons,
			CandidateSource: decisionString(data, "model"),
		}
		if status != "current" || state == DecisionUnresolved {
			if len(record.UnresolvedReasons) == 0 {
				record.UnresolvedReasons = []string{"source_invalidated"}
			}
		}
		if status == "unknown" && len(record.UnresolvedReasons) == 0 {
			record.UnresolvedReasons = []string{"source_unknown"}
		}
		out[topicID] = append(out[topicID], decisionReviewEvent{
			record: record, request: decisionString(data, "request_id"), candidate: candidateID,
			action: action, event: event,
		})
	}
	return out, nil
}

// reviewEvidence reads the turn@revision references from the event.
func reviewEvidence(event plane.Event) []DecisionEvidence {
	raw, ok := event.StructuredData["evidence"]
	if !ok {
		return []DecisionEvidence{}
	}
	list, ok := raw.([]any)
	if !ok {
		return []DecisionEvidence{}
	}
	out := []DecisionEvidence{}
	for _, item := range list {
		ref, ok := item.(string)
		if !ok {
			continue
		}
		at := strings.LastIndex(ref, "@")
		if at < 1 || at == len(ref)-1 {
			out = append(out, DecisionEvidence{TurnID: ref, SourceStatus: "unknown"})
			continue
		}
		out = append(out, DecisionEvidence{TurnID: ref[:at], Revision: ref[at+1:], SourceStatus: "unknown"})
	}
	return out
}

// resolveEvidence checks each reference against the single active attachment of
// its turn, excluding ambient turns.
func (s *DecisionTrailService) resolveEvidence(ctx context.Context, evidence []DecisionEvidence) (string, []string) {
	if len(evidence) == 0 {
		return "unknown", []string{"source_unknown"}
	}
	status := "current"
	var reasons []string
	for i := range evidence {
		if evidence[i].Revision == "" {
			status = "unknown"
			reasons = appendUnique(reasons, "source_unknown")
			continue
		}
		var conversationID, recordingID, revision string
		err := s.Time.Store.DB().QueryRowContext(ctx, `SELECT t.conversation_id, t.recording_id, a.transcript_revision
			FROM turns t JOIN attachment_turns at ON at.turn_id=t.id
			JOIN source_attachments a ON a.id=at.attachment_id
			WHERE t.id=? AND a.active=1
			AND NOT EXISTS(SELECT 1 FROM turn_classes tc WHERE tc.turn_id=t.id AND tc.class='ambient')`, evidence[i].TurnID).
			Scan(&conversationID, &recordingID, &revision)
		if errors.Is(err, sql.ErrNoRows) {
			status = "stale"
			evidence[i].SourceStatus = "stale"
			reasons = appendUnique(reasons, "source_invalidated")
			continue
		}
		if err != nil {
			return "unknown", []string{"source_unknown"}
		}
		evidence[i].ConversationID = conversationID
		evidence[i].RecordingID = recordingID
		if revision != evidence[i].Revision {
			status = "stale"
			evidence[i].SourceStatus = "stale"
			reasons = appendUnique(reasons, "source_invalidated")
			continue
		}
		evidence[i].SourceStatus = "current"
	}
	return status, reasons
}

// buildTopic merges claim-derived records with review records, applies the
// supersede rules and computes the single current slot.
func (s *DecisionTrailService) buildTopic(ctx context.Context, topic WorldlineTopic, reviews []decisionReviewEvent, q DecisionQuery) DecisionTopic {
	out := DecisionTopic{ID: topic.ID, Title: topic.Title, History: []DecisionRecord{}, Unresolved: []DecisionRecord{}}
	seenCandidate := map[string]bool{}
	for _, lane := range topic.Lanes {
		for _, claim := range lane.Claims {
			if claim.Summary == "" {
				continue
			}
			state := DecisionProposal
			if claim.Kind == "observation" {
				state = DecisionObservation
			}
			candidateSource := ""
			isCandidate := state != DecisionObservation && decisionCue.MatchString(claim.Summary)
			if isCandidate {
				state = DecisionCandidate
				candidateSource = decisionCandidateSource
			}
			id := "decision-candidate-" + shortHash(topic.ID+"\x00"+claim.ID+"\x00"+claim.Version)
			if seenCandidate[id] {
				continue
			}
			seenCandidate[id] = true
			evidence := make([]DecisionEvidence, 0, len(claim.Evidence))
			for _, item := range claim.Evidence {
				evidence = append(evidence, DecisionEvidence{
					TurnID: item.TurnID, Revision: item.Revision,
					ConversationID: item.ConversationID, RecordingID: item.RecordingID,
					Text: item.Text, OffsetMS: item.OffsetMS, SourceStatus: "current",
				})
			}
			record := DecisionRecord{
				ID: id, CandidateID: id, TopicID: topic.ID, TopicTitle: topic.Title,
				State: state, Text: claim.Summary, Summary: claim.Summary,
				SourceKind: claim.Kind, SourceID: claim.ID, SourceVersion: claim.Version,
				ValidFrom: claim.ValidFrom, ValidUntil: claim.ValidUntil, TimeBasis: claim.TimeBasis,
				MentionedAt: claim.MentionedAt, Evidence: evidence,
				ConflictsWith:   append([]string{}, claim.ConflictWith...),
				CandidateSource: candidateSource,
			}
			if len(evidence) == 0 && isCandidate {
				record.UnresolvedReasons = []string{"source_unknown"}
			}
			if q.KnownAt != nil {
				if claim.MentionedAt == nil || claim.MentionedAt.After(*q.KnownAt) {
					continue
				}
			}
			out.History = append(out.History, record)
		}
	}
	for _, review := range reviews {
		out.History = append(out.History, review.record)
	}
	sortDecisionHistory(out.History)

	// The newest review governs. A reopen or reject after a confirmation voids
	// it: the slot is empty until a fresh confirmation arrives.
	var latestReview *decisionReviewEvent
	for i := range reviews {
		if latestReview == nil || decisionTime(reviews[i].record).After(decisionTime(latestReview.record)) {
			latestReview = &reviews[i]
		}
	}
	if latestReview != nil && latestReview.action != "confirm" {
		reason := "rejected"
		if latestReview.action == "reopen" {
			reason = "reopened"
		}
		for i := range out.History {
			if out.History[i].State == DecisionConfirmed {
				out.History[i].State = DecisionSuperseded
				out.History[i].SupersededBy = latestReview.record.ID
			}
		}
		out.UnresolvedReasons = appendUnique(out.UnresolvedReasons, reason)
	}

	// Newest confirmation wins; when its source went stale the slot is empty
	// and no older confirmation is resurrected.
	var confirmed []DecisionRecord
	for i := range out.History {
		if out.History[i].State == DecisionConfirmed {
			confirmed = append(confirmed, out.History[i])
		}
	}
	sort.SliceStable(confirmed, func(i, j int) bool { return decisionTime(confirmed[i]).After(decisionTime(confirmed[j])) })
	if len(confirmed) > 0 {
		latest := &confirmed[0]
		latestID := latest.ID
		for i := range out.History {
			if out.History[i].ID == latestID {
				out.Current = &out.History[i]
			}
		}
		for i := range out.History {
			if out.History[i].State == DecisionConfirmed && out.History[i].ID != latestID {
				out.History[i].State = DecisionSuperseded
				out.History[i].SupersededBy = latestID
			}
		}
	}

	// Recompute counts and unresolved buckets after supersede.
	out.Counts = DecisionCounts{}
	out.UnresolvedReasons = nil
	for i := range out.History {
		record := &out.History[i]
		switch record.State {
		case DecisionObservation:
			out.Counts.Observations++
		case DecisionProposal:
			out.Counts.Proposals++
		case DecisionCandidate:
			out.Counts.Candidates++
		case DecisionConfirmed:
			out.Counts.Confirmed++
		case DecisionSuperseded:
			out.Counts.Superseded++
		case DecisionUnresolved:
			out.Counts.Unresolved++
			out.Unresolved = append(out.Unresolved, *record)
			for _, reason := range record.UnresolvedReasons {
				out.UnresolvedReasons = appendUnique(out.UnresolvedReasons, reason)
			}
		}
	}
	if out.Current != nil {
		out.CurrentState = DecisionConfirmed
		return out
	}
	for _, review := range reviews {
		if review.action != "confirm" {
			continue
		}
		for _, record := range out.History {
			if record.ID == review.record.ID && record.State == DecisionUnresolved {
				out.UnresolvedReasons = appendUnique(out.UnresolvedReasons, "latest_confirmation_source_invalidated")
			}
		}
	}
	candidateCount := 0
	for i := range out.History {
		record := &out.History[i]
		if record.State != DecisionCandidate {
			continue
		}
		candidateCount++
		if len(record.Evidence) == 0 {
			record.State = DecisionUnresolved
			record.UnresolvedReasons = appendUnique(record.UnresolvedReasons, "source_unknown")
			out.Counts.Candidates--
			out.Counts.Unresolved++
			out.Unresolved = append(out.Unresolved, *record)
			out.UnresolvedReasons = appendUnique(out.UnresolvedReasons, "source_unknown")
		}
	}
	unresolvedRecorded := false
	for i := range out.History {
		switch out.History[i].State {
		case DecisionUnresolved:
			unresolvedRecorded = true
			for _, reason := range out.History[i].UnresolvedReasons {
				out.UnresolvedReasons = appendUnique(out.UnresolvedReasons, reason)
			}
		}
	}
	switch {
	case len(out.UnresolvedReasons) > 0 || unresolvedRecorded:
		out.CurrentState = DecisionUnresolved
	case candidateCount > 1:
		out.CurrentState = DecisionUnresolved
		out.UnresolvedReasons = []string{"multiple_candidates"}
		for i := range out.History {
			if out.History[i].State == DecisionCandidate {
				out.Unresolved = append(out.Unresolved, out.History[i])
			}
		}
	case candidateCount == 1:
		out.CurrentState = DecisionCandidate
	case out.Counts.Proposals > 0:
		out.CurrentState = DecisionProposal
	default:
		out.CurrentState = DecisionObservation
	}
	return out
}

func decisionTopicHasDecision(topic DecisionTopic) bool {
	return topic.Counts.Candidates > 0 || topic.Counts.Confirmed > 0 ||
		topic.Counts.Superseded > 0 || topic.Counts.Unresolved > 0
}

// decisionTime picks the declared basis in order: valid_from, mentioned_at,
// recorded_at, then ID as a stable tiebreaker.
func decisionTime(record DecisionRecord) time.Time {
	if record.ValidFrom != nil {
		return *record.ValidFrom
	}
	if record.MentionedAt != nil {
		return *record.MentionedAt
	}
	if record.RecordedAt != nil {
		return *record.RecordedAt
	}
	return time.Time{}
}

func sortDecisionHistory(records []DecisionRecord) {
	sort.SliceStable(records, func(i, j int) bool {
		ti, tj := decisionTime(records[i]), decisionTime(records[j])
		if !ti.Equal(tj) {
			return ti.Before(tj)
		}
		return records[i].ID < records[j].ID
	})
}

func eventRecordedAt(event plane.Event) *time.Time {
	if event.RecordedAt != nil {
		t := event.RecordedAt.UTC()
		return &t
	}
	if !event.Timestamp.IsZero() {
		t := event.Timestamp.UTC()
		return &t
	}
	return nil
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

// fitDecisionBudget trims in the documented order until the serialised trail
// fits. Truncated is set before measuring because it changes the length.
func fitDecisionBudget(trail *DecisionTrail, maxBytes int) {
	if size, ok := decisionSize(trail); ok && size <= maxBytes {
		trail.Bytes = size
		return
	}
	trail.Truncated = true
	// 1. Drop oldest history records.
	for {
		trimmed := false
		for i := range trail.Topics {
			if len(trail.Topics[i].History) > 0 {
				trail.Topics[i].History = trail.Topics[i].History[1:]
				trimmed = true
				if size, ok := decisionSize(trail); ok && size <= maxBytes {
					trail.Bytes = size
					return
				}
			}
		}
		if !trimmed {
			break
		}
	}
	// 2. Truncate evidence text.
	for i := range trail.Topics {
		eachDecisionRecord(&trail.Topics[i], func(record *DecisionRecord) {
			for e := range record.Evidence {
				record.Evidence[e].Text = truncateBytes(record.Evidence[e].Text, decisionEvidenceTextLimit)
			}
		})
	}
	if size, ok := decisionSize(trail); ok && size <= maxBytes {
		trail.Bytes = size
		return
	}
	// 3. Drop history and evidence entirely.
	for i := range trail.Topics {
		trail.Topics[i].History = nil
		trail.Topics[i].Unresolved = nil
	}
	for i := range trail.Topics {
		eachDecisionRecord(&trail.Topics[i], func(record *DecisionRecord) {
			record.Evidence = nil
		})
	}
	if size, ok := decisionSize(trail); ok && size <= maxBytes {
		trail.Bytes = size
		return
	}
	// 4. Truncate current/candidate texts from the tail until it fits.
	for {
		longestLen := 0
		for i := range trail.Topics {
			eachDecisionRecord(&trail.Topics[i], func(record *DecisionRecord) {
				if n := len(record.Text); n > longestLen {
					longestLen = n
				}
			})
		}
		if longestLen <= 1 {
			break
		}
		limit := longestLen / 2
		for i := range trail.Topics {
			topic := &trail.Topics[i]
			if topic.Current != nil && len(topic.Current.Text) > limit {
				topic.Current.Text = truncateBytes(topic.Current.Text, limit)
			}
			for h := range topic.History {
				if len(topic.History[h].Text) > limit {
					topic.History[h].Text = truncateBytes(topic.History[h].Text, limit)
				}
			}
		}
		if size, ok := decisionSize(trail); ok && size <= maxBytes {
			trail.Bytes = size
			return
		}
	}
	trail.Bytes, _ = decisionSize(trail)
}

// eachDecisionRecord mutates the topic's records in place. Current aliases one
// history entry, so it is visited once through History; Unresolved entries are
// separate copies.
func eachDecisionRecord(topic *DecisionTopic, fn func(*DecisionRecord)) {
	if topic.Current != nil {
		fn(topic.Current)
	}
	for i := range topic.History {
		fn(&topic.History[i])
	}
	for i := range topic.Unresolved {
		fn(&topic.Unresolved[i])
	}
}

func decisionSize(trail *DecisionTrail) (int, bool) {
	raw, err := json.Marshal(trail)
	if err != nil {
		return 0, false
	}
	trail.Bytes = len(raw)
	return len(raw), true
}

func truncateBytes(value string, limit int) string {
	if limit < 0 {
		limit = 0
	}
	if len(value) <= limit {
		return value
	}
	cut := value[:limit]
	for len(cut) > 0 && !utf8ValidTail(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}

func utf8ValidTail(value string) bool {
	if value == "" {
		return true
	}
	b := value[len(value)-1]
	return b < 0x80 || b >= 0xC0
}

// Review appends one human decision event after validating the version, the
// candidate, the evidence and the request identity.
func (s *DecisionTrailService) Review(ctx context.Context, scope string, req DecisionReview) (DecisionTrail, error) {
	if s == nil || s.Worldline == nil || s.Time == nil {
		return DecisionTrail{}, errors.New("app: decision trail service is not configured")
	}
	req.CandidateID = strings.TrimSpace(req.CandidateID)
	req.RequestID = strings.TrimSpace(req.RequestID)
	req.Action = strings.ToLower(strings.TrimSpace(req.Action))
	req.ExpectedVersion = strings.TrimSpace(req.ExpectedVersion)
	req.Reviewer = strings.TrimSpace(req.Reviewer)
	req.Text = strings.TrimSpace(req.Text)
	if req.Reviewer == "" {
		req.Reviewer = "local-user"
	}
	if req.CandidateID == "" || req.RequestID == "" || req.ExpectedVersion == "" {
		return DecisionTrail{}, fmt.Errorf("%w: expected_version, candidate_id and request_id are required", ErrDecisionValidation)
	}
	switch req.Action {
	case "confirm", "reject", "reopen":
	default:
		return DecisionTrail{}, fmt.Errorf("%w: action must be confirm, reject or reopen", ErrDecisionValidation)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	payloadHash := decisionPayloadHash(req)
	eventID := "decision-review-" + shortHash(scope+"\x00"+req.RequestID)
	history, err := s.Time.History(ctx, scope)
	if err != nil {
		return DecisionTrail{}, err
	}
	for _, event := range history {
		if event.ID != eventID {
			continue
		}
		if decisionString(event.StructuredData, "review_payload_hash") != payloadHash {
			return DecisionTrail{}, ErrDecisionRequestConflict
		}
		return s.Build(ctx, scope, DecisionQuery{TopicID: decisionString(event.StructuredData, "topic_id"), IncludeHistory: true})
	}

	topics, version, err := s.Worldline.Snapshot(ctx, scope)
	if err != nil {
		return DecisionTrail{}, err
	}
	if req.ExpectedVersion != version {
		return DecisionTrail{}, ErrDecisionVersionConflict
	}
	topic, record, ok := findDecisionCandidate(topics, req.CandidateID)
	if !ok {
		return DecisionTrail{}, ErrDecisionNotFound
	}
	if req.Action == "confirm" && len(record.Evidence) == 0 {
		return DecisionTrail{}, fmt.Errorf("%w: a confirmation needs source evidence", ErrDecisionValidation)
	}
	evidence := make([]EvidenceReference, 0, len(record.Evidence))
	for _, item := range record.Evidence {
		evidence = append(evidence, EvidenceReference{TurnID: item.TurnID, Revision: item.Revision})
	}
	if _, _, err := s.Time.validateEvidence(ctx, evidence); err != nil {
		return DecisionTrail{}, fmt.Errorf("%w: %v", ErrDecisionValidation, err)
	}

	// Repeating the last action for the same candidate is a no-op, even with a
	// fresh request id.
	latestAction, latestCandidate, err := latestDecisionAction(ctx, s.Time, scope, topic.ID)
	if err != nil {
		return DecisionTrail{}, err
	}
	if latestCandidate == req.CandidateID && latestAction == req.Action {
		return s.Build(ctx, scope, DecisionQuery{TopicID: topic.ID, IncludeHistory: true})
	}

	text := req.Text
	if text == "" {
		text = record.Text
	}
	refs := make([]string, 0, len(evidence))
	for _, item := range evidence {
		refs = append(refs, item.TurnID+"@"+item.Revision)
	}
	conversationID := singleConversation(record.Evidence)
	now := time.Now().UTC()
	event := plane.Event{
		ID: eventID, Type: "decision.review", Timestamp: now, ObservedAt: now, Source: "local-web",
		Content: text, Confidence: 0, ConversationID: conversationID,
		ValidFrom: req.ValidFrom, ValidUntil: req.ValidUntil,
		StructuredData: map[string]any{
			"scope": scope, "origin": "decision-trail", "topic_id": topic.ID,
			"topic_title": topic.Title, "candidate_id": req.CandidateID, "action": req.Action,
			"text": text, "reviewer": req.Reviewer, "note": req.Note,
			"evidence": refs, "request_id": req.RequestID,
			"review_payload_hash": payloadHash, "model": decisionCandidateSource,
		},
	}
	if err := s.Worldline.appendWorldlineMutation(ctx, scope, req.ExpectedVersion, payloadHash, event); err != nil {
		if strings.Contains(err.Error(), "conflict") || strings.Contains(err.Error(), "UNIQUE") {
			return DecisionTrail{}, ErrDecisionVersionConflict
		}
		return DecisionTrail{}, err
	}
	return s.Build(ctx, scope, DecisionQuery{TopicID: topic.ID, IncludeHistory: true})
}

func decisionPayloadHash(req DecisionReview) string {
	payload := map[string]any{
		"candidate_id": req.CandidateID, "action": req.Action, "text": req.Text,
		"valid_from": req.ValidFrom, "valid_until": req.ValidUntil,
		"reviewer": req.Reviewer, "note": req.Note,
	}
	raw, _ := json.Marshal(payload)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// findDecisionCandidate locates a candidate record and its owning topic.
func findDecisionCandidate(topics []WorldlineTopic, candidateID string) (WorldlineTopic, DecisionRecord, bool) {
	for _, topic := range topics {
		for _, lane := range topic.Lanes {
			for _, claim := range lane.Claims {
				if claim.Summary == "" {
					continue
				}
				id := "decision-candidate-" + shortHash(topic.ID+"\x00"+claim.ID+"\x00"+claim.Version)
				if id != candidateID {
					continue
				}
				evidence := make([]DecisionEvidence, 0, len(claim.Evidence))
				for _, item := range claim.Evidence {
					evidence = append(evidence, DecisionEvidence{TurnID: item.TurnID, Revision: item.Revision})
				}
				return topic, DecisionRecord{ID: id, TopicID: topic.ID, Text: claim.Summary, Evidence: evidence, SourceVersion: claim.Version}, true
			}
		}
	}
	return WorldlineTopic{}, DecisionRecord{}, false
}

func singleConversation(evidence []DecisionEvidence) string {
	seen := ""
	for _, item := range evidence {
		if item.ConversationID == "" {
			continue
		}
		if seen == "" {
			seen = item.ConversationID
			continue
		}
		if seen != item.ConversationID {
			return ""
		}
	}
	return seen
}

// latestDecisionAction returns the newest review action for a topic.
func latestDecisionAction(ctx context.Context, timeSvc *TimeService, scope, topicID string) (action, candidate string, err error) {
	events, err := timeSvc.History(ctx, scope)
	if err != nil {
		return "", "", err
	}
	var newest time.Time
	for _, event := range events {
		if event.Type != "decision.review" || decisionString(event.StructuredData, "topic_id") != topicID {
			continue
		}
		if event.StructuredData["purged"] == true || event.Content == "[purged]" {
			continue
		}
		when := eventRecordedAt(event)
		if when == nil {
			continue
		}
		if when.After(newest) {
			newest = *when
			action = decisionString(event.StructuredData, "action")
			candidate = decisionString(event.StructuredData, "candidate_id")
		}
	}
	return action, candidate, nil
}

// decisionString reads a string out of an unstructured event map.
func decisionString(values map[string]any, key string) string {
	if values == nil {
		return ""
	}
	value, ok := values[key]
	if !ok {
		return ""
	}
	text, _ := value.(string)
	return text
}
