package memory

// CurrentTurnPredicate returns the SQL predicate selecting turns that belong
// to the current state: legacy turns without any source attachment row stay
// current, turns of a superseded attachment (active=0) do not. alias must name
// the turns table or its alias in the caller's query.
//
// Every current-state surface (retrieval, statistics, gap analysis) must scope
// turns through this predicate instead of copying the exclusion SQL; the audit
// surfaces deliberately query without it.
func CurrentTurnPredicate(alias string) string {
	return "NOT EXISTS(SELECT 1 FROM attachment_turns at JOIN source_attachments a ON a.id=at.attachment_id WHERE at.turn_id=" + alias + ".id AND a.active=0)"
}

// turnStalePredicate is the inverse of CurrentTurnPredicate: it matches turns
// whose source attachment was superseded. Legacy turns never match.
func turnStalePredicate(alias string) string {
	return "EXISTS(SELECT 1 FROM attachment_turns at JOIN source_attachments a ON a.id=at.attachment_id WHERE at.turn_id=" + alias + ".id AND a.active=0)"
}

// NoteStalePredicate returns the SQL predicate matching derived notes whose
// staleness state is marked (built from a superseded attachment). alias must
// name the notes table or its alias in the caller's query. Notes without a
// derived_artifacts row (legacy data) are current and never match.
func NoteStalePredicate(alias string) string {
	return "EXISTS(SELECT 1 FROM derived_artifacts d WHERE d.kind='note' AND d.artifact_id=" + alias + ".id AND d.stale=1)"
}
