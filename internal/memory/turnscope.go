package memory

import (
	"context"
)

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

// ItemAmbient reports whether one item is environmental audio rather than
// spoken conversation: a turn classified ambient, or a note whose primary
// evidence turn (ordinal 0) is. It mirrors the store's own search filters so a
// retrieval branch that resolves items by id — where the SQL-level filters
// never ran — enforces exactly the same rule.
func (s *Store) ItemAmbient(ctx context.Context, item Item) (bool, error) {
	switch {
	case item.Kind == "turn":
		return s.turnAmbient(ctx, item.ID)
	case item.Kind == "conversation":
		return false, nil
	default:
		var ambient bool
		err := s.db.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM note_evidence ne JOIN turn_classes tc ON tc.turn_id=ne.turn_id WHERE ne.note_id=? AND ne.ordinal=0 AND tc.class='ambient')`,
			item.ID,
		).Scan(&ambient)
		return ambient, err
	}
}
