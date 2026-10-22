package app

import (
	"context"
	"testing"
	"time"

	"recordingmcp/internal/asrinput"
	"recordingmcp/internal/plane"
)

func TestObserveFactAndCorrectRequireCurrentSource(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	events, err := plane.Open(t.TempDir() + "/events.db")
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	ts := &TimeService{Store: f.store, Events: events}
	turnID := f.turnRowID(t, "seg-a", "t-1")
	at := time.Now().UTC().Add(-time.Minute)
	event := plane.Event{ID: "fact-gui", Type: "fact.observed", Timestamp: at, Source: "local-web", Content: "预算讨论发生过", Confidence: .7,
		StructuredData: map[string]any{"dimension": "topic", "status": "completed"}}
	if n, err := ts.ObserveFact(ctx, "profile-a", FactRequest{Event: event, Evidence: []EvidenceReference{{TurnID: turnID, Revision: "rev-1"}}}); err != nil || n != 1 {
		t.Fatalf("observe fact n=%d err=%v", n, err)
	}
	history, err := ts.History(ctx, "profile-a")
	if err != nil || len(history) != 1 {
		t.Fatalf("history=%+v err=%v", history, err)
	}
	stored := history[0]
	if stored.RecordedAt == nil || stored.Source != "local-web" || stored.StructuredData["status"] != "reported" || stored.StructuredData["origin"] != "audio" || stored.StructuredData["scope"] != "profile-a" {
		t.Fatalf("stored fact lost source/knowledge semantics: %+v", stored)
	}
	content := "预算讨论内容已更正"
	request := CorrectionRequest{Correction: plane.Correction{ID: "fact-gui-r1", Scope: "profile-a", TargetID: stored.ID, ExpectedHead: stored.ID, Content: &content, KnownAt: time.Now().UTC(), Source: "local-web"}, Evidence: []EvidenceReference{{TurnID: turnID, Revision: "rev-1"}}}
	if n, err := ts.Correct(ctx, request); err != nil || n != 1 {
		t.Fatalf("correct n=%d err=%v", n, err)
	}
	request.ID = "fact-gui-r2"
	request.ExpectedHead = stored.ID
	if _, err := ts.Correct(ctx, request); err == nil {
		t.Fatal("stale correction head was accepted")
	}
	if _, err := ts.ObserveFact(ctx, "profile-a", FactRequest{Event: plane.Event{ID: "intent", Type: "intention", Timestamp: time.Now(), Source: "local-web", Content: "will do it"}, Evidence: []EvidenceReference{{TurnID: turnID, Revision: "rev-1"}}}); err == nil {
		t.Fatal("intention was accepted as a fact report")
	}
	if _, err := f.svc.CorrectTranscriptSegment(ctx, f.conversationID, f.recordingID, "seg-a", "rev-1", "rev-2", map[string]string{"t-1": "新文本", "t-2": "第二条原始转写"}); err != nil {
		t.Fatal(err)
	}
	states, err := ts.State(ctx, "profile-a", plane.TemporalQuery{ValidAt: time.Now().UTC(), KnownAt: time.Now().UTC().Add(time.Second)})
	if err != nil || len(states) != 0 {
		t.Fatalf("stale source remained in temporal state: %+v err=%v", states, err)
	}
	history, err = ts.History(ctx, "profile-a")
	if err != nil || len(history) != 2 || history[0].SourceStatus != "source_stale" || history[1].SourceStatus != "source_stale" {
		t.Fatalf("history did not annotate stale source: %+v err=%v", history, err)
	}
	if _, err := ts.ObserveFact(ctx, "profile-a", FactRequest{Event: plane.Event{ID: "stale-fact", Type: "fact.observed", Timestamp: at, Source: "local-web", Content: "stale evidence", Confidence: .5, StructuredData: map[string]any{"dimension": "topic"}}, Evidence: []EvidenceReference{{TurnID: turnID, Revision: "rev-1"}}}); err == nil {
		t.Fatal("superseded evidence was accepted")
	}
}

func TestObserveFactRejectsAmbientAndCrossConversationEvidence(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	events, err := plane.Open(t.TempDir() + "/events.db")
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	ts := &TimeService{Store: f.store, Events: events}
	ambientID := f.turnRowID(t, "seg-a", "t-2")
	if _, err := f.store.DB().Exec(`INSERT INTO turn_classes(turn_id,class,subtype,method,created_at) VALUES(?,'ambient','media','derived',1)`, ambientID); err != nil {
		t.Fatal(err)
	}
	req := FactRequest{Event: plane.Event{ID: "ambient-fact", Type: "fact.observed", Timestamp: time.Now(), Source: "local-web", Content: "ambient", Confidence: .5, StructuredData: map[string]any{"dimension": "topic"}}, Evidence: []EvidenceReference{{TurnID: ambientID, Revision: "rev-1"}}}
	if _, err := ts.ObserveFact(ctx, "profile-a", req); err == nil {
		t.Fatal("ambient evidence was accepted")
	}
	other := sampleSegment("seg-other", 0, nil, []asrinput.Turn{{ID: "t-other", OriginalIndex: 1, Text: "other conversation"}})
	other.Source.ConversationID = "other-conversation"
	other.Source.RecordingID = "rec-other"
	other.Source.ProviderRequestID = "req-other"
	report, err := f.store.IngestTranscript(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	var otherTurnID string
	if err := f.store.DB().QueryRow(`SELECT id FROM turns WHERE conversation_id=? AND original_turn_id='seg-other/rev-1/t-other'`, report.ConversationID).Scan(&otherTurnID); err != nil {
		t.Fatal(err)
	}
	req.Event.ID = "cross-conversation"
	req.Evidence = []EvidenceReference{{TurnID: f.turnRowID(t, "seg-a", "t-1"), Revision: "rev-1"}, {TurnID: otherTurnID, Revision: "rev-1"}}
	if _, err := ts.ObserveFact(ctx, "profile-a", req); err == nil {
		t.Fatal("cross-conversation evidence was accepted")
	}
}

func TestPurgeSourcesRedactsConversationAndAmendments(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	events, err := plane.Open(t.TempDir() + "/events.db")
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	ts := &TimeService{Store: f.store, Events: events}
	turnID := f.turnRowID(t, "seg-a", "t-1")
	at := time.Now().UTC()
	_, err = ts.ObserveFact(ctx, "profile-a", FactRequest{Event: plane.Event{ID: "purge-fact", Type: "fact.observed", Timestamp: at, Source: "web", Content: "private fact", Confidence: .6, StructuredData: map[string]any{"dimension": "topic"}}, Evidence: []EvidenceReference{{TurnID: turnID, Revision: "rev-1"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Correct(ctx, CorrectionRequest{Correction: plane.Correction{ID: "purge-correction", Scope: "profile-a", TargetID: "purge-fact", ExpectedHead: "purge-fact", Content: ptr("private amendment"), KnownAt: at.Add(time.Second), Source: "web"}, Evidence: []EvidenceReference{{TurnID: turnID, Revision: "rev-1"}}}); err != nil {
		t.Fatal(err)
	}
	if err := ts.PurgeSources(ctx, "profile-a", []string{f.conversationID}, []string{turnID}); err != nil {
		t.Fatal(err)
	}
	rows, err := events.History(ctx, "profile-a")
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	for _, row := range rows {
		if row.Content != "[purged]" || row.ConversationID != "" || len(row.Actors) != 0 || len(row.Entities) != 0 || len(row.Tags) != 0 || row.Source != "[purged]" {
			t.Fatalf("event content survived source purge: %+v", row)
		}
	}
}

func ptr(s string) *string { return &s }
