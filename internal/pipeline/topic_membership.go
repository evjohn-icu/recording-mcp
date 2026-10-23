package pipeline

import (
	"context"
	"database/sql"
	"strings"

	"recordingmcp/internal/memory"
)

// rebuildAutomaticTopicMembership drops automatic turn-topic links whose turn
// is no longer current. Human-authored links are retained unchanged as audit
// history. Call after merging candidates and before applying annotations.
func rebuildAutomaticTopicMembership(ctx context.Context, db *sql.DB, conversationID string, annotations memory.Annotations) (memory.Annotations, error) {
	rows, err := db.QueryContext(ctx, `SELECT t.id FROM turns t WHERE t.conversation_id=? AND `+memory.CurrentTurnPredicate("t"), conversationID)
	if err != nil {
		return memory.Annotations{}, err
	}
	active := make(map[string]bool)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return memory.Annotations{}, err
		}
		active[id] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return memory.Annotations{}, err
	}
	if err := rows.Close(); err != nil {
		return memory.Annotations{}, err
	}
	out := annotations
	out.TurnTopics = make([]memory.TurnTopicAnnotation, 0, len(annotations.TurnTopics))
	for _, link := range annotations.TurnTopics {
		autoOnly := len(link.TopicIDs) > 0
		for _, id := range link.TopicIDs {
			if !strings.HasPrefix(id, autoTopicPrefix) {
				autoOnly = false
				break
			}
		}
		if autoOnly && !active[link.TurnID] {
			continue
		}
		out.TurnTopics = append(out.TurnTopics, link)
	}
	return out, nil
}
