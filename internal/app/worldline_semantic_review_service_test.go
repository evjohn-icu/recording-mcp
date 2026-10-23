package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"recordingmcp/internal/memory"
	"recordingmcp/internal/plane"
)

func TestWorldlineReviewAcceptReopenReplayAndCount(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	events, err := plane.Open(t.TempDir() + "/events.db")
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	svc := &WorldlineService{Time: &TimeService{Store: f.store, Events: events}}
	topics, _, _, err := svc.project(ctx, "profile-review")
	if err != nil {
		t.Fatal(err)
	}
	built := buildWorldlineThreadDetails(topics)
	if len(built) == 0 || len(built[0].unknown) == 0 {
		t.Fatal("fixture has no thread entries")
	}
	entry := built[0].unknown[0]
	claim := entry.Claim
	refs, notes, versions := semanticSourcesForClaim(claim)
	if len(refs) == 0 {
		t.Fatal("fixture claim has no evidence")
	}
	subjects := map[string]string{}
	for _, e := range claim.Evidence {
		subjects[e.TurnID+"@"+e.Revision] = e.SubjectID
	}
	now := time.Now().UTC()
	event := plane.Event{ID: "test-semantic-suggestion", Type: "worldline.semantic", Timestamp: now, ObservedAt: now, RecordedAt: &now, Source: "local-web", Content: "semantic relation suggestion", Confidence: .8, StructuredData: map[string]any{"scope": "profile-review", "origin": "worldline", "semantic_status": "review_required", "relation": "unrelated", "model": "test-model", "weights_version": "weights-v1", "input_hash": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "source_snapshot_version": worldlineSourceFingerprint(topics), "candidate_pair_id": "pair-test", "entry_id": entry.ID, "source_claim_ids": []string{claim.ID, claim.ID}, "source_note_ids": notes, "source_note_versions": versions, "references": evidenceStrings(refs), "source_subjects": subjects}}
	if _, err := events.Ingest(ctx, []plane.Event{event}); err != nil {
		t.Fatal(err)
	}
	list, err := svc.Threads(ctx, "profile-review", WorldlineQuery{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if list.Threads[0].SemanticReviewCount != 1 || len(built[0].unknown) == 0 {
		t.Fatalf("initial suggestion is not counted: %+v", list.Threads[0])
	}
	request := WorldlineSemanticReview{ExpectedVersion: list.Version, SuggestionID: event.ID, Decision: "accept", RequestID: "review-accept"}
	version, err := svc.ReviewSemanticRelation(ctx, "profile-review", request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ReviewSemanticRelation(ctx, "profile-review", request); err != nil {
		t.Fatalf("original request replay should succeed after version changes: %v", err)
	}
	accepted, err := svc.Threads(ctx, "profile-review", WorldlineQuery{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if accepted.Threads[0].SemanticReviewCount != 0 {
		t.Fatalf("accepted suggestion remains in review count: %+v", accepted.Threads[0])
	}
	var acceptedSuggestion *WorldlineSemanticSuggestion
	detail, err := svc.Thread(ctx, "profile-review", accepted.Threads[0].ID, WorldlineQuery{Limit: 100}, 0, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range append(detail.Entries, detail.UnknownEntries...) {
		if e.SemanticSuggestion != nil {
			acceptedSuggestion = e.SemanticSuggestion
		}
	}
	if acceptedSuggestion == nil || acceptedSuggestion.RequiresReview || acceptedSuggestion.ReviewStatus != "accept" {
		t.Fatalf("accept was not reflected: %+v", acceptedSuggestion)
	}
	_, err = svc.ReviewSemanticRelation(ctx, "profile-review", WorldlineSemanticReview{ExpectedVersion: version, SuggestionID: event.ID, Decision: "reopen", RequestID: "review-reopen"})
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := svc.Threads(ctx, "profile-review", WorldlineQuery{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Threads[0].SemanticReviewCount != 1 {
		t.Fatalf("reopened suggestion was not counted: %+v", reopened.Threads[0])
	}
	conflict := request
	conflict.Decision = "reject"
	if _, err := svc.ReviewSemanticRelation(ctx, "profile-review", conflict); err == nil {
		t.Fatal("same request_id with a different decision must conflict")
	}
	if _, err := f.store.DB().Exec(`UPDATE notes SET summary=summary || ' 修订'`); err != nil {
		t.Fatal(err)
	}
	current, _, currentVersion, err := svc.project(ctx, "profile-review")
	if err != nil {
		t.Fatal(err)
	}
	_ = current
	if _, err := svc.ReviewSemanticRelation(ctx, "profile-review", WorldlineSemanticReview{ExpectedVersion: currentVersion, SuggestionID: event.ID, Decision: "reject", RequestID: "review-stale-note"}); err == nil || !strings.Contains(err.Error(), "source version is stale") {
		t.Fatalf("stale source note review err=%v", err)
	}
}

func TestWorldlineSemanticReviewRejectsStaleSpeakerSnapshot(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	events, err := plane.Open(t.TempDir() + "/events.db")
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	svc := &WorldlineService{Time: &TimeService{Store: f.store, Events: events}}
	topics, _, version, err := svc.project(ctx, "profile-review-identity")
	if err != nil {
		t.Fatal(err)
	}
	built := buildWorldlineThreadDetails(topics)
	entry := built[0].unknown[0]
	refs, notes, versions := semanticSourcesForClaim(entry.Claim)
	wrong := map[string]string{}
	for _, ref := range refs {
		wrong[ref.TurnID+"@"+ref.Revision] = "stale-speaker"
	}
	now := time.Now().UTC()
	ev := plane.Event{ID: "stale-identity-suggestion", Type: "worldline.semantic", Timestamp: now, ObservedAt: now, Source: "local-web", Content: "suggestion", Confidence: .5, StructuredData: map[string]any{"scope": "profile-review-identity", "references": evidenceStrings(refs), "source_note_ids": notes, "source_note_versions": versions, "source_subjects": wrong, "model": "model", "weights_version": "weights", "source_snapshot_version": worldlineSourceFingerprint(topics)}}
	if _, err := events.Ingest(ctx, []plane.Event{ev}); err != nil {
		t.Fatal(err)
	}
	_, _, version, err = svc.project(ctx, "profile-review-identity")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ReviewSemanticRelation(ctx, "profile-review-identity", WorldlineSemanticReview{ExpectedVersion: version, SuggestionID: ev.ID, Decision: "accept", RequestID: "review-stale-identity"}); err == nil || !strings.Contains(err.Error(), "speaker identity snapshot is stale") {
		t.Fatalf("stale identity review err=%v", err)
	}
}

func TestWorldlineSemanticReviewRejectsSupersededSuggestion(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	events, err := plane.Open(t.TempDir() + "/events.db")
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	svc := &WorldlineService{Time: &TimeService{Store: f.store, Events: events}}
	topics, _, _, err := svc.project(ctx, "profile-review-latest")
	if err != nil {
		t.Fatal(err)
	}
	built := buildWorldlineThreadDetails(topics)
	entry := built[0].unknown[0]
	refs, notes, versions := semanticSourcesForClaim(entry.Claim)
	subjects := map[string]string{}
	for _, evidence := range entry.Claim.Evidence {
		subjects[evidence.TurnID+"@"+evidence.Revision] = evidence.SubjectID
	}
	now := time.Now().UTC()
	makeSuggestion := func(id string, at time.Time) plane.Event {
		return plane.Event{ID: id, Type: "worldline.semantic", Timestamp: at, ObservedAt: at, RecordedAt: &at, Source: "local-web", Content: "semantic relation suggestion", Confidence: .8, StructuredData: map[string]any{"scope": "profile-review-latest", "origin": "worldline", "semantic_status": "review_required", "relation": "unrelated", "model": "test-model", "weights_version": "weights-v1", "source_snapshot_version": worldlineSourceFingerprint(topics), "candidate_pair_id": id, "entry_id": entry.ID, "source_note_ids": notes, "source_note_versions": versions, "references": evidenceStrings(refs), "source_subjects": subjects}}
	}
	old := makeSuggestion("suggestion-old", now)
	latest := makeSuggestion("suggestion-latest", now.Add(time.Second))
	if _, err := events.Ingest(ctx, []plane.Event{old, latest}); err != nil {
		t.Fatal(err)
	}
	_, _, version, err := svc.project(ctx, "profile-review-latest")
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.ReviewSemanticRelation(ctx, "profile-review-latest", WorldlineSemanticReview{ExpectedVersion: version, SuggestionID: old.ID, Decision: "accept", RequestID: "review-old-suggestion"})
	if err == nil || !strings.Contains(err.Error(), "superseded") {
		t.Fatalf("superseded suggestion should be rejected, got %v", err)
	}
	version, err = svc.ReviewSemanticRelation(ctx, "profile-review-latest", WorldlineSemanticReview{ExpectedVersion: version, SuggestionID: latest.ID, Decision: "accept", RequestID: "review-latest-suggestion"})
	if err != nil {
		t.Fatalf("current suggestion should remain reviewable: %v", err)
	}
	detail, err := svc.Thread(ctx, "profile-review-latest", built[0].thread.ID, WorldlineQuery{Limit: 100, Status: "all"}, 0, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, current := range append(detail.Entries, detail.UnknownEntries...) {
		if current.ID == entry.ID && (current.SemanticSuggestion == nil || current.SemanticSuggestion.SuggestionID != latest.ID || current.SemanticSuggestion.ReviewStatus != "accept") {
			t.Fatalf("review did not apply to latest suggestion (version %s): %+v", version, current.SemanticSuggestion)
		}
	}
}

func TestWorldlineSemanticReviewRejectAndInsufficientStaySeparateFromRelation(t *testing.T) {
	for _, decision := range []string{"reject", "insufficient"} {
		t.Run(decision, func(t *testing.T) {
			ctx := context.Background()
			f := newFixture(t)
			events, err := plane.Open(t.TempDir() + "/events.db")
			if err != nil {
				t.Fatal(err)
			}
			defer events.Close()
			svc := &WorldlineService{Time: &TimeService{Store: f.store, Events: events}}
			topics, _, _, err := svc.project(ctx, "profile-review-decision-"+decision)
			if err != nil {
				t.Fatal(err)
			}
			entry := buildWorldlineThreadDetails(topics)[0].unknown[0]
			refs, notes, versions := semanticSourcesForClaim(entry.Claim)
			subjects := map[string]string{}
			for _, evidence := range entry.Claim.Evidence {
				subjects[evidence.TurnID+"@"+evidence.Revision] = evidence.SubjectID
			}
			now := time.Now().UTC()
			id := "suggestion-" + decision
			event := plane.Event{ID: id, Type: "worldline.semantic", Timestamp: now, ObservedAt: now, RecordedAt: &now, Source: "local-web", Content: "suggestion", Confidence: .8, StructuredData: map[string]any{"scope": "profile-review-decision-" + decision, "origin": "worldline", "semantic_status": "review_required", "relation": "unrelated", "model": "test-model", "weights_version": "weights-v1", "source_snapshot_version": worldlineSourceFingerprint(topics), "entry_id": entry.ID, "source_note_ids": notes, "source_note_versions": versions, "references": evidenceStrings(refs), "source_subjects": subjects}}
			if _, err := events.Ingest(ctx, []plane.Event{event}); err != nil {
				t.Fatal(err)
			}
			_, _, version, err := svc.project(ctx, "profile-review-decision-"+decision)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := svc.ReviewSemanticRelation(ctx, "profile-review-decision-"+decision, WorldlineSemanticReview{ExpectedVersion: version, SuggestionID: id, Decision: decision, RequestID: "review-" + decision}); err != nil {
				t.Fatal(err)
			}
			detail, err := svc.Thread(ctx, "profile-review-decision-"+decision, buildWorldlineThreadDetails(topics)[0].thread.ID, WorldlineQuery{Limit: 100, Status: "all"}, 0, 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, current := range append(detail.Entries, detail.UnknownEntries...) {
				if current.ID == entry.ID {
					found = current.SemanticSuggestion != nil && current.SemanticSuggestion.Relation == "unrelated" && current.SemanticSuggestion.ReviewStatus == decision && !current.SemanticSuggestion.RequiresReview
				}
			}
			if !found {
				t.Fatalf("review %q changed relation or was not recorded separately", decision)
			}
			rows, err := events.Search(ctx, plane.Query{})
			if err != nil {
				t.Fatal(err)
			}
			for _, row := range rows {
				if row.Type == "fact.observed" || row.Type == "topic.fact" {
					t.Fatalf("semantic review promoted a fact: %+v", row)
				}
			}
		})
	}
}

func TestWorldlineSemanticReviewAndSuggestionArePurgedWhenSourceIsForgotten(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	events, err := plane.Open(t.TempDir() + "/events.db")
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	f.svc.ForgetPlane = events
	svc := &WorldlineService{Time: &TimeService{Store: f.store, Events: events}}
	scope := "profile-review-forget"
	topics, _, _, err := svc.project(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	entry := buildWorldlineThreadDetails(topics)[0].unknown[0]
	refs, notes, versions := semanticSourcesForClaim(entry.Claim)
	subjects := map[string]string{}
	for _, evidence := range entry.Claim.Evidence {
		subjects[evidence.TurnID+"@"+evidence.Revision] = evidence.SubjectID
	}
	now := time.Now().UTC()
	suggestion := plane.Event{ID: "suggestion-to-forget", Type: "worldline.semantic", Timestamp: now, ObservedAt: now, RecordedAt: &now, Source: "local-web", Content: "suggestion", Confidence: .8, StructuredData: map[string]any{"scope": scope, "origin": "worldline", "semantic_status": "review_required", "relation": "unrelated", "model": "test-model", "weights_version": "weights-v1", "source_snapshot_version": worldlineSourceFingerprint(topics), "entry_id": entry.ID, "source_note_ids": notes, "source_note_versions": versions, "references": evidenceStrings(refs), "source_subjects": subjects}}
	if _, err := events.Ingest(ctx, []plane.Event{suggestion}); err != nil {
		t.Fatal(err)
	}
	_, _, version, err := svc.project(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ReviewSemanticRelation(ctx, scope, WorldlineSemanticReview{ExpectedVersion: version, SuggestionID: suggestion.ID, Decision: "accept", RequestID: "review-to-forget"}); err != nil {
		t.Fatal(err)
	}
	turnID := refs[0].TurnID
	preview, err := f.svc.PreviewForget(ctx, memory.ForgetRequest{Scope: "turn", TurnID: turnID})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.CommitForget(ctx, memory.ForgetRequest{Scope: "turn", TurnID: turnID, ExpectedState: preview.ExpectedState})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := events.Search(ctx, plane.Query{})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if (row.ID == suggestion.ID || row.Type == "worldline.semantic_review") && row.StructuredData["purged"] != true {
			t.Fatalf("forgotten source left semantic payload behind: %+v", row)
		}
	}
}
