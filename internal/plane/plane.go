// Package plane implements an append-only local context event store and deterministic views.
package plane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	_ "modernc.org/sqlite"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"
)

type Event struct {
	ID             string         `json:"id"`
	Type           string         `json:"type"`
	Timestamp      time.Time      `json:"timestamp"`
	Source         string         `json:"source"`
	Actors         []string       `json:"actors,omitempty"`
	Entities       []string       `json:"entities,omitempty"`
	Content        string         `json:"content"`
	StructuredData map[string]any `json:"structured_data,omitempty"`
	Confidence     float64        `json:"confidence"`
	Priority       string         `json:"priority,omitempty"`
	ObservedAt     time.Time      `json:"observed_at"`
	ValidFrom      *time.Time     `json:"valid_from,omitempty"`
	ValidUntil     *time.Time     `json:"valid_until,omitempty"`
	TTLSeconds     int            `json:"ttl_seconds,omitempty"`
	Tags           []string       `json:"tags,omitempty"`
	ConversationID string         `json:"conversation_id,omitempty"`
}
type Query struct {
	Query         string     `json:"query"`
	Person        string     `json:"person"`
	StartTime     *time.Time `json:"start_time,omitempty"`
	EndTime       *time.Time `json:"end_time,omitempty"`
	Entity        string     `json:"entity"`
	Topics        []string   `json:"topics,omitempty"`
	MinConfidence float64    `json:"min_confidence"`
}
type State struct {
	ID             string         `json:"id"`
	Dimension      string         `json:"dimension"`
	Status         string         `json:"status"`
	Summary        string         `json:"summary"`
	Actors         []string       `json:"actors,omitempty"`
	Entities       []string       `json:"entities,omitempty"`
	Tags           []string       `json:"tags,omitempty"`
	Provenance     []string       `json:"provenance"`
	StructuredData map[string]any `json:"structured_data,omitempty"`
	Confidence     float64        `json:"confidence"`
	UpdatedAt      time.Time      `json:"updated_at"`
}
type Repository interface {
	Append(context.Context, []Event) (int, error)
	Events(context.Context) ([]Event, error)
	Close() error
}
type sqliteRepository struct{ db *sql.DB }
type Service struct {
	repo Repository
	mu   sync.Mutex
}

func New(repo Repository) *Service { return &Service{repo: repo} }
func Open(path string) (*Service, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`PRAGMA busy_timeout=5000; CREATE TABLE IF NOT EXISTS events(id TEXT PRIMARY KEY,payload TEXT NOT NULL); CREATE TRIGGER IF NOT EXISTS events_no_update BEFORE UPDATE ON events BEGIN SELECT RAISE(ABORT,'immutable events'); END; CREATE TRIGGER IF NOT EXISTS events_no_delete BEFORE DELETE ON events BEGIN SELECT RAISE(ABORT,'immutable events'); END;`)
	if err != nil {
		db.Close()
		return nil, err
	}
	return New(&sqliteRepository{db}), nil
}
func (s *Service) Close() error          { return s.repo.Close() }
func (r *sqliteRepository) Close() error { return r.db.Close() }
func (r *sqliteRepository) Events(ctx context.Context) ([]Event, error) {
	rows, err := r.db.QueryContext(ctx, "SELECT payload FROM events ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var b string
		var e Event
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(b), &e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
func (r *sqliteRepository) Append(ctx context.Context, events []Event) (int, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	n := 0
	for _, e := range events {
		b, err := json.Marshal(e)
		if err != nil {
			return 0, err
		}
		var old string
		err = tx.QueryRowContext(ctx, "SELECT payload FROM events WHERE id=?", e.ID).Scan(&old)
		if err == nil {
			if old != string(b) {
				return 0, fmt.Errorf("conflicting event ID %q", e.ID)
			}
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO events(id,payload) VALUES(?,?)", e.ID, string(b)); err != nil {
			return 0, err
		}
		n++
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}
func normalize(e Event) (Event, error) {
	if e.StructuredData != nil {
		copied := map[string]any{}
		for k, v := range e.StructuredData {
			copied[k] = v
		}
		e.StructuredData = copied
	}
	e.ID = strings.TrimSpace(e.ID)
	e.Type = strings.ToLower(strings.TrimSpace(e.Type))
	if e.ID == "" || e.Type == "" || e.Timestamp.IsZero() || e.Source == "" {
		return e, errors.New("event requires id, type, timestamp and source")
	}
	if math.IsNaN(e.Confidence) || math.IsInf(e.Confidence, 0) || e.Confidence < 0 || e.Confidence > 1 || e.TTLSeconds < 0 || int64(e.TTLSeconds) > math.MaxInt64/int64(time.Second) {
		return e, errors.New("invalid confidence or TTL")
	}
	if e.Priority != "" && e.Priority != "high" && e.Priority != "normal" && e.Priority != "low" {
		return e, errors.New("priority must be high, normal or low")
	}
	if d := str(e.StructuredData, "dimension"); d != "" && !contains([]string{"commitment", "person", "task", "topic"}, d) {
		return e, errors.New("invalid dimension")
	}
	if d := str(e.StructuredData, "dimension"); d != "" {
		e.StructuredData["dimension"] = strings.ToLower(d)
	}
	if lifecycle(e.Type) {
		if d := str(e.StructuredData, "dimension"); d != "" && d != "commitment" {
			return e, errors.New("lifecycle dimension must be commitment")
		}
		expected := ""
		if e.Type == "commitment.completed" {
			expected = "completed"
		}
		if e.Type == "commitment.cancelled" {
			expected = "cancelled"
		}
		if status := str(e.StructuredData, "status"); expected != "" && status != "" && status != expected {
			return e, errors.New("lifecycle status contradicts event type")
		}
	}
	if lifecycle(e.Type) && str(e.StructuredData, "target_id") == "" {
		return e, errors.New("lifecycle event requires target_id")
	}
	if e.Type == "deadline.changed" {
		if str(e.StructuredData, "status") != "" {
			return e, errors.New("deadline.changed must not change status")
		}
		if _, err := time.Parse(time.RFC3339, str(e.StructuredData, "deadline")); err != nil {
			return e, errors.New("deadline.changed requires RFC3339 deadline")
		}
	}
	if e.ObservedAt.IsZero() {
		e.ObservedAt = e.Timestamp
	}
	e.Timestamp = e.Timestamp.UTC()
	e.ObservedAt = e.ObservedAt.UTC()
	if e.ValidFrom != nil {
		t := e.ValidFrom.UTC()
		e.ValidFrom = &t
	}
	if e.ValidUntil != nil {
		t := e.ValidUntil.UTC()
		e.ValidUntil = &t
	}
	if e.ValidFrom != nil && e.ValidUntil != nil && !e.ValidUntil.After(*e.ValidFrom) {
		return e, errors.New("valid_until must follow valid_from")
	}
	e.Actors = unique(e.Actors)
	e.Entities = unique(e.Entities)
	e.Tags = unique(e.Tags)
	b, err := json.Marshal(e)
	if err != nil {
		return e, err
	}
	err = json.Unmarshal(b, &e)
	return e, err
}
func unique(v []string) []string {
	m := map[string]bool{}
	for _, s := range v {
		if s = strings.TrimSpace(s); s != "" {
			m[s] = true
		}
	}
	out := []string{}
	for s := range m {
		out = append(out, s)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil
	}
	return out
}
func (s *Service) Ingest(ctx context.Context, events []Event) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Event, len(events))
	ids := map[string]Event{}
	for i, e := range events {
		n, err := normalize(e)
		if err != nil {
			return 0, fmt.Errorf("event %d: %w", i, err)
		}
		if prior, ok := ids[n.ID]; ok && !reflect.DeepEqual(prior, n) {
			return 0, fmt.Errorf("conflicting event ID %q", n.ID)
		}
		ids[n.ID] = n
		out[i] = n
	}
	existing, err := s.repo.Events(ctx)
	if err != nil {
		return 0, err
	}
	all := append(append([]Event{}, existing...), out...)
	seen := map[string]bool{}
	dedup := []Event{}
	for _, e := range all {
		if !seen[e.ID] {
			dedup = append(dedup, e)
			seen[e.ID] = true
		}
	}
	all = dedup
	sortEvents(all)
	known := map[string]Event{}
	for _, e := range all {
		key := stateKey(e, dimension(e))
		if target := str(e.StructuredData, "target_id"); target != "" {
			prior, ok := known[key]
			if !ok || !e.Timestamp.After(prior.Timestamp) {
				return 0, fmt.Errorf("feedback target %q must exist before feedback timestamp", target)
			}
			if lifecycle(e.Type) && dimension(prior) != "commitment" {
				return 0, errors.New("lifecycle target must be commitment")
			}
		}
		known[key] = e
	}
	return s.repo.Append(ctx, out)
}
func lifecycle(typ string) bool {
	return typ == "commitment.completed" || typ == "commitment.cancelled" || typ == "deadline.changed"
}
func contains(v []string, q string) bool {
	for _, s := range v {
		if strings.EqualFold(s, q) {
			return true
		}
	}
	return false
}
func (s *Service) Search(ctx context.Context, q Query) ([]Event, error) {
	if math.IsNaN(q.MinConfidence) || math.IsInf(q.MinConfidence, 0) || q.MinConfidence < 0 || q.MinConfidence > 1 {
		return nil, errors.New("min_confidence must be between 0 and 1")
	}
	if q.StartTime != nil && q.EndTime != nil && q.StartTime.After(*q.EndTime) {
		return nil, errors.New("start_time must not follow end_time")
	}
	all, err := s.repo.Events(ctx)
	if err != nil {
		return nil, err
	}
	out := []Event{}
	for _, e := range all {
		if e.Confidence < q.MinConfidence || q.Person != "" && !contains(e.Actors, q.Person) && !contains(e.Entities, q.Person) || q.Entity != "" && !contains(e.Entities, q.Entity) || q.StartTime != nil && e.Timestamp.Before(*q.StartTime) || q.EndTime != nil && e.Timestamp.After(*q.EndTime) {
			continue
		}
		if len(q.Topics) > 0 {
			ok := false
			for _, t := range q.Topics {
				ok = ok || contains(e.Tags, t) || contains(e.Entities, t)
			}
			if !ok {
				continue
			}
		}
		b, _ := json.Marshal(e)
		if q.Query != "" && !strings.Contains(strings.ToLower(string(b)), strings.ToLower(q.Query)) {
			continue
		}
		out = append(out, e)
	}
	sortEvents(out)
	return out, nil
}
func sortEvents(es []Event) {
	sort.Slice(es, func(i, j int) bool {
		if es[i].Timestamp.Equal(es[j].Timestamp) {
			if es[i].ObservedAt.Equal(es[j].ObservedAt) {
				return es[i].ID < es[j].ID
			}
			return es[i].ObservedAt.Before(es[j].ObservedAt)
		}
		return es[i].Timestamp.Before(es[j].Timestamp)
	})
}
func valid(e Event, at time.Time) bool {
	if e.Timestamp.After(at) || e.ObservedAt.After(at) || e.ValidFrom != nil && at.Before(*e.ValidFrom) || e.ValidUntil != nil && !at.Before(*e.ValidUntil) {
		return false
	}
	return e.TTLSeconds == 0 || at.Before(e.Timestamp.Add(time.Duration(e.TTLSeconds)*time.Second))
}
func str(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}
func dimension(e Event) string {
	if e.Type == "deadline.changed" {
		return "commitment"
	}
	d := str(e.StructuredData, "dimension")
	if d != "" {
		return d
	}
	for _, d = range []string{"commitment", "person", "task", "topic"} {
		if strings.Contains(e.Type, d) {
			return d
		}
	}
	return "topic"
}
func stateKey(e Event, d string) string {
	for _, k := range []string{"target_id", "state_id", "commitment_id", "task_id", "person_id", "topic_id", "entity_id"} {
		if id := str(e.StructuredData, k); id != "" {
			return d + ":" + id
		}
	}
	if d != "topic" && len(e.Entities) > 0 {
		return d + ":" + e.Entities[0]
	}
	if d == "person" && len(e.Actors) > 0 {
		return d + ":" + e.Actors[0]
	}
	return d + ":" + e.ID
}
func (s *Service) CurrentState(ctx context.Context, dims []string, at time.Time) ([]State, error) {
	if at.IsZero() {
		return nil, errors.New("projection time required")
	}
	for _, d := range dims {
		if !contains([]string{"commitment", "person", "task", "topic"}, d) {
			return nil, errors.New("invalid dimension")
		}
	}
	es, err := s.repo.Events(ctx)
	if err != nil {
		return nil, err
	}
	sortEvents(es)
	m := map[string]State{}
	for _, e := range es {
		if e.Timestamp.After(at) || e.ObservedAt.After(at) || e.ValidFrom != nil && at.Before(*e.ValidFrom) {
			continue
		}
		d := dimension(e)
		if len(dims) > 0 && !contains(dims, d) {
			continue
		}
		key := stateKey(e, d)
		status := str(e.StructuredData, "status")
		if status == "" {
			status = "active"
			for _, v := range []string{"completed", "cancelled", "withdrawn", "retracted"} {
				if strings.Contains(e.Type, v) {
					status = v
				}
			}
		}
		provenance := []string{e.ID}
		if prior, ok := m[key]; ok && str(e.StructuredData, "target_id") != "" {
			if len(e.Actors) == 0 {
				e.Actors = prior.Actors
			}
			if len(e.Entities) == 0 {
				e.Entities = prior.Entities
			}
			if len(e.Tags) == 0 {
				e.Tags = prior.Tags
			}
			preserveStatus := e.Type == "deadline.changed" && str(e.StructuredData, "status") == ""
			merged := map[string]any{}
			for k, v := range prior.StructuredData {
				merged[k] = v
			}
			for k, v := range e.StructuredData {
				merged[k] = v
			}
			e.StructuredData = merged
			provenance = append(append([]string{}, prior.Provenance...), e.ID)
			if prior.Confidence < e.Confidence {
				e.Confidence = prior.Confidence
			}
			if preserveStatus {
				status = prior.Status
			}
		}
		if e.StructuredData == nil {
			e.StructuredData = map[string]any{}
		}
		e.StructuredData["status"] = status
		m[key] = State{ID: key, Dimension: d, Status: status, Summary: e.Content, Actors: e.Actors, Entities: e.Entities, Tags: e.Tags, Provenance: provenance, StructuredData: e.StructuredData, Confidence: e.Confidence, UpdatedAt: e.Timestamp}
	}
	// Expiry suppresses the latest state, never resurrecting older superseded events.
	byID := map[string]Event{}
	for _, e := range es {
		byID[e.ID] = e
	}
	out := []State{}
	for _, st := range m {
		if valid(byID[st.Provenance[len(st.Provenance)-1]], at) {
			out = append(out, st)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
func (s *Service) Commitments(ctx context.Context, status, person, deadlineBefore string, at time.Time) ([]State, error) {
	states, err := s.CurrentState(ctx, []string{"commitment"}, at)
	if err != nil {
		return nil, err
	}
	var before time.Time
	if deadlineBefore != "" {
		before, err = time.Parse(time.RFC3339, deadlineBefore)
		if err != nil {
			return nil, err
		}
	}
	out := []State{}
	for _, st := range states {
		if status != "" && st.Status != status || person != "" && !contains(st.Actors, person) && !contains(st.Entities, person) {
			continue
		}
		if !before.IsZero() {
			deadline, err := time.Parse(time.RFC3339, str(st.StructuredData, "deadline"))
			if err != nil || !deadline.Before(before) {
				continue
			}
		}
		out = append(out, st)
	}
	return out, nil
}
