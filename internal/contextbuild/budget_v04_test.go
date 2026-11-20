package contextbuild

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"recordingmcp/internal/memory"
)

// itemByID returns the returned item with that id.
func itemByID(pkg Package, id string) (memory.Item, bool) {
	for _, item := range pkg.Items {
		if item.ID == id {
			return item, true
		}
	}
	return memory.Item{}, false
}

// itemTextBytes is the documented item budget weight.
func itemTextBytes(item memory.Item) int { return len(item.Summary) + len(item.Content) }

// evidenceText returns the returned evidence text for a turn id.
func evidenceText(pkg Package, turnID string) (string, bool) {
	for _, ref := range pkg.Evidence {
		if ref.TurnID == turnID {
			return ref.Text, true
		}
	}
	return "", false
}

// evidenceIndexByTurn returns the position of one turn in pkg.Evidence.
func evidenceIndexByTurn(pkg Package, turnID string) int {
	for i, ref := range pkg.Evidence {
		if ref.TurnID == turnID {
			return i
		}
	}
	return -1
}

func TestContextBudgetSkipsOversizedCandidates(t *testing.T) {
	t.Run("oversized_note_then_small_turn", func(t *testing.T) {
		f := newFixture(t)
		start := "2026-03-01T10:00:00Z"
		f.conversation("conv:a", "Alpha", "rec:a")
		f.recording("rec:a", "conv:a", &start)
		f.note("n:huge", "conv:a", "observation", "budget marker "+strings.Repeat("x", 200), nil, nil)
		f.turn("t:small", "conv:a", "rec:a", 0, 1000, nil, "budget marker small turn fact")

		pkg := f.build(nil, Request{Query: "budget marker", MaxBytes: 80})
		if containsString(itemIDs(pkg.Items), "n:huge") {
			t.Fatalf("oversized note was truncated into the package: %v", itemIDs(pkg.Items))
		}
		if !containsString(itemIDs(pkg.Items), "t:small") {
			t.Fatalf("small turn after an oversized note was dropped: %v", itemIDs(pkg.Items))
		}
		if !containsString(pkg.Trace.Budget.SkippedItemIDs, "n:huge") {
			t.Fatalf("skipped item ids = %v, want n:huge", pkg.Trace.Budget.SkippedItemIDs)
		}
		if !pkg.Truncated || !hasGap(pkg, "item_budget_exceeded") {
			t.Fatalf("truncated=%v gaps=%+v", pkg.Truncated, pkg.Gaps)
		}
		if pkg.Trace.Budget.BytesUsed > 80 {
			t.Fatalf("bytes_used = %d exceeds the budget", pkg.Trace.Budget.BytesUsed)
		}
	})

	t.Run("same_source_oversized_note_does_not_hide_turn", func(t *testing.T) {
		f := newFixture(t)
		start := "2026-03-01T10:00:00Z"
		f.conversation("conv:a", "Alpha", "rec:a")
		f.recording("rec:a", "conv:a", &start)
		f.turn("t:1", "conv:a", "rec:a", 0, 1000, nil, "same source marker turn")
		f.note("n:huge", "conv:a", "observation", "same source marker "+strings.Repeat("y", 200), nil, []string{"t:1"})

		pkg := f.build(nil, Request{Query: "same source marker", MaxBytes: 80, MaxItems: 2})
		if containsString(itemIDs(pkg.Items), "n:huge") {
			t.Fatalf("oversized note entered the package: %v", itemIDs(pkg.Items))
		}
		if !containsString(itemIDs(pkg.Items), "t:1") {
			t.Fatalf("turn was deduped by a skipped note's sources: %v", itemIDs(pkg.Items))
		}
	})

	t.Run("utf8_byte_edges_are_exact", func(t *testing.T) {
		f := newFixture(t)
		start := "2026-03-01T10:00:00Z"
		f.conversation("conv:a", "Alpha", "rec:a")
		f.recording("rec:a", "conv:a", &start)
		const summary = "预算📅"
		f.note("n:exact", "conv:a", "observation", summary, nil, nil)
		exact := len(summary)

		fit := f.build(nil, Request{Query: "预算", MaxBytes: exact, MaxItems: 1})
		if _, ok := itemByID(fit, "n:exact"); !ok {
			t.Fatalf("exact-fit note was rejected: items=%v skipped=%v", itemIDs(fit.Items), fit.Trace.Budget.SkippedItemIDs)
		}
		if fit.Trace.Budget.BytesUsed != exact {
			t.Fatalf("bytes_used = %d, want %d", fit.Trace.Budget.BytesUsed, exact)
		}

		short := f.build(nil, Request{Query: "预算", MaxBytes: exact - 1, MaxItems: 1})
		if _, ok := itemByID(short, "n:exact"); ok {
			t.Fatalf("note entered a budget one byte too small: %v", itemIDs(short.Items))
		}
		if !containsString(short.Trace.Budget.SkippedItemIDs, "n:exact") {
			t.Fatalf("skipped item ids = %v", short.Trace.Budget.SkippedItemIDs)
		}
	})
}

func TestContextBudgetEvidenceWindows(t *testing.T) {
	// windowFixture seeds one note citing t:2 between t:1 and t:3.
	windowFixture := func(t *testing.T) *fixture {
		t.Helper()
		f := newFixture(t)
		start := "2026-03-01T10:00:00Z"
		f.conversation("conv:a", "Alpha", "rec:a")
		f.recording("rec:a", "conv:a", &start)
		f.turn("t:1", "conv:a", "rec:a", 0, 1000, nil, "window evidence alpha")
		f.turn("t:2", "conv:a", "rec:a", 1, 2000, nil, "window evidence centre")
		f.turn("t:3", "conv:a", "rec:a", 2, 3000, nil, "window evidence omega")
		f.note("n:1", "conv:a", "observation", "window evidence note", nil, []string{"t:2"})
		return f
	}

	t.Run("center_plus_previous_only", func(t *testing.T) {
		f := windowFixture(t)
		probe := f.build(nil, Request{Query: "window evidence", IncludeRawEvidence: true, MaxItems: 1, MaxBytes: 4096})
		note, ok := itemByID(probe, "n:1")
		if !ok {
			t.Fatalf("probe note missing: %v", itemIDs(probe.Items))
		}
		centre, _ := evidenceText(probe, "t:2")
		previous, _ := evidenceText(probe, "t:1")
		budget := itemTextBytes(note) + len(centre) + len(previous)

		pkg := f.build(nil, Request{Query: "window evidence", IncludeRawEvidence: true, MaxItems: 1, MaxBytes: budget})
		if _, ok := evidenceText(pkg, "t:2"); !ok {
			t.Fatalf("centre turn missing: %+v", pkg.Evidence)
		}
		if _, ok := evidenceText(pkg, "t:1"); !ok {
			t.Fatalf("previous turn missing: %+v", pkg.Evidence)
		}
		if _, ok := evidenceText(pkg, "t:3"); ok {
			t.Fatalf("next turn entered a budget that only fits the previous one: %+v", pkg.Evidence)
		}
		if !containsString(pkg.Trace.Budget.SkippedEvidenceTurnIDs, "t:3") {
			t.Fatalf("skipped evidence = %v, want t:3", pkg.Trace.Budget.SkippedEvidenceTurnIDs)
		}
		if !hasGap(pkg, "evidence_budget_exceeded") {
			t.Fatalf("gaps = %+v", pkg.Gaps)
		}
		// Emitted refs keep transcript order, not attempt order.
		if evidenceIndexByTurn(pkg, "t:1") > evidenceIndexByTurn(pkg, "t:2") {
			t.Fatalf("evidence not in transcript order: %+v", pkg.Evidence)
		}
	})

	t.Run("oversized_center_skips_window_and_continues", func(t *testing.T) {
		f := newFixture(t)
		start := "2026-03-01T10:00:00Z"
		f.conversation("conv:a", "Alpha", "rec:a")
		f.recording("rec:a", "conv:a", &start)
		f.turn("t:first", "conv:a", "rec:a", 0, 1000, nil, "continue marker first")
		f.turn("t:huge", "conv:a", "rec:a", 1, 2000, nil, "continue marker huge "+strings.Repeat("z", 300))
		f.note("n:huge", "conv:a", "observation", "continue marker huge note", nil, []string{"t:huge"})
		f.note("n:second", "conv:a", "observation", "continue marker second note", nil, []string{"t:first"})

		probe := f.build(nil, Request{Query: "continue marker", IncludeRawEvidence: true, MaxBytes: 4096})
		hugeNote, _ := itemByID(probe, "n:huge")
		secondNote, _ := itemByID(probe, "n:second")
		firstText, _ := evidenceText(probe, "t:first")
		budget := itemTextBytes(hugeNote) + itemTextBytes(secondNote) + len(firstText)

		pkg := f.build(nil, Request{Query: "continue marker", IncludeRawEvidence: true, MaxBytes: budget})
		if _, ok := evidenceText(pkg, "t:huge"); ok {
			t.Fatalf("oversized centre entered evidence: %+v", pkg.Evidence)
		}
		if _, ok := evidenceText(pkg, "t:first"); !ok {
			t.Fatalf("window for the next cited item was not attempted: %+v", pkg.Evidence)
		}
		if !containsString(pkg.Trace.Budget.SkippedEvidenceTurnIDs, "t:huge") {
			t.Fatalf("skipped evidence = %v, want t:huge", pkg.Trace.Budget.SkippedEvidenceTurnIDs)
		}
	})

	t.Run("four_cited_items_get_three_windows", func(t *testing.T) {
		f := newFixture(t)
		start := "2026-03-01T10:00:00Z"
		// One recording per cited turn so no centre is another centre's
		// neighbour: the window count is the only limit under test.
		for i := 1; i <= 4; i++ {
			conv := fmt.Sprintf("conv:%d", i)
			rec := fmt.Sprintf("rec:%d", i)
			f.conversation(conv, fmt.Sprintf("Alpha %d", i), rec)
			f.recording(rec, conv, &start)
			f.turn(fmt.Sprintf("t:%d", i), conv, rec, 0, 1000, nil, fmt.Sprintf("three windows fact %d", i))
			f.note(fmt.Sprintf("n:%d", i), conv, "observation", fmt.Sprintf("three windows note %d", i), nil, []string{fmt.Sprintf("t:%d", i)})
		}
		pkg := f.build(nil, Request{Query: "three windows", IncludeRawEvidence: true, MaxBytes: 4096})
		for i := 1; i <= 3; i++ {
			if _, ok := evidenceText(pkg, fmt.Sprintf("t:%d", i)); !ok {
				t.Fatalf("centre for accepted item %d missing: %+v", i, pkg.Evidence)
			}
		}
		if _, ok := evidenceText(pkg, "t:4"); ok {
			t.Fatalf("a fourth window was attached: %+v", pkg.Evidence)
		}
	})

	t.Run("shared_references_dedupe_and_record_items", func(t *testing.T) {
		f := newFixture(t)
		start := "2026-03-01T10:00:00Z"
		f.conversation("conv:a", "Alpha", "rec:a")
		f.recording("rec:a", "conv:a", &start)
		f.turn("t:shared", "conv:a", "rec:a", 0, 1000, nil, "shared reference fact")
		f.turn("t:other", "conv:a", "rec:a", 1, 2000, nil, "shared reference other fact")
		f.note("n:1", "conv:a", "observation", "shared reference note one", nil, []string{"t:shared"})
		f.note("n:2", "conv:a", "observation", "shared reference note two", nil, []string{"t:shared", "t:other"})

		pkg := f.build(nil, Request{Query: "shared reference", IncludeRawEvidence: true, MaxBytes: 4096})
		count := 0
		for _, ref := range pkg.Evidence {
			if ref.TurnID == "t:shared" {
				count++
				if len(ref.UsedByItemIDs) != 2 {
					t.Fatalf("used_by = %v, want both notes", ref.UsedByItemIDs)
				}
			}
		}
		if count != 1 {
			t.Fatalf("shared turn emitted %d times: %+v", count, pkg.Evidence)
		}
	})

	t.Run("same_text_different_turns_are_kept", func(t *testing.T) {
		f := newFixture(t)
		start := "2026-03-01T10:00:00Z"
		f.conversation("conv:a", "Alpha", "rec:a")
		f.recording("rec:a", "conv:a", &start)
		f.turn("t:a", "conv:a", "rec:a", 0, 1000, nil, "identical echoed text")
		f.turn("t:b", "conv:a", "rec:a", 1, 2000, nil, "identical echoed text")
		f.note("n:1", "conv:a", "observation", "echoed text note", nil, []string{"t:a"})

		pkg := f.build(nil, Request{Query: "echoed text", IncludeRawEvidence: true, MaxBytes: 4096})
		if _, ok := evidenceText(pkg, "t:a"); !ok {
			t.Fatalf("centre missing: %+v", pkg.Evidence)
		}
		if _, ok := evidenceText(pkg, "t:b"); !ok {
			t.Fatalf("same-text neighbour was merged away: %+v", pkg.Evidence)
		}
	})

	t.Run("scope_filtered_neighbour_is_not_evidence", func(t *testing.T) {
		f := newFixture(t)
		start := "2026-03-01T10:00:00Z"
		f.conversation("conv:a", "Alpha", "rec:a")
		f.recording("rec:a", "conv:a", &start)
		f.topic("topic:a", "Alpha topic")
		f.topic("topic:b", "Beta topic")
		f.turn("t:1", "conv:a", "rec:a", 0, 1000, nil, "scoped evidence centre")
		f.turn("t:2", "conv:a", "rec:a", 1, 2000, nil, "scoped evidence unrelated")
		f.turnTopic("t:1", "topic:a")
		f.turnTopic("t:2", "topic:b")
		f.note("n:1", "conv:a", "observation", "scoped evidence note", []string{"topic:a"}, []string{"t:1"})

		pkg := f.build(nil, Request{TopicIDs: []string{"topic:a"}, Query: "scoped evidence", IncludeRawEvidence: true, MaxBytes: 4096})
		if _, ok := evidenceText(pkg, "t:1"); !ok {
			t.Fatalf("centre missing: %+v", pkg.Evidence)
		}
		if _, ok := evidenceText(pkg, "t:2"); ok {
			t.Fatalf("out-of-scope neighbour entered evidence: %+v", pkg.Evidence)
		}
		if !containsString(pkg.Trace.Budget.SkippedEvidenceTurnIDs, "t:2") {
			t.Fatalf("skipped evidence = %v, want t:2", pkg.Trace.Budget.SkippedEvidenceTurnIDs)
		}
	})

	t.Run("max_items_reports_generic_gap", func(t *testing.T) {
		f := newFixture(t)
		start := "2026-03-01T10:00:00Z"
		f.conversation("conv:a", "Alpha", "rec:a")
		f.recording("rec:a", "conv:a", &start)
		for i := 1; i <= 3; i++ {
			f.note(fmt.Sprintf("n:%d", i), "conv:a", "observation", fmt.Sprintf("max items note %d", i), nil, nil)
		}
		pkg := f.build(nil, Request{Query: "max items", MaxItems: 2, MaxBytes: 4096})
		if len(pkg.Items) != 2 {
			t.Fatalf("items = %v, want 2", itemIDs(pkg.Items))
		}
		if !pkg.Truncated || !hasGap(pkg, "budget_exhausted") {
			t.Fatalf("truncated=%v gaps=%+v", pkg.Truncated, pkg.Gaps)
		}
	})

	t.Run("invalid_utf8_is_a_content_error", func(t *testing.T) {
		f := newFixture(t)
		start := "2026-03-01T10:00:00Z"
		f.conversation("conv:a", "Alpha", "rec:a")
		f.recording("rec:a", "conv:a", &start)
		f.note("n:bad", "conv:a", "observation", "utf8 marker "+string([]byte{0xff}), nil, nil)
		_, err := Build(context.Background(), Deps{Store: f.store, DB: f.db}, Request{Query: "utf8 marker"})
		if err == nil || !strings.Contains(err.Error(), "invalid_utf8_content") {
			t.Fatalf("err = %v, want invalid_utf8_content", err)
		}
	})

	t.Run("identical_windows_are_deterministic", func(t *testing.T) {
		f := windowFixture(t)
		req := Request{Query: "window evidence", IncludeRawEvidence: true, MaxBytes: 4096}
		first := f.build(nil, req)
		second := f.build(nil, req)
		if !reflect.DeepEqual(first, second) {
			t.Fatalf("identical builds differ")
		}
	})
}

// recordingReranker records the items it was given and reverses them.
type recordingReranker struct {
	seen []string
}

func (r *recordingReranker) Rerank(_ context.Context, _ string, items []memory.Item) ([]memory.Item, error) {
	r.seen = r.seen[:0]
	for _, item := range items {
		r.seen = append(r.seen, item.ID)
	}
	out := make([]memory.Item, len(items))
	for i := range items {
		out[i] = items[len(items)-1-i]
	}
	return out, nil
}

func TestContextBudgetRerankSkipsOversizedCandidate(t *testing.T) {
	f := newFixture(t)
	start := "2026-03-01T10:00:00Z"
	f.conversation("conv:a", "Alpha", "rec:a")
	f.recording("rec:a", "conv:a", &start)
	f.note("n:huge", "conv:a", "observation", "rerank marker "+strings.Repeat("q", maxRerankInputBytes), nil, nil)
	f.note("n:short", "conv:a", "observation", "rerank marker short", nil, nil)
	f.turn("t:short", "conv:a", "rec:a", 0, 1000, nil, "rerank marker turn")

	reranker := &recordingReranker{}
	pkg := f.build(reranker, Request{Query: "rerank marker", RetrievalVersion: "v2", MaxBytes: 4096})

	if containsString(reranker.seen, "n:huge") {
		t.Fatalf("oversized candidate was submitted to rerank: %v", reranker.seen)
	}
	if !containsString(reranker.seen, "n:short") || !containsString(reranker.seen, "t:short") {
		t.Fatalf("rerank subset dropped a fitting candidate: %v", reranker.seen)
	}
	if !pkg.Trace.DecisionRerank {
		t.Fatalf("subset rerank did not run: gaps=%+v", pkg.Gaps)
	}
	if hasGap(pkg, "rerank_failed") {
		t.Fatalf("rerank failed on a valid subset: %+v", pkg.Gaps)
	}
	if !hasGap(pkg, "rerank_input_limited") {
		t.Fatalf("gaps = %+v, want rerank_input_limited", pkg.Gaps)
	}
}
