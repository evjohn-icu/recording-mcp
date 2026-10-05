package memory

import (
	"context"
	"database/sql"
	"testing"

	"recordingmcp/internal/asrinput"
)

// turnPerson returns the SpeakerPersonID of one turn item in a page, and
// whether the turn was present at all.
func turnPerson(page Page, turnID string) (bool, *string) {
	for _, item := range page.Items {
		if item.Kind == "turn" && item.ID == turnID {
			return true, item.SpeakerPersonID
		}
	}
	return false, nil
}

// searchHitPerson returns the SpeakerPersonID of one search hit, and whether
// the hit was present at all.
func searchHitPerson(page SearchPage, id string) (bool, *string) {
	for _, hit := range page.Hits {
		if hit.ID == id {
			return true, hit.SpeakerPersonID
		}
	}
	return false, nil
}

// assertTurnEntityNull pins that retrieval resolves identity as a projection
// and never writes the legacy turns.speaker_entity_id column.
func assertTurnEntityNull(t *testing.T, s *Store, turnID string) {
	t.Helper()
	var entity sql.NullString
	if err := s.db.QueryRow(`SELECT speaker_entity_id FROM turns WHERE id=?`, turnID).Scan(&entity); err != nil {
		t.Fatal(err)
	}
	if entity.Valid {
		t.Fatalf("turn %s speaker_entity_id = %q, want NULL", turnID, entity.String)
	}
}

// TestRetrievalResolvesConfirmedSpeakerIdentity seeds two revisions of one
// segment (rev-1 superseded by rev-2), confirms the shared diarization label
// as a different person in each revision, and pins that Query/GetItem project
// the identity of each turn's own revision: the stale rev-1 turn keeps alice
// while the current rev-2 turn resolves bob.
func TestRetrievalResolvesConfirmedSpeakerIdentity(t *testing.T) {
	s := openTempStore(t)
	ctx := context.Background()

	first, err := s.IngestTranscript(ctx, sampleResult("seg-a", "rev-1", 0))
	if err != nil {
		t.Fatal(err)
	}
	v2 := sampleResult("seg-a", "rev-2", 0)
	v2.Source.SupersedesRevision = "rev-1"
	if _, err := s.IngestTranscript(ctx, v2); err != nil {
		t.Fatal(err)
	}

	recordingID := first.RecordingID
	v1TurnID := stableKey("turn", recordingID, "seg-a/rev-1/utt-1")
	v2TurnID := stableKey("turn", recordingID, "seg-a/rev-2/utt-1")

	label := "spk:device-1:1"
	v1Binding, err := s.ProposeSpeakerBinding(ctx, recordingID, label, "rev-1", []string{v1TurnID}, "model-x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConfirmSpeakerBinding(ctx, v1Binding.ID, "person:alice", "reviewer-a"); err != nil {
		t.Fatal(err)
	}
	v2Binding, err := s.ProposeSpeakerBinding(ctx, recordingID, label, "rev-2", []string{v2TurnID}, "model-x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConfirmSpeakerBinding(ctx, v2Binding.ID, "person:bob", "reviewer-a"); err != nil {
		t.Fatal(err)
	}

	// Audit page: both revisions, each turn resolves against its own revision.
	audit, err := s.Query(ctx, Query{RecordingID: recordingID, IncludeTurns: true, IncludeStale: true, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if found, person := turnPerson(audit, v1TurnID); !found || person == nil || *person != "person:alice" {
		t.Fatalf("rev-1 turn identity = %v (found=%v), want person:alice", person, found)
	}
	if found, person := turnPerson(audit, v2TurnID); !found || person == nil || *person != "person:bob" {
		t.Fatalf("rev-2 turn identity = %v (found=%v), want person:bob", person, found)
	}

	// Current page: the superseded rev-1 turn is gone, rev-2 still resolves bob.
	current, err := s.Query(ctx, Query{RecordingID: recordingID, IncludeTurns: true, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if found, _ := turnPerson(current, v1TurnID); found {
		t.Fatalf("superseded turn %s appeared in the current page", v1TurnID)
	}
	if found, person := turnPerson(current, v2TurnID); !found || person == nil || *person != "person:bob" {
		t.Fatalf("current rev-2 turn identity = %v (found=%v), want person:bob", person, found)
	}

	// GetItem resolves the same way, stale path included.
	v1Item, err := s.GetItem(ctx, v1TurnID, true)
	if err != nil {
		t.Fatal(err)
	}
	if v1Item.Item.SpeakerPersonID == nil || *v1Item.Item.SpeakerPersonID != "person:alice" {
		t.Fatalf("GetItem rev-1 identity = %v, want person:alice", v1Item.Item.SpeakerPersonID)
	}
	if len(v1Item.Item.DerivedFrom) != 1 || v1Item.Item.DerivedFrom[0] != "rev-1" {
		t.Fatalf("GetItem rev-1 derived_from = %v, want [rev-1]", v1Item.Item.DerivedFrom)
	}
	v2Item, err := s.GetItem(ctx, v2TurnID, true)
	if err != nil {
		t.Fatal(err)
	}
	if v2Item.Item.SpeakerPersonID == nil || *v2Item.Item.SpeakerPersonID != "person:bob" {
		t.Fatalf("GetItem rev-2 identity = %v, want person:bob", v2Item.Item.SpeakerPersonID)
	}

	assertTurnEntityNull(t, s, v1TurnID)
	assertTurnEntityNull(t, s, v2TurnID)
}

// TestRetrievalLeavesUnconfirmedSpeakerUnresolved pins that only confirmed
// bindings project: an unbound label and labels whose binding is proposed,
// rejected or revoked all leave SpeakerPersonID nil, and turns.speaker_entity_id
// stays NULL.
func TestRetrievalLeavesUnconfirmedSpeakerUnresolved(t *testing.T) {
	s := openTempStore(t)
	ctx := context.Background()

	labels := map[string]string{
		"utt-1": "spk:unbound",
		"utt-2": "spk:proposed",
		"utt-3": "spk:rejected",
		"utt-4": "spk:revoked",
	}
	result := sampleResult("seg-a", "rev-1", 0)
	result.Turns = nil
	for i, id := range []string{"utt-1", "utt-2", "utt-3", "utt-4"} {
		label := labels[id]
		start, end := int64(10+i*100), int64(100+i*100)
		result.Turns = append(result.Turns, asrinput.Turn{
			ID: id, OriginalIndex: i + 1, Text: "hello " + id,
			LocalStartMS: &start, LocalEndMS: &end, GlobalStartMS: &start, GlobalEndMS: &end,
			SpeakerScopedID: &label,
		})
	}
	report, err := s.IngestTranscript(ctx, result)
	if err != nil {
		t.Fatal(err)
	}
	recordingID := report.RecordingID
	turnID := func(id string) string { return stableKey("turn", recordingID, "seg-a/rev-1/"+id) }

	proposed, err := s.ProposeSpeakerBinding(ctx, recordingID, labels["utt-2"], "rev-1", []string{turnID("utt-2")}, "model-x")
	if err != nil {
		t.Fatal(err)
	}
	if proposed.Status != speakerBindingProposed {
		t.Fatalf("utt-2 binding status = %q, want proposed", proposed.Status)
	}

	rejected, err := s.ProposeSpeakerBinding(ctx, recordingID, labels["utt-3"], "rev-1", []string{turnID("utt-3")}, "model-x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RejectSpeakerBinding(ctx, rejected.ID, "reviewer-b", "wrong speaker"); err != nil {
		t.Fatal(err)
	}

	revoked, err := s.ProposeSpeakerBinding(ctx, recordingID, labels["utt-4"], "rev-1", []string{turnID("utt-4")}, "model-x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConfirmSpeakerBinding(ctx, revoked.ID, "person:carol", "reviewer-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RevokeSpeakerBinding(ctx, revoked.ID, "reviewer-c", "misidentified"); err != nil {
		t.Fatal(err)
	}

	page, err := s.Query(ctx, Query{RecordingID: recordingID, IncludeTurns: true, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"utt-1", "utt-2", "utt-3", "utt-4"} {
		tid := turnID(id)
		if found, person := turnPerson(page, tid); !found || person != nil {
			t.Fatalf("turn %s (%s) identity = %v (found=%v), want nil", id, labels[id], person, found)
		}
		item, err := s.GetItem(ctx, tid, true)
		if err != nil {
			t.Fatal(err)
		}
		if item.Item.SpeakerPersonID != nil {
			t.Fatalf("GetItem turn %s (%s) identity = %v, want nil", id, labels[id], item.Item.SpeakerPersonID)
		}
		assertTurnEntityNull(t, s, tid)
	}
}

// TestRetrievalLegacyTurnWithoutRevisionHasNoIdentity pins that a turn with no
// attachment rows has no transcript revision, so identity is never projected
// even when a confirmed binding exists for the recording.
func TestRetrievalLegacyTurnWithoutRevisionHasNoIdentity(t *testing.T) {
	s := openTempStore(t)
	ctx := context.Background()

	report, err := s.IngestTranscript(ctx, sampleResult("seg-a", "rev-1", 0))
	if err != nil {
		t.Fatal(err)
	}
	recordingID := report.RecordingID
	label := "spk:device-1:1"
	ingestedTurnID := stableKey("turn", recordingID, "seg-a/rev-1/utt-1")
	binding, err := s.ProposeSpeakerBinding(ctx, recordingID, label, "rev-1", []string{ingestedTurnID}, "model-x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConfirmSpeakerBinding(ctx, binding.ID, "person:alice", "reviewer-a"); err != nil {
		t.Fatal(err)
	}

	// A legacy turn row with a speaker label but no attachment_turns link.
	legacyID := stableKey("turn", recordingID, "legacy/utt-9")
	if _, err := s.db.ExecContext(ctx, `INSERT INTO turns(id,conversation_id,recording_id,segment_index,global_start_ms,global_end_ms,local_start_ms,local_end_ms,speaker_scoped_id,speaker_entity_id,content,definite,source_sha256,transcript_version,original_turn_id,turn_json) VALUES(?,?,?,?,?,?,?,?,?,NULL,?,?,?,?,?,?)`,
		legacyID, report.ConversationID, recordingID, 9, 5000, 5900, 5000, 5900, label, "legacy hello", nil, "sha", "legacy-v1", "legacy/utt-9", "{}"); err != nil {
		t.Fatal(err)
	}

	item, err := s.GetItem(ctx, legacyID, true)
	if err != nil {
		t.Fatal(err)
	}
	if item.Item.SpeakerPersonID != nil {
		t.Fatalf("legacy turn identity = %v, want nil", item.Item.SpeakerPersonID)
	}
	if len(item.Item.DerivedFrom) != 0 {
		t.Fatalf("legacy turn derived_from = %v, want empty", item.Item.DerivedFrom)
	}
	assertTurnEntityNull(t, s, legacyID)
}

// TestSearchHitsCarryConfirmedSpeakerIdentity seeds two revisions of one
// segment (rev-1 superseded by rev-2), confirms the shared diarization label as
// a different person in each revision, and pins that SearchMemory projects the
// identity of each turn's own revision exactly like Query/GetItem: the stale
// rev-1 turn keeps alice while the current rev-2 turn resolves bob. A turn with
// an unbound label in the current revision stays nil, and the default
// (current-only) search never surfaces the superseded rev-1 hit.
func TestSearchHitsCarryConfirmedSpeakerIdentity(t *testing.T) {
	s := openTempStore(t)
	ctx := context.Background()

	first, err := s.IngestTranscript(ctx, sampleResult("seg-a", "rev-1", 0))
	if err != nil {
		t.Fatal(err)
	}
	v2 := sampleResult("seg-a", "rev-2", 0)
	v2.Source.SupersedesRevision = "rev-1"
	unbound := "spk:unbound"
	unboundStart, unboundEnd := int64(900), int64(1000)
	v2.Turns = append(v2.Turns, asrinput.Turn{
		ID: "utt-2", OriginalIndex: 2, Text: "hello seg-a unbound",
		LocalStartMS: &unboundStart, LocalEndMS: &unboundEnd,
		GlobalStartMS: &unboundStart, GlobalEndMS: &unboundEnd,
		SpeakerScopedID: &unbound,
	})
	if _, err := s.IngestTranscript(ctx, v2); err != nil {
		t.Fatal(err)
	}

	recordingID := first.RecordingID
	v1TurnID := stableKey("turn", recordingID, "seg-a/rev-1/utt-1")
	v2TurnID := stableKey("turn", recordingID, "seg-a/rev-2/utt-1")
	v2UnboundID := stableKey("turn", recordingID, "seg-a/rev-2/utt-2")

	label := "spk:device-1:1"
	v1Binding, err := s.ProposeSpeakerBinding(ctx, recordingID, label, "rev-1", []string{v1TurnID}, "model-x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConfirmSpeakerBinding(ctx, v1Binding.ID, "person:alice", "reviewer-a"); err != nil {
		t.Fatal(err)
	}
	v2Binding, err := s.ProposeSpeakerBinding(ctx, recordingID, label, "rev-2", []string{v2TurnID}, "model-x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConfirmSpeakerBinding(ctx, v2Binding.ID, "person:bob", "reviewer-a"); err != nil {
		t.Fatal(err)
	}

	// Audit search: both revisions, each turn resolves against its own revision.
	audit, err := s.SearchMemory(ctx, MemorySearch{Text: "hello seg-a", Scope: "raw", IncludeStale: true, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if found, person := searchHitPerson(audit, v1TurnID); !found || person == nil || *person != "person:alice" {
		t.Fatalf("rev-1 hit identity = %v (found=%v), want person:alice", person, found)
	}
	if found, person := searchHitPerson(audit, v2TurnID); !found || person == nil || *person != "person:bob" {
		t.Fatalf("rev-2 hit identity = %v (found=%v), want person:bob", person, found)
	}
	if found, person := searchHitPerson(audit, v2UnboundID); !found || person != nil {
		t.Fatalf("unbound hit identity = %v (found=%v), want nil", person, found)
	}

	// Current search: the superseded rev-1 hit is gone, rev-2 still resolves bob.
	current, err := s.SearchMemory(ctx, MemorySearch{Text: "hello seg-a", Scope: "raw", Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if found, _ := searchHitPerson(current, v1TurnID); found {
		t.Fatalf("superseded hit %s appeared in the current search", v1TurnID)
	}
	if found, person := searchHitPerson(current, v2TurnID); !found || person == nil || *person != "person:bob" {
		t.Fatalf("current rev-2 hit identity = %v (found=%v), want person:bob", person, found)
	}
	if found, person := searchHitPerson(current, v2UnboundID); !found || person != nil {
		t.Fatalf("current unbound hit identity = %v (found=%v), want nil", person, found)
	}

	assertTurnEntityNull(t, s, v1TurnID)
	assertTurnEntityNull(t, s, v2TurnID)
}
