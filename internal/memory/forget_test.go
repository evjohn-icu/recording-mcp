package memory

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"recordingmcp/internal/asrinput"
	"recordingmcp/internal/process"
)

func TestForgetTurnTombstoneIsGranularAndIdempotent(t *testing.T) {
	ctx := context.Background()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tr, a := synthetic()
	if _, err = s.Import(ctx, tr, a, "x.wav", strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	jobID, _, err := process.Enqueue(ctx, s.DB(), process.Spec{Stage: "embed", InputSnapshotID: a.ConversationID, IdempotencyKey: "forget-retry", PayloadJSON: `{"conversation_id":"` + a.ConversationID + `"}`}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB().Exec(`INSERT INTO processing_retry_requests(request_id,job_id,expected_attempt,created_at) VALUES('forget-request',?,0,?)`, jobID, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PreviewForget(ctx, ForgetRequest{Scope: "turn", TurnID: turnKey(a.RecordingID, "seg01-turn0001")}); err == nil {
		t.Fatal("missing timing must block granular forget")
	}
	req := ForgetRequest{Scope: "turn", TurnID: turnKey(a.RecordingID, "seg01-turn0002")}
	p, err := s.PreviewForget(ctx, req)
	if err != nil || p.Turns != 1 {
		t.Fatalf("preview %+v %v", p, err)
	}
	req.ExpectedState = p.ExpectedState
	res, err := s.CommitForget(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "pending_external_cleanup" {
		t.Fatalf("status %+v", res)
	}
	if _, err = s.GetItem(ctx, turnKey(a.RecordingID, "seg01-turn0001"), true); err != nil {
		t.Fatalf("unselected turn removed: %v", err)
	}
	if _, err = s.GetItem(ctx, turnKey(a.RecordingID, "seg01-turn0002"), true); err == nil {
		t.Fatal("forgotten turn remains")
	}
	again, err := s.CommitForget(ctx, req)
	if err != nil || !again.AlreadyCommitted {
		t.Fatalf("replay %+v %v", again, err)
	}
	if err = s.GuardForgotten(ctx, a.RecordingID, 1); err == nil {
		t.Fatal("segment-level guard did not fence source")
	}
	var retryRows int
	if err = s.DB().QueryRow(`SELECT COUNT(*) FROM processing_retry_requests WHERE job_id=?`, jobID).Scan(&retryRows); err != nil || retryRows != 0 {
		t.Fatalf("forgotten processing retry request retained: rows=%d err=%v", retryRows, err)
	}
}

func TestAINoteSuppressionPersistsWithoutTextHash(t *testing.T) {
	ctx := context.Background()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tr, a := synthetic()
	if _, err = s.Import(ctx, tr, a, "x.wav", strings.Repeat("b", 64)); err != nil {
		t.Fatal(err)
	}
	sourceSegment := "seg-stable-1"
	if err = s.SuppressAINote(ctx, a.RecordingID, sourceSegment, "suggestion"); err != nil {
		t.Fatal(err)
	}
	ok, err := s.IsAINoteSuppressed(ctx, a.RecordingID, sourceSegment, "suggestion")
	if err != nil || !ok {
		t.Fatalf("suppression %v %v", ok, err)
	}
	var n int
	if err = s.DB().QueryRow(`SELECT COUNT(*) FROM notes WHERE kind='suggestion'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("unrelated legacy note should remain %d %v", n, err)
	}
}

func TestSuppressNoteIsAtomicStableAndKeepsSource(t *testing.T) {
	ctx := context.Background()
	s := openTempStore(t)
	result := sampleResult("seg-note", "rev-1", 0)
	report, err := s.IngestTranscript(ctx, result)
	if err != nil {
		t.Fatal(err)
	}
	ann := Annotations{ConversationID: report.ConversationID, Title: "Notes", Summary: "summary to clear", AnnotationVersion: "annotation-v1", AnnotationBasis: "AI inference", Notes: []NoteAnnotation{{ID: "note:suppress-me", Kind: "suggestion", Summary: "generated note", EvidenceTurnIDs: []string{"seg-note/rev-1/utt-1"}}}}
	if err = s.ApplyAnnotations(ctx, report.ConversationID, "", ann); err != nil {
		t.Fatal(err)
	}
	var noteID string
	if err = s.DB().QueryRow(`SELECT id FROM notes WHERE conversation_id=?`, report.ConversationID).Scan(&noteID); err != nil {
		t.Fatal(err)
	}
	if err = s.SuppressNote(ctx, report.ConversationID, noteID, "stale-version"); err == nil {
		t.Fatal("stale annotation version accepted")
	}
	opID, err := s.SuppressNoteOperation(ctx, report.ConversationID, noteID, "annotation-v1")
	if err != nil {
		t.Fatal(err)
	}
	op, err := s.SuppressionCleanupStatus(ctx, opID)
	if err != nil || op.Status != "pending" || op.NoteID != noteID || op.ConversationID != report.ConversationID {
		t.Fatalf("suppression cleanup operation not durable: %+v %v", op, err)
	}
	retriedID, err := s.SuppressNoteOperation(ctx, report.ConversationID, noteID, "stale-version")
	if err != nil || retriedID != opID {
		t.Fatalf("suppression retry did not return existing operation: %q %v", retriedID, err)
	}
	var notes, turns, suppressions int
	if err = s.DB().QueryRow(`SELECT COUNT(*) FROM notes WHERE id=?`, noteID).Scan(&notes); err != nil {
		t.Fatal(err)
	}
	if err = s.DB().QueryRow(`SELECT COUNT(*) FROM turns WHERE conversation_id=?`, report.ConversationID).Scan(&turns); err != nil {
		t.Fatal(err)
	}
	if err = s.DB().QueryRow(`SELECT COUNT(*) FROM ai_note_suppressions WHERE recording_id=? AND source_segment_id='seg-note' AND kind='suggestion'`, report.RecordingID).Scan(&suppressions); err != nil {
		t.Fatal(err)
	}
	if notes != 0 || turns != 1 || suppressions != 1 {
		t.Fatalf("notes=%d turns=%d suppressions=%d", notes, turns, suppressions)
	}
	var summary string
	if err = s.DB().QueryRow(`SELECT summary FROM conversations WHERE id=?`, report.ConversationID).Scan(&summary); err != nil {
		t.Fatal(err)
	}
	if summary != "" {
		t.Fatalf("stale summary retained: %q", summary)
	}
	if err = s.CompleteSuppressionCleanup(ctx, opID); err != nil {
		t.Fatal(err)
	}
	if err = s.CompleteSuppressionCleanup(ctx, opID); err != nil {
		t.Fatalf("completion should be idempotent: %v", err)
	}
	op, err = s.SuppressionCleanupStatus(ctx, opID)
	if err != nil || op.Status != "complete" {
		t.Fatalf("suppression cleanup did not complete: %+v %v", op, err)
	}
	revised := sampleResult("seg-note", "rev-2", 0)
	revised.Source.SupersedesRevision = "rev-1"
	revised.Source.ProviderRequestID = "req-seg-note-rev2"
	if _, err = s.IngestTranscript(ctx, revised); err != nil {
		t.Fatal(err)
	}
	var newTurn string
	if err = s.DB().QueryRow(`SELECT id FROM turns WHERE recording_id=? AND original_turn_id=?`, report.RecordingID, "seg-note/rev-2/utt-1").Scan(&newTurn); err != nil {
		t.Fatal(err)
	}
	evidence, _ := json.Marshal([]string{newTurn})
	if _, err = s.DB().Exec(`INSERT INTO memory_candidates(id,conversation_id,kind,summary,evidence_turn_ids_json,status,created_at) VALUES('candidate-suppressed',?,'suggestion','paraphrase',?,'validated',1)`, report.ConversationID, string(evidence)); err == nil {
		t.Fatal("same kind candidate crossed source suppression after reprocessing")
	}
}

func TestAnnotationEditAfterForgetReusesOnlyMatchingSanitizedHistory(t *testing.T) {
	ctx := context.Background()
	s := openTempStore(t)
	r := sampleResult("seg-edit", "rev-1", 0)
	start2, end2 := int64(910), int64(980)
	globalStart2, globalEnd2 := start2, end2
	r.Turns = append(r.Turns, asrinput.Turn{ID: "utt-2", OriginalIndex: 2, Text: "keep this", LocalStartMS: &start2, LocalEndMS: &end2, GlobalStartMS: &globalStart2, GlobalEndMS: &globalEnd2})
	report, err := s.IngestTranscript(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	ann := Annotations{ConversationID: report.ConversationID, Title: "History", Summary: "clear this", AnnotationVersion: "annotation-v1", AnnotationBasis: "AI inference", Notes: []NoteAnnotation{
		{ID: "note:forget", Kind: "observation", Summary: "remove", EvidenceTurnIDs: []string{"seg-edit/rev-1/utt-1"}},
		{ID: "note:keep", Kind: "suggestion", Summary: "keep", EvidenceTurnIDs: []string{"seg-edit/rev-1/utt-2"}},
	}}
	if err = s.ApplyAnnotations(ctx, report.ConversationID, "", ann); err != nil {
		t.Fatal(err)
	}
	var forgetTurnID string
	if err = s.DB().QueryRow(`SELECT id FROM turns WHERE conversation_id=? AND original_turn_id='seg-edit/rev-1/utt-1'`, report.ConversationID).Scan(&forgetTurnID); err != nil {
		t.Fatal(err)
	}
	p, err := s.PreviewForget(ctx, ForgetRequest{Scope: "turn", TurnID: forgetTurnID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CommitForget(ctx, ForgetRequest{Scope: "turn", TurnID: forgetTurnID, ExpectedState: p.ExpectedState}); err != nil {
		t.Fatal(err)
	}
	current, version, err := s.CurrentAnnotations(ctx, report.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	if len(current.Notes) != 1 || current.Notes[0].ID != "note:keep" {
		t.Fatalf("unexpected post-forget annotations: %+v", current.Notes)
	}
	current.Notes[0].Summary = "edited after forget"
	current.AnnotationVersion = "manual-after-forget"
	if err = s.ApplyAnnotations(ctx, report.ConversationID, version, current); err != nil {
		t.Fatalf("edit after forget should reuse identical sanitized history: %v", err)
	}
	var n int
	if err = s.DB().QueryRow(`SELECT COUNT(*) FROM annotation_history WHERE conversation_id=? AND annotation_version=?`, report.ConversationID, version).Scan(&n); err != nil || n != 1 {
		t.Fatalf("sanitized current version history rows=%d err=%v", n, err)
	}
}

func TestTurnForgetFenceBlocksRenamedOverlappingReingest(t *testing.T) {
	ctx := context.Background()
	s := openTempStore(t)
	original := sampleResult("seg-original", "rev-1", 0)
	report, err := s.IngestTranscript(ctx, original)
	if err != nil {
		t.Fatal(err)
	}
	var turnID string
	if err = s.DB().QueryRow(`SELECT id FROM turns WHERE conversation_id=?`, report.ConversationID).Scan(&turnID); err != nil {
		t.Fatal(err)
	}
	req := ForgetRequest{Scope: "turn", TurnID: turnID}
	p, err := s.PreviewForget(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	req.ExpectedState = p.ExpectedState
	if _, err = s.CommitForget(ctx, req); err != nil {
		t.Fatal(err)
	}
	replay := sampleResult("renamed-segment", "rev-2", 0)
	if _, e := s.IngestTranscript(ctx, replay); e == nil {
		t.Fatal("overlapping renamed source reingest passed granular tombstone")
	}
}

func TestRecordingForgetKeepsOtherRecordingNoteAndHistory(t *testing.T) {
	ctx := context.Background()
	s := openTempStore(t)
	one := sampleResult("seg-one", "rev-1", 0)
	r1, err := s.IngestTranscript(ctx, one)
	if err != nil {
		t.Fatal(err)
	}
	two := sampleResult("seg-two", "rev-1", 1000)
	two.Source.RecordingID = "rec-two"
	r2, err := s.IngestTranscript(ctx, two)
	if err != nil {
		t.Fatal(err)
	}
	ann := Annotations{ConversationID: r1.ConversationID, Title: "Shared", Summary: "two recordings", AnnotationVersion: "annotation-v1", AnnotationBasis: "AI inference", Notes: []NoteAnnotation{{ID: "note-one", Kind: "observation", Summary: "first source", EvidenceTurnIDs: []string{"seg-one/rev-1/utt-1"}}, {ID: "note-two", Kind: "suggestion", Summary: "second source", EvidenceTurnIDs: []string{"seg-two/rev-1/utt-1"}}}}
	if err = s.ApplyAnnotations(ctx, r1.ConversationID, "", ann); err != nil {
		t.Fatal(err)
	}
	req := ForgetRequest{Scope: "recording", RecordingID: r1.RecordingID}
	p, err := s.PreviewForget(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	req.ExpectedState = p.ExpectedState
	if _, err = s.CommitForget(ctx, req); err != nil {
		t.Fatal(err)
	}
	var n int
	if err = s.DB().QueryRow(`SELECT COUNT(*) FROM notes WHERE conversation_id=?`, r2.ConversationID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("surviving recording note count=%d", n)
	}
	var title, version string
	if err = s.DB().QueryRow(`SELECT title,annotation_version FROM conversations WHERE id=?`, r2.ConversationID).Scan(&title, &version); err != nil {
		t.Fatal(err)
	}
	if title != "Shared" || !strings.HasPrefix(version, "forget-") {
		t.Fatalf("current annotation revision title=%q version=%q", title, version)
	}
	var historyVersion string
	if err = s.DB().QueryRow(`SELECT annotation_version FROM annotation_history WHERE conversation_id=? AND annotation_version=?`, r2.ConversationID, version).Scan(&historyVersion); err != nil {
		t.Fatal(err)
	}
}

func TestProfileRetirementFencePurgesAndBlocksEveryIngress(t *testing.T) {
	ctx := context.Background()
	s := openTempStore(t)
	tr, ann := synthetic()
	if _, err := s.Import(ctx, tr, ann, "synthetic.wav", strings.Repeat("d", 64)); err != nil {
		t.Fatal(err)
	}
	if err := s.BeginProfileRetirement(ctx, "retire-test"); err != nil {
		t.Fatal(err)
	}
	if err := s.BeginProfileRetirement(ctx, "retire-test"); err != nil {
		t.Fatal("begin retirement was not idempotent:", err)
	}
	if _, err := s.DB().Exec(`UPDATE conversations SET title='resurrected' WHERE id=?`, ann.ConversationID); err == nil {
		t.Fatal("retired profile accepted canonical update")
	}
	if err := s.PurgeRetiredProfile(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.PurgeRetiredProfile(ctx); err != nil {
		t.Fatal("purge retry failed:", err)
	}
	for _, table := range []string{"conversations", "recordings", "turns", "notes", "source_attachments", "processing_jobs", "sync_recordings", "forget_tombstones"} {
		var n int
		if err := s.DB().QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("retired profile retained %d rows in %s", n, table)
		}
	}
	id, retired, err := s.ProfileRetirement(ctx)
	if err != nil || id != "retire-test" || !retired {
		t.Fatalf("retirement marker lost: %q %v %v", id, retired, err)
	}
	if _, err = s.DB().Exec(`INSERT INTO sync_recordings(recording_id,owner,device_id,state,updated_at) VALUES('new','owner','dev','downloading',1)`); err == nil {
		t.Fatal("retired profile accepted sync ingress")
	}
}

func TestForgetPreviewHashIncludesSyncChunksAndUsesPCMOffsets(t *testing.T) {
	ctx := context.Background()
	s := openTempStore(t)
	result := sampleResult("seg-sync", "rev-1", 0)
	report, err := s.IngestTranscript(ctx, result)
	if err != nil {
		t.Fatal(err)
	}
	// Sync ledgers use the provider's recording ID from source_json, while
	// canonical recordings use the internal stable ID returned by ingestion.
	if _, err = s.DB().Exec(`INSERT INTO sync_recordings(recording_id,owner,device_id,state,total_samples,stitched_path,updated_at) VALUES('rec-1', 'owner','device','ingested',16000,'/audio/stitched.pcm',1)`); err != nil {
		t.Fatal(err)
	}
	var turnID string
	if err = s.DB().QueryRow(`SELECT id FROM turns WHERE recording_id=?`, report.RecordingID).Scan(&turnID); err != nil {
		t.Fatal(err)
	}
	req := ForgetRequest{Scope: "turn", TurnID: turnID}
	preview, err := s.PreviewForget(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if preview.RequiresBroadenedAudio {
		t.Fatal("known 16kHz stitched PCM range unexpectedly needs broadening")
	}
	if _, err = s.DB().Exec(`INSERT INTO sync_chunks(recording_id,seq,sha256,bytes,file_path,downloaded_at) VALUES('rec-1',0,'chunk-hash',32000,'/audio/chunk-0.pcm',1)`); err != nil {
		t.Fatal(err)
	}
	req.ExpectedState = preview.ExpectedState
	if _, err = s.CommitForget(ctx, req); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("changed chunk manifest did not invalidate preview: %v", err)
	}
}
