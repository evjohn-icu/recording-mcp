package contextbuild

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"recordingmcp/internal/pipeline"
	"recordingmcp/internal/semantic"
)

// fakeSemantic is an in-test SemanticRetriever: no embeddings, just a fixed
// hit list, so a test can inject hits the retrieval SQL would never return.
type fakeSemantic struct {
	hits []semantic.SemanticHit
}

func (f *fakeSemantic) Search(ctx context.Context, query string, opts semantic.SearchOptions) ([]semantic.SemanticHit, error) {
	return f.hits, nil
}

func (f *fakeSemantic) Ready() bool { return len(f.hits) > 0 }

func (f *fakeSemantic) Describe() string { return "fake/test" }

// buildSemantic runs one build with an explicit semantic retriever.
func (f *fixture) buildSemantic(retriever semantic.SemanticRetriever, req Request) Package {
	f.t.Helper()
	pkg, err := Build(context.Background(), Deps{Store: f.store, DB: f.db, Semantic: retriever}, req)
	if err != nil {
		f.t.Fatalf("Build: %v", err)
	}
	return pkg
}

// rankingOf returns the Ranking entry of one item id.
func rankingOf(pkg Package, itemID string) (ItemRanking, bool) {
	for _, rank := range pkg.Ranking {
		if rank.ItemID == itemID {
			return rank, true
		}
	}
	return ItemRanking{}, false
}

func hasSource(rank ItemRanking, source string) bool {
	for _, s := range rank.Sources {
		if s == source {
			return true
		}
	}
	return false
}

func TestSemanticBranchAddsRankingSources(t *testing.T) {
	f := newFixture(t)
	start := "2026-03-01T10:00:00Z"
	f.conversation("conv:a", "Alpha", "rec:a")
	f.recording("rec:a", "conv:a", &start)
	f.turn("t:sem", "conv:a", "rec:a", 0, 1000, nil, "量子通信发展规划评估")
	f.turn("t:other", "conv:a", "rec:a", 1, 2000, nil, "unrelated chatter")

	// The query shares no substring with the hit text: only the semantic
	// branch can reach the item.
	pkg := f.buildSemantic(&fakeSemantic{hits: []semantic.SemanticHit{
		{Kind: "turn", ArtifactID: "t:sem", ConversationID: "conv:a", Score: 0.9},
	}}, Request{Query: "量子规划"})

	if !containsString(itemIDs(pkg.Items), "t:sem") {
		t.Fatalf("semantic-only item missing: %v", itemIDs(pkg.Items))
	}
	rank, ok := rankingOf(pkg, "t:sem")
	if !ok {
		t.Fatalf("no ranking entry for the semantic hit: %+v", pkg.Ranking)
	}
	if !hasSource(rank, SourceSemantic) || len(rank.Sources) != 1 {
		t.Fatalf("sources = %v, want [semantic]", rank.Sources)
	}
	if rank.Branches[SourceSemantic] != 0.9 {
		t.Fatalf("branch score = %v, want 0.9", rank.Branches)
	}
	// fused score = weight 0.8 * raw 0.9
	if rank.Score < 0.71 || rank.Score > 0.73 {
		t.Fatalf("retrieval_score = %f, want ~0.72", rank.Score)
	}
	if !pkg.Coverage.Branches.Semantic {
		t.Fatalf("coverage.branches.semantic = false")
	}
	if pkg.Trace.Semantic.Indexed != 1 || !containsString(pkg.Trace.Semantic.HitIDs, "t:sem") {
		t.Fatalf("semantic trace = %+v", pkg.Trace.Semantic)
	}
	if got := len(pkg.Ranking); got != len(pkg.Items) {
		t.Fatalf("ranking has %d entries for %d items", got, len(pkg.Items))
	}
}

func TestSemanticHitCannotBypassStaleFilter(t *testing.T) {
	f := newFixture(t)
	f.ingest("seg-stale", "rev-1", "", "utt-1", "旧版搬迁计划说明")
	staleTurn := f.ingestedTurnID("seg-stale", "rev-1")
	f.ingest("seg-stale", "rev-2", "rev-1", "utt-1", "新版搬迁计划说明")

	pkg := f.buildSemantic(&fakeSemantic{hits: []semantic.SemanticHit{
		{Kind: "turn", ArtifactID: staleTurn, ConversationID: "meeting-scope", Score: 0.99},
	}}, Request{Query: "搬迁计划"})

	if containsString(itemIDs(pkg.Items), staleTurn) {
		t.Fatalf("superseded turn returned: %v", itemIDs(pkg.Items))
	}
	if pkg.Trace.Semantic.RejectedStale != 1 {
		t.Fatalf("rejected_stale = %d, want 1", pkg.Trace.Semantic.RejectedStale)
	}
	if pkg.Coverage.Branches.Semantic {
		t.Fatalf("coverage.branches.semantic = true without accepted semantic hits")
	}
}

func TestSemanticHitCannotBypassAmbientFilter(t *testing.T) {
	f := newFixture(t)
	start := "2026-03-01T10:00:00Z"
	f.conversation("conv:a", "Alpha", "rec:a")
	f.recording("rec:a", "conv:a", &start)
	f.turn("t:amb", "conv:a", "rec:a", 0, 1000, nil, "小爱同学，打开空调")
	f.exec(`INSERT INTO turn_classes(turn_id,class,subtype,method,created_at) VALUES('t:amb','ambient','voice_assistant','rule',0)`)

	pkg := f.buildSemantic(&fakeSemantic{hits: []semantic.SemanticHit{
		{Kind: "turn", ArtifactID: "t:amb", ConversationID: "conv:a", Score: 0.99},
	}}, Request{Query: "空调"})

	if containsString(itemIDs(pkg.Items), "t:amb") {
		t.Fatalf("ambient turn returned: %v", itemIDs(pkg.Items))
	}
}

func TestSemanticHitCannotEscapeConversationScope(t *testing.T) {
	f := newFixture(t)
	start := "2026-03-01T10:00:00Z"
	f.conversation("conv:a", "Alpha", "rec:a")
	f.recording("rec:a", "conv:a", &start)
	f.conversation("conv:b", "Beta", "rec:b")
	f.recording("rec:b", "conv:b", &start)
	f.turn("t:a", "conv:a", "rec:a", 0, 1000, nil, "scope probe alpha")
	f.turn("t:b", "conv:b", "rec:b", 0, 1000, nil, "scope probe beta")

	pkg := f.buildSemantic(&fakeSemantic{hits: []semantic.SemanticHit{
		{Kind: "turn", ArtifactID: "t:b", ConversationID: "conv:b", Score: 0.99},
	}}, Request{Query: "scope probe", ConversationID: "conv:a"})

	if containsString(itemIDs(pkg.Items), "t:b") {
		t.Fatalf("out-of-scope conversation leaked: %v", itemIDs(pkg.Items))
	}
	if pkg.Trace.Semantic.RejectedScope != 1 {
		t.Fatalf("rejected_scope = %d, want 1", pkg.Trace.Semantic.RejectedScope)
	}
}

func TestSemanticHitCannotEscapeTopicScope(t *testing.T) {
	f := newFixture(t)
	start := "2026-03-01T10:00:00Z"
	f.conversation("conv:a", "Alpha", "rec:a")
	f.recording("rec:a", "conv:a", &start)
	f.turn("t:in", "conv:a", "rec:a", 0, 1000, nil, "topic probe inside")
	f.turn("t:out", "conv:a", "rec:a", 1, 2000, nil, "topic probe outside")
	f.topic("topic:in", "内")
	f.topic("topic:out", "外")
	f.turnTopic("t:in", "topic:in")
	f.turnTopic("t:out", "topic:out")

	pkg := f.buildSemantic(&fakeSemantic{hits: []semantic.SemanticHit{
		{Kind: "turn", ArtifactID: "t:out", ConversationID: "conv:a", Score: 0.99},
	}}, Request{Query: "topic probe", TopicIDs: []string{"topic:in"}})

	if containsString(itemIDs(pkg.Items), "t:out") {
		t.Fatalf("out-of-scope topic leaked: %v", itemIDs(pkg.Items))
	}
	if pkg.Trace.Semantic.RejectedScope != 1 {
		t.Fatalf("rejected_scope = %d, want 1", pkg.Trace.Semantic.RejectedScope)
	}
}

func TestSemanticHitCannotEscapeTimeScope(t *testing.T) {
	f := newFixture(t)
	march := "2026-03-01T10:00:00Z"
	f.conversation("conv:early", "Early", "rec:early")
	f.recording("rec:early", "conv:early", &march)
	f.turn("t:early", "conv:early", "rec:early", 0, 1000, nil, "time probe")

	after := mustTime(t, "2026-04-01T00:00:00Z")
	pkg := f.buildSemantic(&fakeSemantic{hits: []semantic.SemanticHit{
		{Kind: "turn", ArtifactID: "t:early", ConversationID: "conv:early", Score: 0.99},
	}}, Request{Query: "time probe", StartedAfter: after})

	if containsString(itemIDs(pkg.Items), "t:early") {
		t.Fatalf("out-of-scope time leaked: %v", itemIDs(pkg.Items))
	}
	if pkg.Trace.Semantic.RejectedScope != 1 {
		t.Fatalf("rejected_scope = %d, want 1", pkg.Trace.Semantic.RejectedScope)
	}
}

func TestSupersededRevisionEmbeddingDoesNotLeak(t *testing.T) {
	f := newFixture(t)
	// Index the rev-1 turn while it is current, exactly like a deployment that
	// indexes before the corrected revision arrives.
	f.ingest("seg-rev", "rev-1", "", "utt-1", "搬迁计划初稿说明")
	rev1 := f.ingestedTurnID("seg-rev", "rev-1")
	ix := &semantic.Indexer{DB: f.db, Provider: semantic.NewHashProvider(64)}
	if _, err := ix.IndexAll(context.Background()); err != nil {
		t.Fatalf("index: %v", err)
	}
	// Supersede it; the embedding row stays, the query path must filter it.
	f.ingest("seg-rev", "rev-2", "rev-1", "utt-1", "搬迁计划修订版说明")

	retriever := &semantic.SQLiteRetriever{DB: f.db, Provider: semantic.NewHashProvider(64)}
	if !retriever.Ready() {
		t.Fatal("retriever not ready after indexing")
	}
	pkg := f.buildSemantic(retriever, Request{Query: "搬迁计划初稿说明"})

	if containsString(itemIDs(pkg.Items), rev1) {
		t.Fatalf("superseded revision resurrected by semantic recall: %v", itemIDs(pkg.Items))
	}
	for _, hit := range pkg.Trace.Semantic.HitIDs {
		if hit == rev1 {
			t.Fatalf("superseded revision in semantic hit ids: %v", pkg.Trace.Semantic.HitIDs)
		}
	}
}

func TestConfirmedLinkAssistsRetrieval(t *testing.T) {
	f := newFixture(t)
	start := "2026-03-01T10:00:00Z"
	f.conversation("conv:a", "Alpha", "rec:a")
	f.recording("rec:a", "conv:a", &start)
	f.conversation("conv:b", "Beta", "rec:b")
	f.recording("rec:b", "conv:b", &start)
	f.topic("topic:a", "预算")
	f.topic("topic:b", "招聘")
	f.topicConversation("topic:a", "conv:a")
	f.topicConversation("topic:b", "conv:b")
	f.turn("t:a", "conv:a", "rec:a", 0, 1000, nil, "预算评审启动")
	f.turn("t:b", "conv:b", "rec:b", 0, 1000, nil, "招聘预算分摊")
	f.note("n:a", "conv:a", "observation", "预算评审启动会议纪要", []string{"topic:a"}, []string{"t:a"})
	f.note("n:b", "conv:b", "observation", "招聘预算分摊纪要", []string{"topic:b"}, []string{"t:b"})
	f.topicLink("topic:a", "topic:b", "follow_up", []string{"t:a"}, pipeline.ReviewConfirm)

	pkg := f.buildSemantic(nil, Request{Query: "预算评审启动"})

	if !containsString(itemIDs(pkg.Items), "n:b") {
		t.Fatalf("link-assisted item missing: %v", itemIDs(pkg.Items))
	}
	rank, ok := rankingOf(pkg, "n:b")
	if !ok {
		t.Fatalf("no ranking entry for n:b: %+v", pkg.Ranking)
	}
	if !hasSource(rank, SourceLink) {
		t.Fatalf("sources = %v, want confirmed_link", rank.Sources)
	}
	if len(rank.LinkIDs) == 0 {
		t.Fatalf("retrieval_link_ids empty: %+v", rank)
	}
}

func TestProposedLinkDoesNotAssistRetrieval(t *testing.T) {
	f := newFixture(t)
	start := "2026-03-01T10:00:00Z"
	f.conversation("conv:a", "Alpha", "rec:a")
	f.recording("rec:a", "conv:a", &start)
	f.conversation("conv:b", "Beta", "rec:b")
	f.recording("rec:b", "conv:b", &start)
	f.topic("topic:a", "预算")
	f.topic("topic:b", "招聘")
	f.topicConversation("topic:a", "conv:a")
	f.topicConversation("topic:b", "conv:b")
	f.turn("t:a", "conv:a", "rec:a", 0, 1000, nil, "预算评审启动")
	f.turn("t:b", "conv:b", "rec:b", 0, 1000, nil, "招聘预算分摊")
	f.note("n:a", "conv:a", "observation", "预算评审启动会议纪要", []string{"topic:a"}, []string{"t:a"})
	f.note("n:b", "conv:b", "observation", "招聘预算分摊纪要", []string{"topic:b"}, []string{"t:b"})
	f.topicLink("topic:a", "topic:b", "follow_up", []string{"t:a"}, "")

	pkg := f.buildSemantic(nil, Request{Query: "预算评审启动"})

	if containsString(itemIDs(pkg.Items), "n:b") {
		t.Fatalf("proposed link assisted retrieval: %v", itemIDs(pkg.Items))
	}
}

func TestLexicalOnlyFallbackWhenSemanticUnavailable(t *testing.T) {
	f := newFixture(t)
	start := "2026-03-01T10:00:00Z"
	f.conversation("conv:a", "Alpha", "rec:a")
	f.recording("rec:a", "conv:a", &start)
	f.turn("t:1", "conv:a", "rec:a", 0, 1000, nil, "fallback probe marker")

	degraded := f.buildSemantic(nil, Request{Query: "fallback probe marker"})
	lexical := f.build(nil, Request{Query: "fallback probe marker", Mode: "lexical"})

	if !containsString(itemIDs(degraded.Items), "t:1") {
		t.Fatalf("degraded build lost the lexical hit: %v", itemIDs(degraded.Items))
	}
	if !reflect.DeepEqual(itemIDs(degraded.Items), itemIDs(lexical.Items)) {
		t.Fatalf("lexical fallback differs from lexical mode: %v vs %v",
			itemIDs(degraded.Items), itemIDs(lexical.Items))
	}
	if !degraded.Degraded {
		t.Fatal("nil semantic backend must degrade the package")
	}
	if !hasGap(degraded, "semantic_unavailable") {
		t.Fatalf("missing semantic_unavailable gap: %+v", degraded.Gaps)
	}
	if !strings.Contains(degraded.DegradeReason, "semantic backend not configured") {
		t.Fatalf("degrade reason = %q", degraded.DegradeReason)
	}
}

func TestModeLexicalSkipsSemanticBranch(t *testing.T) {
	f := newFixture(t)
	start := "2026-03-01T10:00:00Z"
	f.conversation("conv:a", "Alpha", "rec:a")
	f.recording("rec:a", "conv:a", &start)
	f.turn("t:1", "conv:a", "rec:a", 0, 1000, nil, "lexical mode probe")
	f.turn("t:2", "conv:a", "rec:a", 1, 2000, nil, "quantum communication roadmap")

	pkg := f.buildSemantic(&fakeSemantic{hits: []semantic.SemanticHit{
		{Kind: "turn", ArtifactID: "t:2", ConversationID: "conv:a", Score: 0.99},
	}}, Request{Query: "lexical mode probe", Mode: "lexical"})

	if containsString(itemIDs(pkg.Items), "t:2") {
		t.Fatalf("lexical mode ran the semantic branch: %v", itemIDs(pkg.Items))
	}
	if hasGap(pkg, "semantic_unavailable") || hasGap(pkg, "semantic_index_empty") {
		t.Fatalf("lexical mode must not report semantic gaps: %+v", pkg.Gaps)
	}
	if pkg.Trace.Semantic.Indexed != 0 || len(pkg.Trace.Semantic.HitIDs) != 0 {
		t.Fatalf("semantic trace touched in lexical mode: %+v", pkg.Trace.Semantic)
	}
	for _, rank := range pkg.Ranking {
		if hasSource(rank, SourceSemantic) {
			t.Fatalf("semantic source in lexical mode: %+v", rank)
		}
	}
}

func TestModeAcceptsLiteralHybrid(t *testing.T) {
	f := newFixture(t)
	start := "2026-03-01T10:00:00Z"
	f.conversation("conv:a", "Alpha", "rec:a")
	f.recording("rec:a", "conv:a", &start)
	f.turn("t:1", "conv:a", "rec:a", 0, 1000, nil, "hybrid literal probe")

	pkg := f.buildSemantic(nil, Request{Query: "hybrid literal probe", Mode: "hybrid"})
	if !containsString(itemIDs(pkg.Items), "t:1") {
		t.Fatalf("mode=hybrid rejected or empty: %v", itemIDs(pkg.Items))
	}
}

func TestModeRejectsUnknownValue(t *testing.T) {
	f := newFixture(t)
	_, err := Build(context.Background(), Deps{Store: f.store, DB: f.db}, Request{Query: "x", Mode: "fuzzy"})
	if err == nil || err.Error() != "build_context: mode must be hybrid or lexical" {
		t.Fatalf("err = %v, want the documented mode error", err)
	}
}

func TestFusionIsDeterministic(t *testing.T) {
	f := newFixture(t)
	start := "2026-03-01T10:00:00Z"
	f.conversation("conv:a", "Alpha", "rec:a")
	f.recording("rec:a", "conv:a", &start)
	f.turn("t:1", "conv:a", "rec:a", 0, 1000, nil, "determinism probe alpha")
	f.note("n:1", "conv:a", "observation", "determinism probe note", []string{"topic:a"}, []string{"t:1"})

	first := f.buildSemantic(&fakeSemantic{hits: []semantic.SemanticHit{
		{Kind: "turn", ArtifactID: "t:1", ConversationID: "conv:a", Score: 0.77},
	}}, Request{Query: "determinism probe"})
	second := f.buildSemantic(&fakeSemantic{hits: []semantic.SemanticHit{
		{Kind: "turn", ArtifactID: "t:1", ConversationID: "conv:a", Score: 0.77},
	}}, Request{Query: "determinism probe"})

	a, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatalf("identical requests produced different packages:\n%s\n%s", a, b)
	}
}

func TestRecencyBonusOnlyWithClock(t *testing.T) {
	f := newFixture(t)
	fresh := "2026-03-02T10:00:00Z"
	old := "2026-01-01T10:00:00Z"
	f.conversation("conv:fresh", "Fresh", "rec:fresh")
	f.recording("rec:fresh", "conv:fresh", &fresh)
	f.conversation("conv:old", "Old", "rec:old")
	f.recording("rec:old", "conv:old", &old)
	f.turn("t:fresh", "conv:fresh", "rec:fresh", 0, 1000, nil, "recency probe marker")
	f.turn("t:old", "conv:old", "rec:old", 0, 1000, nil, "recency probe marker")

	now := mustTime(t, "2026-03-03T10:00:00Z")
	bonus := f.build(nil, Request{Query: "recency probe marker", Now: now})
	if itemIDs(bonus.Items)[0] != "t:fresh" {
		t.Fatalf("recency bonus ignored: %v", itemIDs(bonus.Items))
	}
	plain := f.build(nil, Request{Query: "recency probe marker"})
	if itemIDs(plain.Items)[0] != "t:fresh" {
		t.Fatalf("tie broken differently without a clock: %v", itemIDs(plain.Items))
	}
	freshRank, _ := rankingOf(bonus, "t:fresh")
	freshPlain, _ := rankingOf(plain, "t:fresh")
	if freshRank.Score <= freshPlain.Score {
		t.Fatalf("recency bonus not applied: %f vs %f", freshRank.Score, freshPlain.Score)
	}
	oldRank, _ := rankingOf(bonus, "t:old")
	if oldRank.Score >= freshRank.Score {
		t.Fatalf("older recording outranked the newer one: %f vs %f", oldRank.Score, freshRank.Score)
	}
}
