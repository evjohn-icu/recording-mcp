package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"recordingmcp/internal/asrinput"
)

// revisionResult builds a one-segment, multi-turn final transcript snapshot
// whose turns are evenly spaced inside the segment. The i-th turn sits at
// local 100+200i ms, so its global time is offset+100+200i and the ordering of
// a window is deterministic and easy to reason about.
func revisionResult(segment, revision string, offset int64, texts ...string) asrinput.Result {
	r := sampleResult(segment, revision, offset)
	r.Turns = make([]asrinput.Turn, 0, len(texts))
	for i, text := range texts {
		localStart := int64(100 + i*200)
		localEnd := localStart + 100
		globalStart := offset + localStart
		globalEnd := offset + localEnd
		sp := "spk:device-1:1"
		r.Turns = append(r.Turns, asrinput.Turn{
			ID:              fmt.Sprintf("utt-%d", i+1),
			OriginalIndex:   i + 1,
			Text:            text,
			LocalStartMS:    &localStart,
			LocalEndMS:      &localEnd,
			GlobalStartMS:   &globalStart,
			GlobalEndMS:     &globalEnd,
			SpeakerScopedID: &sp,
		})
	}
	return r
}

// withTurnLocals rewrites a revisionResult's turn timestamps to the given
// local start times (each turn ends 100 ms later), keeping global = segment
// offset + local. It lets tests interleave revisions by global time.
func withTurnLocals(r asrinput.Result, locals ...int64) asrinput.Result {
	for i := range r.Turns {
		start, end := locals[i], locals[i]+100
		globalStart, globalEnd := r.Source.SegmentOffsetMS+start, r.Source.SegmentOffsetMS+end
		r.Turns[i].LocalStartMS, r.Turns[i].LocalEndMS = &start, &end
		r.Turns[i].GlobalStartMS, r.Turns[i].GlobalEndMS = &globalStart, &globalEnd
	}
	return r
}

// ingestedTurnID derives the persisted turn id for one utterance of a segment
// revision, matching IngestTranscript's stableKey derivation.
func ingestedTurnID(segment, revision, utterance string) string {
	recordingID := stableKey("recording", "device-1", "rec-1")
	return stableKey("turn", recordingID, segment+"/"+revision+"/"+utterance)
}

// evidenceIDs returns the window turn ids in returned order.
func evidenceIDs(ev EvidenceContext) []string {
	out := make([]string, 0, len(ev.Turns))
	for _, turn := range ev.Turns {
		out = append(out, turn.ID)
	}
	return out
}

// TestEvidenceWindowExcludesSupersededNeighbours pins that the current-state
// evidence window is scoped to the active transcript revision: after rev-2
// replaces rev-1 on the same segment, a window around a rev-2 turn contains
// only rev-2 turns, in deterministic global order, and the superseded rev-1
// neighbours never leak in. The audit window around the same target is
// contrasted to show the full revision history stays reachable.
func TestEvidenceWindowExcludesSupersededNeighbours(t *testing.T) {
	ctx := context.Background()
	s := openTempStore(t)

	if _, err := s.IngestTranscript(ctx, revisionResult("seg-a", "rev-1", 0, "alpha one", "alpha two")); err != nil {
		t.Fatal(err)
	}
	v2 := revisionResult("seg-a", "rev-2", 0, "beta one", "beta two")
	v2.Source.SupersedesRevision = "rev-1"
	if _, err := s.IngestTranscript(ctx, v2); err != nil {
		t.Fatal(err)
	}

	a := ingestedTurnID("seg-a", "rev-1", "utt-1")
	b := ingestedTurnID("seg-a", "rev-1", "utt-2")
	aPrime := ingestedTurnID("seg-a", "rev-2", "utt-1")
	bPrime := ingestedTurnID("seg-a", "rev-2", "utt-2")

	ev, err := s.GetCurrentEvidenceContext(ctx, aPrime, 3, 1<<20)
	if err != nil {
		t.Fatalf("current evidence context: %v", err)
	}
	if got := evidenceIDs(ev); !sameStrings(got, []string{aPrime, bPrime}) {
		t.Fatalf("current window = %v, want only the current neighbours %v", got, []string{aPrime, bPrime})
	}
	for _, turn := range ev.Turns {
		if turn.Stale {
			t.Fatalf("current window contains a stale turn: %+v", turn)
		}
		if turn.ID == a || turn.ID == b {
			t.Fatalf("superseded turn %s leaked into the current window", turn.ID)
		}
	}
	if ev.Stale {
		t.Fatalf("current target reported stale: %+v", ev)
	}

	audit, err := s.GetEvidenceContext(ctx, aPrime, 3, 1<<20)
	if err != nil {
		t.Fatalf("audit evidence context: %v", err)
	}
	if got := evidenceIDs(audit); !sameStrings(got, []string{a, aPrime, b, bPrime}) {
		t.Fatalf("audit window = %v, want the full revision history in transcript order", got)
	}
}

// TestAuditEvidenceStillReadable pins the two read paths around a superseded
// revision: GetEvidenceContext (audit) returns a superseded target and its
// neighbours with per-turn Stale markers and EvidenceContext.Stale set for the
// stale target, while GetCurrentEvidenceContext refuses the stale target with
// an ErrNotFound wrapper and only serves current turns for a current target.
func TestAuditEvidenceStillReadable(t *testing.T) {
	ctx := context.Background()
	s := openTempStore(t)

	if _, err := s.IngestTranscript(ctx, revisionResult("seg-a", "rev-1", 0, "alpha one", "alpha two")); err != nil {
		t.Fatal(err)
	}
	v2 := revisionResult("seg-a", "rev-2", 0, "beta one", "beta two")
	v2.Source.SupersedesRevision = "rev-1"
	if _, err := s.IngestTranscript(ctx, v2); err != nil {
		t.Fatal(err)
	}

	a := ingestedTurnID("seg-a", "rev-1", "utt-1")
	b := ingestedTurnID("seg-a", "rev-1", "utt-2")
	aPrime := ingestedTurnID("seg-a", "rev-2", "utt-1")
	bPrime := ingestedTurnID("seg-a", "rev-2", "utt-2")

	ev, err := s.GetEvidenceContext(ctx, a, 3, 1<<20)
	if err != nil {
		t.Fatalf("audit evidence context: %v", err)
	}
	if got := evidenceIDs(ev); !sameStrings(got, []string{a, aPrime, b, bPrime}) {
		t.Fatalf("audit window = %v, want the superseded target and every neighbour", got)
	}
	wantStale := map[string]bool{a: true, aPrime: false, b: true, bPrime: false}
	targets := 0
	for _, turn := range ev.Turns {
		if turn.Stale != wantStale[turn.ID] {
			t.Fatalf("turn %s Stale=%v, want %v", turn.ID, turn.Stale, wantStale[turn.ID])
		}
		if turn.IsTarget {
			targets++
			if turn.ID != a {
				t.Fatalf("target flag on %s, want %s", turn.ID, a)
			}
		}
	}
	if targets != 1 {
		t.Fatalf("target flags = %d, want exactly one", targets)
	}
	if !ev.Stale {
		t.Fatalf("superseded target not flagged stale: %+v", ev)
	}

	if _, err := s.GetCurrentEvidenceContext(ctx, a, 3, 1<<20); !errors.Is(err, ErrNotFound) {
		t.Fatalf("current lookup of superseded turn err = %v, want ErrNotFound", err)
	} else if !strings.Contains(err.Error(), "turn not found in current revision") {
		t.Fatalf("current lookup error = %v, want the current-revision message", err)
	}

	cur, err := s.GetCurrentEvidenceContext(ctx, aPrime, 3, 1<<20)
	if err != nil {
		t.Fatalf("current lookup of current turn: %v", err)
	}
	if got := evidenceIDs(cur); !sameStrings(got, []string{aPrime, bPrime}) {
		t.Fatalf("current window = %v, want current turns only", got)
	}
	if cur.Stale {
		t.Fatalf("current target reported stale: %+v", cur)
	}
}

// TestCurrentEvidenceWindowOrdering pins that filtering the window to the
// current revision preserves the global transcript ordering: with rev-1 and
// rev-2 turns interleaved by global_start_ms across segments, the current-only
// window keeps exactly the current turns in ascending global order, while the
// audit window shows where the filtered-out stale turns sat.
func TestCurrentEvidenceWindowOrdering(t *testing.T) {
	ctx := context.Background()
	s := openTempStore(t)

	// seg-a rev-1: A at global 100, B at global 500 (stale after supersede).
	if _, err := s.IngestTranscript(ctx, withTurnLocals(revisionResult("seg-a", "rev-1", 0, "a one", "a two"), 100, 500)); err != nil {
		t.Fatal(err)
	}
	// seg-b rev-1 (same recording, offset 0): C at global 200, D at global 600.
	if _, err := s.IngestTranscript(ctx, withTurnLocals(revisionResult("seg-b", "rev-1", 0, "b one", "b two"), 200, 600)); err != nil {
		t.Fatal(err)
	}
	// seg-a rev-2: A' at global 300, B' at global 700, interleaving seg-b.
	v2 := withTurnLocals(revisionResult("seg-a", "rev-2", 0, "a one v2", "a two v2"), 300, 700)
	v2.Source.SupersedesRevision = "rev-1"
	if _, err := s.IngestTranscript(ctx, v2); err != nil {
		t.Fatal(err)
	}

	a := ingestedTurnID("seg-a", "rev-1", "utt-1")
	b := ingestedTurnID("seg-a", "rev-1", "utt-2")
	aPrime := ingestedTurnID("seg-a", "rev-2", "utt-1")
	bPrime := ingestedTurnID("seg-a", "rev-2", "utt-2")
	c := ingestedTurnID("seg-b", "rev-1", "utt-1")
	d := ingestedTurnID("seg-b", "rev-1", "utt-2")

	audit, err := s.GetEvidenceContext(ctx, c, 10, 1<<20)
	if err != nil {
		t.Fatalf("audit evidence context: %v", err)
	}
	if got := evidenceIDs(audit); !sameStrings(got, []string{a, c, aPrime, b, d, bPrime}) {
		t.Fatalf("audit window = %v, want stale and current turns interleaved by global time", got)
	}

	cur, err := s.GetCurrentEvidenceContext(ctx, c, 10, 1<<20)
	if err != nil {
		t.Fatalf("current evidence context: %v", err)
	}
	if got := evidenceIDs(cur); !sameStrings(got, []string{c, aPrime, d, bPrime}) {
		t.Fatalf("current window = %v, want current turns in global order after filtering", got)
	}
	for _, turn := range cur.Turns {
		if turn.Stale {
			t.Fatalf("current window contains stale turn %s", turn.ID)
		}
	}
}
