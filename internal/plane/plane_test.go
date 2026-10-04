package plane

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestPlane(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	s, err := Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	event := func(id, typ, dim, tag string) Event {
		return Event{ID: id, Type: typ, Timestamp: at.Add(-time.Hour), Source: "test", Content: id, Actors: []string{"Chen"}, Entities: []string{id}, Tags: []string{tag}, Confidence: .9, StructuredData: map[string]any{"dimension": dim, "state_id": id}}
	}
	a := event("bom", "commitment.created", "commitment", "bom")
	b := event("demo-company", "topic.fact", "topic", "career")
	c := event("friend", "person.profile", "person", "relationship")
	low := event("low", "task.created", "task", "task")
	low.Confidence = .2
	expired := event("expired", "task.created", "task", "task")
	expired.TTLSeconds = 3600
	events := []Event{a, b, c, low, expired}
	if n, err := s.Ingest(ctx, events); err != nil || n != 5 {
		t.Fatalf("ingest %d %v", n, err)
	}
	if n, err := s.Ingest(ctx, events); err != nil || n != 0 {
		t.Fatalf("replay %d %v", n, err)
	}
	conflict := a
	conflict.Content = "changed"
	if _, err = s.Ingest(ctx, []Event{event("new", "task.created", "task", "task"), conflict}); err == nil {
		t.Fatal("conflict accepted")
	}
	all, _ := s.Search(ctx, Query{})
	if len(all) != 5 {
		t.Fatal("batch not atomic")
	}
	done := Event{ID: "done", Type: "commitment.completed", Timestamp: at, Source: "feedback", Confidence: 1, StructuredData: map[string]any{"target_id": "bom"}, Content: "done"}
	if _, err = s.Ingest(ctx, []Event{done}); err != nil {
		t.Fatal(err)
	}
	states, err := s.Commitments(ctx, "completed", "Chen", "", at)
	if err != nil || len(states) != 1 || states[0].Provenance[len(states[0].Provenance)-1] != "done" {
		t.Fatalf("feedback %+v %v", states, err)
	}
	before, _ := s.CurrentState(ctx, []string{"commitment"}, at.Add(-time.Nanosecond))
	if len(before) != 1 || before[0].Status != "active" {
		t.Fatal("as-of projection")
	}
}
func TestValidityBoundaries(t *testing.T) {
	at := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	end := at.Add(time.Hour)
	e := Event{Timestamp: at, ObservedAt: at, ValidFrom: &at, ValidUntil: &end, TTLSeconds: 3600}
	if valid(e, at.Add(-time.Nanosecond)) || !valid(e, at) || valid(e, end) {
		t.Fatal("validity boundaries")
	}
}
func TestImmutableDatabase(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "immutable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, err = s.Ingest(context.Background(), []Event{{ID: "one", Type: "topic.fact", Timestamp: time.Now(), Source: "test", Confidence: 1}})
	if err != nil {
		t.Fatal(err)
	}
	db := s.repo.(*sqliteRepository).db
	for _, q := range []string{"UPDATE events SET payload='{}'", "DELETE FROM events"} {
		if _, err = db.Exec(q); err == nil {
			t.Fatal("mutable events")
		}
	}
}

func TestValidationAndSearch(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "query.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	at := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	original := Event{ID: "c", Type: "commitment.created", Timestamp: at, Source: "test", Entities: []string{"Chen"}, Confidence: .3, StructuredData: map[string]any{"commitment_id": "c"}}
	if _, err = s.Ingest(ctx, []Event{original}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Search(ctx, Query{Person: "Chen"})
	if err != nil || len(rows) != 1 {
		t.Fatal("person entities filter", err)
	}
	for _, e := range []Event{{ID: "bad", Type: "commitment.completed", Timestamp: at.Add(time.Hour), Source: "test", Confidence: 1}, {ID: "bad", Type: "commitment.completed", Timestamp: at, Source: "test", Confidence: 1, StructuredData: map[string]any{"target_id": "c"}}, {ID: "bad", Type: "commitment.cancelled", Timestamp: at.Add(time.Hour), Source: "test", Confidence: 1, StructuredData: map[string]any{"target_id": "unknown"}}} {
		if _, err = s.Ingest(ctx, []Event{e}); err == nil {
			t.Fatal("invalid feedback accepted")
		}
	}
	done := Event{ID: "done", Type: "commitment.completed", Timestamp: at.Add(time.Hour), Source: "test", Confidence: 1, StructuredData: map[string]any{"target_id": "c"}}
	if _, err = s.Ingest(ctx, []Event{done}); err != nil {
		t.Fatal(err)
	}
	if n, err := s.Ingest(ctx, []Event{done}); err != nil || n != 0 {
		t.Fatal("feedback replay", err)
	}
	states, err := s.CurrentState(ctx, nil, at.Add(time.Hour))
	if err != nil || len(states) != 1 || states[0].Confidence != .3 || len(states[0].Provenance) != 2 {
		t.Fatal("feedback confidence/provenance", states, err)
	}
	start, end := at.Add(time.Hour), at
	if _, err = s.Search(ctx, Query{StartTime: &start, EndTime: &end}); err == nil {
		t.Fatal("invalid range")
	}
	if _, err = s.Search(ctx, Query{MinConfidence: 2}); err == nil {
		t.Fatal("invalid threshold")
	}
	if _, err = s.CurrentState(ctx, []string{"invalid"}, at); err == nil {
		t.Fatal("invalid dimension")
	}
}

func TestDeadlinePatchPreservesTerminalStatus(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "deadline.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	at := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	events := []Event{{ID: "created", Type: "commitment.created", Timestamp: at, Source: "test", Confidence: .9, StructuredData: map[string]any{"state_id": "c"}}, {ID: "completed", Type: "commitment.completed", Timestamp: at.Add(time.Hour), Source: "test", Confidence: 1, StructuredData: map[string]any{"target_id": "c"}}, {ID: "deadline", Type: "deadline.changed", Timestamp: at.Add(2 * time.Hour), Source: "test", Confidence: 1, StructuredData: map[string]any{"target_id": "c", "deadline": at.Add(24 * time.Hour).Format(time.RFC3339)}}}
	if _, err = s.Ingest(ctx, events); err != nil {
		t.Fatal(err)
	}
	states, err := s.CurrentState(ctx, nil, at.Add(2*time.Hour))
	if err != nil || len(states) != 1 || states[0].Status != "completed" || len(states[0].Provenance) != 3 {
		t.Fatalf("deadline reopened terminal commitment: %+v %v", states, err)
	}
	if states[0].StructuredData["status"] != "completed" {
		t.Fatal("structured status contradicts projected status")
	}
	bad := events[2]
	bad.ID = "bad-deadline"
	bad.Timestamp = at.Add(3 * time.Hour)
	bad.StructuredData = map[string]any{"target_id": "c", "deadline": at.Add(48 * time.Hour).Format(time.RFC3339), "status": "pending"}
	if _, err := s.Ingest(ctx, []Event{bad}); err == nil {
		t.Fatal("deadline event changed status")
	}
}

func TestStrictLifecycleAndTTL(t *testing.T) {
	at := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	for _, data := range []map[string]any{{"target_id": "c", "status": "pending"}, {"target_id": "c", "dimension": "task"}} {
		if _, err := normalize(Event{ID: "bad", Type: "commitment.completed", Timestamp: at, Source: "test", Confidence: 1, StructuredData: data}); err == nil {
			t.Fatal("contradictory lifecycle accepted")
		}
	}
	if _, err := normalize(Event{ID: "bad", Type: "task.created", Timestamp: at, Source: "test", Confidence: 1, TTLSeconds: int(^uint(0) >> 1)}); err == nil {
		t.Fatal("overflow TTL accepted")
	}
	data := map[string]any{"dimension": "Task"}
	e, err := normalize(Event{ID: "good", Type: "task.created", Timestamp: at, Source: "test", Confidence: 1, StructuredData: data})
	if err != nil || dimension(e) != "task" || data["dimension"] != "Task" {
		t.Fatal("dimension normalization/caller mutation", err)
	}
}
