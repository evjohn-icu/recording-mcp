package eval

import (
	"context"
	"database/sql"
	"fmt"
	"os"

	"recordingmcp/internal/contextbuild"
	"recordingmcp/internal/memory"
)

// questionEval is the raw per-question evaluation before presentation fields
// are filled in.
type questionEval struct {
	hits             int
	goldFound        int
	recall           float64
	staleLeakage     float64
	relationAssisted int
	coverage         *float64
	invalidRefs      int
}

// staleTurnSet returns the ids of every turn that has at least one superseded
// (active=0) backing attachment, matching the store's own staleness rule.
func staleTurnSet(ctx context.Context, db *sql.DB) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT at.turn_id
		FROM attachment_turns at JOIN source_attachments a ON a.id=at.attachment_id
		WHERE a.active=0`)
	if err != nil {
		return nil, fmt.Errorf("stale turns: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// knownTurnSet returns the ids of every row in the turns table.
func knownTurnSet(ctx context.Context, db *sql.DB) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT id FROM turns`)
	if err != nil {
		return nil, fmt.Errorf("turn ids: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// loadNoteEvidence returns every note's evidence turn ids keyed by note id.
func loadNoteEvidence(ctx context.Context, db *sql.DB) (map[string][]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT note_id, turn_id FROM note_evidence ORDER BY note_id, ordinal`)
	if err != nil {
		return nil, fmt.Errorf("note evidence: %w", err)
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var note, turn string
		if err := rows.Scan(&note, &turn); err != nil {
			return nil, err
		}
		out[note] = append(out[note], turn)
	}
	return out, rows.Err()
}

// evidenceIDs reports the source evidence turn ids of one package item: the
// stored turn row id for turn items and the note's evidence list otherwise.
// contextbuild only populates EvidenceTurnIDs for items recalled by the
// lexical branch, so a note's evidence is resolved from the store when the
// item does not carry it.
func evidenceIDs(item memory.Item, noteEvidence map[string][]string) []string {
	if item.Kind == "turn" {
		if item.ID != "" {
			return []string{item.ID}
		}
		return nil
	}
	if len(item.EvidenceTurnIDs) > 0 {
		return item.EvidenceTurnIDs
	}
	return noteEvidence[item.ID]
}

// evaluateQuestion computes every per-question metric against one package.
func evaluateQuestion(q Question, pkg contextbuild.Package, staleTurns, knownTurns map[string]bool, noteEvidence map[string][]string) questionEval {
	var ev questionEval
	ev.hits = len(pkg.Items)

	// Evidence recall over the first five items.
	first := pkg.Items
	if len(first) > 5 {
		first = first[:5]
	}
	found := map[string]bool{}
	for _, item := range first {
		for _, id := range evidenceIDs(item, noteEvidence) {
			found[id] = true
		}
	}
	for _, gold := range q.GoldEvidenceTurnIDs {
		if found[gold] {
			ev.goldFound++
		}
	}
	if n := len(q.GoldEvidenceTurnIDs); n > 0 {
		ev.recall = float64(ev.goldFound) / float64(n)
	}

	// Stale leakage: an item is stale when the builder says so or when every
	// evidence turn behind it is superseded.
	stale := 0
	invalid := map[string]bool{}
	for _, item := range pkg.Items {
		if item.Stale {
			stale++
			continue
		}
		ids := evidenceIDs(item, noteEvidence)
		if len(ids) > 0 && allStale(ids, staleTurns) {
			stale++
		}
		for _, id := range ids {
			if !knownTurns[id] {
				invalid[id] = true
			}
		}
	}
	for _, ref := range pkg.Evidence {
		if !knownTurns[ref.TurnID] {
			invalid[ref.TurnID] = true
		}
	}
	ev.invalidRefs = len(invalid)
	if ev.hits > 0 {
		ev.staleLeakage = float64(stale) / float64(ev.hits)
	}

	// Relation assistance: a hit whose item carries a topic reached only by a
	// confirmed link expansion.
	lexTopics := map[string]bool{}
	for _, hitID := range pkg.Trace.Lexical.HitIDs {
		for _, item := range pkg.Items {
			if item.ID == hitID {
				for _, topic := range item.TopicIDs {
					lexTopics[topic] = true
				}
			}
		}
	}
	linkTopics := map[string]bool{}
	for _, expansion := range pkg.Trace.LinksExpanded {
		linkTopics[expansion.FromID] = true
		linkTopics[expansion.ToID] = true
	}
	relationOnly := map[string]bool{}
	for topic := range linkTopics {
		if !lexTopics[topic] {
			relationOnly[topic] = true
		}
	}
	gold := map[string]bool{}
	for _, id := range q.GoldEvidenceTurnIDs {
		gold[id] = true
	}
	for _, item := range pkg.Items {
		assisted := false
		for _, topic := range item.TopicIDs {
			if relationOnly[topic] {
				assisted = true
				break
			}
		}
		if !assisted {
			continue
		}
		for _, id := range evidenceIDs(item, noteEvidence) {
			if gold[id] {
				ev.relationAssisted++
			}
		}
	}

	// Topic coverage over the authored gold topics.
	if len(q.GoldTopicIDs) > 0 {
		covered := 0
		packageTopics := map[string]bool{}
		for _, topic := range pkg.Coverage.TopicIDs {
			packageTopics[topic] = true
		}
		for _, topic := range q.GoldTopicIDs {
			if packageTopics[topic] {
				covered++
			}
		}
		v := float64(covered) / float64(len(q.GoldTopicIDs))
		ev.coverage = &v
	}
	return ev
}

func allStale(ids []string, stale map[string]bool) bool {
	for _, id := range ids {
		if !stale[id] {
			return false
		}
	}
	return true
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}
