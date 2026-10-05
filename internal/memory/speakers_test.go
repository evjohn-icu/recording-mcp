package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// speakerEvent is one decoded speaker_binding_events row, ordered by
// created_at then id.
type speakerEvent struct {
	Action string
	Actor  string
	Note   string
}

// seedRecording creates the conversation and recording a speaker binding can
// reference. Bindings are scoped to a recording id, so the FK to recordings
// must resolve.
func seedRecording(t *testing.T, s *Store, conversationID, recordingID string) {
	t.Helper()
	mustExec(t, s, `INSERT INTO conversations(id,title,summary,annotation_version,annotation_basis,recording_id,source_sha256,transcript_version,duration_ms,transcript_json,annotations_json)
		VALUES(?,?,'','v1','',?, 'sha','v1',1000,'{}','{}')`, conversationID, "title", recordingID)
	mustExec(t, s, `INSERT INTO recordings(id,conversation_id,source_file,source_sha256,duration_ms,recording_started_at,absolute_time_known)
		VALUES(?,?,'file','sha',1000,NULL,0)`, recordingID, conversationID)
}

func speakerBindingEvents(t *testing.T, s *Store, bindingID string) []speakerEvent {
	t.Helper()
	rows, err := s.db.Query(`SELECT action, actor, note FROM speaker_binding_events WHERE binding_id=? ORDER BY created_at, id`, bindingID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []speakerEvent
	for rows.Next() {
		var e speakerEvent
		if err := rows.Scan(&e.Action, &e.Actor, &e.Note); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func openSpeakerStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "speakers.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// loadSpeakerBinding reads one binding row straight from the store handle so
// tests can assert that review history is immutable.
func loadSpeakerBinding(t *testing.T, s *Store, id string) SpeakerBinding {
	t.Helper()
	var (
		b        SpeakerBinding
		person   sql.NullString
		evidence string
	)
	if err := s.db.QueryRow(`SELECT id, recording_id, transcript_revision, speaker_scoped_id, person_id, status, evidence_json, reviewer, version, created_at, updated_at
		FROM speaker_bindings WHERE id=?`, id).Scan(&b.ID, &b.RecordingID, &b.TranscriptRevision, &b.SpeakerScopedID, &person, &b.Status,
		&evidence, &b.Reviewer, &b.Version, &b.CreatedAt, &b.UpdatedAt); err != nil {
		t.Fatal(err)
	}
	if person.Valid {
		b.PersonID = person.String
	}
	if err := json.Unmarshal([]byte(evidence), &b.EvidenceTurnIDs); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSpeakerBindingLifecycle(t *testing.T) {
	ctx := context.Background()
	s := openSpeakerStore(t)
	seedRecording(t, s, "conv-1", "rec-1")

	b, err := s.ProposeSpeakerBinding(ctx, "rec-1", "SPK_00", "rev-1", []string{"t1", "t2"}, "model-x")
	if err != nil {
		t.Fatal(err)
	}
	if b.Status != "proposed" || b.Version != 1 || b.PersonID != "" || b.TranscriptRevision != "rev-1" {
		t.Fatalf("bad proposal: %+v", b)
	}
	if b.ID != speakerBindingID("rec-1", "SPK_00", 1) || !strings.HasPrefix(b.ID, "sb:") {
		t.Fatalf("unexpected deterministic id %q", b.ID)
	}
	if len(b.EvidenceTurnIDs) != 2 || b.EvidenceTurnIDs[0] != "t1" || b.EvidenceTurnIDs[1] != "t2" {
		t.Fatalf("evidence not stored: %+v", b.EvidenceTurnIDs)
	}
	if b.CreatedAt == 0 || b.UpdatedAt == 0 {
		t.Fatalf("timestamps not set: %+v", b)
	}

	// A proposed binding is not an accepted identity yet.
	proj, err := s.SpeakerProjection(ctx, "rec-1", "rev-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(proj) != 0 {
		t.Fatalf("proposed binding leaked into projection: %v", proj)
	}

	confirmed, err := s.ConfirmSpeakerBinding(ctx, b.ID, "person:alice", "reviewer-a")
	if err != nil {
		t.Fatal(err)
	}
	// Version is the stable row ordinal; review never rewrites it.
	if confirmed.Status != "confirmed" || confirmed.PersonID != "person:alice" || confirmed.Reviewer != "reviewer-a" || confirmed.Version != 1 {
		t.Fatalf("bad confirmation: %+v", confirmed)
	}

	proj, err = s.SpeakerProjection(ctx, "rec-1", "rev-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(proj) != 1 || proj["SPK_00"] != "person:alice" {
		t.Fatalf("projection = %v, want SPK_00 -> person:alice", proj)
	}

	events := speakerBindingEvents(t, s, b.ID)
	want := []speakerEvent{{"propose", "model-x", ""}, {"confirm", "reviewer-a", ""}}
	if len(events) != len(want) {
		t.Fatalf("events = %+v, want %+v", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("event %d = %+v, want %+v", i, events[i], want[i])
		}
	}
}

func TestSpeakerBindingRejectKeepsProjectionEmpty(t *testing.T) {
	ctx := context.Background()
	s := openSpeakerStore(t)
	seedRecording(t, s, "conv-1", "rec-1")

	b, err := s.ProposeSpeakerBinding(ctx, "rec-1", "SPK_01", "rev-1", []string{"t1"}, "model-x")
	if err != nil {
		t.Fatal(err)
	}
	rejected, err := s.RejectSpeakerBinding(ctx, b.ID, "reviewer-b", "wrong speaker")
	if err != nil {
		t.Fatal(err)
	}
	if rejected.Status != "rejected" || rejected.PersonID != "" || rejected.Version != 1 || rejected.Reviewer != "reviewer-b" {
		t.Fatalf("bad rejection: %+v", rejected)
	}

	proj, err := s.SpeakerProjection(ctx, "rec-1", "rev-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(proj) != 0 {
		t.Fatalf("rejected binding leaked into projection: %v", proj)
	}

	events := speakerBindingEvents(t, s, b.ID)
	want := []speakerEvent{{"propose", "model-x", ""}, {"reject", "reviewer-b", "wrong speaker"}}
	if len(events) != len(want) {
		t.Fatalf("events = %+v, want %+v", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("event %d = %+v, want %+v", i, events[i], want[i])
		}
	}
}

func TestSpeakerBindingRevokeClearsProjection(t *testing.T) {
	ctx := context.Background()
	s := openSpeakerStore(t)
	seedRecording(t, s, "conv-1", "rec-1")

	b, err := s.ProposeSpeakerBinding(ctx, "rec-1", "SPK_02", "rev-1", nil, "model-x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConfirmSpeakerBinding(ctx, b.ID, "person:carol", "reviewer-a"); err != nil {
		t.Fatal(err)
	}
	proj, err := s.SpeakerProjection(ctx, "rec-1", "rev-1")
	if err != nil {
		t.Fatal(err)
	}
	if proj["SPK_02"] != "person:carol" {
		t.Fatalf("projection missing confirmed binding: %v", proj)
	}

	revoked, err := s.RevokeSpeakerBinding(ctx, b.ID, "reviewer-c", "misidentified")
	if err != nil {
		t.Fatal(err)
	}
	if revoked.Status != "revoked" || revoked.Reviewer != "reviewer-c" || revoked.Version != 1 {
		t.Fatalf("bad revocation: %+v", revoked)
	}
	proj, err = s.SpeakerProjection(ctx, "rec-1", "rev-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(proj) != 0 {
		t.Fatalf("revoked binding still resolves: %v", proj)
	}

	events := speakerBindingEvents(t, s, b.ID)
	want := []speakerEvent{{"propose", "model-x", ""}, {"confirm", "reviewer-a", ""}, {"revoke", "reviewer-c", "misidentified"}}
	if len(events) != len(want) {
		t.Fatalf("events = %+v, want %+v", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("event %d = %+v, want %+v", i, events[i], want[i])
		}
	}
}

func TestSpeakerBindingScopedToRecording(t *testing.T) {
	ctx := context.Background()
	s := openSpeakerStore(t)
	seedRecording(t, s, "conv-1", "rec-1")
	seedRecording(t, s, "conv-2", "rec-2")

	first, err := s.ProposeSpeakerBinding(ctx, "rec-1", "SPK_00", "rev-1", []string{"t1"}, "model-x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConfirmSpeakerBinding(ctx, first.ID, "person:alice", "reviewer-a"); err != nil {
		t.Fatal(err)
	}
	// The same label in another recording is an unrelated speaker and must not
	// inherit the confirmed identity.
	proj2, err := s.SpeakerProjection(ctx, "rec-2", "rev-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(proj2) != 0 {
		t.Fatalf("projection for other recording not empty: %v", proj2)
	}

	second, err := s.ProposeSpeakerBinding(ctx, "rec-2", "SPK_00", "rev-1", []string{"t9"}, "model-x")
	if err != nil {
		t.Fatal(err)
	}
	if second.ID == first.ID {
		t.Fatalf("bindings in different recordings share id %q", second.ID)
	}
	if _, err := s.ConfirmSpeakerBinding(ctx, second.ID, "person:bob", "reviewer-b"); err != nil {
		t.Fatal(err)
	}

	proj1, err := s.SpeakerProjection(ctx, "rec-1", "rev-1")
	if err != nil {
		t.Fatal(err)
	}
	proj2, err = s.SpeakerProjection(ctx, "rec-2", "rev-1")
	if err != nil {
		t.Fatal(err)
	}
	if proj1["SPK_00"] != "person:alice" || proj2["SPK_00"] != "person:bob" {
		t.Fatalf("cross-recording leakage: rec-1=%v rec-2=%v", proj1, proj2)
	}
}

func TestSpeakerBindingReproposeRefreshesAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s := openSpeakerStore(t)
	seedRecording(t, s, "conv-1", "rec-1")

	first, err := s.ProposeSpeakerBinding(ctx, "rec-1", "SPK_03", "rev-1", []string{"t1"}, "model-x")
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.ProposeSpeakerBinding(ctx, "rec-1", "SPK_03", "rev-2", []string{"t2", "t3"}, "model-y")
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != first.ID {
		t.Fatalf("re-proposal created a second binding: %q vs %q", again.ID, first.ID)
	}
	if again.Status != "proposed" || again.Version != 1 || again.TranscriptRevision != "rev-2" {
		t.Fatalf("re-proposal did not refresh: %+v", again)
	}
	if len(again.EvidenceTurnIDs) != 2 || again.EvidenceTurnIDs[0] != "t2" {
		t.Fatalf("evidence not refreshed: %+v", again.EvidenceTurnIDs)
	}
	var bindings int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM speaker_bindings WHERE recording_id='rec-1' AND speaker_scoped_id='SPK_03'`).Scan(&bindings); err != nil {
		t.Fatal(err)
	}
	if bindings != 1 {
		t.Fatalf("bindings = %d, want 1", bindings)
	}
	if events := speakerBindingEvents(t, s, first.ID); len(events) != 1 {
		t.Fatalf("re-proposal duplicated the propose event: %+v", events)
	}
}

func TestSpeakerBindingReviewTransitions(t *testing.T) {
	ctx := context.Background()
	s := openSpeakerStore(t)
	seedRecording(t, s, "conv-1", "rec-1")

	if _, err := s.ConfirmSpeakerBinding(ctx, "sb:missing", "person:alice", "reviewer-a"); err == nil {
		t.Fatal("confirming an unknown binding succeeded")
	}
	if _, err := s.RejectSpeakerBinding(ctx, "sb:missing", "reviewer-a", ""); err == nil {
		t.Fatal("rejecting an unknown binding succeeded")
	}

	b, err := s.ProposeSpeakerBinding(ctx, "rec-1", "SPK_04", "rev-1", []string{"t1"}, "model-x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RevokeSpeakerBinding(ctx, b.ID, "reviewer-a", "not yet confirmed"); err == nil {
		t.Fatal("revoking a proposed binding succeeded")
	}

	confirmed, err := s.ConfirmSpeakerBinding(ctx, b.ID, "person:dave", "reviewer-a")
	if err != nil {
		t.Fatal(err)
	}
	// Re-confirming the same person is a no-op: no version bump, no event.
	replay, err := s.ConfirmSpeakerBinding(ctx, b.ID, "person:dave", "reviewer-a")
	if err != nil {
		t.Fatal(err)
	}
	if replay.Version != confirmed.Version || replay.Status != "confirmed" {
		t.Fatalf("idempotent confirm changed the row: %+v", replay)
	}
	if _, err := s.ConfirmSpeakerBinding(ctx, b.ID, "person:erin", "reviewer-a"); err == nil {
		t.Fatal("re-confirming as another person succeeded")
	}
	// Confirming or revoking a rejected binding is refused.
	if _, err := s.RejectSpeakerBinding(ctx, b.ID, "reviewer-b", "reject confirmed"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RevokeSpeakerBinding(ctx, b.ID, "reviewer-b", "no"); err == nil {
		t.Fatal("revoking a rejected binding succeeded")
	}
	if _, err := s.ConfirmSpeakerBinding(ctx, b.ID, "person:dave", "reviewer-a"); err == nil {
		t.Fatal("confirming a rejected binding succeeded")
	}

	events := speakerBindingEvents(t, s, b.ID)
	want := []speakerEvent{{"propose", "model-x", ""}, {"confirm", "reviewer-a", ""}, {"reject", "reviewer-b", "reject confirmed"}}
	if len(events) != len(want) {
		t.Fatalf("events = %+v, want %+v", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("event %d = %+v, want %+v", i, events[i], want[i])
		}
	}
}

// TestSpeakerBindingRevisionScopedIdentity proves that a diarization label is
// never resolved across transcript revisions: the same speaker_0 in two
// revisions does not inherit identity, and a confirmed binding resolves only
// in the revision it was reviewed against.
func TestSpeakerBindingRevisionScopedIdentity(t *testing.T) {
	ctx := context.Background()
	s := openSpeakerStore(t)
	seedRecording(t, s, "conv-1", "rec-1")

	v1, err := s.ProposeSpeakerBinding(ctx, "rec-1", "speaker_0", "rev-1", []string{"t1"}, "model-x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConfirmSpeakerBinding(ctx, v1.ID, "person:alice", "reviewer-a"); err != nil {
		t.Fatal(err)
	}
	// The label resolved for rev-1; the same label in rev-2 is unknown.
	projV1, err := s.SpeakerProjection(ctx, "rec-1", "rev-1")
	if err != nil {
		t.Fatal(err)
	}
	if projV1["speaker_0"] != "person:alice" {
		t.Fatalf("rev-1 projection = %v, want speaker_0 -> person:alice", projV1)
	}
	projV2, err := s.SpeakerProjection(ctx, "rec-1", "rev-2")
	if err != nil {
		t.Fatal(err)
	}
	if len(projV2) != 0 {
		t.Fatalf("rev-2 inherited identity from rev-1: %v", projV2)
	}

	// rev-2 reuses the label for a different person: a new claim, not a
	// mutation of the rev-1 verdict.
	v2, err := s.ProposeSpeakerBinding(ctx, "rec-1", "speaker_0", "rev-2", []string{"t9"}, "model-x")
	if err != nil {
		t.Fatal(err)
	}
	if v2.ID == v1.ID || v2.Version != 2 {
		t.Fatalf("rev-2 claim did not create a new versioned row: %+v", v2)
	}
	if _, err := s.ConfirmSpeakerBinding(ctx, v2.ID, "person:bob", "reviewer-b"); err != nil {
		t.Fatal(err)
	}
	projV1, err = s.SpeakerProjection(ctx, "rec-1", "rev-1")
	if err != nil {
		t.Fatal(err)
	}
	projV2, err = s.SpeakerProjection(ctx, "rec-1", "rev-2")
	if err != nil {
		t.Fatal(err)
	}
	if projV1["speaker_0"] != "person:alice" || projV2["speaker_0"] != "person:bob" {
		t.Fatalf("revision scoping broken: rev-1=%v rev-2=%v", projV1, projV2)
	}
	// The rev-1 verdict is untouched history.
	if v1again := loadSpeakerBinding(t, s, v1.ID); v1again.Status != "confirmed" || v1again.PersonID != "person:alice" {
		t.Fatalf("rev-1 binding mutated: %+v", v1again)
	}
}

// TestSpeakerBindingReproposeAfterRevokeAndReject proves the lifecycle allows
// fresh claims after a terminal verdict while keeping full history: proposal
// v1 -> confirm Alice -> revoke -> proposal v2 -> confirm Bob works, and a
// rejection is terminal history for its own row but never blocks a new
// proposal (documented policy).
func TestSpeakerBindingReproposeAfterRevokeAndReject(t *testing.T) {
	ctx := context.Background()
	s := openSpeakerStore(t)
	seedRecording(t, s, "conv-1", "rec-1")

	v1, err := s.ProposeSpeakerBinding(ctx, "rec-1", "speaker_0", "rev-1", []string{"t1"}, "model-x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConfirmSpeakerBinding(ctx, v1.ID, "person:alice", "reviewer-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RevokeSpeakerBinding(ctx, v1.ID, "reviewer-a", "wrong person"); err != nil {
		t.Fatal(err)
	}
	// Re-propose after revoke: a new claim on new evidence/revision.
	v2, err := s.ProposeSpeakerBinding(ctx, "rec-1", "speaker_0", "rev-2", []string{"t2"}, "model-x")
	if err != nil {
		t.Fatal(err)
	}
	if v2.Version != 2 || v2.Status != "proposed" {
		t.Fatalf("re-propose after revoke did not create a new claim: %+v", v2)
	}
	if _, err := s.ConfirmSpeakerBinding(ctx, v2.ID, "person:bob", "reviewer-b"); err != nil {
		t.Fatal(err)
	}
	proj, err := s.SpeakerProjection(ctx, "rec-1", "rev-2")
	if err != nil {
		t.Fatal(err)
	}
	if proj["speaker_0"] != "person:bob" {
		t.Fatalf("rev-2 projection = %v, want person:bob", proj)
	}
	// History: row 1 keeps its verdict and its events.
	if row1 := loadSpeakerBinding(t, s, v1.ID); row1.Status != "revoked" {
		t.Fatalf("revoked row mutated: %+v", row1)
	}
	if events := speakerBindingEvents(t, s, v1.ID); len(events) != 3 {
		t.Fatalf("row 1 history incomplete: %+v", events)
	}

	// Documented reject policy: the rejected row is immutable history and a
	// later claim with fresh evidence creates the next version.
	v3, err := s.ProposeSpeakerBinding(ctx, "rec-1", "speaker_0", "rev-3", []string{"t3"}, "model-x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RejectSpeakerBinding(ctx, v3.ID, "reviewer-c", "insufficient evidence"); err != nil {
		t.Fatal(err)
	}
	if rejectedRow := loadSpeakerBinding(t, s, v3.ID); rejectedRow.Status != "rejected" {
		t.Fatalf("rejected row mutated: %+v", rejectedRow)
	}
	v4, err := s.ProposeSpeakerBinding(ctx, "rec-1", "speaker_0", "rev-3", []string{"t3", "t4"}, "model-y")
	if err != nil {
		t.Fatal(err)
	}
	if v4.ID == v3.ID || v4.Version != 4 || v4.Status != "proposed" {
		t.Fatalf("re-propose after reject did not create a fresh claim: %+v", v4)
	}
	if again := loadSpeakerBinding(t, s, v3.ID); again.Status != "rejected" || len(again.EvidenceTurnIDs) != 1 {
		t.Fatalf("rejected row history damaged: %+v", again)
	}
}

// TestListSpeakerBindingsOrderAndFilter proves the read path returns every
// version of a recording's bindings grouped by label and ordered oldest first,
// includes terminal verdicts, narrows to one label on request and never leaks
// a binding across recording ids.
func TestListSpeakerBindingsOrderAndFilter(t *testing.T) {
	ctx := context.Background()
	s := openSpeakerStore(t)
	seedRecording(t, s, "conv-1", "rec-1")
	seedRecording(t, s, "conv-2", "rec-2")

	// A binding on another recording must not appear in rec-1's list.
	if _, err := s.ProposeSpeakerBinding(ctx, "rec-2", "speaker_0", "rev-1", nil, "model-x"); err != nil {
		t.Fatal(err)
	}

	// Propose out of label order, then add a second version of speaker_0 so the
	// version tiebreak inside one label is exercised.
	if _, err := s.ProposeSpeakerBinding(ctx, "rec-1", "speaker_1", "rev-1", []string{"t1"}, "model-x"); err != nil {
		t.Fatal(err)
	}
	speaker0v1, err := s.ProposeSpeakerBinding(ctx, "rec-1", "speaker_0", "rev-1", []string{"t2"}, "model-x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RejectSpeakerBinding(ctx, speaker0v1.ID, "reviewer-b", "wrong speaker"); err != nil {
		t.Fatal(err)
	}
	speaker0v2, err := s.ProposeSpeakerBinding(ctx, "rec-1", "speaker_0", "rev-1", []string{"t2", "t3"}, "model-y")
	if err != nil {
		t.Fatal(err)
	}
	if speaker0v2.Version != 2 {
		t.Fatalf("expected a second version of speaker_0, got %+v", speaker0v2)
	}

	all, err := s.ListSpeakerBindings(ctx, "rec-1", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("bindings = %d, want 3: %+v", len(all), all)
	}
	wantIDs := []string{speaker0v1.ID, speaker0v2.ID, speakerBindingID("rec-1", "speaker_1", 1)}
	for i, want := range wantIDs {
		if all[i].ID != want {
			t.Fatalf("binding %d id = %q, want %q (order: speaker_0 v1, speaker_0 v2, speaker_1 v1)", i, all[i].ID, want)
		}
	}
	// Terminal verdicts are part of the audit surface, not filtered out.
	if all[0].Status != speakerBindingRejected || all[1].Status != speakerBindingProposed {
		t.Fatalf("statuses = %q, %q; want rejected then proposed", all[0].Status, all[1].Status)
	}
	if all[1].TranscriptRevision != "rev-1" || len(all[1].EvidenceTurnIDs) != 2 {
		t.Fatalf("second version lost its fields: %+v", all[1])
	}

	narrowed, err := s.ListSpeakerBindings(ctx, "rec-1", "speaker_0")
	if err != nil {
		t.Fatal(err)
	}
	if len(narrowed) != 2 || narrowed[0].ID != speaker0v1.ID || narrowed[1].ID != speaker0v2.ID {
		t.Fatalf("narrowed list = %+v, want both speaker_0 versions", narrowed)
	}

	none, err := s.ListSpeakerBindings(ctx, "rec-1", "speaker_9")
	if err != nil {
		t.Fatal(err)
	}
	if none == nil || len(none) != 0 {
		t.Fatalf("unknown label = %+v, want empty non-nil slice", none)
	}

	// rec-2 only has its own label; the filter is recording-scoped.
	other, err := s.ListSpeakerBindings(ctx, "rec-2", "speaker_0")
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 1 || other[0].RecordingID != "rec-2" {
		t.Fatalf("rec-2 list = %+v, want its own single binding", other)
	}
}
