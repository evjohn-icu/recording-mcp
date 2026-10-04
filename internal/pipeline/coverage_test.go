package pipeline

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// seedCoverageConversation inserts a conversation whose four turns have the
// deterministic LoadConversationTurns order [t4, t1, t2, t3]: t4 and t1 carry
// global start times (50 before 100), while t2 and t3 have none and fall back
// to segment index.
func seedCoverageConversation(t *testing.T, db *sql.DB, conversationID string) {
	t.Helper()
	start50 := int64(50)
	start100 := int64(100)
	seedConversation(t, db, conversationID, []struct {
		id      string
		seg     int
		start   *int64
		orig    int
		speaker *string
	}{
		{id: "t1", seg: 0, start: &start100, orig: 1},
		{id: "t2", seg: 1, start: nil, orig: 2},
		{id: "t3", seg: 2, start: nil, orig: 3},
		{id: "t4", seg: 0, start: &start50, orig: 4},
	})
}

func TestRecordCoverageAndList(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedCoverageConversation(t, db, "conv-1")
	now := time.UnixMilli(1700000000000)

	if err := RecordCoverage(ctx, db, "conv-1", "run-1", "extract-v1", []string{"t1", "t3"}, now); err != nil {
		t.Fatalf("record coverage: %v", err)
	}

	got, err := ListCoverage(ctx, db, "conv-1", "extract-v1")
	if err != nil {
		t.Fatalf("list coverage: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListCoverage = %d rows, want 2", len(got))
	}
	// Same covered_at, so the ordering falls back to turn id ascending.
	want := []Coverage{
		{ConversationID: "conv-1", TurnID: "t1", ProcessorVersion: "extract-v1", RunID: "run-1"},
		{ConversationID: "conv-1", TurnID: "t3", ProcessorVersion: "extract-v1", RunID: "run-1"},
	}
	for i, w := range want {
		g := got[i]
		if g.ConversationID != w.ConversationID || g.TurnID != w.TurnID ||
			g.ProcessorVersion != w.ProcessorVersion || g.RunID != w.RunID {
			t.Fatalf("ListCoverage[%d] = %+v, want %+v", i, g, w)
		}
		if !g.CoveredAt.Equal(now) {
			t.Fatalf("ListCoverage[%d].CoveredAt = %v, want %v", i, g.CoveredAt, now)
		}
	}

	// An empty processor version lists every version's rows.
	all, err := ListCoverage(ctx, db, "conv-1", "")
	if err != nil {
		t.Fatalf("list all coverage: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("ListCoverage(\"\") = %d rows, want 2", len(all))
	}

	// A version with no rows yields an empty (non-nil) slice.
	none, err := ListCoverage(ctx, db, "conv-1", "extract-v2")
	if err != nil {
		t.Fatalf("list other version: %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("ListCoverage(extract-v2) = %d rows, want 0", len(none))
	}
}

func TestRecordCoverageIdempotent(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedCoverageConversation(t, db, "conv-1")
	now := time.UnixMilli(1700000000000)

	if err := RecordCoverage(ctx, db, "conv-1", "run-1", "extract-v1", []string{"t1", "t2"}, now); err != nil {
		t.Fatalf("first record: %v", err)
	}
	// Re-recording the same turns under a later run must not duplicate rows and
	// must keep the first run's identity; t3 is new and is added.
	if err := RecordCoverage(ctx, db, "conv-1", "run-2", "extract-v1", []string{"t1", "t2", "t3"}, now.Add(time.Hour)); err != nil {
		t.Fatalf("second record: %v", err)
	}
	if err := RecordCoverage(ctx, db, "conv-1", "run-3", "extract-v1", nil, now); err != nil {
		t.Fatalf("empty record: %v", err)
	}

	got, err := ListCoverage(ctx, db, "conv-1", "extract-v1")
	if err != nil {
		t.Fatalf("list coverage: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("ListCoverage = %d rows, want 3", len(got))
	}
	byTurn := map[string]Coverage{}
	for _, c := range got {
		byTurn[c.TurnID] = c
	}
	for _, id := range []string{"t1", "t2"} {
		if c := byTurn[id]; c.RunID != "run-1" || !c.CoveredAt.Equal(now) {
			t.Fatalf("turn %s kept %+v, want run-1 at %v", id, c, now)
		}
	}
	if c := byTurn["t3"]; c.RunID != "run-2" || !c.CoveredAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("turn t3 = %+v, want run-2 at %v", c, now.Add(time.Hour))
	}
}

func TestUncoveredTurnIDsOrdered(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedCoverageConversation(t, db, "conv-1")

	// With no coverage every turn is uncovered, in LoadConversationTurns order.
	got, err := UncoveredTurnIDs(ctx, db, "conv-1", "extract-v1")
	if err != nil {
		t.Fatalf("uncovered: %v", err)
	}
	assertStrings(t, got, []string{"t4", "t1", "t2", "t3"})

	// The order must agree with LoadConversationTurns exactly.
	turns, err := LoadConversationTurns(ctx, db, "conv-1")
	if err != nil {
		t.Fatalf("load turns: %v", err)
	}
	if len(turns) != len(got) {
		t.Fatalf("LoadConversationTurns = %d turns, want %d", len(turns), len(got))
	}
	for i, tr := range turns {
		if tr.ID != got[i] {
			t.Fatalf("order mismatch at %d: LoadConversationTurns=%s, UncoveredTurnIDs=%s", i, tr.ID, got[i])
		}
	}

	// Covering a non-prefix subset leaves the remaining turns in the same order.
	if err := RecordCoverage(ctx, db, "conv-1", "run-1", "extract-v1", []string{"t1", "t3"}, time.Now()); err != nil {
		t.Fatalf("record coverage: %v", err)
	}
	got, err = UncoveredTurnIDs(ctx, db, "conv-1", "extract-v1")
	if err != nil {
		t.Fatalf("uncovered after record: %v", err)
	}
	assertStrings(t, got, []string{"t4", "t2"})

	// Covering everything yields an empty (non-nil) result.
	if err := RecordCoverage(ctx, db, "conv-1", "run-1", "extract-v1", []string{"t4", "t2"}, time.Now()); err != nil {
		t.Fatalf("record remaining: %v", err)
	}
	got, err = UncoveredTurnIDs(ctx, db, "conv-1", "extract-v1")
	if err != nil {
		t.Fatalf("uncovered after full record: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("uncovered = %v, want empty", got)
	}
}

func TestUncoveredTurnIDsProcessorVersionIsolation(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedCoverageConversation(t, db, "conv-1")
	now := time.UnixMilli(1700000000000)

	if err := RecordCoverage(ctx, db, "conv-1", "run-a", "extract-v1", []string{"t1", "t2"}, now); err != nil {
		t.Fatalf("record v1: %v", err)
	}

	// Coverage under extract-v1 does not hide turns from extract-v2.
	got, err := UncoveredTurnIDs(ctx, db, "conv-1", "extract-v2")
	if err != nil {
		t.Fatalf("uncovered v2: %v", err)
	}
	assertStrings(t, got, []string{"t4", "t1", "t2", "t3"})

	got, err = UncoveredTurnIDs(ctx, db, "conv-1", "extract-v1")
	if err != nil {
		t.Fatalf("uncovered v1: %v", err)
	}
	assertStrings(t, got, []string{"t4", "t3"})

	// Recording a second version adds independent rows.
	if err := RecordCoverage(ctx, db, "conv-1", "run-b", "extract-v2", []string{"t4"}, now); err != nil {
		t.Fatalf("record v2: %v", err)
	}
	got, err = UncoveredTurnIDs(ctx, db, "conv-1", "extract-v2")
	if err != nil {
		t.Fatalf("uncovered v2 after record: %v", err)
	}
	assertStrings(t, got, []string{"t1", "t2", "t3"})

	got, err = UncoveredTurnIDs(ctx, db, "conv-1", "extract-v1")
	if err != nil {
		t.Fatalf("uncovered v1 after v2 record: %v", err)
	}
	assertStrings(t, got, []string{"t4", "t3"})

	all, err := ListCoverage(ctx, db, "conv-1", "")
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("ListCoverage(\"\") = %d rows, want 3", len(all))
	}
	versions := map[string]int{}
	for _, c := range all {
		versions[c.ProcessorVersion]++
	}
	if versions["extract-v1"] != 2 || versions["extract-v2"] != 1 {
		t.Fatalf("version counts = %v, want extract-v1:2 extract-v2:1", versions)
	}
}

// assertStrings fails when got does not match want element for element.
func assertStrings(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}
