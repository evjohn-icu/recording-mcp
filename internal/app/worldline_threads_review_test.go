package app

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"recordingmcp/internal/asrinput"
	"recordingmcp/internal/memory"
	"recordingmcp/internal/plane"
)

func TestWorldlineThreadsExactTitleAndRepeatedSummaryDoNotInferPersonOrFact(t *testing.T) {
	started := testStartedAt
	f := newFixtureWithStart(t, &started)
	ctx := context.Background()

	// The two existing notes and the note in another conversation share the
	// same exact source topic title and summary, but no speaker binding exists.
	if _, err := f.store.DB().Exec(`UPDATE topics SET title='项目轮岗' WHERE id='topic:a'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.DB().Exec(`UPDATE notes SET summary='同事在家远程工作'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.DB().Exec(`UPDATE turns SET speaker_scoped_id='speaker:local-a' WHERE id=?`, f.turnRowID(t, "seg-a", "t-1")); err != nil {
		t.Fatal(err)
	}
	otherTurn := turnWithTimes("other-turn", 1, "同事在家远程工作", 0, 500, 1500)
	otherTurn.SpeakerScopedID = stringPtr("speaker:local-b")
	other := sampleSegment("other-session", 0, &started, []asrinput.Turn{otherTurn})
	other.Source.DeviceID = "device:worldline-other"
	other.Source.RecordingID = "recording:worldline-other"
	other.Source.ConversationID = "conversation:worldline-other"
	other.Source.ProviderRequestID = "request:worldline-other"
	report, err := f.store.IngestTranscript(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.ApplyAnnotations(ctx, report.ConversationID, "", memory.Annotations{
		ConversationID:    report.ConversationID,
		Title:             "synthetic second session",
		RecordingID:       report.RecordingID,
		AnnotationVersion: "thread-v1",
		Topics:            []memory.TopicAnnotation{{ID: "topic:other", Title: "项目轮岗"}},
		Notes:             []memory.NoteAnnotation{{ID: "note:other", Kind: "observation", TopicIDs: []string{"topic:other"}, Summary: "同事在家远程工作", EvidenceTurnIDs: []string{originalTurnID("other-session", "other-turn")}}},
	}); err != nil {
		t.Fatal(err)
	}

	events, err := plane.Open(t.TempDir() + "/events.db")
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	svc := &WorldlineService{Time: &TimeService{Store: f.store, Events: events}}
	listed, err := svc.Threads(ctx, "profile-threads", WorldlineQuery{Limit: 100, Status: "all"})
	if err != nil {
		t.Fatal(err)
	}
	if listed.Total != 1 || len(listed.Threads) != 1 {
		t.Fatalf("exact source title should form one topical thread: %+v", listed)
	}
	thread := listed.Threads[0]
	if thread.Title != "项目轮岗" || thread.ConversationCount != 2 || thread.EntryCount != 3 {
		t.Fatalf("thread aggregated the wrong evidence: %+v", thread)
	}
	detail, err := svc.Thread(ctx, "profile-threads", thread.ID, WorldlineQuery{Limit: 100, Status: "all"}, 0, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if detail.EntriesTotal != 3 || detail.UnknownEntriesTotal != 0 || len(detail.Entries) != 3 {
		t.Fatalf("thread detail lost entries: %+v", detail)
	}
	seenConversations := map[string]bool{}
	seenClaimIDs := map[string]bool{}
	for _, entry := range detail.Entries {
		if entry.Status != "unconfirmed" || entry.Relation == "repeated_statement" {
			t.Fatalf("same summary was incorrectly treated as confirmed repetition: %+v", entry)
		}
		if entry.TimeKnown && entry.Claim.ValidFrom != nil {
			t.Fatalf("source mention time was mistaken for effective time: %+v", entry)
		}
		if entry.SubjectKey == "" || !strings.HasPrefix(entry.SubjectKey, "unknown:") {
			t.Fatalf("unbound speaker acquired an identity: %+v", entry)
		}
		if entry.Category == "facts" && entry.Relation != "first_mention" && entry.Relation != "subsequent_mention" && entry.Relation != "review_required" {
			t.Fatalf("unbound factual repetition lacks an uncertainty relation: %+v", entry)
		}
		if entry.Category == "facts" && entry.Relation != "review_required" && !strings.Contains(entry.RelationReason, "identity_unconfirmed") {
			t.Fatalf("unbound factual claim lacks an identity caveat: %+v", entry)
		}
		if seenClaimIDs[entry.ClaimID] {
			t.Fatalf("same claim was duplicated within thread: %+v", entry)
		}
		seenClaimIDs[entry.ClaimID] = true
		for _, evidence := range entry.Evidence {
			seenConversations[evidence.ConversationID] = true
			var current int
			if err := f.store.DB().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM turns t JOIN attachment_turns at ON at.turn_id=t.id JOIN source_attachments a ON a.id=at.attachment_id WHERE t.id=? AND t.conversation_id=? AND t.recording_id=? AND a.active=1 AND a.transcript_revision=? AND NOT EXISTS(SELECT 1 FROM turn_classes tc WHERE tc.turn_id=t.id AND tc.class='ambient'))`, evidence.TurnID, evidence.ConversationID, evidence.RecordingID, evidence.Revision).Scan(&current); err != nil {
				t.Fatal(err)
			}
			if current != 1 {
				t.Fatalf("thread contains a stale evidence reference: %+v", evidence)
			}
		}
	}
	if len(seenConversations) != 2 {
		t.Fatalf("cross-session evidence not retained individually: %v", seenConversations)
	}
}

func TestWorldlineThreadRenameIsReversibleAndLegacyIDStillResolves(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	events, err := plane.Open(t.TempDir() + "/events.db")
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	svc := &WorldlineService{Time: &TimeService{Store: f.store, Events: events}}
	before, err := svc.Threads(ctx, "profile-alias", WorldlineQuery{Limit: 100})
	if err != nil || before.Total == 0 {
		t.Fatalf("threads: %+v %v", before, err)
	}
	legacy := before.Threads[0]
	legacyTitleID := "thread-" + shortHash(normalizeTopicTitle(legacy.Title))
	version, err := svc.RenameThread(ctx, "profile-alias", legacy.ID, "手动别名", before.Version, "rename-1")
	if err != nil {
		t.Fatal(err)
	}
	if replay, err := svc.RenameThread(ctx, "profile-alias", legacy.ID, "手动别名", before.Version, "rename-1"); err != nil || replay != version {
		t.Fatalf("same rename request did not replay: version=%q err=%v", replay, err)
	}
	if _, err := svc.RenameThread(ctx, "profile-alias", legacy.ID, "另一个标题", before.Version, "rename-1"); err == nil || !strings.Contains(err.Error(), "request_id conflict") {
		t.Fatalf("different payload reused request id: %v", err)
	}
	after, err := svc.Threads(ctx, "profile-alias", WorldlineQuery{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	var renamed WorldlineThread
	for _, th := range after.Threads {
		if th.ID == legacy.ID {
			renamed = th
		}
	}
	if renamed.ID != legacy.ID || renamed.Title != "手动别名" {
		t.Fatalf("rename changed identity or failed: %+v", renamed)
	}
	if _, err := svc.Time.Store.DB().Exec(`UPDATE topics SET title='来源标题更新' WHERE id=?`, legacy.SourceTopicIDs[0]); err != nil {
		t.Fatal(err)
	}
	currentList, err := svc.Threads(ctx, "profile-alias", WorldlineQuery{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if currentList.Threads[0].ID != legacy.ID || currentList.Threads[0].Title != "手动别名" {
		t.Fatalf("source rename changed stable alias identity: %+v", currentList.Threads[0])
	}
	if _, err := svc.Thread(ctx, "profile-alias", legacyTitleID, WorldlineQuery{Limit: 100}, 0, 0, 100); err != nil {
		t.Fatalf("pre-stable legacy ID no longer redirects: %v", err)
	}
	if _, err := svc.Thread(ctx, "profile-alias", legacy.ID, WorldlineQuery{Limit: 100}, 0, 0, 100); err != nil {
		t.Fatalf("legacy thread ID no longer resolves: %v", err)
	}
	if _, err := svc.RenameThread(ctx, "profile-alias", legacy.ID, "", currentList.Version, "rename-2"); err != nil {
		t.Fatal(err)
	}
	restored, err := svc.Threads(ctx, "profile-alias", WorldlineQuery{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, th := range restored.Threads {
		if th.ID == legacy.ID && th.Title != "来源标题更新" {
			t.Fatalf("empty alias should restore source title: %+v", th)
		}
	}
}

func TestWorldlineManualThreadGroupCanBeRevokedWithoutMergingClaims(t *testing.T) {
	f := newFixture(t)
	if _, err := f.store.DB().Exec(`INSERT INTO topics(id,title) VALUES('topic:b','另一个来源标题')`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.DB().Exec(`UPDATE notes SET topic_ids_json='["topic:b"]' WHERE summary='第二条归纳'`); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	events, err := plane.Open(t.TempDir() + "/events.db")
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	svc := &WorldlineService{Time: &TimeService{Store: f.store, Events: events}}
	before, err := svc.Threads(ctx, "profile-group", WorldlineQuery{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if before.Total != 2 {
		t.Fatalf("setup should have two source threads: %+v", before)
	}
	ids := []string{before.Threads[0].ID, before.Threads[1].ID}
	wantEntries := before.Threads[0].EntryCount + before.Threads[1].EntryCount
	version, err := svc.GroupThreads(ctx, "profile-group", ids, "人工分组", "group", "", before.Version, "group-1")
	if err != nil {
		t.Fatal(err)
	}
	grouped, err := svc.Threads(ctx, "profile-group", WorldlineQuery{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if grouped.Total != 1 || !grouped.Threads[0].ManualGroup || grouped.Threads[0].EntryCount != wantEntries {
		t.Fatalf("manual grouping changed the wrong projection: %+v", grouped)
	}
	for _, id := range ids {
		if _, err := svc.Thread(ctx, "profile-group", id, WorldlineQuery{Limit: 100}, 0, 0, 100); err != nil {
			t.Fatalf("old thread ID %s did not resolve: %v", id, err)
		}
	}
	if _, err := svc.GroupThreads(ctx, "profile-group", nil, "", "revoke", grouped.Threads[0].ID, version, "ungroup-1"); err != nil {
		t.Fatal(err)
	}
	restored, err := svc.Threads(ctx, "profile-group", WorldlineQuery{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if restored.Total != 2 {
		t.Fatalf("revoke did not restore source topics: %+v", restored)
	}
}

func TestWorldlineMutationVersionIsAtomicAcrossServiceInstances(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	path := t.TempDir() + "/events.db"
	events1, err := plane.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer events1.Close()
	events2, err := plane.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer events2.Close()
	s1 := &WorldlineService{Time: &TimeService{Store: f.store, Events: events1}}
	s2 := &WorldlineService{Time: &TimeService{Store: f.store, Events: events2}}
	list, err := s1.Threads(ctx, "profile-cas", WorldlineQuery{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	id := list.Threads[0].ID
	start := make(chan struct{})
	errs := make([]error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, errs[0] = s1.RenameThread(ctx, "profile-cas", id, "名字甲", list.Version, "cas-a")
	}()
	go func() {
		defer wg.Done()
		<-start
		_, errs[1] = s2.RenameThread(ctx, "profile-cas", id, "名字乙", list.Version, "cas-b")
	}()
	close(start)
	wg.Wait()
	success, conflict := 0, 0
	for _, e := range errs {
		if e == nil {
			success++
		} else if strings.Contains(e.Error(), "conflict") || strings.Contains(e.Error(), "version conflict") {
			conflict++
		} else {
			t.Fatalf("unexpected mutation error: %v", e)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("CAS outcomes success=%d conflict=%d errs=%v", success, conflict, errs)
	}
}

func TestWorldlineRenameCanTargetThreadBeyondFirstHundred(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	turn := f.turnRowID(t, "seg-a", "t-1")
	tx, err := f.store.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 105; i++ {
		id := fmt.Sprintf("topic-bulk-%03d", i)
		note := fmt.Sprintf("note-bulk-%03d", i)
		if _, err = tx.Exec(`INSERT INTO topics(id,title) VALUES(?,?)`, id, fmt.Sprintf("单独主题%03d", i)); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		if _, err = tx.Exec(`INSERT INTO notes(id,conversation_id,kind,summary,topic_ids_json,claim_mode,interpretation_note,annotation_version) VALUES(?,?,'observation','批量来源',?,'inference','','bulk-v1')`, note, f.conversationID, `["`+id+`"]`); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		if _, err = tx.Exec(`INSERT INTO note_evidence(note_id,turn_id,ordinal) VALUES(?,?,0)`, note, turn); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	events, err := plane.Open(t.TempDir() + "/events.db")
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	svc := &WorldlineService{Time: &TimeService{Store: f.store, Events: events}}
	page, err := svc.Threads(ctx, "profile-bulk", WorldlineQuery{Limit: 100, Offset: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Threads) < 1 {
		t.Fatal("expected page after offset 100")
	}
	target := page.Threads[len(page.Threads)-1]
	if _, err := svc.RenameThread(ctx, "profile-bulk", target.ID, "第105个主题的别名", page.Version, "rename-beyond-100"); err != nil {
		t.Fatalf("rename beyond first page failed: %v", err)
	}
}

func TestWorldlineThreadsFilteredTotalsAndDetailPagesAgree(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	var turnID string
	if err := f.store.DB().QueryRow(`SELECT id FROM turns WHERE recording_id=? AND original_turn_id=?`, f.recordingID, originalTurnID("seg-a", "t-1")).Scan(&turnID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.DB().Exec(`INSERT INTO topics(id,title) VALUES('topic:extra','第二主题')`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.DB().Exec(`INSERT INTO topic_conversations(topic_id,conversation_id) VALUES('topic:extra',?)`, f.conversationID); err != nil {
		t.Fatal(err)
	}
	noteID := memory.NoteRowID(f.conversationID, "note:extra")
	if _, err := f.store.DB().Exec(`INSERT INTO notes(id,conversation_id,kind,summary,topic_ids_json,claim_mode,interpretation_note,annotation_version) VALUES(?,?,'observation','第二主题的合成主张','["topic:extra"]','inference','synthetic fixture','v1')`, noteID, f.conversationID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.DB().Exec(`INSERT INTO note_evidence(note_id,turn_id,ordinal) VALUES(?,?,0)`, noteID, turnID); err != nil {
		t.Fatal(err)
	}
	events, err := plane.Open(t.TempDir() + "/events.db")
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	svc := &WorldlineService{Time: &TimeService{Store: f.store, Events: events}}
	first, err := svc.Threads(ctx, "profile-threads", WorldlineQuery{Offset: 0, Limit: 1, Status: "all"})
	if err != nil {
		t.Fatal(err)
	}
	if first.Total != 2 || len(first.Threads) != 1 || first.Next == nil || *first.Next != 1 {
		t.Fatalf("first page totals/next offset disagree: %+v", first)
	}
	second, err := svc.Threads(ctx, "profile-threads", WorldlineQuery{Offset: *first.Next, Limit: 1, Status: "all"})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Threads) != 1 || second.Threads[0].ID == first.Threads[0].ID {
		t.Fatalf("thread pages are not complete: first=%+v second=%+v", first.Threads, second.Threads)
	}
	filtered, err := svc.Threads(ctx, "profile-threads", WorldlineQuery{Limit: 10, Status: "all", Query: "第二主题"})
	if err != nil {
		t.Fatal(err)
	}
	if filtered.Total != 1 || len(filtered.Threads) != 1 || filtered.Threads[0].Title != "第二主题" {
		t.Fatalf("thread search total disagrees with filtered rows: %+v", filtered)
	}
	detail, err := svc.Thread(ctx, "profile-threads", filtered.Threads[0].ID, WorldlineQuery{Limit: 100, Status: "all", Query: "第二主题"}, 0, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if detail.EntriesTotal+detail.UnknownEntriesTotal != 1 {
		t.Fatalf("filtered list and detail totals disagree: %+v", detail)
	}
	noMatch, err := svc.Threads(ctx, "profile-threads", WorldlineQuery{Limit: 10, Status: "reviewed"})
	if err != nil {
		t.Fatal(err)
	}
	if noMatch.Total != 0 || len(noMatch.Threads) != 0 {
		t.Fatalf("status filter escaped into results: %+v", noMatch)
	}
}

func TestWorldlineUnknownMentionEntriesHaveStableOrderAndRemainUndated(t *testing.T) {
	makeTopics := func(ids []string) []WorldlineTopic {
		lanes := make([]WorldlineLane, 0, len(ids))
		for _, id := range ids {
			claim := WorldlineClaim{ID: id, Version: "v1", Summary: "synthetic unknown-time note", Status: "unconfirmed", ChangeType: "report", SourceTopicID: "topic:x", SourceTopicTitle: "项目轮岗", SubjectKey: "unknown:" + id, CategoryKey: "facts", Metadata: map[string]any{"source_note_id": id}, Evidence: []WorldlineEvidence{{TurnID: "turn-" + id, Revision: "rev-1", ConversationID: "conversation-" + id}}}
			lanes = append(lanes, WorldlineLane{ID: "lane-" + id, Title: "项目轮岗", Subject: "unknown", Category: "facts", Claims: []WorldlineClaim{claim}})
		}
		return []WorldlineTopic{{ID: "macro", Title: "工作与项目", Lanes: lanes}}
	}
	first := buildWorldlineThreadDetails(makeTopics([]string{"note-z", "note-a", "note-m"}))
	second := buildWorldlineThreadDetails(makeTopics([]string{"note-m", "note-z", "note-a"}))
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("unexpected thread groups: %d / %d", len(first), len(second))
	}
	firstIDs, secondIDs := []string{}, []string{}
	for _, entry := range first[0].unknown {
		if entry.TimeKnown || entry.MentionedAt != nil || entry.Relation != "unknown_time" || entry.RelationReason != "mention_timestamp_unavailable" || entry.Claim.ValidFrom != nil {
			t.Fatalf("unknown mention borrowed a date or relation: %+v", entry)
		}
		firstIDs = append(firstIDs, entry.ID)
	}
	for _, entry := range second[0].unknown {
		secondIDs = append(secondIDs, entry.ID)
	}
	if !reflect.DeepEqual(firstIDs, secondIDs) || !sort.StringsAreSorted(firstIDs) {
		t.Fatalf("unknown-time entry ordering unstable: %v vs %v", firstIDs, secondIDs)
	}
}

func stringPtr(value string) *string { return &value }
