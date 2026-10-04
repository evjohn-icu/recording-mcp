package pipeline

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"
)

// LinkStatus values recorded by ValidateTopicLinks and stored in memory_links.
// Validation only classifies a proposal as accepted ("validated") or rejected;
// InsertLinks stores accepted proposals as "proposed" suggestions awaiting
// human review, because the curator never merges automatically.
const (
	linkStatusValidated = "validated"
	linkStatusRejected  = "rejected"
	linkStatusProposed  = "proposed"
)

// maxLinkReasonRunes bounds the free-text reason of a link proposal.
const maxLinkReasonRunes = 500

// topicLinkRelations is the closed set of relations allowed between two topics.
var topicLinkRelations = map[string]bool{
	"same_topic":    true,
	"follow_up":     true,
	"contradiction": true,
	"update":        true,
}

// LinkProposal is one association an agent proposes between two memory records.
// For the topic_link kind FromTopicID and ToTopicID name existing topics and
// EvidenceTurnIDs lists the turns supporting the association.
type LinkProposal struct {
	Kind            string
	FromTopicID     string
	ToTopicID       string
	Relation        string
	Reason          string
	EvidenceTurnIDs []string
	Confidence      float64
}

// ValidatedLink is a proposal together with the outcome of evidence
// validation. Status is "validated" when the proposal passed every rule and
// "rejected" otherwise; RejectionReason is only meaningful for rejected links.
type ValidatedLink struct {
	LinkProposal
	Status          string
	RejectionReason string
}

// MemoryLink is one persisted memory_links row. FromKind/FromID and
// ToKind/ToID identify the two endpoints, EvidenceTurnIDs is the decoded
// evidence list and CreatedAt is derived from the unix-millisecond column.
type MemoryLink struct {
	ID              string
	FromKind        string
	FromID          string
	ToKind          string
	ToID            string
	Relation        string
	Reason          string
	EvidenceTurnIDs []string
	Confidence      float64
	Status          string
	Model           string
	CreatedAt       time.Time
}

// ValidateTopicLinks checks each proposal against the topic-link rules: the
// kind must be "topic_link", both topics must exist and differ, the relation
// must be one of the known values, the reason must be non-empty and at most
// 500 runes, at least one evidence turn must be present and every evidence
// turn must exist, and the confidence must lie in [0, 1]. Every proposal is
// returned in input order with its status; a rejected proposal keeps its
// original fields and gains a RejectionReason. The error is only non-nil for a
// database failure.
func ValidateTopicLinks(ctx context.Context, db *sql.DB, proposals []LinkProposal) ([]ValidatedLink, error) {
	// Collect the ids that need to exist so existence is resolved with one
	// query per table instead of one query per proposal.
	topicIDs := make([]string, 0, len(proposals)*2)
	turnIDs := make([]string, 0, len(proposals))
	for _, p := range proposals {
		if p.FromTopicID != "" {
			topicIDs = append(topicIDs, p.FromTopicID)
		}
		if p.ToTopicID != "" {
			topicIDs = append(topicIDs, p.ToTopicID)
		}
		for _, id := range p.EvidenceTurnIDs {
			if id != "" {
				turnIDs = append(turnIDs, id)
			}
		}
	}
	topics, err := existingIDs(ctx, db, "topics", topicIDs)
	if err != nil {
		return nil, err
	}
	turns, err := existingIDs(ctx, db, "turns", turnIDs)
	if err != nil {
		return nil, err
	}

	out := make([]ValidatedLink, 0, len(proposals))
	for _, p := range proposals {
		link := ValidatedLink{LinkProposal: p}
		switch reason := rejectionReason(p, topics, turns); {
		case reason != "":
			link.Status = linkStatusRejected
			link.RejectionReason = reason
		default:
			link.Status = linkStatusValidated
		}
		out = append(out, link)
	}
	return out, nil
}

// rejectionReason returns the first rule a proposal violates, or "" when the
// proposal is valid.
func rejectionReason(p LinkProposal, topics, turns map[string]bool) string {
	if p.Kind != "topic_link" {
		return fmt.Sprintf("kind must be topic_link, got %q", p.Kind)
	}
	if !topics[p.FromTopicID] {
		return fmt.Sprintf("from topic %q not found", p.FromTopicID)
	}
	if !topics[p.ToTopicID] {
		return fmt.Sprintf("to topic %q not found", p.ToTopicID)
	}
	if p.FromTopicID == p.ToTopicID {
		return "from and to topic must differ"
	}
	if !topicLinkRelations[p.Relation] {
		return fmt.Sprintf("invalid relation %q", p.Relation)
	}
	if strings.TrimSpace(p.Reason) == "" {
		return "reason is empty"
	}
	if utf8.RuneCountInString(strings.TrimSpace(p.Reason)) > maxLinkReasonRunes {
		return fmt.Sprintf("reason exceeds %d characters", maxLinkReasonRunes)
	}
	if len(p.EvidenceTurnIDs) == 0 {
		return "evidence must contain at least one turn"
	}
	for _, id := range p.EvidenceTurnIDs {
		if !turns[id] {
			return fmt.Sprintf("evidence turn %q not found", id)
		}
	}
	if math.IsNaN(p.Confidence) || p.Confidence < 0 || p.Confidence > 1 {
		return fmt.Sprintf("confidence %v out of range [0,1]", p.Confidence)
	}
	return ""
}

// existingIDs returns the subset of ids present in the given table. The table
// name is a package-internal constant, never caller input.
func existingIDs(ctx context.Context, db *sql.DB, table string, ids []string) (map[string]bool, error) {
	found := map[string]bool{}
	if len(ids) == 0 {
		return found, nil
	}
	unique := make([]string, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		unique = append(unique, id)
	}
	if len(unique) == 0 {
		return found, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(unique)), ",")
	args := make([]any, len(unique))
	for i, id := range unique {
		args[i] = id
	}
	rows, err := db.QueryContext(ctx, "SELECT id FROM "+table+" WHERE id IN ("+placeholders+")", args...)
	if err != nil {
		return nil, fmt.Errorf("pipeline: check %s existence: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("pipeline: check %s existence: scan: %w", table, err)
		}
		found[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pipeline: check %s existence: read: %w", table, err)
	}
	return found, nil
}

// linkID derives the deterministic id of a memory link from its endpoint and
// relation tuple. The same tuple always yields the same id, which is what makes
// InsertLinks idempotent.
func linkID(fromKind, fromID, toKind, toID, relation string) string {
	sum := sha256.Sum256([]byte(strings.Join(
		[]string{fromKind, fromID, toKind, toID, relation}, "|")))
	return "link:" + hex.EncodeToString(sum[:])[:32]
}

// InsertLinks stores accepted topic-link proposals as "proposed" suggestions
// and returns their deterministic ids in input order. Rejected links and links
// whose kind is not topic_link are skipped: rejected evidence is never
// recorded, and only topic endpoints are written. Rows are inserted with
// INSERT OR IGNORE, so re-inserting the same (from, to, relation) tuple is a
// no-op; all writes happen in a single transaction.
func InsertLinks(ctx context.Context, db *sql.DB, model string, links []ValidatedLink, now time.Time) ([]string, error) {
	ids := make([]string, 0, len(links))

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("pipeline: insert links: begin: %w", err)
	}
	defer tx.Rollback()

	for _, l := range links {
		if l.Kind != "topic_link" || l.Status != linkStatusValidated {
			continue
		}
		evidenceJSON, err := json.Marshal(evidenceOrEmpty(l.EvidenceTurnIDs))
		if err != nil {
			return nil, fmt.Errorf("pipeline: insert links: marshal evidence: %w", err)
		}
		id := linkID("topic", l.FromTopicID, "topic", l.ToTopicID, l.Relation)
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO memory_links(id, from_kind, from_id, to_kind, to_id, relation, reason, evidence_turn_ids_json, confidence, status, model, created_at)
			 VALUES(?, 'topic', ?, 'topic', ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, l.FromTopicID, l.ToTopicID, l.Relation, l.Reason,
			string(evidenceJSON), l.Confidence, linkStatusProposed, model, now.UnixMilli(),
		); err != nil {
			return nil, fmt.Errorf("pipeline: insert links: insert %s: %w", id, err)
		}
		ids = append(ids, id)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("pipeline: insert links: commit: %w", err)
	}
	return ids, nil
}

// ListLinks loads memory links in deterministic order (created_at, then id).
// An empty status returns links of every status; otherwise only rows with
// exactly that status are returned.
func ListLinks(ctx context.Context, db *sql.DB, status string) ([]MemoryLink, error) {
	query := `SELECT id, from_kind, from_id, to_kind, to_id, relation, reason, evidence_turn_ids_json, confidence, status, model, created_at
	          FROM memory_links`
	args := []any{}
	if status != "" {
		query += ` WHERE status = ?`
		args = append(args, status)
	}
	query += ` ORDER BY created_at, id`

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("pipeline: list links: %w", err)
	}
	defer rows.Close()

	var out []MemoryLink
	for rows.Next() {
		var (
			l              MemoryLink
			evidenceJSON   string
			createdAtMilli int64
		)
		if err := rows.Scan(&l.ID, &l.FromKind, &l.FromID, &l.ToKind, &l.ToID,
			&l.Relation, &l.Reason, &evidenceJSON, &l.Confidence, &l.Status, &l.Model,
			&createdAtMilli); err != nil {
			return nil, fmt.Errorf("pipeline: list links: scan: %w", err)
		}
		l.CreatedAt = time.UnixMilli(createdAtMilli)
		if err := json.Unmarshal([]byte(evidenceJSON), &l.EvidenceTurnIDs); err != nil {
			return nil, fmt.Errorf("pipeline: list links %s: unmarshal evidence: %w", l.ID, err)
		}
		if l.EvidenceTurnIDs == nil {
			l.EvidenceTurnIDs = []string{}
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pipeline: list links: read: %w", err)
	}
	return out, nil
}
