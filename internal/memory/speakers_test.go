package memory

import (
	"context"
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
	if b.ID != speakerBindingID("rec-1", "SPK_00") || !strings.HasPrefix(b.ID, "sb:") {
		t.Fatalf("unexpected deterministic id %q", b.ID)
	}
	if len(b.EvidenceTurnIDs) != 2 || b.EvidenceTurnIDs[0] != "t1" || b.EvidenceTurnIDs[1] != "t2" {
		t.Fatalf("evidence not stored: %+v", b.EvidenceTurnIDs)
	}
	if b.CreatedAt == 0 || b.UpdatedAt == 0 {
		t.Fatalf("timestamps not set: %+v", b)
	}

	// A proposed binding is not an accepted identity yet.
	proj, err := s.SpeakerProjection(ctx, "rec-1")
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
	if confirmed.Status != "confirmed" || confirmed.PersonID != "person:alice" || confirmed.Reviewer != "reviewer-a" || confirmed.Version != 2 {
		t.Fatalf("bad confirmation: %+v", confirmed)
	}

	proj, err = s.SpeakerProjection(ctx, "rec-1")
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

	proj, err := s.SpeakerProjection(ctx, "rec-1")
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
	proj, err := s.SpeakerProjection(ctx, "rec-1")
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
	if revoked.Status != "revoked" || revoked.Reviewer != "reviewer-c" || revoked.Version != 2 {
		t.Fatalf("bad revocation: %+v", revoked)
	}
	proj, err = s.SpeakerProjection(ctx, "rec-1")
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
	proj2, err := s.SpeakerProjection(ctx, "rec-2")
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

	proj1, err := s.SpeakerProjection(ctx, "rec-1")
	if err != nil {
		t.Fatal(err)
	}
	proj2, err = s.SpeakerProjection(ctx, "rec-2")
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
