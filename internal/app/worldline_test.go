package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"recordingmcp/internal/plane"
)

func TestWorldlineProjectionAndFeedbackUseFullLaneProjection(t *testing.T) {
	f := newFixture(t)
	events, err := plane.Open(t.TempDir() + "/events.db")
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	svc := &WorldlineService{Time: &TimeService{Store: f.store, Events: events}}
	ctx := context.Background()
	listed, err := svc.List(ctx, "profile-test", WorldlineQuery{Status: "all", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if listed.Total != 1 || len(listed.Topics) != 1 || len(listed.Topics[0].Lanes) == 0 {
		t.Fatalf("unexpected projection: %+v", listed)
	}
	topic, err := svc.Topic(ctx, "profile-test", listed.Topics[0].ID, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if topic.Total != 2 {
		t.Fatalf("topic claims=%d, want both note claims", topic.Total)
	}
	var target WorldlineLane
	for _, lane := range topic.Topic.Lanes {
		if len(lane.Claims) > 0 {
			target = lane
			break
		}
	}
	if target.ID == "" {
		t.Fatal("missing lane")
	}
	newVersion, err := svc.Feedback(ctx, "profile-test", WorldlineFeedback{ExpectedVersion: listed.Version, Action: "rename_topic", TargetTopicID: target.ID, Title: "人工确认标题"})
	if err != nil {
		t.Fatal(err)
	}
	if newVersion == listed.Version {
		t.Fatal("feedback did not advance version")
	}
	updated, err := svc.Topic(ctx, "profile-test", listed.Topics[0].ID, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, lane := range updated.Topic.Lanes {
		found = found || lane.ID == target.ID && lane.Title == "人工确认标题"
	}
	if !found {
		t.Fatalf("rename was not projected: %+v", updated.Topic.Lanes)
	}
	if _, err := f.store.DB().Exec(`UPDATE notes SET summary=summary||'改写',annotation_version='v2'`); err != nil {
		t.Fatal(err)
	}
	refreshed, err := svc.Topic(ctx, "profile-test", listed.Topics[0].ID, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, lane := range refreshed.Topic.Lanes {
		if lane.ID == target.ID && lane.Title == "人工确认标题" {
			t.Fatal("feedback overlay survived a source-note revision")
		}
	}
	if _, err := svc.Feedback(ctx, "profile-test", WorldlineFeedback{ExpectedVersion: listed.Version, Action: "rename_topic", TargetTopicID: target.ID, Title: "stale"}); err == nil || !strings.Contains(err.Error(), "version conflict") {
		t.Fatalf("stale feedback err=%v, want conflict", err)
	}
}

func TestWorldlineDatesRequireExplicitClaimValidityLanguage(t *testing.T) {
	evidence := WorldlineEvidence{Text: "我自2024-03-12开始在新团队工作"}
	claim := WorldlineClaim{Summary: "那天提到过工作安排", TimeBasis: "unknown", TimePrecision: "unknown", Evidence: []WorldlineEvidence{evidence}, Metadata: map[string]any{}}
	applyClaimTime(&claim)
	if claim.ValidFrom != nil || claim.TimePrecision != "unknown" {
		t.Fatalf("mention was mistaken for validity: %+v", claim)
	}
	claim.Summary = "自2024-03-12开始在新团队工作"
	applyClaimTime(&claim)
	if claim.ValidFrom == nil || claim.ValidFrom.In(time.FixedZone("Asia/Shanghai", 8*60*60)).Format("2006-01-02") != "2024-03-12" || claim.TimeBasis != "explicit_date" {
		t.Fatalf("explicit validity date not extracted: %+v", claim)
	}
	claim.ValidFrom, claim.ValidUntil = nil, nil
	claim.Summary = "截至2024-03-12仍在该团队工作"
	evidence.Text = claim.Summary
	claim.Evidence = []WorldlineEvidence{evidence}
	applyClaimTime(&claim)
	if claim.ValidFrom != nil || claim.ValidUntil == nil || claim.TimeBasis != "explicit_valid_until" {
		t.Fatalf("cutoff was mistaken for a valid-from date: %+v", claim)
	}
	claim.ValidUntil = nil
	claim.Summary = "自2024-03-12开始在新团队工作"
	evidence.Text = "那天聊了新团队"
	claim.Evidence = []WorldlineEvidence{evidence}
	applyClaimTime(&claim)
	if claim.ValidFrom != nil {
		t.Fatalf("summary-only date was treated as sourced: %+v", claim)
	}
}

func TestWorldlineUnknownEvidenceNeverBorrowsLaterSpeaker(t *testing.T) {
	sources := []worldlineSource{{claim: WorldlineClaim{ID: "c", Summary: "claim", SubjectKey: "", Evidence: []WorldlineEvidence{{SubjectID: "person-a"}, {SubjectID: ""}, {SubjectID: "person-a"}}}, topicIDs: []string{"t"}, topicTitles: map[string]string{"t": "项目"}}}
	topics, err := buildWorldline(sources)
	if err != nil {
		t.Fatal(err)
	}
	if len(topics) != 1 || len(topics[0].Lanes) != 1 || topics[0].Lanes[0].Subject != "unknown" {
		t.Fatalf("unknown/mixed evidence was attributed: %+v", topics)
	}
}

func TestWorldlineRangeIncludesMentionAxisWithoutLosingValidAxis(t *testing.T) {
	valid := time.Date(2024, 10, 3, 0, 0, 0, 0, time.UTC)
	mentioned := time.Date(2024, 10, 11, 0, 0, 0, 0, time.UTC)
	topics := []WorldlineTopic{{Lanes: []WorldlineLane{{Claims: []WorldlineClaim{{ValidFrom: &valid, MentionedAt: &mentioned, Summary: "x", Evidence: []WorldlineEvidence{{TurnID: "t"}}}}}}}}
	r := worldlineDataRange(topics)
	if r.Start == nil || r.End == nil || !r.Start.Equal(valid) || !r.End.Equal(mentioned) || r.ValidEnd == nil || !r.ValidEnd.Equal(valid) || r.MentionedEnd == nil || !r.MentionedEnd.Equal(mentioned) {
		t.Fatalf("range lost time-axis meaning: %+v", r)
	}
}

func TestWorldlineFilterIncludesUntilOnlyAndSearchesRenamedLane(t *testing.T) {
	from := time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2024, 4, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2024, 3, 15, 0, 0, 0, 0, time.UTC)
	topics := []WorldlineTopic{{ID: "macro", Title: "工作与项目", Lanes: []WorldlineLane{{ID: "lane", Title: "用户重命名的主题", Category: "facts", Claims: []WorldlineClaim{{ID: "cutoff", Summary: "在职状态", ValidUntil: &until, Evidence: []WorldlineEvidence{{TurnID: "turn"}}}, {ID: "unknown", Summary: "未知日期", Evidence: []WorldlineEvidence{{TurnID: "turn2"}}}}}}}}
	got := filterWorldline(topics, WorldlineQuery{From: &from, To: &to, Status: "all"})
	if len(got) != 1 || len(got[0].Lanes) != 1 || len(got[0].Lanes[0].Claims) != 1 || got[0].Lanes[0].Claims[0].ID != "cutoff" {
		t.Fatalf("until-only overlap filter = %+v", got)
	}
	got = filterWorldline(topics, WorldlineQuery{Query: "重命名", Status: "all"})
	if len(got) != 1 || len(got[0].Lanes) != 1 || len(got[0].Lanes[0].Claims) != 2 {
		t.Fatalf("renamed lane title search = %+v", got)
	}
}

func TestWorldlineExcludesNotesWithAmbientEvidence(t *testing.T) {
	f := newFixture(t)
	events, err := plane.Open(t.TempDir() + "/events.db")
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	svc := &WorldlineService{Time: &TimeService{Store: f.store, Events: events}}
	ctx := context.Background()
	ambientTurn := f.turnRowID(t, "seg-a", "t-1")
	if _, err := f.store.DB().Exec(`INSERT INTO turn_classes(turn_id,class,subtype,method,created_at) VALUES(?,'ambient','media','derived',1)`, ambientTurn); err != nil {
		t.Fatal(err)
	}
	got, err := svc.List(ctx, "profile-ambient", WorldlineQuery{Status: "all", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if got.Stats.Claims != 1 {
		t.Fatalf("claims with one ambient source = %d, want only the unaffected note", got.Stats.Claims)
	}
	otherAmbient := f.turnRowID(t, "seg-a", "t-2")
	if _, err := f.store.DB().Exec(`INSERT INTO turn_classes(turn_id,class,subtype,method,created_at) VALUES(?,'ambient','media','derived',1)`, otherAmbient); err != nil {
		t.Fatal(err)
	}
	got, err = svc.List(ctx, "profile-ambient", WorldlineQuery{Status: "all", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if got.Stats.Claims != 0 {
		t.Fatalf("claims after all evidence is ambient = %d, want 0", got.Stats.Claims)
	}
}

func TestWorldlineDoesNotProjectOldTranscriptRevision(t *testing.T) {
	f := newFixture(t)
	events, err := plane.Open(t.TempDir() + "/events.db")
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	svc := &WorldlineService{Time: &TimeService{Store: f.store, Events: events}}
	ctx := context.Background()
	before, err := svc.List(ctx, "profile-revision", WorldlineQuery{Status: "all", Limit: 100})
	if err != nil || before.Stats.Claims != 2 {
		t.Fatalf("before correction claims=%d err=%v", before.Stats.Claims, err)
	}
	if _, err := f.svc.CorrectTranscriptSegment(ctx, f.conversationID, f.recordingID, "seg-a", "rev-1", "rev-2", map[string]string{"t-1": "已校正的第一条转写", "t-2": "已校正的第二条转写"}); err != nil {
		t.Fatal(err)
	}
	after, err := svc.List(ctx, "profile-revision", WorldlineQuery{Status: "all", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if after.Stats.Claims != 0 {
		t.Fatalf("stale revision claims=%d, want 0", after.Stats.Claims)
	}
}

func TestWorldlineGenerationIsIdempotentForRequestID(t *testing.T) {
	f := newFixture(t)
	events, err := plane.Open(t.TempDir() + "/events.db")
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	svc := &WorldlineService{Time: &TimeService{Store: f.store, Events: events}}
	ctx := context.Background()
	first, err := svc.Generate(ctx, "profile-generation", "same-request")
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.Generate(ctx, "profile-generation", "same-request")
	if err != nil {
		t.Fatal(err)
	}
	if first["status"] != "complete" || second["status"] != "complete" || first["job_id"] != second["job_id"] {
		t.Fatalf("generation results: first=%v second=%v", first, second)
	}
	history, err := events.History(ctx, "profile-generation")
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range history {
		if event.Type == "worldline.generation" {
			count++
		}
	}
	if count != 3 {
		t.Fatalf("generation events=%d, want one pending/running/complete set", count)
	}
}

func TestWorldlineThreadsUseExactTitlesAndKeepUnknownIdentityIndependent(t *testing.T) {
	at1 := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	at2 := at1.Add(24 * time.Hour)
	at3 := at2.Add(24 * time.Hour)
	makeClaim := func(id, title, subject, summary, conversation string, at *time.Time, evidenceText string) WorldlineClaim {
		return WorldlineClaim{ID: id, Version: "v1", Summary: summary, Kind: "observation", Status: "unconfirmed", SubjectKey: subject, CategoryKey: "facts", SourceTopicTitle: title, SourceTopicID: "source:" + id, MentionedAt: at, TimeBasis: "unknown", TimePrecision: "unknown", Evidence: []WorldlineEvidence{{TurnID: "turn:" + id, Revision: "rev1", ConversationID: conversation, Text: evidenceText}}, ConflictWith: []string{}}
	}
	first := makeClaim("c1", "项目 Alpha", "person:p1", "公司位于北区", "conv-1", &at1, "公司位于北区")
	repeated := makeClaim("c2", " 项目   Alpha ", "person:p1", "公司位于北区", "conv-2", &at2, "公司位于北区")
	unknownChange := makeClaim("c3", "项目 Alpha", "unknown:c3", "公司现在改为位于南区", "conv-3", &at3, "公司现在改为位于南区")
	unknownChange.Status = "unconfirmed"
	noTime := makeClaim("c4", "项目 Alpha", "unknown:c4", "另一个发言人的未定日期说法", "conv-3", nil, "该说法")
	differentTitle := makeClaim("c5", "项目 Beta", "person:p1", "别的话题", "conv-1", &at1, "别的话题")
	topics := []WorldlineTopic{{ID: "macro-work", Title: "工作与项目", Lanes: []WorldlineLane{
		{ID: "lane-1", Title: "项目 Alpha", Subject: "p1", Category: "facts", Claims: []WorldlineClaim{first}},
		{ID: "lane-2", Title: "项目 Alpha", Subject: "p1", Category: "facts", Claims: []WorldlineClaim{repeated}},
		{ID: "lane-3", Title: "项目 Alpha", Subject: "unknown", Category: "facts", Claims: []WorldlineClaim{unknownChange, noTime}},
		{ID: "lane-4", Title: "项目 Beta", Subject: "p1", Category: "facts", Claims: []WorldlineClaim{differentTitle}},
	}}}
	built := buildWorldlineThreadDetails(topics)
	if len(built) != 2 {
		t.Fatalf("thread count=%d, want exact source titles only: %+v", len(built), built)
	}
	var alpha *threadBuild
	for i := range built {
		if normalizeTopicTitle(built[i].thread.Title) == "项目 alpha" {
			alpha = &built[i]
		}
	}
	if alpha == nil {
		t.Fatal("missing source topic thread")
	}
	if alpha.thread.ConversationCount != 3 || alpha.thread.EntryCount != 4 || len(alpha.entries) != 3 || len(alpha.unknown) != 1 {
		t.Fatalf("thread counts=%+v dated=%d unknown=%d", alpha.thread, len(alpha.entries), len(alpha.unknown))
	}
	if alpha.entries[0].ClaimID != "c1" || alpha.entries[0].Relation != "first_mention" {
		t.Fatalf("first chronological entry=%+v", alpha.entries[0])
	}
	if alpha.entries[1].ClaimID != "c2" || alpha.entries[1].Relation != "repeated_statement" {
		t.Fatalf("repeat relation=%+v", alpha.entries[1])
	}
	if alpha.entries[2].ClaimID != "c3" || alpha.entries[2].Relation != "review_required" || alpha.entries[2].Status != "unconfirmed" {
		t.Fatalf("change clue must remain review-only: %+v", alpha.entries[2])
	}
	if alpha.unknown[0].ClaimID != "c4" || alpha.unknown[0].Relation != "unknown_time" {
		t.Fatalf("unknown-time entry was placed into chronology: %+v", alpha.unknown[0])
	}
	if alpha.entries[2].RelationReason == "" || alpha.entries[2].Claim.Evidence[0].Text == "" {
		t.Fatalf("missing explanation or source evidence: %+v", alpha.entries[2])
	}
}

func TestWorldlineTopicDetailAppliesTheListFiltersBeforePagination(t *testing.T) {
	f := newFixture(t)
	events, err := plane.Open(t.TempDir() + "/events.db")
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	svc := &WorldlineService{Time: &TimeService{Store: f.store, Events: events}}
	ctx := context.Background()
	listed, err := svc.List(ctx, "profile-topic-filters", WorldlineQuery{Status: "all", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Topics) != 1 {
		t.Fatalf("topics=%d", len(listed.Topics))
	}
	id := listed.Topics[0].ID
	unfiltered, err := svc.Topic(ctx, "profile-topic-filters", id, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if unfiltered.Total != 2 {
		t.Fatalf("unfiltered total=%d", unfiltered.Total)
	}
	matched, err := svc.Topic(ctx, "profile-topic-filters", id, 0, 20, WorldlineQuery{Status: "all", Query: "第一条归纳"})
	if err != nil {
		t.Fatal(err)
	}
	if matched.Total != 1 || len(matched.Topic.Lanes) != 1 || matched.Topic.Lanes[0].Claims[0].Summary != "第一条归纳" {
		t.Fatalf("q filter escaped into details: %+v", matched)
	}
	status, err := svc.Topic(ctx, "profile-topic-filters", id, 0, 20, WorldlineQuery{Status: "reviewed"})
	if err != nil {
		t.Fatal(err)
	}
	if status.Total != 0 {
		t.Fatalf("status filter total=%d, want 0", status.Total)
	}
	from := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 0, 1)
	date, err := svc.Topic(ctx, "profile-topic-filters", id, 0, 20, WorldlineQuery{Status: "all", From: &from, To: &to})
	if err != nil {
		t.Fatal(err)
	}
	if date.Total != 0 {
		t.Fatalf("date filter total=%d, want unknown-time claims excluded", date.Total)
	}
}
