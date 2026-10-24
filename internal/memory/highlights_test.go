package memory

import (
	"context"
	"errors"
	"testing"
)

func TestHighlightCreateReplayVersionAndDelete(t *testing.T) {
	ctx := context.Background()
	s := openTempStore(t)
	report, err := s.IngestTranscript(ctx, sampleResult("seg-highlight", "rev-1", 0))
	if err != nil {
		t.Fatal(err)
	}
	attachmentID := stableKey("attachment", report.RecordingID, "seg-highlight", "rev-1")
	start, end := int64(20), int64(80)
	input := HighlightCreate{RequestID: "req-highlight-1", RecordingID: report.RecordingID, AnchorKind: "range", StartMS: &start, EndMS: &end, SourceAttachmentID: attachmentID, TranscriptRevision: "rev-1", Tag: "important", NoteText: "keep this"}
	created, err := s.CreateHighlight(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if created.State != "current" || created.Version != 1 || created.ConversationID != report.ConversationID {
		t.Fatalf("unexpected highlight: %+v", created)
	}
	replay, err := s.CreateHighlight(ctx, input)
	if err != nil || replay.ID != created.ID {
		t.Fatalf("same request did not replay: %+v %v", replay, err)
	}
	input.NoteText = "different payload"
	if _, err := s.CreateHighlight(ctx, input); !errors.Is(err, ErrHighlightRequestConflict) {
		t.Fatalf("changed request payload: %v", err)
	}
	if _, err := s.UpdateHighlight(ctx, created.ID, HighlightUpdate{ExpectedVersion: 1, NoteText: stringPtr("updated")}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateHighlight(ctx, created.ID, HighlightUpdate{ExpectedVersion: 1, NoteText: stringPtr("lost update")}); !errors.Is(err, ErrHighlightVersionConflict) {
		t.Fatalf("stale version accepted: %v", err)
	}
	if err := s.DeleteHighlight(ctx, created.ID, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateHighlight(ctx, HighlightCreate{RequestID: "req-highlight-1", RecordingID: report.RecordingID, AnchorKind: "range", StartMS: &start, EndMS: &end, SourceAttachmentID: attachmentID, TranscriptRevision: "rev-1", Tag: "important", NoteText: "keep this"}); !errors.Is(err, ErrHighlightGone) {
		t.Fatalf("deleted request revived: %v", err)
	}
	if err := s.BeginProfileRetirement(ctx, "retire-highlights"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateHighlight(ctx, HighlightCreate{RequestID: "req-highlight-1", RecordingID: report.RecordingID, AnchorKind: "range", StartMS: &start, EndMS: &end, SourceAttachmentID: attachmentID, TranscriptRevision: "rev-1", Tag: "important", NoteText: "keep this"}); !errors.Is(err, ErrProfileRetired) {
		t.Fatalf("retired replay was not fenced: %v", err)
	}
}

func TestHighlightRequiresCurrentMatchingEvidence(t *testing.T) {
	ctx := context.Background()
	s := openTempStore(t)
	report, err := s.IngestTranscript(ctx, sampleResult("seg-highlight", "rev-1", 0))
	if err != nil {
		t.Fatal(err)
	}
	attachmentID := stableKey("attachment", report.RecordingID, "seg-highlight", "rev-1")
	turnID := stableKey("turn", report.RecordingID, "seg-highlight/rev-1/utt-1")
	start, end := int64(20), int64(80)
	input := HighlightCreate{RequestID: "req-turn-highlight", RecordingID: report.RecordingID, AnchorKind: "turn", StartMS: &start, EndMS: &end, TurnID: turnID, SourceAttachmentID: attachmentID, TranscriptRevision: "rev-1", Tag: "question"}
	if _, err := s.CreateHighlight(ctx, input); err != nil {
		t.Fatal(err)
	}
	input.RequestID = "req-wrong-turn"
	input.TurnID = "other-recording/turn"
	if _, err := s.CreateHighlight(ctx, input); !errors.Is(err, ErrHighlightSourceMismatch) {
		t.Fatalf("foreign turn accepted: %v", err)
	}
	newRevision := sampleResult("seg-highlight", "rev-2", 0)
	newRevision.Source.SupersedesRevision = "rev-1"
	_, err = s.IngestTranscript(ctx, newRevision)
	if err != nil {
		t.Fatal(err)
	}
	items, err := s.ListHighlights(ctx, report.RecordingID)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].State != "needs_relink" || items[0].QuoteText != nil {
		t.Fatalf("stale evidence was quoted: %+v", items)
	}
	newAttachmentID := stableKey("attachment", report.RecordingID, "seg-highlight", "rev-2")
	newTurnID := stableKey("turn", report.RecordingID, "seg-highlight/rev-2/utt-1")
	relinked, err := s.UpdateHighlight(ctx, items[0].ID, HighlightUpdate{ExpectedVersion: items[0].Version, Relink: &HighlightRelink{TurnID: newTurnID, SourceAttachmentID: newAttachmentID, TranscriptRevision: "rev-2"}})
	if err != nil || relinked.State != "current" || relinked.StartMS == nil || *relinked.StartMS != start || relinked.QuoteText == nil {
		t.Fatalf("relink did not preserve and refresh evidence: %+v %v", relinked, err)
	}
}

func TestHighlightForgetPurgesOverlapAndScrubsRequestHash(t *testing.T) {
	ctx := context.Background()
	s := openTempStore(t)
	report, err := s.IngestTranscript(ctx, sampleResult("seg-highlight", "rev-1", 0))
	if err != nil {
		t.Fatal(err)
	}
	attachmentID := stableKey("attachment", report.RecordingID, "seg-highlight", "rev-1")
	turnID := stableKey("turn", report.RecordingID, "seg-highlight/rev-1/utt-1")
	start, end := int64(30), int64(60)
	input := HighlightCreate{RequestID: "req-forget-highlight", RecordingID: report.RecordingID, AnchorKind: "range", StartMS: &start, EndMS: &end, TurnID: turnID, SourceAttachmentID: attachmentID, TranscriptRevision: "rev-1", Tag: "follow_up", NoteText: "erase this"}
	h, err := s.CreateHighlight(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	req := ForgetRequest{Scope: "turn", TurnID: turnID}
	preview, err := s.PreviewForget(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Highlights != 1 {
		t.Fatalf("preview counted %d highlights", preview.Highlights)
	}
	req.ExpectedState = preview.ExpectedState
	if _, err = s.CommitForget(ctx, req); err != nil {
		t.Fatal(err)
	}
	if _, err = s.GetHighlight(ctx, h.ID); !errors.Is(err, ErrHighlightGone) {
		t.Fatalf("forgotten highlight survived: %v", err)
	}
	var status, hash string
	if err = s.DB().QueryRow(`SELECT status,payload_sha256 FROM memory_highlight_requests WHERE request_id=?`, input.RequestID).Scan(&status, &hash); err != nil || status != "forgotten" || hash != "" {
		t.Fatalf("request ledger not scrubbed: %q %q %v", status, hash, err)
	}
	point := int64(950)
	outside := HighlightCreate{RequestID: "req-after-outside-forget", RecordingID: report.RecordingID, AnchorKind: "point", StartMS: &point, EndMS: &point, SourceAttachmentID: attachmentID, TranscriptRevision: "rev-1", Tag: "question"}
	if _, err = s.CreateHighlight(ctx, outside); err != nil {
		t.Fatalf("unrelated point was fenced: %v", err)
	}
}

func stringPtr(v string) *string { return &v }

func TestHighlightsTimeOnlyForgetPreviewCASAndRetirement(t *testing.T) {
	ctx := context.Background()
	s := openTempStore(t)
	report, e := s.IngestTranscript(ctx, sampleResult("seg-highlight", "rev-1", 0))
	if e != nil {
		t.Fatal(e)
	}
	att := stableKey("attachment", report.RecordingID, "seg-highlight", "rev-1")
	turn := stableKey("turn", report.RecordingID, "seg-highlight/rev-1/utt-1")
	start, end := int64(30), int64(60)
	input := HighlightCreate{RequestID: "time-only", RecordingID: report.RecordingID, AnchorKind: "range", StartMS: &start, EndMS: &end, SourceAttachmentID: att, TranscriptRevision: "rev-1", Tag: "important", NoteText: "synthetic time-only secret"}
	h, e := s.CreateHighlight(ctx, input)
	if e != nil {
		t.Fatal(e)
	}
	point := int64(30)
	pointIn := input
	pointIn.RequestID = "point-only"
	pointIn.AnchorKind = "point"
	pointIn.StartMS = &point
	pointIn.EndMS = nil
	if _, e = s.CreateHighlight(ctx, pointIn); e != nil {
		t.Fatal(e)
	}
	req := ForgetRequest{Scope: "turn", TurnID: turn}
	p, e := s.PreviewForget(ctx, req)
	if e != nil {
		t.Fatal(e)
	}
	if p.Highlights != 2 {
		t.Fatalf("time-only overlap omitted: %+v", p)
	}
	if _, e = s.UpdateHighlight(ctx, h.ID, HighlightUpdate{ExpectedVersion: 1, NoteText: stringPtr("changed secret")}); e != nil {
		t.Fatal(e)
	}
	req.ExpectedState = p.ExpectedState
	if _, e = s.CommitForget(ctx, req); e == nil {
		t.Fatal("stale preview accepted after marker update")
	}
	p, e = s.PreviewForget(ctx, req)
	if e != nil {
		t.Fatal(e)
	}
	req.ExpectedState = p.ExpectedState
	if _, e = s.CommitForget(ctx, req); e != nil {
		t.Fatal(e)
	}
	list, e := s.ListHighlights(ctx, report.RecordingID)
	if e != nil || len(list) != 0 {
		t.Fatalf("time anchors survived: %+v %v", list, e)
	}
	input.RequestID = "new-request-for-forgotten"
	if _, e = s.CreateHighlight(ctx, input); !errors.Is(e, ErrHighlightSourceForgotten) {
		t.Fatalf("new request bypassed tombstone: %v", e)
	}
	if e = s.BeginProfileRetirement(ctx, "retired-h"); e != nil {
		t.Fatal(e)
	}
	if _, e = s.CreateHighlight(ctx, input); !errors.Is(e, ErrProfileRetired) {
		t.Fatalf("retired create: %v", e)
	}
	if e = s.PurgeRetiredProfile(ctx); e != nil {
		t.Fatal(e)
	}
	for _, table := range []string{"memory_highlights", "memory_highlight_requests"} {
		var n int
		if e = s.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); e != nil || n != 0 {
			t.Fatalf("retired residual %s: %d %v", table, n, e)
		}
	}
}

func TestHighlightConcurrentCASDoesNotLoseUpdate(t *testing.T) {
	ctx := context.Background()
	s := openTempStore(t)
	r, e := s.IngestTranscript(ctx, sampleResult("seg-highlight", "rev-1", 0))
	if e != nil {
		t.Fatal(e)
	}
	start, end := int64(20), int64(80)
	h, e := s.CreateHighlight(ctx, HighlightCreate{RequestID: "concurrent", RecordingID: r.RecordingID, AnchorKind: "range", StartMS: &start, EndMS: &end, SourceAttachmentID: stableKey("attachment", r.RecordingID, "seg-highlight", "rev-1"), TranscriptRevision: "rev-1", Tag: "important"})
	if e != nil {
		t.Fatal(e)
	}
	results := make(chan error, 2)
	for _, note := range []string{"writer-one", "writer-two"} {
		go func(v string) {
			_, err := s.UpdateHighlight(ctx, h.ID, HighlightUpdate{ExpectedVersion: 1, NoteText: &v})
			results <- err
		}(note)
	}
	successes, conflicts := 0, 0
	for i := 0; i < 2; i++ {
		err := <-results
		if err == nil {
			successes++
		} else if errors.Is(err, ErrHighlightVersionConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("success %d conflict %d", successes, conflicts)
	}
	got, e := s.GetHighlight(ctx, h.ID)
	if e != nil || got.Version != 2 {
		t.Fatalf("version %+v %v", got, e)
	}
}

func TestHighlightConversationForgetCannotReplay(t *testing.T) {
	ctx := context.Background()
	s := openTempStore(t)
	r, e := s.IngestTranscript(ctx, sampleResult("seg-highlight", "rev-1", 0))
	if e != nil {
		t.Fatal(e)
	}
	point := int64(20)
	in := HighlightCreate{RequestID: "conversation-forget", RecordingID: r.RecordingID, AnchorKind: "point", StartMS: &point, SourceAttachmentID: stableKey("attachment", r.RecordingID, "seg-highlight", "rev-1"), TranscriptRevision: "rev-1", Tag: "question", NoteText: "synthetic conversation secret"}
	if _, e = s.CreateHighlight(ctx, in); e != nil {
		t.Fatal(e)
	}
	req := ForgetRequest{Scope: "conversation", ConversationID: r.ConversationID}
	p, e := s.PreviewForget(ctx, req)
	if e != nil {
		t.Fatal(e)
	}
	req.ExpectedState = p.ExpectedState
	if _, e = s.CommitForget(ctx, req); e != nil {
		t.Fatal(e)
	}
	if _, e = s.CreateHighlight(ctx, in); !errors.Is(e, ErrHighlightGone) {
		t.Fatalf("replay after conversation forget: %v", e)
	}
	in.RequestID = "fresh-after-conversation-forget"
	if _, e = s.CreateHighlight(ctx, in); !errors.Is(e, ErrHighlightSourceForgotten) {
		t.Fatalf("new request after conversation forget: %v", e)
	}
}

func TestHighlightCASAcrossIndependentStoreConnections(t *testing.T) {
	ctx := context.Background()
	s := openTempStore(t)
	var seq int
	var name, path string
	if e := s.DB().QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &path); e != nil {
		t.Fatal(e)
	}
	other, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer other.Close()
	r, e := s.IngestTranscript(ctx, sampleResult("seg-highlight", "rev-1", 0))
	if e != nil {
		t.Fatal(e)
	}
	start, end := int64(20), int64(80)
	h, e := s.CreateHighlight(ctx, HighlightCreate{RequestID: "two-connections", RecordingID: r.RecordingID, AnchorKind: "range", StartMS: &start, EndMS: &end, SourceAttachmentID: stableKey("attachment", r.RecordingID, "seg-highlight", "rev-1"), TranscriptRevision: "rev-1", Tag: "important"})
	if e != nil {
		t.Fatal(e)
	}
	for version := int64(1); version <= 10; version++ {
		gate := make(chan struct{})
		results := make(chan error, 2)
		for _, store := range []*Store{s, other} {
			go func(store *Store) {
				<-gate
				_, err := store.UpdateHighlight(ctx, h.ID, HighlightUpdate{ExpectedVersion: version, NoteText: stringPtr("synthetic concurrent update")})
				results <- err
			}(store)
		}
		close(gate)
		success, conflict := 0, 0
		for i := 0; i < 2; i++ {
			err := <-results
			if err == nil {
				success++
			} else if errors.Is(err, ErrHighlightVersionConflict) {
				conflict++
			} else {
				t.Fatalf("concurrent store update: %v", err)
			}
		}
		if success != 1 || conflict != 1 {
			t.Fatalf("CAS across connections: success=%d conflict=%d", success, conflict)
		}
	}
}
