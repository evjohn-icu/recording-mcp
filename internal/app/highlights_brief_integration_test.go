package app

import (
	"context"
	"encoding/json"
	"path/filepath"
	"recordingmcp/internal/contextbrief"
	"recordingmcp/internal/memory"
	"strings"
	"testing"
)

func TestHighlightsBriefRevisionForgetSyntheticJourney(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	pcm := filepath.Join(f.audioDir, "journey.pcm")
	writeAudioFile(t, pcm, 30000)
	f.insertSync(t, "ingested", pcm)
	_, e := f.store.DB().Exec(`UPDATE sync_recordings SET total_samples=480000`)
	if e != nil {
		t.Fatal(e)
	}
	turn := f.turnRowID(t, "seg-a", "t-1")
	src, e := f.store.TranscriptSourceForTurn(ctx, turn)
	if e != nil {
		t.Fatal(e)
	}
	start, end := int64(1000), int64(2000)
	input := memory.HighlightCreate{RequestID: "journey", RecordingID: f.recordingID, AnchorKind: "turn", StartMS: &start, EndMS: &end, TurnID: turn, SourceAttachmentID: src.SourceAttachmentID, TranscriptRevision: src.Result.Source.TranscriptRevision, Tag: "follow_up", NoteText: "synthetic private follow-up marker"}
	h, e := f.svc.CreateHighlight(ctx, input)
	if e != nil {
		t.Fatal(e)
	}
	replay, e := f.svc.CreateHighlight(ctx, input)
	if e != nil || replay.ID != h.ID {
		t.Fatalf("replay %v %+v", e, replay)
	}
	req := contextbrief.Request{Scope: contextbrief.Scope{Kind: "conversation", ConversationIDs: []string{f.conversationID}}}
	b, e := f.svc.BuildContextBrief(ctx, req)
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(b)
	if !strings.Contains(string(raw), input.NoteText) {
		t.Fatalf("highlight absent from brief: %s", raw)
	}
	for _, entry := range b.Sections.FollowUpCandidates {
		if entry.AssertionState != "proposed" {
			t.Fatalf("followup confirmed: %+v", entry)
		}
	}
	c, e := f.svc.HighlightAudioClip(ctx, h.ID)
	if e != nil {
		t.Fatal(e)
	}
	c.Close()
	// A second independent profile has identical source IDs but no marker.
	other := newFixture(t)
	if _, e = other.svc.GetHighlight(ctx, h.ID); e == nil {
		t.Fatal("cross-profile marker read")
	}
	updates := map[string]string{}
	for _, tr := range src.Result.Turns {
		updates[tr.ID] = "synthetic corrected quote " + tr.ID
	}
	if _, e = f.store.CorrectTranscriptSegment(ctx, f.conversationID, f.recordingID, src.SegmentID, src.Result.Source.TranscriptRevision, "journey-v2", updates); e != nil {
		t.Fatal(e)
	}
	stale, e := f.svc.GetHighlight(ctx, h.ID)
	if e != nil || stale.State != "needs_relink" || stale.QuoteText != nil {
		t.Fatalf("stale evidence exposed: %+v %v", stale, e)
	}
	next, e := f.svc.BuildContextBrief(ctx, req)
	if e != nil {
		t.Fatal(e)
	}
	raw, _ = json.Marshal(next)
	if strings.Contains(string(raw), input.NoteText) || strings.Contains(string(raw), "第一条原始转写") {
		t.Fatalf("old evidence remained: %s", raw)
	}
	if next.SnapshotID == b.SnapshotID {
		t.Fatal("revision did not change snapshot")
	}
	forget := memory.ForgetRequest{Scope: "recording", RecordingID: f.recordingID}
	preview, e := f.svc.PreviewForget(ctx, forget)
	if e != nil {
		t.Fatal(e)
	}
	forget.ExpectedState = preview.ExpectedState
	op, e := f.svc.CommitForget(ctx, forget)
	if e != nil || op.CleanupPending {
		t.Fatalf("forget incomplete: %+v %v", op, e)
	}
	if _, e = f.svc.CreateHighlight(ctx, input); e == nil {
		t.Fatal("forgotten replay revived")
	}
	after, e := f.svc.BuildContextBrief(ctx, req)
	if e != nil {
		t.Fatal(e)
	}
	if len(after.Evidence) != 0 {
		t.Fatal("forgotten evidence in brief")
	}
	var n int
	if e = f.store.DB().QueryRow(`SELECT COUNT(*) FROM memory_highlights`).Scan(&n); e != nil || n != 0 {
		t.Fatalf("marker residual: %d %v", n, e)
	}
	var hash string
	if e = f.store.DB().QueryRow(`SELECT payload_sha256 FROM memory_highlight_requests WHERE request_id='journey'`).Scan(&hash); e != nil || hash != "" {
		t.Fatalf("request content digest residual: %q %v", hash, e)
	}
}

func TestBriefHardTopicScopeMixedEvidenceAndConflictPreserved(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	a := f.turnRowID(t, "seg-a", "t-1")
	b := f.turnRowID(t, "seg-a", "t-2")
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := f.store.DB().Exec(q, args...); e != nil {
			t.Fatal(e)
		}
	}
	exec(`INSERT OR IGNORE INTO turn_topics(turn_id,topic_id) VALUES(?, 'topic:a')`, a)
	exec(`DELETE FROM turn_topics WHERE turn_id=?`, b)
	for _, kind := range []string{"request", "intention", "constraint"} {
		exec(`INSERT INTO notes(id,conversation_id,kind,summary,topic_ids_json,claim_mode,interpretation_note,annotation_version) VALUES(?,?,?,?,'[]','inference','','v1')`, "brief-"+kind, f.conversationID, kind, "synthetic "+kind)
		exec(`INSERT INTO note_evidence(note_id,turn_id,ordinal) VALUES(?,?,0)`, "brief-"+kind, a)
	}
	exec(`INSERT INTO notes(id,conversation_id,kind,summary,topic_ids_json,claim_mode,interpretation_note,annotation_version) VALUES('brief-mixed',?,'observation','OUTSIDE-EVIDENCE-SUMMARY','[]','inference','','v1')`, f.conversationID)
	exec(`INSERT INTO note_evidence(note_id,turn_id,ordinal) VALUES('brief-mixed',?,0),('brief-mixed',?,1)`, a, b)
	ev, _ := json.Marshal([]string{a, b})
	exec(`INSERT INTO memory_links(id,from_kind,from_id,to_kind,to_id,relation,reason,evidence_turn_ids_json,confidence,status,model,created_at) VALUES('brief-conflict','turn',?,'turn',?,'contradiction','双方原话均保留，不裁决',?,0.5,'proposed','synthetic',1)`, a, b, string(ev))
	topic, e := f.svc.BuildContextBrief(ctx, contextbrief.Request{Scope: contextbrief.Scope{Kind: "topic", TopicIDs: []string{"topic:a"}}})
	if e != nil {
		t.Fatal(e)
	}
	for _, x := range topic.Evidence {
		if x.TurnID == b {
			t.Fatal("topic leaked outside evidence")
		}
	}
	raw, _ := json.Marshal(topic)
	if strings.Contains(string(raw), "OUTSIDE-EVIDENCE-SUMMARY") || strings.Contains(string(raw), "双方原话均保留") {
		t.Fatalf("mixed summary leaked: %s", raw)
	}
	counts := map[string]bool{}
	for _, x := range topic.Sections.FollowUpCandidates {
		if strings.HasPrefix(x.Text, "synthetic ") {
			if x.AssertionState != "proposed" {
				t.Fatalf("candidate confirmed: %+v", x)
			}
			counts[x.Text] = true
		}
	}
	if len(counts) != 3 {
		t.Fatalf("missing proposed kinds: %v", counts)
	}
	all, e := f.svc.BuildContextBrief(ctx, contextbrief.Request{Scope: contextbrief.Scope{Kind: "conversation", ConversationIDs: []string{f.conversationID}}})
	if e != nil {
		t.Fatal(e)
	}
	if len(all.Sections.ChangesConflicts) != 1 || len(all.Sections.ChangesConflicts[0].EvidenceIDs) != 2 {
		t.Fatalf("conflict evidence missing: %+v", all.Sections.ChangesConflicts)
	}
	for _, x := range all.Sections.ChangesConflicts {
		if x.AssertionState == "explicitly_confirmed" {
			t.Fatal("conflict resolved as fact")
		}
	}
}
