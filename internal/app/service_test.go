package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"recordingmcp/internal/memory"
)

func appErr(t *testing.T, err error) *Error {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	e, ok := AsError(err)
	if !ok {
		t.Fatalf("expected an app error, got %T: %v", err, err)
	}
	return e
}

func TestConversationViewUnknownTimeListsNotesAndEvidence(t *testing.T) {
	f := newFixture(t)
	view, err := f.svc.ConversationView(context.Background(), f.conversationID)
	if err != nil {
		t.Fatal(err)
	}
	if view.RecordingStartedAt != nil {
		t.Fatalf("unknown time must stay nil, got %v", view.RecordingStartedAt)
	}
	if view.Title != "测试会话" || view.Summary != "两条待确认的归纳" || view.AnnotationVersion != "v1" {
		t.Fatalf("unexpected header: %+v", view)
	}
	if len(view.Recordings) != 1 || view.Recordings[0].RecordingID != f.recordingID {
		t.Fatalf("unexpected recordings: %+v", view.Recordings)
	}
	if view.Recordings[0].LocalAudio != AudioUnavailable {
		t.Fatalf("no sync row must be unavailable, got %q", view.Recordings[0].LocalAudio)
	}
	if len(view.Topics) != 1 || len(view.Notes) != 2 {
		t.Fatalf("unexpected notes/topics: %+v", view)
	}
	first := view.Notes[0]
	if first.NoteID != "note:one" || first.RowID != memory.NoteRowID(f.conversationID, "note:one") {
		t.Fatalf("logical id mapping is wrong: %+v", first)
	}
	if len(first.EvidenceTurnIDs) != 1 || first.EvidenceTurnIDs[0] != f.turnRowID(t, "seg-a", "t-1") {
		t.Fatalf("evidence ids are not resolved to stored turns: %+v", first.EvidenceTurnIDs)
	}
	if first.AnnotationVersion != "v1" || first.Edited {
		t.Fatalf("unexpected note metadata: %+v", first)
	}
	if view.Processing.AnalysisStatus != "partial" {
		t.Fatalf("unexpected status: %+v", view.Processing)
	}
}

func TestConversationViewDropsStaleNotes(t *testing.T) {
	f := newFixture(t)
	rowID := memory.NoteRowID(f.conversationID, "note:one")
	if _, err := f.store.DB().Exec(`UPDATE derived_artifacts SET stale=1 WHERE kind='note' AND artifact_id=?`, rowID); err != nil {
		t.Fatal(err)
	}
	view, err := f.svc.ConversationView(context.Background(), f.conversationID)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Notes) != 1 || view.Notes[0].NoteID != "note:two" {
		t.Fatalf("a stale note must not be shown as current: %+v", view.Notes)
	}
}

func TestConversationViewKnownTimeAndMissingConversation(t *testing.T) {
	started := testStartedAt
	f := newFixtureWithStart(t, &started)
	view, err := f.svc.ConversationView(context.Background(), f.conversationID)
	if err != nil {
		t.Fatal(err)
	}
	if view.RecordingStartedAt == nil || !view.Recordings[0].AbsoluteTimeKnown {
		t.Fatalf("known start time lost: %+v", view)
	}
	if _, err := f.svc.ConversationView(context.Background(), "conv:nope"); err == nil {
		t.Fatal("missing conversation accepted")
	} else if e := appErr(t, err); e.Status != 404 || e.Code != CodeNotFound {
		t.Fatalf("unexpected error: %+v", e)
	}
}

func TestEditNotePreservesOtherNotesAndLogicalID(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	rowID := memory.NoteRowID(f.conversationID, "note:one")

	before, err := f.svc.AnnotationHistory(ctx, f.conversationID)
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.svc.EditNote(ctx, f.conversationID, rowID, "v1", "web-1", "  人工改写的归纳  ")
	if err != nil {
		t.Fatal(err)
	}
	if res.AnnotationVersion != "web-1" || res.NoteID != "note:one" || res.RowID != rowID {
		t.Fatalf("unexpected edit result: %+v", res)
	}
	view, err := f.svc.ConversationView(ctx, f.conversationID)
	if err != nil {
		t.Fatal(err)
	}
	if view.AnnotationVersion != "web-1" || len(view.Notes) != 2 {
		t.Fatalf("edit lost notes: %+v", view)
	}
	edited := view.Notes[0]
	if edited.Summary != "人工改写的归纳" || !edited.Edited || edited.Kind != "suggestion" {
		t.Fatalf("unexpected edited note: %+v", edited)
	}
	if len(edited.EvidenceTurnIDs) != 1 || len(edited.TopicIDs) != 1 {
		t.Fatalf("edit dropped evidence or topics: %+v", edited)
	}
	if view.Notes[1].Summary != "第二条归纳" || view.Notes[1].Edited {
		t.Fatalf("sibling note changed: %+v", view.Notes[1])
	}
	history, err := f.svc.AnnotationHistory(ctx, f.conversationID)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != len(before)+1 {
		t.Fatalf("history should grow by one revision: before=%d after=%d", len(before), len(history))
	}
	if !history[len(history)-1].Current || history[len(history)-1].Version != "web-1" {
		t.Fatalf("current revision is not last: %+v", history[len(history)-1])
	}

	// A second edit keeps the same logical id and therefore the same row id.
	res, err = f.svc.EditNote(ctx, f.conversationID, rowID, "web-1", "web-2", "再改一次")
	if err != nil {
		t.Fatal(err)
	}
	if res.NoteID != "note:one" || res.RowID != rowID {
		t.Fatalf("note identity is not stable across edits: %+v", res)
	}
}

func TestEditNoteConflictReportsCurrentVersionAndNote(t *testing.T) {
	f := newFixture(t)
	rowID := memory.NoteRowID(f.conversationID, "note:one")
	_, err := f.svc.EditNote(context.Background(), f.conversationID, rowID, "stale-version", "web-1", "我的改写")
	e := appErr(t, err)
	if e.Status != 409 || e.Code != CodeAnnotationVersionConflict {
		t.Fatalf("unexpected error: %+v", e)
	}
	if e.CurrentVersion != "v1" {
		t.Fatalf("conflict must report the stored version: %+v", e)
	}
	if e.CurrentNote == nil || e.CurrentNote.Summary != "第一条归纳" || e.CurrentNote.RowID != rowID {
		t.Fatalf("conflict must carry the current note: %+v", e.CurrentNote)
	}
	if len(e.Current.Notes) != 2 {
		t.Fatalf("conflict must carry the current logical view: %+v", e.Current)
	}
}

func TestEditNoteRetryIsIdempotent(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	rowID := memory.NoteRowID(f.conversationID, "note:one")
	if _, err := f.svc.EditNote(ctx, f.conversationID, rowID, "v1", "web-1", "最终措辞"); err != nil {
		t.Fatal(err)
	}
	historyAfterFirst, err := f.svc.AnnotationHistory(ctx, f.conversationID)
	if err != nil {
		t.Fatal(err)
	}
	// Exact replay of the committed edit: same key, same wording.
	res, err := f.svc.EditNote(ctx, f.conversationID, rowID, "v1", "web-1", "最终措辞")
	if err != nil {
		t.Fatalf("replay must succeed: %v", err)
	}
	if res.AnnotationVersion != "web-1" || res.NoteID != "note:one" {
		t.Fatalf("unexpected replay result: %+v", res)
	}
	historyAfterReplay, err := f.svc.AnnotationHistory(ctx, f.conversationID)
	if err != nil {
		t.Fatal(err)
	}
	if len(historyAfterReplay) != len(historyAfterFirst) {
		t.Fatalf("replay added history: %d -> %d", len(historyAfterFirst), len(historyAfterReplay))
	}
	var jobs int
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM processing_jobs WHERE stage='embed' AND idempotency_key=?`,
		"embed|"+f.conversationID+"|web-1").Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if jobs != 1 {
		t.Fatalf("replay must not enqueue a second embed job, got %d", jobs)
	}
}

func TestEditNoteReusedVersionIsConflict(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	rowID := memory.NoteRowID(f.conversationID, "note:one")
	if _, err := f.svc.EditNote(ctx, f.conversationID, rowID, "v1", "web-1", "第一次"); err != nil {
		t.Fatal(err)
	}
	// v1 already exists in history, so reusing it would rewrite the archive.
	_, err := f.svc.EditNote(ctx, f.conversationID, rowID, "web-1", "v1", "第二次")
	e := appErr(t, err)
	if e.Status != 409 || e.Code != CodeAnnotationVersionConflict {
		t.Fatalf("reused version must be a conflict, got %+v", e)
	}
	if e.CurrentVersion != "web-1" || e.CurrentNote == nil || e.CurrentNote.Summary != "第一次" {
		t.Fatalf("conflict payload is wrong: %+v", e)
	}
}

func TestEditNoteRejectsInvalidInput(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	rowID := memory.NoteRowID(f.conversationID, "note:one")
	cases := []struct {
		name                             string
		rowID, expected, newVersion, sum string
		code                             string
		status                           int
	}{
		{"unknown note", memory.NoteRowID(f.conversationID, "note:nope"), "v1", "web-1", "x", CodeNoteNotFound, 404},
		{"empty summary", rowID, "v1", "web-1", "   ", CodeValidationFailed, 400},
		{"same version", rowID, "v1", "v1", "x", CodeValidationFailed, 400},
		{"control characters", rowID, "v1", "web-1\n", "x", CodeValidationFailed, 400},
		{"too long", rowID, "v1", "web-1", strings.Repeat("长", 4001), CodeValidationFailed, 400},
	}
	for _, tc := range cases {
		_, err := f.svc.EditNote(ctx, f.conversationID, tc.rowID, tc.expected, tc.newVersion, tc.sum)
		if err == nil {
			t.Fatalf("%s: accepted", tc.name)
		}
		e, ok := AsError(err)
		if !ok {
			t.Fatalf("%s: unexpected error type %T: %v", tc.name, err, err)
		}
		if e.Code != tc.code || e.Status != tc.status {
			t.Fatalf("%s: got %s/%d want %s/%d (%v)", tc.name, e.Code, e.Status, tc.code, tc.status, err)
		}
	}
}

func TestTurnPageOffsetCountsOnlyTurns(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	first, err := f.svc.TurnPage(ctx, f.conversationID, f.recordingID, false, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Items) != 1 || first.Items[0].TurnID != originalTurnID("seg-a", "t-1") || first.NextOffset == nil || *first.NextOffset != 1 {
		t.Fatalf("first turn page includes mixed-query offset effects: %+v", first)
	}
	second, err := f.svc.TurnPage(ctx, f.conversationID, f.recordingID, false, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Items) != 1 || second.Items[0].TurnID != originalTurnID("seg-a", "t-2") || second.NextOffset == nil || *second.NextOffset != 2 {
		t.Fatalf("second turn page skipped/duplicated a turn: %+v", second)
	}
	last, err := f.svc.TurnPage(ctx, f.conversationID, f.recordingID, false, 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(last.Items) != 1 || last.Items[0].TurnID != originalTurnID("seg-b", "t-3") || last.NextOffset != nil {
		t.Fatalf("last turn page incorrect: %+v", last)
	}
}

func TestGetWebItemAddsAudioAndCurrentSpeakerProjection(t *testing.T) {
	f := newFixture(t)
	id := f.turnRowID(t, "seg-a", "t-1")
	got, err := f.svc.GetWebItem(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Item.Kind != "turn" || got.Item.AudioStatus != AudioUnavailable {
		t.Fatalf("web item lacks explicit audio status: %+v", got.Item)
	}
	if got.Item.SpeakerPersonID != nil || got.Item.TranscriptVersion == "" || len(got.Item.DerivedFrom) == 0 {
		t.Fatalf("turn current-revision metadata missing: %+v", got.Item.Item)
	}
}

func TestTurnPageRequiresRecordingAndReportsAudioStatus(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.svc.TurnPage(ctx, f.conversationID, "", false, 0, 0); err == nil {
		t.Fatal("missing recording id accepted")
	} else if e := appErr(t, err); e.Code != CodeValidationFailed {
		t.Fatalf("unexpected error: %+v", e)
	}
	if _, err := f.svc.TurnPage(ctx, f.conversationID, "recording:nope", false, 0, 0); err == nil {
		t.Fatal("unknown recording accepted")
	} else if e := appErr(t, err); e.Code != CodeNotFound {
		t.Fatalf("unexpected error: %+v", e)
	}
	page, err := f.svc.TurnPage(ctx, f.conversationID, f.recordingID, false, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 3 {
		t.Fatalf("expected 3 turns, got %d", len(page.Items))
	}
	for _, item := range page.Items {
		if item.Kind != "turn" {
			t.Fatalf("page leaked a non-turn item: %+v", item)
		}
		if item.Content == "" || item.ConversationID != f.conversationID {
			t.Fatalf("turn row is incomplete: %+v", item.Item)
		}
		want := AudioUnavailable
		if item.GlobalStartMS == nil {
			want = AudioOffsetUnknown
		}
		if item.AudioStatus != want {
			t.Fatalf("turn %s: got %q want %q", item.ID, item.AudioStatus, want)
		}
	}
}

func TestEditNotePropagatesStoreValidation(t *testing.T) {
	f := newFixture(t)
	// An annotation view whose title is empty must still be editable: the
	// service restores the conversation title instead of failing validation.
	ctx := context.Background()
	current, version, err := f.store.CurrentAnnotations(ctx, f.conversationID)
	if err != nil {
		t.Fatal(err)
	}
	current.Title = ""
	if err := f.store.ApplyAnnotations(ctx, f.conversationID, version, memory.Annotations{
		ConversationID: f.conversationID, AnnotationVersion: "v2", Title: current.Title,
		AnnotationBasis: current.AnnotationBasis, Summary: current.Summary,
		Topics: current.Topics, Notes: current.Notes, TurnTopics: current.TurnTopics,
	}); err == nil {
		t.Fatal("store accepted an empty title, the fixture assumption is wrong")
	}
	rowID := memory.NoteRowID(f.conversationID, "note:one")
	res, err := f.svc.EditNote(ctx, f.conversationID, rowID, "v1", "web-9", "没有标题也要能改")
	if err != nil {
		t.Fatal(err)
	}
	if res.AnnotationVersion != "web-9" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if _, err := f.store.Conversation(ctx, f.conversationID); err != nil {
		t.Fatal(err)
	}
}

func TestAsErrorIsFalseForPlainErrors(t *testing.T) {
	if _, ok := AsError(errors.New("plain")); ok {
		t.Fatal("plain error classified as app error")
	}
	if _, ok := AsError(nil); ok {
		t.Fatal("nil classified as app error")
	}
}
