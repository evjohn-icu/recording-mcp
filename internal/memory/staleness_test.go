package memory

import (
	"context"
	"database/sql"
	"testing"
)

// attachmentState reads the persisted active flag, supersedes revision and
// result snapshot for one source attachment.
func attachmentState(t *testing.T, s *Store, id string) (active int, supersedes sql.NullString, resultJSON string) {
	t.Helper()
	if err := s.db.QueryRow(`SELECT active,supersedes_revision,result_json FROM source_attachments WHERE id=?`, id).Scan(&active, &supersedes, &resultJSON); err != nil {
		t.Fatalf("read attachment %s: %v", id, err)
	}
	return active, supersedes, resultJSON
}

// attachmentTurnCount counts the live attachment_turns rows for one attachment.
func attachmentTurnCount(t *testing.T, s *Store, id string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM attachment_turns WHERE attachment_id=?`, id).Scan(&n); err != nil {
		t.Fatalf("count attachment_turns for %s: %v", id, err)
	}
	return n
}

// TestIngestRevisionReplayIdempotent pins that replaying the identical
// (recording, segment, revision) snapshot changes nothing: the second ingest
// reports Inserted=false with the same persisted segment state and no extra
// extract job.
func TestIngestRevisionReplayIdempotent(t *testing.T) {
	s := openTempStore(t)
	ctx := context.Background()

	first, err := s.IngestTranscript(ctx, sampleResult("seg-a", "rev-1", 0))
	if err != nil {
		t.Fatal(err)
	}
	if !first.Inserted {
		t.Fatalf("first ingest reported Inserted=false: %+v", first)
	}
	second, err := s.IngestTranscript(ctx, sampleResult("seg-a", "rev-1", 0))
	if err != nil {
		t.Fatal(err)
	}
	if second.Inserted {
		t.Fatalf("replay reported Inserted=true: %+v", second)
	}
	if second.Turns != first.Turns || second.SegmentIndex != first.SegmentIndex || second.AnalysisStatus != first.AnalysisStatus {
		t.Fatalf("replay report diverged from first: first=%+v second=%+v", first, second)
	}
	if n, _ := countExtractJobs(t, s); n != 1 {
		t.Fatalf("replay produced %d extract jobs, want 1", n)
	}
}

// TestIngestSupersedeActiveRevision pins the happy-path replacement: the new
// revision becomes the active attachment, the old revision stays as immutable
// history with active=0 and untouched content, and exactly one new extract job
// is enqueued for the replacement.
func TestIngestSupersedeActiveRevision(t *testing.T) {
	s := openTempStore(t)
	ctx := context.Background()

	recordingID := stableKey("recording", "device-1", "rec-1")
	v1AttachmentID := stableKey("attachment", recordingID, "seg-a", "rev-1")
	v2AttachmentID := stableKey("attachment", recordingID, "seg-a", "rev-2")

	if _, err := s.IngestTranscript(ctx, sampleResult("seg-a", "rev-1", 0)); err != nil {
		t.Fatal(err)
	}
	oldActive, _, oldResult := attachmentState(t, s, v1AttachmentID)
	if oldActive != 1 {
		t.Fatalf("v1 attachment active=%d, want 1", oldActive)
	}
	oldTurns := attachmentTurnCount(t, s, v1AttachmentID)

	v2 := sampleResult("seg-a", "rev-2", 0)
	v2.Source.SupersedesRevision = "rev-1"
	report, err := s.IngestTranscript(ctx, v2)
	if err != nil {
		t.Fatalf("supersede rejected: %v", err)
	}
	if !report.Inserted {
		t.Fatalf("supersede reported Inserted=false: %+v", report)
	}

	newActive, newSupersedes, _ := attachmentState(t, s, v2AttachmentID)
	if newActive != 1 || !newSupersedes.Valid || newSupersedes.String != "rev-1" {
		t.Fatalf("new attachment state active=%d supersedes=%v, want active=1 supersedes=rev-1", newActive, newSupersedes)
	}
	oldActive, oldSupersedes, afterResult := attachmentState(t, s, v1AttachmentID)
	if oldActive != 0 {
		t.Fatalf("superseded attachment active=%d, want 0", oldActive)
	}
	if oldSupersedes.Valid {
		t.Fatalf("original revision unexpectedly carries supersedes_revision=%q", oldSupersedes.String)
	}
	if afterResult != oldResult {
		t.Fatalf("superseded result_json was mutated:\n before=%s\n after=%s", oldResult, afterResult)
	}
	if afterTurns := attachmentTurnCount(t, s, v1AttachmentID); afterTurns != oldTurns {
		t.Fatalf("superseded attachment turns changed: %d -> %d", oldTurns, afterTurns)
	}
	if n, _ := countExtractJobs(t, s); n != 2 {
		t.Fatalf("after supersede got %d extract jobs, want 2", n)
	}
}

// TestIngestSupersedeRejectsInvalidTarget pins the three rejection paths with
// their exact error literals: unknown target, self target, and a target that
// exists for the segment but is no longer the active revision.
func TestIngestSupersedeRejectsInvalidTarget(t *testing.T) {
	s := openTempStore(t)
	ctx := context.Background()

	unknown := sampleResult("seg-a", "rev-2", 0)
	unknown.Source.SupersedesRevision = "rev-9"
	if _, err := s.IngestTranscript(ctx, unknown); err == nil || err.Error() != `supersedes target "rev-9" not found for segment "seg-a"` {
		t.Fatalf("unknown target error = %v", err)
	}

	self := sampleResult("seg-a", "rev-1", 0)
	self.Source.SupersedesRevision = "rev-1"
	if _, err := s.IngestTranscript(ctx, self); err == nil || err.Error() != "supersedes_revision must differ from transcript_revision" {
		t.Fatalf("self target error = %v", err)
	}

	if _, err := s.IngestTranscript(ctx, sampleResult("seg-a", "rev-1", 0)); err != nil {
		t.Fatal(err)
	}
	v2 := sampleResult("seg-a", "rev-2", 0)
	v2.Source.SupersedesRevision = "rev-1"
	if _, err := s.IngestTranscript(ctx, v2); err != nil {
		t.Fatalf("valid supersede rejected: %v", err)
	}
	v3 := sampleResult("seg-a", "rev-3", 0)
	v3.Source.SupersedesRevision = "rev-1"
	want := `supersedes target "rev-1" is not the active revision for segment "seg-a" (active "rev-2")`
	if _, err := s.IngestTranscript(ctx, v3); err == nil || err.Error() != want {
		t.Fatalf("non-head target error = %v, want %s", err, want)
	}
}

// TestIngestSupersedeRejectsCrossSegment pins that a supersede target belonging
// to another segment is rejected with the exact error literal.
func TestIngestSupersedeRejectsCrossSegment(t *testing.T) {
	s := openTempStore(t)
	ctx := context.Background()

	if _, err := s.IngestTranscript(ctx, sampleResult("seg-a", "rev-1", 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.IngestTranscript(ctx, sampleResult("seg-b", "rev-2", 1000)); err != nil {
		t.Fatal(err)
	}
	v3 := sampleResult("seg-a", "rev-3", 0)
	v3.Source.SupersedesRevision = "rev-2"
	want := `supersedes target "rev-2" belongs to segment "seg-b"; cross-segment supersede is not allowed`
	if _, err := s.IngestTranscript(ctx, v3); err == nil || err.Error() != want {
		t.Fatalf("cross-segment error = %v, want %s", err, want)
	}
}

// TestSupersededSourceRemainsAuditable pins that a superseded attachment keeps
// its row, its turns, and its attachment_turns links, and that the evidence
// context for a turn materialized by that attachment is reachable and flagged
// stale.
func TestSupersededSourceRemainsAuditable(t *testing.T) {
	s := openTempStore(t)
	ctx := context.Background()

	recordingID := stableKey("recording", "device-1", "rec-1")
	v1AttachmentID := stableKey("attachment", recordingID, "seg-a", "rev-1")
	v1TurnID := stableKey("turn", recordingID, "seg-a/rev-1/utt-1")

	if _, err := s.IngestTranscript(ctx, sampleResult("seg-a", "rev-1", 0)); err != nil {
		t.Fatal(err)
	}
	active, _, _ := attachmentState(t, s, v1AttachmentID)
	if active != 1 {
		t.Fatalf("v1 attachment active=%d before supersede, want 1", active)
	}
	v2 := sampleResult("seg-a", "rev-2", 0)
	v2.Source.SupersedesRevision = "rev-1"
	if _, err := s.IngestTranscript(ctx, v2); err != nil {
		t.Fatal(err)
	}

	active, _, _ = attachmentState(t, s, v1AttachmentID)
	if active != 0 {
		t.Fatalf("v1 attachment active=%d after supersede, want 0", active)
	}
	var turns int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM turns WHERE id=?`, v1TurnID).Scan(&turns); err != nil {
		t.Fatal(err)
	}
	if turns != 1 {
		t.Fatalf("v1 turn row count=%d, want 1", turns)
	}
	if n := attachmentTurnCount(t, s, v1AttachmentID); n != 1 {
		t.Fatalf("v1 attachment_turns count=%d, want 1", n)
	}

	ev, err := s.GetEvidenceContext(ctx, v1TurnID, 2, 1<<20)
	if err != nil {
		t.Fatalf("evidence context for superseded turn: %v", err)
	}
	found := false
	for _, te := range ev.Turns {
		if te.ID == v1TurnID && te.Text == "hello seg-a" {
			found = true
		}
	}
	if !found {
		t.Fatalf("evidence window lacks v1 turn: %+v", ev)
	}
	if !ev.Stale {
		t.Fatalf("superseded v1 evidence context not flagged stale: %+v", ev)
	}
}

// TestSupersedeEnqueuesExtractForNewRevision pins the replacement job identity:
// the new attachment's extract job carries the idempotency key derived from the
// new attachment id.
func TestSupersedeEnqueuesExtractForNewRevision(t *testing.T) {
	s := openTempStore(t)
	ctx := context.Background()

	recordingID := stableKey("recording", "device-1", "rec-1")
	newAttachmentID := stableKey("attachment", recordingID, "seg-a", "rev-2")
	wantKey := stableKey("job", "extract", newAttachmentID, "extract-v1")

	if _, err := s.IngestTranscript(ctx, sampleResult("seg-a", "rev-1", 0)); err != nil {
		t.Fatal(err)
	}
	v2 := sampleResult("seg-a", "rev-2", 0)
	v2.Source.SupersedesRevision = "rev-1"
	if _, err := s.IngestTranscript(ctx, v2); err != nil {
		t.Fatal(err)
	}

	var key string
	if err := s.db.QueryRow(`SELECT idempotency_key FROM processing_jobs WHERE stage='extract' AND input_snapshot_id=?`, newAttachmentID).Scan(&key); err != nil {
		t.Fatalf("extract job for new attachment: %v", err)
	}
	if key != wantKey {
		t.Fatalf("new attachment extract job key=%q, want %q", key, wantKey)
	}
	if n, _ := countExtractJobs(t, s); n != 2 {
		t.Fatalf("expected 2 extract jobs after supersede, got %d", n)
	}
}

// TestRepeatedInvalidationDoesNotDuplicateJobs pins that explicitly invalidating
// artifacts sourced from an attachment marks them once and is a no-op on
// repeat, never spawning additional work.
func TestRepeatedInvalidationDoesNotDuplicateJobs(t *testing.T) {
	s := openTempStore(t)
	ctx := context.Background()

	recordingID := stableKey("recording", "device-1", "rec-1")
	v1AttachmentID := stableKey("attachment", recordingID, "seg-a", "rev-1")

	report, err := s.IngestTranscript(ctx, sampleResult("seg-a", "rev-1", 0))
	if err != nil {
		t.Fatal(err)
	}
	ann := Annotations{
		ConversationID:    report.ConversationID,
		Title:             "v1 evidence note",
		AnnotationVersion: "annotation-v1",
		Topics:            []TopicAnnotation{{ID: "topic:audit", Title: "Audit"}},
		Notes: []NoteAnnotation{{
			ID:              "note:v1",
			Kind:            "observation",
			TopicIDs:        []string{"topic:audit"},
			Summary:         "observation backed by the first revision",
			EvidenceTurnIDs: []string{"seg-a/rev-1/utt-1"},
		}},
	}
	if err := s.ApplyAnnotations(ctx, report.ConversationID, "", ann); err != nil {
		t.Fatalf("apply annotations: %v", err)
	}
	before, _ := countExtractJobs(t, s)

	first, err := s.InvalidateDerivedArtifacts(ctx, v1AttachmentID, staleReasonSuperseded)
	if err != nil {
		t.Fatal(err)
	}
	if first != 1 {
		t.Fatalf("first invalidation marked %d artifacts, want 1", first)
	}
	second, err := s.InvalidateDerivedArtifacts(ctx, v1AttachmentID, staleReasonSuperseded)
	if err != nil {
		t.Fatal(err)
	}
	if second != 0 {
		t.Fatalf("repeat invalidation marked %d artifacts, want 0", second)
	}
	if after, _ := countExtractJobs(t, s); after != before {
		t.Fatalf("invalidation changed extract job count: %d -> %d", before, after)
	}
}

// TestProcessingStatusCountsCurrentRevisionsOnly pins that GetProcessingStatus
// reports only the active transcript revision: after rev-2 supersedes rev-1 the
// turn counts drop to rev-2's, the stale rev-1 note no longer counts toward the
// analysis status, and identity/unclassified turn counts are scoped to current
// turns. The superseded revision stays in the audit tables, and annotating the
// current turn returns the status to partial.
func TestProcessingStatusCountsCurrentRevisionsOnly(t *testing.T) {
	ctx := context.Background()
	s := openTempStore(t)

	report, err := s.IngestTranscript(ctx, revisionResult("seg-a", "rev-1", 0, "one", "two", "three"))
	if err != nil {
		t.Fatal(err)
	}
	conv := report.ConversationID

	if err := s.ApplyAnnotations(ctx, conv, "", Annotations{
		ConversationID:    conv,
		Title:             "rev-1 notes",
		AnnotationVersion: "ann-v1",
		Topics:            []TopicAnnotation{{ID: "topic:rev", Title: "Revision"}},
		Notes: []NoteAnnotation{{
			ID:              "note:v1",
			Kind:            "observation",
			TopicIDs:        []string{"topic:rev"},
			Summary:         "note backed by the first revision",
			EvidenceTurnIDs: []string{"seg-a/rev-1/utt-1"},
		}},
	}); err != nil {
		t.Fatalf("apply v1 annotations: %v", err)
	}

	before, err := s.GetProcessingStatus(ctx, conv)
	if err != nil {
		t.Fatal(err)
	}
	if before.ReceivedSegments != 1 || before.OriginalTurns != 3 || before.TopicLinkedTurns != 1 {
		t.Fatalf("v1 turn counts = %+v, want one segment, three turns and the note's turn classified", before)
	}
	if before.IdentityUnconfirmedTurns != 3 || before.UnclassifiedTurns != 2 {
		t.Fatalf("v1 turn scoping = %+v, want three unconfirmed turns and two without a topic", before)
	}
	if before.AnalysisStatus != "partial" {
		t.Fatalf("v1 analysis status = %q, want partial with a live note", before.AnalysisStatus)
	}

	v2 := revisionResult("seg-a", "rev-2", 0, "three", "four")
	v2.Source.SupersedesRevision = "rev-1"
	if _, err := s.IngestTranscript(ctx, v2); err != nil {
		t.Fatal(err)
	}

	after, err := s.GetProcessingStatus(ctx, conv)
	if err != nil {
		t.Fatal(err)
	}
	if after.ReceivedSegments != 1 || after.OriginalTurns != 2 {
		t.Fatalf("post-supersede segment/turn counts = %+v, want one active segment and two current turns", after)
	}
	if after.IdentityUnconfirmedTurns != 2 || after.UnclassifiedTurns != 2 || after.TopicLinkedTurns != 0 {
		t.Fatalf("post-supersede turn scoping = %+v, want only the two current turns", after)
	}
	if after.AnalysisStatus != "pending" {
		t.Fatalf("post-supersede analysis status = %q, want pending once the v1 note is stale", after.AnalysisStatus)
	}

	// The superseded revision remains auditable even though it no longer counts.
	var totalTurns int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM turns WHERE conversation_id=?`, conv).Scan(&totalTurns); err != nil {
		t.Fatal(err)
	}
	if totalTurns != 5 {
		t.Fatalf("stored turns = %d, want all five revisions retained for audit", totalTurns)
	}
	ev, err := s.GetEvidenceContext(ctx, ingestedTurnID("seg-a", "rev-1", "utt-1"), 1, 1<<20)
	if err != nil {
		t.Fatalf("audit evidence for superseded turn: %v", err)
	}
	if !ev.Stale {
		t.Fatalf("superseded turn not flagged stale in audit: %+v", ev)
	}

	// Annotating the current turn brings the status back to partial.
	if err := s.ApplyAnnotations(ctx, conv, "ann-v1", Annotations{
		ConversationID:    conv,
		Title:             "rev-2 notes",
		AnnotationVersion: "ann-v2",
		Topics:            []TopicAnnotation{{ID: "topic:rev", Title: "Revision"}},
		Notes: []NoteAnnotation{{
			ID:              "note:v2",
			Kind:            "observation",
			TopicIDs:        []string{"topic:rev"},
			Summary:         "note backed by the current revision",
			EvidenceTurnIDs: []string{"seg-a/rev-2/utt-1"},
		}},
	}); err != nil {
		t.Fatalf("apply v2 annotations: %v", err)
	}
	partial, err := s.GetProcessingStatus(ctx, conv)
	if err != nil {
		t.Fatal(err)
	}
	if partial.AnalysisStatus != "partial" {
		t.Fatalf("status after annotating the current turn = %q, want partial", partial.AnalysisStatus)
	}
	if partial.OriginalTurns != 2 {
		t.Fatalf("status after re-annotation = %+v, want current turn count preserved", partial)
	}
}
