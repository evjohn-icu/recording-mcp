package plane

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestIndependentTemporalAxesAndScopedCorrection(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "temporal.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	valid := time.Now().UTC().Add(-2 * time.Hour)
	known := valid.Add(time.Hour)
	original := Event{ID: "fact-1", Type: "topic.fact", Timestamp: valid, ObservedAt: known, Source: "human", Content: "old", Confidence: .8, StructuredData: map[string]any{"dimension": "topic", "state_id": "fact-1", "scope": "profile-a"}}
	if _, err = s.Ingest(ctx, []Event{original}); err != nil {
		t.Fatal(err)
	}
	if original.RecordedAt != nil {
		t.Fatal("ingest must not mutate the caller's event")
	}
	historyBefore, err := s.History(ctx, "profile-a")
	if err != nil || len(historyBefore) != 1 || historyBefore[0].RecordedAt == nil {
		t.Fatalf("server receipt time missing from persisted event: %+v %v", historyBefore, err)
	}
	if historyBefore[0].RecordedAt.Equal(known) {
		t.Fatal("source observation time was used as server receipt time")
	}
	before, err := s.TemporalState(ctx, TemporalQuery{Scope: "profile-a", ValidAt: valid, KnownAt: historyBefore[0].RecordedAt.Add(-time.Nanosecond)})
	if err != nil || len(before) != 0 {
		t.Fatalf("future knowledge leaked: %v %v", before, err)
	}
	atKnown, err := s.TemporalState(ctx, TemporalQuery{Scope: "profile-a", ValidAt: valid, KnownAt: historyBefore[0].RecordedAt.Add(time.Hour)})
	if err != nil || len(atKnown) != 1 {
		t.Fatalf("fact not visible: %v %v", atKnown, err)
	}
	newText := "corrected"
	correction := Correction{ID: "corr-1", Scope: "profile-a", TargetID: "fact-1", ExpectedHead: "fact-1", Content: &newText, KnownAt: known.Add(time.Hour), Source: "human"}
	if _, err = s.Correct(ctx, correction); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Correct(ctx, Correction{ID: "corr-2", Scope: "profile-a", TargetID: "fact-1", ExpectedHead: "fact-1", KnownAt: known.Add(2 * time.Hour), Source: "human"}); err == nil {
		t.Fatal("stale optimistic head accepted")
	}
	state, err := s.TemporalState(ctx, TemporalQuery{Scope: "profile-a", ValidAt: valid.Add(time.Hour), KnownAt: known.Add(2 * time.Hour)})
	if err != nil || len(state) != 1 || state[0].Summary != "corrected" || len(state[0].Provenance) != 2 {
		t.Fatalf("correction projection: %+v %v", state, err)
	}
	other, err := s.TemporalState(ctx, TemporalQuery{Scope: "profile-b", ValidAt: known.Add(time.Hour), KnownAt: known.Add(time.Hour)})
	if err != nil || len(other) != 0 {
		t.Fatalf("cross-profile leak: %+v %v", other, err)
	}
	history, err := s.History(ctx, "profile-a")
	if err != nil || len(history) != 2 {
		t.Fatalf("history %+v %v", history, err)
	}
}

func TestLegacyEventWithoutRecordedAtIsUnknownForTemporalState(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "legacy-known.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	at := time.Now().UTC().Add(-time.Hour)
	known := time.Now().UTC().Add(-time.Minute)
	e := Event{ID: "legacy", Type: "topic.fact", Timestamp: at, ObservedAt: known, KnownAt: &known, Source: "legacy", Content: "old import", Confidence: 1, StructuredData: map[string]any{"dimension": "topic", "state_id": "legacy", "scope": "p"}}
	if _, err := s.repo.Append(ctx, []Event{e}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.TemporalState(ctx, TemporalQuery{Scope: "p", ValidAt: time.Now().UTC(), KnownAt: time.Now().UTC()})
	if err != nil || len(rows) != 0 {
		t.Fatalf("unknown legacy knowledge time projected as known: %+v %v", rows, err)
	}
	history, err := s.History(ctx, "p")
	if err != nil || len(history) != 1 || history[0].RecordedAt != nil {
		t.Fatalf("legacy history should retain clearly unknown recorded_at: %+v %v", history, err)
	}
}

func TestRetractionAndPurgeAreExplicit(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "purge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	at := time.Now().UTC()
	e := Event{ID: "fact", Type: "topic.fact", Timestamp: at, ObservedAt: at, Source: "test", Content: "secret", Confidence: 1, StructuredData: map[string]any{"dimension": "topic", "state_id": "fact", "scope": "p"}}
	if _, err = s.Ingest(ctx, []Event{e}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Correct(ctx, Correction{ID: "ret", Scope: "p", TargetID: "fact", ExpectedHead: "fact", Retract: true, KnownAt: at.Add(time.Second), Source: "human"}); err != nil {
		t.Fatal(err)
	}
	if err = s.PurgeContent(ctx, []string{"fact"}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.History(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range rows {
		if row.ID == "fact" {
			found = row.Content == "[purged]"
		}
	}
	if !found {
		t.Fatalf("purged history %+v", rows)
	}
	if _, err = s.repo.(*sqliteRepository).db.Exec("UPDATE events SET payload='{}'"); err == nil {
		t.Fatal("ordinary mutation re-enabled")
	}
}
