package semantic

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"recordingmcp/internal/memory"
)

// minSemanticScore drops near-orthogonal hits so the semantic branch never
// floods the candidate set with noise. It is a fixed, documented threshold.
const minSemanticScore = 0.05

const (
	defaultSearchLimit = 40
	maxSearchLimit     = 200
)

// SemanticHit is one artifact recalled by vector similarity.
type SemanticHit struct {
	Kind           string
	ArtifactID     string
	ConversationID string
	Score          float64 // cosine in [0, 1]
}

// SearchOptions bounds one semantic search. An empty Kinds means both notes and
// turns; an empty ConversationID leaves the conversation unconstrained.
type SearchOptions struct {
	Kinds          []string // "note"/"turn"; empty = both
	ConversationID string
	Limit          int
}

// SemanticRetriever is the pluggable semantic backend boundary: a backend that
// cannot serve (no index rows, no provider) must degrade, never fail the
// caller's retrieval.
type SemanticRetriever interface {
	Search(ctx context.Context, query string, opts SearchOptions) ([]SemanticHit, error)
	Ready() bool
	Describe() string // provider/model summary for traces
}

// SQLiteRetriever serves semantic recall from memory_embeddings. Its SQL
// applies the same stale and ambient filters the lexical layer uses, so a
// semantic hit can never bypass the isolation invariants.
type SQLiteRetriever struct {
	DB       *sql.DB
	Provider Provider
}

var _ SemanticRetriever = (*SQLiteRetriever)(nil)

// Describe returns a stable provider/model/dims summary for traces.
func (r *SQLiteRetriever) Describe() string {
	if r == nil || r.Provider == nil {
		return ""
	}
	return fmt.Sprintf("%s/%s@%d", r.Provider.ID(), r.Provider.Model(), r.Provider.Dims())
}

// Ready reports whether at least one embedding row exists for the configured
// provider/model/dims. An empty index is not an error; it means the semantic
// branch must degrade.
func (r *SQLiteRetriever) Ready() bool {
	if r == nil || r.DB == nil || r.Provider == nil {
		return false
	}
	var n int
	if err := r.DB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM memory_embeddings WHERE provider_id=? AND model_id=? AND dims=?`,
		r.Provider.ID(), r.Provider.Model(), r.Provider.Dims(),
	).Scan(&n); err != nil {
		return false
	}
	return n > 0
}

// Search embeds the query and returns the closest current, non-ambient
// artifacts. A nil retriever or empty query returns no hits and no error.
func (r *SQLiteRetriever) Search(ctx context.Context, query string, opts SearchOptions) ([]SemanticHit, error) {
	if r == nil || r.DB == nil || r.Provider == nil {
		return nil, nil
	}
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, nil
	}
	vectors, err := r.Provider.Embed(ctx, []string{q})
	if err != nil {
		return nil, err
	}
	if len(vectors) != 1 {
		return nil, fmt.Errorf("semantic: provider returned %d vectors for one query", len(vectors))
	}
	queryVec := vectors[0]

	rows, err := r.DB.QueryContext(ctx, candidateSQL, r.Provider.ID(), r.Provider.Model(), r.Provider.Dims(), opts.ConversationID, opts.ConversationID)
	if err != nil {
		return nil, fmt.Errorf("semantic: select candidates: %w", err)
	}
	defer rows.Close()

	hits := []SemanticHit{}
	for rows.Next() {
		var kind, id, conv string
		var blob []byte
		if err := rows.Scan(&kind, &id, &conv, &blob); err != nil {
			return nil, err
		}
		if !kindAllowed(kind, opts.Kinds) {
			continue
		}
		vec, err := Decode(blob, r.Provider.Dims())
		if err != nil {
			return nil, err
		}
		score, err := Cosine(queryVec, vec)
		if err != nil {
			return nil, err
		}
		if score < minSemanticScore {
			continue
		}
		hits = append(hits, SemanticHit{Kind: kind, ArtifactID: id, ConversationID: conv, Score: score})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		if hits[i].Kind != hits[j].Kind {
			return hits[i].Kind < hits[j].Kind
		}
		return hits[i].ArtifactID < hits[j].ArtifactID
	})
	limit := opts.Limit
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	if limit > maxSearchLimit {
		limit = maxSearchLimit
	}
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

// candidateSQL selects embedding rows for one provider/model/dims that still
// resolve to a current, non-ambient artifact. The turn clause reuses the
// store's CurrentTurnPredicate so a superseded revision can never be recalled;
// the note clause mirrors the store's ambient rule (primary evidence turn,
// ordinal 0).
var candidateSQL = `SELECT e.kind, e.artifact_id, e.conversation_id, e.vector FROM memory_embeddings e
WHERE e.provider_id=? AND e.model_id=? AND e.dims=?
  AND (?='' OR e.conversation_id=?)
  AND (e.kind<>'turn' OR EXISTS(SELECT 1 FROM turns t WHERE t.id=e.artifact_id AND ` + memory.CurrentTurnPredicate("t") + ` AND NOT EXISTS(SELECT 1 FROM turn_classes tc WHERE tc.turn_id=t.id AND tc.class='ambient')))
  AND (e.kind<>'note' OR EXISTS(SELECT 1 FROM notes n WHERE n.id=e.artifact_id AND NOT ` + memory.NoteStalePredicate("n") + ` AND NOT EXISTS(SELECT 1 FROM note_evidence ne JOIN turn_classes tc ON tc.turn_id=ne.turn_id WHERE ne.note_id=n.id AND ne.ordinal=0 AND tc.class='ambient')))`

func kindAllowed(kind string, kinds []string) bool {
	if len(kinds) == 0 {
		return true
	}
	for _, k := range kinds {
		if k == kind {
			return true
		}
	}
	return false
}
