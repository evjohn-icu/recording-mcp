package semantic

import (
	"container/heap"
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
	// MinimumSimilarity overrides the default admission threshold when set.
	// A nil value preserves the historical 0.05 cutoff.
	MinimumSimilarity *float64
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
	return r.searchVector(ctx, vectors[0], opts)
}

func (r *SQLiteRetriever) searchVector(ctx context.Context, queryVec Vector, opts SearchOptions) ([]SemanticHit, error) {

	rows, err := r.DB.QueryContext(ctx, candidateSQL, r.Provider.ID(), r.Provider.Model(), r.Provider.Dims(), opts.ConversationID, opts.ConversationID)
	if err != nil {
		return nil, fmt.Errorf("semantic: select candidates: %w", err)
	}
	defer rows.Close()

	limit := searchLimit(opts.Limit)
	hits := make(hitHeap, 0, limit)
	for rows.Next() {
		var kind, id, conv, content, contentHash string
		var blob []byte
		if err := rows.Scan(&kind, &id, &conv, &content, &contentHash, &blob); err != nil {
			return nil, err
		}
		// An artifact can be edited while its replacement embedding is queued.
		// Never expose the old vector until its matching index row is committed.
		if sha256hex(content) != contentHash {
			continue
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
		minimumScore := minSemanticScore
		if r.MinimumSimilarity != nil {
			minimumScore = *r.MinimumSimilarity
		}
		if score < minimumScore {
			continue
		}
		h := SemanticHit{Kind: kind, ArtifactID: id, ConversationID: conv, Score: score}
		if len(hits) < limit {
			heap.Push(&hits, h)
		} else if betterHit(h, hits[0]) {
			hits[0] = h
			heap.Fix(&hits, 0)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	ordered := []SemanticHit(hits)
	sort.Slice(ordered, func(i, j int) bool { return betterHit(ordered[i], ordered[j]) })
	return ordered, nil
}

func searchLimit(n int) int {
	if n <= 0 {
		return defaultSearchLimit
	}
	if n > maxSearchLimit {
		return maxSearchLimit
	}
	return n
}

func betterHit(a, b SemanticHit) bool {
	if a.Score != b.Score {
		return a.Score > b.Score
	}
	if a.Kind != b.Kind {
		return a.Kind < b.Kind
	}
	return a.ArtifactID < b.ArtifactID
}

// hitHeap keeps the worst retained hit at its root, bounding exact-search
// result memory to the requested result count.
type hitHeap []SemanticHit

func (h hitHeap) Len() int           { return len(h) }
func (h hitHeap) Less(i, j int) bool { return betterHit(h[j], h[i]) }
func (h hitHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *hitHeap) Push(x any)        { *h = append(*h, x.(SemanticHit)) }
func (h *hitHeap) Pop() any          { old := *h; n := len(old); x := old[n-1]; *h = old[:n-1]; return x }

// candidateSQL selects embedding rows for one provider/model/dims that still
// resolve to a current, non-ambient artifact. The turn clause reuses the
// store's CurrentTurnPredicate so a superseded revision can never be recalled;
// the note clause mirrors the store's ambient rule (primary evidence turn,
// ordinal 0).
var candidateSQL = `SELECT e.kind, e.artifact_id, e.conversation_id,
 CASE e.kind WHEN 'note' THEN (SELECT n.summary FROM notes n WHERE n.id=e.artifact_id)
             WHEN 'turn' THEN (SELECT t.content FROM turns t WHERE t.id=e.artifact_id) END,
 e.content_sha256, e.vector FROM memory_embeddings e
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
