package eval

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

// retrievalScope is the explicit hard scope of one retrieval question. The
// harness counts any returned item outside it as a scope escape.
type retrievalScope struct {
	ConversationID string   `json:"conversation_id,omitempty"`
	TopicID        string   `json:"topic_id,omitempty"`
	TopicRef       *TurnRef `json:"topic_ref,omitempty"`
	StartedAfter   string   `json:"started_after,omitempty"`
	StartedBefore  string   `json:"started_before,omitempty"`
}

// authoredRetrievalQuestion is the on-disk shape of one retrieval question.
// Gold answers reuse the human turn-ref convention of questions.json; the
// scope carries the question's explicit hard filters.
type authoredRetrievalQuestion struct {
	ID                    string         `json:"id"`
	Query                 string         `json:"query"`
	Purpose               string         `json:"purpose"`
	GoldTurnRefs          []TurnRef      `json:"gold_turn_refs"`
	GoldTopicRefs         []TurnRef      `json:"gold_topic_refs"`
	Scope                 retrievalScope `json:"scope"`
	ExpectAmbientExcluded bool           `json:"expect_ambient_excluded"`
	ExpectStaleExcluded   bool           `json:"expect_stale_excluded"`
	Notes                 string         `json:"notes"`
}

type retrievalFile struct {
	Description string                      `json:"description"`
	Questions   []authoredRetrievalQuestion `json:"questions"`
}

// RetrievalQuestion is one resolved retrieval question.
type RetrievalQuestion struct {
	ID                    string
	Query                 string
	Purpose               string
	Notes                 string
	GoldEvidenceTurnIDs   []string
	GoldTopicIDs          []string
	ScopeConversationID   string
	ScopeTopicID          string
	ScopeStartedAfter     *time.Time
	ScopeStartedBefore    *time.Time
	ExpectAmbientExcluded bool
	ExpectStaleExcluded   bool
}

// LoadRetrievalQuestions reads the retrieval fixture and resolves every human
// reference against the ingested database. A missing file disables the
// retrieval pass.
func LoadRetrievalQuestions(ctx context.Context, db *sql.DB, path string) ([]RetrievalQuestion, error) {
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read retrieval questions: %w", err)
	}
	var file retrievalFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("decode retrieval questions: %w", err)
	}
	if len(file.Questions) == 0 {
		return nil, errors.New("retrieval questions fixture is empty")
	}
	seen := map[string]bool{}
	out := make([]RetrievalQuestion, 0, len(file.Questions))
	for i, aq := range file.Questions {
		if aq.ID == "" {
			return nil, fmt.Errorf("retrieval question %d has no id", i)
		}
		if seen[aq.ID] {
			return nil, fmt.Errorf("duplicate retrieval question id %q", aq.ID)
		}
		seen[aq.ID] = true
		if aq.Query == "" {
			return nil, fmt.Errorf("retrieval question %q has no query", aq.ID)
		}
		q := RetrievalQuestion{
			ID:                    aq.ID,
			Query:                 aq.Query,
			Purpose:               aq.Purpose,
			Notes:                 aq.Notes,
			ExpectAmbientExcluded: aq.ExpectAmbientExcluded,
			ExpectStaleExcluded:   aq.ExpectStaleExcluded,
		}
		for _, ref := range aq.GoldTurnRefs {
			id, err := resolveTurnRef(ctx, db, ref)
			if err != nil {
				return nil, fmt.Errorf("retrieval question %q: %w", aq.ID, err)
			}
			q.GoldEvidenceTurnIDs = appendUnique(q.GoldEvidenceTurnIDs, id)
		}
		for _, ref := range aq.GoldTopicRefs {
			id, err := resolveTopicRef(ctx, db, ref)
			if err != nil {
				return nil, fmt.Errorf("retrieval question %q: %w", aq.ID, err)
			}
			q.GoldTopicIDs = appendUnique(q.GoldTopicIDs, id)
		}
		if len(q.GoldEvidenceTurnIDs) == 0 {
			return nil, fmt.Errorf("retrieval question %q: no gold evidence turns resolved", aq.ID)
		}
		q.ScopeConversationID = aq.Scope.ConversationID
		if q.ScopeConversationID != "" {
			stored, err := resolveConversationID(ctx, db, q.ScopeConversationID)
			if err != nil {
				return nil, fmt.Errorf("retrieval question %q: %w", aq.ID, err)
			}
			q.ScopeConversationID = stored
		}
		q.ScopeTopicID = aq.Scope.TopicID
		if aq.Scope.TopicRef != nil {
			id, err := resolveTopicRef(ctx, db, *aq.Scope.TopicRef)
			if err != nil {
				return nil, fmt.Errorf("retrieval question %q: scope topic: %w", aq.ID, err)
			}
			if q.ScopeTopicID != "" && q.ScopeTopicID != id {
				return nil, fmt.Errorf("retrieval question %q: scope topic_id and topic_ref disagree", aq.ID)
			}
			q.ScopeTopicID = id
		}
		if aq.Scope.StartedAfter != "" {
			t, err := time.Parse(time.RFC3339, aq.Scope.StartedAfter)
			if err != nil {
				return nil, fmt.Errorf("retrieval question %q: started_after: %w", aq.ID, err)
			}
			q.ScopeStartedAfter = &t
		}
		if aq.Scope.StartedBefore != "" {
			t, err := time.Parse(time.RFC3339, aq.Scope.StartedBefore)
			if err != nil {
				return nil, fmt.Errorf("retrieval question %q: started_before: %w", aq.ID, err)
			}
			q.ScopeStartedBefore = &t
		}
		out = append(out, q)
	}
	return out, nil
}

// resolveConversationID maps a scope conversation reference onto the stored
// conversation id. The corpus and fixtures author human source ids
// ("conv-venue-rehearsal"); stored ids are stableKey hashes, so a reference
// that is not itself a stored id is resolved through the ingested source
// snapshots.
func resolveConversationID(ctx context.Context, db *sql.DB, ref string) (string, error) {
	var id string
	err := db.QueryRowContext(ctx, `SELECT id FROM conversations WHERE id=?`, ref).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	err = db.QueryRowContext(ctx,
		`SELECT DISTINCT conversation_id FROM source_attachments WHERE json_extract(source_json,'$.conversation_id')=?`, ref,
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("scope conversation %q not found", ref)
	}
	return id, err
}
