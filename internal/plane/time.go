package plane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"
)

// TemporalQuery separates the time a fact applies from the time it became known.
type TemporalQuery struct {
	Scope       string
	Dimensions  []string
	ValidAt     time.Time
	KnownAt     time.Time
	ExcludedIDs []string
}

// Correction is append-only feedback against one existing event. ExpectedHead
// supplies optimistic concurrency; Scope prevents cross-profile references.
type Correction struct {
	ID           string     `json:"id"`
	Scope        string     `json:"scope"`
	TargetID     string     `json:"target_id"`
	ExpectedHead string     `json:"expected_head"`
	Retract      bool       `json:"retract,omitempty"`
	Content      *string    `json:"content,omitempty"`
	ValidAt      *time.Time `json:"valid_at,omitempty"`
	KnownAt      time.Time  `json:"known_at"`
	Source       string     `json:"source"`
	References   []string   `json:"references,omitempty"`
}

// History returns events scoped to the requested profile without rewriting
// their immutable source rows.
func (s *Service) History(ctx context.Context, scope string) ([]Event, error) {
	if strings.TrimSpace(scope) == "" {
		return nil, errors.New("scope required")
	}
	es, err := s.repo.Events(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Event, 0)
	for _, e := range es {
		if str(e.StructuredData, "scope") == scope {
			out = append(out, e)
		}
	}
	sortEvents(out)
	return out, nil
}

// Correct appends a correction/retraction only if ExpectedHead is the current
// head for the target in Scope. Corrections do not infer completed intentions.
func (s *Service) Correct(ctx context.Context, c Correction) (int, error) {
	if strings.TrimSpace(c.Scope) == "" || strings.TrimSpace(c.TargetID) == "" || strings.TrimSpace(c.ExpectedHead) == "" || strings.TrimSpace(c.ID) == "" || strings.TrimSpace(c.Source) == "" || c.KnownAt.IsZero() {
		return 0, errors.New("correction requires id, scope, target, expected_head, source and known_at")
	}
	if !c.Retract && (c.Content == nil || strings.TrimSpace(*c.Content) == "") {
		return 0, errors.New("correction content cannot be empty")
	}
	r, ok := s.repo.(*sqliteRepository)
	if !ok {
		return 0, errors.New("atomic correction requires the SQLite event repository")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return r.correct(ctx, c)
}

func (r *sqliteRepository) correct(ctx context.Context, c Correction) (int, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, "SELECT payload FROM events")
	if err != nil {
		return 0, err
	}
	events := []Event{}
	byID := map[string]Event{}
	for rows.Next() {
		var raw string
		var e Event
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return 0, err
		}
		if err := json.Unmarshal([]byte(raw), &e); err != nil {
			rows.Close()
			return 0, err
		}
		events = append(events, e)
		byID[e.ID] = e
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	target, exists := byID[c.TargetID]
	if !exists || str(target.StructuredData, "scope") != c.Scope {
		return 0, errors.New("correction target not found in scope")
	}
	if target.Type != "fact.observed" && target.Type != "topic.fact" && target.Type != "fact.corrected" {
		return 0, errors.New("only reported fact events can be corrected; intentions and requests remain unconfirmed")
	}
	typ := "fact.corrected"
	status := "reported"
	if c.Retract {
		typ, status = "fact.retracted", "retracted"
	}
	stateID := factStateID(target)
	validAt := c.ValidAt
	if validAt == nil {
		t := target.Timestamp
		if target.ValidAt != nil {
			t = *target.ValidAt
		} else if target.ValidFrom != nil {
			t = *target.ValidFrom
		}
		validAt = &t
	}
	data := map[string]any{
		"dimension": dimension(target), "scope": c.Scope, "target_id": stateID,
		"target_event_id": c.TargetID, "state_id": stateID, "status": status,
		"claim_mode": str(target.StructuredData, "claim_mode"), "expected_head": c.ExpectedHead,
	}
	if len(c.References) > 0 {
		data["references"] = unique(c.References)
	}
	e := Event{ID: c.ID, Type: typ, Timestamp: c.KnownAt, ObservedAt: c.KnownAt, KnownAt: &c.KnownAt,
		ValidAt: validAt, Source: c.Source, Content: target.Content, Confidence: target.Confidence,
		Actors: target.Actors, Entities: target.Entities, Tags: target.Tags, ConversationID: target.ConversationID, StructuredData: data}
	if c.Content != nil {
		e.Content = *c.Content
	}
	if old, ok := byID[c.ID]; ok {
		e.RecordedAt = old.RecordedAt
		n, err := normalize(e)
		if err != nil {
			return 0, err
		}
		if reflect.DeepEqual(old, n) {
			return 0, tx.Commit()
		}
		return 0, fmt.Errorf("conflicting event ID %q", c.ID)
	}
	currentHead := c.TargetID
	var latest time.Time
	for _, prior := range events {
		if prior.Type != "fact.corrected" && prior.Type != "fact.retracted" || str(prior.StructuredData, "target_event_id") != c.TargetID || str(prior.StructuredData, "scope") != c.Scope {
			continue
		}
		at := prior.Timestamp
		if prior.RecordedAt != nil {
			at = *prior.RecordedAt
		}
		if latest.IsZero() || at.After(latest) || at.Equal(latest) && prior.ID > currentHead {
			currentHead, latest = prior.ID, at
		}
	}
	if c.ExpectedHead != currentHead {
		return 0, fmt.Errorf("optimistic conflict: expected head %q, current head %q", c.ExpectedHead, currentHead)
	}
	now := time.Now().UTC()
	e.RecordedAt = &now
	n, err := normalize(e)
	if err != nil {
		return 0, err
	}
	raw, err := json.Marshal(n)
	if err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, "INSERT INTO events(id,payload) VALUES(?,?)", n.ID, string(raw))
	if err != nil {
		return 0, err
	}
	count, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int(count), nil
}

func factStateID(e Event) string {
	for _, k := range []string{"target_id", "state_id", "commitment_id", "task_id", "person_id", "topic_id", "entity_id"} {
		if id := str(e.StructuredData, k); id != "" {
			return id
		}
	}
	if dimension(e) != "topic" && len(e.Entities) > 0 {
		return e.Entities[0]
	}
	return e.ID
}

// TemporalState projects facts at independent valid-time and knowledge-time
// cutoffs. CurrentState retains its historical semantics unchanged.
func (s *Service) TemporalState(ctx context.Context, q TemporalQuery) ([]State, error) {
	if q.ValidAt.IsZero() || q.KnownAt.IsZero() {
		return nil, errors.New("valid_at and known_at required")
	}
	for _, d := range q.Dimensions {
		if !contains([]string{"commitment", "person", "task", "topic"}, d) {
			return nil, errors.New("invalid dimension")
		}
	}
	es, err := s.repo.Events(ctx)
	if err != nil {
		return nil, err
	}
	eligible := make([]Event, 0, len(es))
	for _, e := range es {
		if contains(q.ExcludedIDs, e.ID) {
			continue
		}
		if q.Scope != "" && str(e.StructuredData, "scope") != q.Scope {
			continue
		}
		va := e.Timestamp
		if e.ValidAt != nil {
			va = *e.ValidAt
		} else if e.ValidFrom != nil {
			va = *e.ValidFrom
		}
		// A source-declared observed_at/known_at is not a server receipt time.
		// Legacy events with no recorded_at are intentionally excluded from
		// bitemporal state; their missing knowledge time remains visible in
		// History as an omitted recorded_at field.
		if e.RecordedAt == nil {
			continue
		}
		ka := *e.RecordedAt
		if va.After(q.ValidAt) || ka.After(q.KnownAt) || e.ValidUntil != nil && !q.ValidAt.Before(*e.ValidUntil) || e.TTLSeconds > 0 && !q.ValidAt.Before(va.Add(time.Duration(e.TTLSeconds)*time.Second)) {
			continue
		}
		// The legacy reducer consumes Timestamp as its ordering coordinate.
		// Supply valid-time ordering only in this private projection snapshot.
		e.Timestamp = va
		e.ObservedAt = ka
		e.RecordedAt = nil
		e.KnownAt = nil
		e.ValidFrom = nil
		e.ValidUntil = nil
		e.TTLSeconds = 0
		eligible = append(eligible, e)
	}
	// The reducer's cutoff is only a ceiling; eligibility above applied both
	// independent axes and expiration against ValidAt.
	cutoff := q.ValidAt
	if q.KnownAt.After(cutoff) {
		cutoff = q.KnownAt
	}
	// Legacy reducer is intentionally reused after staging the two-time filter.
	tmp := New(&sliceRepository{events: eligible})
	return tmp.CurrentState(ctx, q.Dimensions, cutoff)
}

// sliceRepository isolates a projection snapshot; it cannot append durable data.
type sliceRepository struct{ events []Event }

func (r *sliceRepository) Events(context.Context) ([]Event, error) {
	return append([]Event(nil), r.events...), nil
}
func (r *sliceRepository) Append(context.Context, []Event) (int, error) {
	return 0, errors.New("read-only snapshot")
}
func (r *sliceRepository) Close() error { return nil }

// PurgeContent is explicit maintenance for forgetting requests. It is not an
// event update and must be invoked on a dedicated SQLite service instance.
func (s *Service) PurgeContent(ctx context.Context, ids []string) error {
	r, ok := s.repo.(*sqliteRepository)
	if !ok {
		return errors.New("content purge requires sqlite maintenance service")
	}
	return purgeSQLiteContent(ctx, r, ids)
}
func purgeSQLiteContent(ctx context.Context, r *sqliteRepository, ids []string) error {
	if len(ids) == 0 {
		return errors.New("event IDs required")
	}
	if _, err := r.db.ExecContext(ctx, "PRAGMA secure_delete=ON"); err != nil {
		return fmt.Errorf("enable secure deletion: %w", err)
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// The immutable triggers are bypassed only inside this explicitly named
	// maintenance operation, after which an irreversible redacted payload is stored.
	if _, err = tx.ExecContext(ctx, "DROP TRIGGER IF EXISTS events_no_update"); err != nil {
		return err
	}
	for _, id := range unique(ids) {
		var raw string
		if err = tx.QueryRowContext(ctx, "SELECT payload FROM events WHERE id=?", id).Scan(&raw); err != nil {
			return err
		}
		var e Event
		if err = json.Unmarshal([]byte(raw), &e); err != nil {
			return err
		}
		e.Content = "[purged]"
		e.Source = "[purged]"
		e.Actors = nil
		e.Entities = nil
		e.Tags = nil
		e.Priority = ""
		e.Confidence = 0
		e.ConversationID = ""
		e.SourceStatus = ""
		e.StructuredData = map[string]any{"purged": true, "scope": str(e.StructuredData, "scope"), "dimension": dimension(e)}
		b, _ := json.Marshal(e)
		if _, err = tx.ExecContext(ctx, "UPDATE events SET payload=? WHERE id=?", string(b), id); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, "CREATE TRIGGER events_no_update BEFORE UPDATE ON events BEGIN SELECT RAISE(ABORT,'immutable events'); END"); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	var busy, logFrames, checkpointed int
	if err := r.db.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &checkpointed); err != nil {
		return fmt.Errorf("checkpoint purged event data: %w", err)
	}
	if busy != 0 {
		return fmt.Errorf("checkpoint purged event data is busy (%d of %d frames checkpointed)", checkpointed, logFrames)
	}
	if _, err := r.db.ExecContext(ctx, "VACUUM"); err != nil {
		return fmt.Errorf("reclaim purged event storage: %w", err)
	}
	return nil
}
