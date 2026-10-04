package pipeline

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Coverage is one recorded analysis-coverage entry: it states that a processor
// version has already evaluated one conversation turn, optionally naming the
// decision run that did so.
type Coverage struct {
	ConversationID   string
	TurnID           string
	ProcessorVersion string
	RunID            string
	CoveredAt        time.Time
}

// RecordCoverage marks every turn id as covered for one conversation and
// processor version. Rows are inserted with INSERT OR IGNORE, so recording the
// same (conversation, turn, processor version) again keeps the first row and
// never fails; the primary key makes the call idempotent. Every row is written
// in a single transaction, and an empty turn id list is a no-op.
func RecordCoverage(ctx context.Context, db *sql.DB, conversationID, runID, processorVersion string, turnIDs []string, now time.Time) error {
	if len(turnIDs) == 0 {
		return nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("pipeline: record coverage: begin: %w", err)
	}
	defer tx.Rollback()

	for _, turnID := range turnIDs {
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO analysis_coverage(conversation_id, turn_id, processor_version, run_id, covered_at)
			 VALUES(?, ?, ?, ?, ?)`,
			conversationID, turnID, processorVersion, runID, now.UnixMilli(),
		); err != nil {
			return fmt.Errorf("pipeline: record coverage for turn %s: %w", turnID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("pipeline: record coverage: commit: %w", err)
	}
	return nil
}

// ListCoverage returns the recorded coverage entries for one conversation,
// ordered by coverage time and then turn id. A non-empty processorVersion
// restricts the result to that version; an empty processorVersion returns
// entries for every version.
func ListCoverage(ctx context.Context, db *sql.DB, conversationID, processorVersion string) ([]Coverage, error) {
	query := `SELECT conversation_id, turn_id, processor_version, run_id, covered_at
		 FROM analysis_coverage WHERE conversation_id = ?`
	args := []any{conversationID}
	if processorVersion != "" {
		query += ` AND processor_version = ?`
		args = append(args, processorVersion)
	}
	query += ` ORDER BY covered_at, turn_id, processor_version`

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("pipeline: list coverage: %w", err)
	}
	defer rows.Close()

	out := make([]Coverage, 0)
	for rows.Next() {
		var (
			c         Coverage
			coveredAt int64
		)
		if err := rows.Scan(&c.ConversationID, &c.TurnID, &c.ProcessorVersion, &c.RunID, &coveredAt); err != nil {
			return nil, fmt.Errorf("pipeline: list coverage: scan: %w", err)
		}
		c.CoveredAt = time.UnixMilli(coveredAt)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pipeline: list coverage: read rows: %w", err)
	}
	return out, nil
}

// UncoveredTurnIDs returns the ids of the conversation's turns that have no
// coverage row for the given processor version, in the same deterministic order
// LoadConversationTurns uses: turns with a global start time first (ascending),
// then by segment index, then by original turn id.
func UncoveredTurnIDs(ctx context.Context, db *sql.DB, conversationID, processorVersion string) ([]string, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT t.id
		 FROM turns t
		 LEFT JOIN analysis_coverage ac
		   ON ac.conversation_id = t.conversation_id
		  AND ac.turn_id = t.id
		  AND ac.processor_version = ?
		 WHERE t.conversation_id = ? AND ac.turn_id IS NULL
		 ORDER BY (t.global_start_ms IS NULL), t.global_start_ms, t.segment_index, t.original_turn_id`,
		processorVersion, conversationID)
	if err != nil {
		return nil, fmt.Errorf("pipeline: uncovered turn ids: %w", err)
	}
	defer rows.Close()

	out := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("pipeline: uncovered turn ids: scan: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pipeline: uncovered turn ids: read rows: %w", err)
	}
	return out, nil
}
