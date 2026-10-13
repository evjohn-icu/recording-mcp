package projection

import "testing"

// TestCommitmentConflictRuleOnCurrentCommitments pins the supersede-cue
// mechanics directly on hand-built current commitments. The e2e/eval fixtures
// only ever reach proposed commitments (nothing confirms one), so without this
// test the rule would have no coverage at any level.
func TestCommitmentConflictRuleOnCurrentCommitments(t *testing.T) {
	objects := []Object{
		{ID: "n:early", Kind: KindCommitment, Subject: "topic:move",
			Summary: "计划搬去珠海定居", Status: StatusCurrent,
			EvidenceTurnIDs: []string{"t:1"}, DerivedFrom: []string{"note:n:early"},
			ConversationIDs: []string{"conv:1"}},
		{ID: "n:later", Kind: KindCommitment, Subject: "topic:move",
			Summary: "暂时不搬珠海了", Status: StatusCurrent,
			EvidenceTurnIDs: []string{"t:2"}, DerivedFrom: []string{"note:n:later"},
			ConversationIDs: []string{"conv:1"}},
		{ID: "n:prop", Kind: KindCommitment, Subject: "topic:other",
			Summary: "打算下周开始跑步", Status: StatusProposed, StatusReason: "unconfirmed",
			EvidenceTurnIDs: []string{"t:3"}, DerivedFrom: []string{"note:n:prop"},
			ConversationIDs: []string{"conv:1"}},
	}
	ms := func(v int64) *int64 { return &v }
	primaryStart := map[string]*int64{
		"n:early": ms(1000),
		"n:later": ms(2000),
		"n:prop":  ms(3000),
	}

	conflicts := applyCommitmentConflicts(objects, primaryStart)

	earlier, later := objects[0], objects[1]
	if earlier.Status != StatusSuperseded || earlier.StatusReason != "negated_by_later_note" {
		t.Fatalf("earlier = %+v, want superseded/negated_by_later_note", earlier)
	}
	if len(earlier.ConflictWith) != 1 || earlier.ConflictWith[0] != "n:later" {
		t.Fatalf("earlier conflict_with = %v, want [n:later]", earlier.ConflictWith)
	}
	if later.Status != StatusCurrent {
		t.Fatalf("later = %+v, want current", later)
	}
	if len(later.ConflictWith) != 1 || later.ConflictWith[0] != "n:early" {
		t.Fatalf("later conflict_with = %v, want [n:early]", later.ConflictWith)
	}
	if len(conflicts) != 1 {
		t.Fatalf("conflicts = %+v, want exactly one", conflicts)
	}
	c := conflicts[0]
	if c.EarlierID != "n:early" || c.LaterID != "n:later" || c.Cue != "暂时不" || c.Subject != "topic:move" || c.Kind != KindCommitment {
		t.Fatalf("conflict = %+v", c)
	}
	// A proposed commitment must be untouched by the cue rule.
	prop := objects[2]
	if prop.Status != StatusProposed || prop.StatusReason != "unconfirmed" || len(prop.ConflictWith) != 0 {
		t.Fatalf("proposed object mutated by conflict rule: %+v", prop)
	}
}
