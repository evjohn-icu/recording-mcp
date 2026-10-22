package semantic

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	modsqlite "modernc.org/sqlite"
)

var semanticIndexGeneration atomic.Uint64

func init() {
	// modernc's pure-Go pre-update hook catches writes through this process,
	// including writes on the same SQLite connection where data_version stays
	// constant.
	modsqlite.RegisterConnectionHook(func(conn modsqlite.ExecQuerierContext, _ string) error {
		if hooker, ok := conn.(modsqlite.HookRegisterer); ok {
			hooker.RegisterPreUpdateHook(func(data modsqlite.SQLitePreUpdateData) {
				if data.TableName == "memory_embeddings" {
					semanticIndexGeneration.Add(1)
				}
			})
		}
		return nil
	})
}

// BuildHNSWSidecar creates a deterministic per-profile index from already
// stored embeddings. It makes no provider calls and writes atomically.
func BuildHNSWSidecar(ctx context.Context, db *sql.DB, provider Provider, path string) error {
	if db == nil || provider == nil {
		return fmt.Errorf("semantic: database and provider required")
	}
	genBefore := semanticIndexGeneration.Load()
	dataBefore, err := getDataVersion(ctx, db)
	if err != nil {
		return err
	}
	rows, err := db.QueryContext(ctx, `SELECT kind,artifact_id,conversation_id,content_sha256,vector FROM memory_embeddings WHERE provider_id=? AND model_id=? AND dims=? ORDER BY kind,artifact_id`, provider.ID(), provider.Model(), provider.Dims())
	if err != nil {
		return err
	}
	defer rows.Close()
	idx := NewHNSW(provider.Dims())
	sum := sha256.New()
	for rows.Next() {
		var item HNSWItem
		var blob []byte
		if err := rows.Scan(&item.Kind, &item.ID, &item.ConversationID, &item.ContentHash, &blob); err != nil {
			return err
		}
		v, err := Decode(blob, provider.Dims())
		if err != nil {
			return err
		}
		item.Vector = v
		fmt.Fprintf(sum, "%s\x00%s\x00%s\n", item.Kind, item.ID, item.ContentHash)
		if _, err := sum.Write(blob); err != nil {
			return err
		}
		if err := idx.Add(item); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	dataAfter, err := getDataVersion(ctx, db)
	if err != nil {
		return err
	}
	if genBefore != semanticIndexGeneration.Load() || dataBefore != dataAfter {
		return fmt.Errorf("semantic: embeddings changed during sidecar build; retry")
	}
	fingerprint := hex.EncodeToString(sum.Sum(nil))
	return SaveHNSWSidecar(path, sidecarFile{Version: 1, ProviderID: provider.ID(), Model: provider.Model(), Dims: provider.Dims(), Fingerprint: fingerprint, Index: idx})
}

// HNSWRetriever uses a pure-Go HNSW sidecar for candidate generation and then
// rechecks all hits against authoritative SQLite rows and semantic filters.
// A missing/stale sidecar or a filtered candidate set with fewer than K rows
// falls back to exact SQLite retrieval.
type HNSWRetriever struct {
	DB          *sql.DB
	Provider    Provider
	Exact       *SQLiteRetriever
	mu          sync.RWMutex
	index       *HNSW
	generation  uint64
	dataVersion int64
}

func OpenHNSWRetriever(ctx context.Context, db *sql.DB, provider Provider, path string) (*HNSWRetriever, error) {
	if db == nil || provider == nil {
		return nil, fmt.Errorf("semantic: database and provider required")
	}
	genBefore := semanticIndexGeneration.Load()
	dataBefore, dataErr := getDataVersion(ctx, db)
	idx, err := loadMatchingSidecar(ctx, db, provider, path)
	r := &HNSWRetriever{DB: db, Provider: provider, Exact: &SQLiteRetriever{DB: db, Provider: provider}}
	genAfter := semanticIndexGeneration.Load()
	dataAfter, dataAfterErr := getDataVersion(ctx, db)
	if err != nil || dataErr != nil || dataAfterErr != nil || genBefore != genAfter || dataBefore != dataAfter {
		return r, nil
	}
	r.index = idx
	r.generation = genAfter
	r.dataVersion = dataAfter
	return r, nil
}

func getDataVersion(ctx context.Context, db *sql.DB) (int64, error) {
	var version int64
	err := db.QueryRowContext(ctx, `PRAGMA data_version`).Scan(&version)
	return version, err
}
func loadMatchingSidecar(ctx context.Context, db *sql.DB, p Provider, path string) (*HNSW, error) {
	rows, err := db.QueryContext(ctx, `SELECT kind,artifact_id,content_sha256,vector FROM memory_embeddings WHERE provider_id=? AND model_id=? AND dims=? ORDER BY kind,artifact_id`, p.ID(), p.Model(), p.Dims())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sum := sha256.New()
	for rows.Next() {
		var kind, id, contentHash string
		var blob []byte
		if err := rows.Scan(&kind, &id, &contentHash, &blob); err != nil {
			return nil, err
		}
		fmt.Fprintf(sum, "%s\x00%s\x00%s\n", kind, id, contentHash)
		_, _ = sum.Write(blob)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	fp := hex.EncodeToString(sum.Sum(nil))
	return LoadHNSWSidecar(path, p.ID(), p.Model(), fp, p.Dims())
}
func (r *HNSWRetriever) Ready() bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	idx := r.index
	r.mu.RUnlock()
	return r.Provider != nil && r.DB != nil && ((idx != nil && len(idx.Nodes) > 0) || (r.Exact != nil && r.Exact.Ready()))
}
func (r *HNSWRetriever) HasIndex() bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.index != nil
}
func (r *HNSWRetriever) Describe() string {
	if r == nil || r.Provider == nil {
		return ""
	}
	mode := "hnsw"
	r.mu.RLock()
	idx := r.index
	r.mu.RUnlock()
	if idx == nil {
		mode = "exact-fallback"
	}
	return fmt.Sprintf("%s/%s@%d/%s", r.Provider.ID(), r.Provider.Model(), r.Provider.Dims(), mode)
}

// Invalidate waits for graph traversal readers, drops the graph and clears its
// vector storage. Subsequent searches use exact SQLite retrieval. Any file
// deletion policy remains with the caller.
func (r *HNSWRetriever) Invalidate() {
	if r == nil {
		return
	}
	r.mu.Lock()
	idx := r.index
	r.index = nil
	if idx != nil {
		for i := range idx.Nodes {
			for j := range idx.Nodes[i].Item.Vector {
				idx.Nodes[i].Item.Vector[j] = 0
			}
			idx.Nodes[i].Item.Vector = nil
			for j := range idx.Nodes[i].Links {
				idx.Nodes[i].Links[j] = nil
			}
			idx.Nodes[i].Links = nil
		}
		idx.Nodes = nil
		idx.Entry, idx.MaxLevel = -1, -1
	}
	r.mu.Unlock()
}

func (r *HNSWRetriever) Search(ctx context.Context, query string, opts SearchOptions) ([]SemanticHit, error) {
	if r == nil {
		return nil, nil
	}
	if r.Exact == nil || r.Provider == nil {
		return r.exact(ctx, query, opts)
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	vectors, err := r.Provider.Embed(ctx, []string{query})
	if err != nil {
		return nil, err
	}
	if len(vectors) != 1 {
		return nil, fmt.Errorf("semantic: provider returned %d vectors for one query", len(vectors))
	}
	queryVec := vectors[0]
	r.mu.RLock()
	idx := r.index
	if idx == nil {
		r.mu.RUnlock()
		return r.Exact.searchVector(ctx, queryVec, opts)
	}
	dataVersion, err := getDataVersion(ctx, r.DB)
	if err != nil || dataVersion != r.dataVersion || semanticIndexGeneration.Load() != r.generation {
		r.mu.RUnlock()
		return r.Exact.searchVector(ctx, queryVec, opts)
	}
	limit := searchLimit(opts.Limit)
	for _, n := range []int{max(limit*64, 2048)} {
		items := idx.Search(queryVec, n, n)
		r.mu.RUnlock()
		if len(items) == 0 {
			break
		}
		hits, err := r.validateCandidates(ctx, items, queryVec, opts)
		if err != nil {
			return nil, err
		}
		if len(hits) >= limit {
			return hits[:limit], nil
		}
	}
	return r.Exact.searchVector(ctx, queryVec, opts)
}
func (r *HNSWRetriever) exact(ctx context.Context, q string, o SearchOptions) ([]SemanticHit, error) {
	if r == nil || r.Exact == nil {
		return nil, nil
	}
	return r.Exact.Search(ctx, q, o)
}
func (r *HNSWRetriever) validateCandidates(ctx context.Context, items []HNSWItem, q Vector, opts SearchOptions) ([]SemanticHit, error) {
	args := []any{r.Provider.ID(), r.Provider.Model(), r.Provider.Dims(), opts.ConversationID, opts.ConversationID}
	clauses := make([]string, 0, len(items))
	for _, it := range items {
		if !kindAllowed(it.Kind, opts.Kinds) {
			continue
		}
		clauses = append(clauses, "(e.kind=? AND e.artifact_id=?)")
		args = append(args, it.Kind, it.ID)
	}
	if len(clauses) == 0 {
		return nil, nil
	}
	sqlq := candidateSQL + " AND (" + strings.Join(clauses, " OR ") + ")"
	rows, err := r.DB.QueryContext(ctx, sqlq, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	hits := make([]SemanticHit, 0, len(items))
	for rows.Next() {
		var kind, id, conv, content, hash string
		var blob []byte
		if err := rows.Scan(&kind, &id, &conv, &content, &hash, &blob); err != nil {
			return nil, err
		}
		if sha256hex(content) != hash {
			continue
		}
		vec, err := Decode(blob, r.Provider.Dims())
		if err != nil {
			return nil, err
		}
		score, err := Cosine(q, vec)
		if err != nil {
			return nil, err
		}
		if score >= minSemanticScore {
			hits = append(hits, SemanticHit{Kind: kind, ArtifactID: id, ConversationID: conv, Score: score})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(hits, func(i, j int) bool { return betterHit(hits[i], hits[j]) })
	if len(hits) > maxSearchLimit {
		hits = hits[:maxSearchLimit]
	}
	return hits, nil
}
