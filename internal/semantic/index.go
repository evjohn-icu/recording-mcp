package semantic

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"recordingmcp/internal/memory"
)

// embedBatch bounds how many artifacts one Provider.Embed call carries, so a
// remote provider is never asked for an unbounded request body.
const embedBatch = 64

// Indexer builds and prunes the memory_embeddings index for one provider.
type Indexer struct {
	DB       *sql.DB
	Provider Provider
}

// IndexReport summarizes one indexing run. Skipped counts artifacts whose
// content hash already had an embedding for the configured provider/model, so
// a second run over unchanged data reports Skipped equal to the first run's
// Notes+Turns.
type IndexReport struct {
	Notes         int    `json:"notes"`
	Turns         int    `json:"turns"`
	Skipped       int    `json:"skipped"`
	Pruned        int    `json:"pruned"`
	EmbedRequests int    `json:"embed_requests"`
	EmbeddedItems int    `json:"embedded_items"`
	ProviderID    string `json:"provider_id"`
	Model         string `json:"model"`
	SidecarBuilt  bool   `json:"sidecar_built,omitempty"`
	SidecarPath   string `json:"sidecar_path,omitempty"`
}

type artifact struct {
	kind string
	id   string
	conv string
	text string
}

// IndexAll indexes every current non-stale artifact in the database.
func (ix *Indexer) IndexAll(ctx context.Context) (IndexReport, error) {
	return ix.IndexConversation(ctx, "")
}

// IndexConversation indexes the current non-stale artifacts of one
// conversation, or every conversation when conversationID is empty. Stale
// notes are excluded (they are not current); ambient turns and ambient-primary
// notes are indexed on purpose so the query-time filter is what keeps them out
// of results.
func (ix *Indexer) IndexConversation(ctx context.Context, conversationID string) (IndexReport, error) {
	rep := IndexReport{}
	if ix == nil || ix.DB == nil {
		return rep, errors.New("semantic: indexer database is required")
	}
	if ix.Provider == nil {
		return rep, errors.New("semantic: indexer provider is required")
	}
	rep.ProviderID = ix.Provider.ID()
	rep.Model = ix.Provider.Model()

	for _, kind := range []string{"turn", "note"} {
		after := ""
		for {
			batch, err := ix.artifactsPage(ctx, kind, conversationID, after)
			if err != nil {
				return rep, err
			}
			if len(batch) == 0 {
				break
			}
			after = batch[len(batch)-1].id
			if err := ix.indexBatch(ctx, batch, &rep); err != nil {
				return rep, err
			}
			if len(batch) < embedBatch {
				break
			}
		}
	}
	return rep, nil
}

type pendingArtifact struct {
	artifact
	sha string
}

func (ix *Indexer) indexBatch(ctx context.Context, arts []artifact, rep *IndexReport) error {
	todo := make([]pendingArtifact, 0, len(arts))
	for _, a := range arts {
		sum := sha256hex(a.text)
		exists, err := ix.embeddingExists(ctx, a, sum)
		if err != nil {
			return err
		}
		if exists {
			rep.Skipped++
			continue
		}
		todo = append(todo, pendingArtifact{artifact: a, sha: sum})
	}
	if len(todo) == 0 {
		return nil
	}
	texts := make([]string, len(todo))
	for i, p := range todo {
		texts[i] = p.text
	}
	rep.EmbedRequests++
	rep.EmbeddedItems += len(texts)
	vectors, err := EmbedTexts(ctx, ix.Provider, texts, RoleDocument)
	if err != nil {
		return err
	}
	if len(vectors) != len(texts) {
		return fmt.Errorf("semantic: provider returned %d vectors for %d texts", len(vectors), len(texts))
	}
	for i, p := range todo {
		if len(vectors[i]) != ix.Provider.Dims() {
			return fmt.Errorf("semantic: provider returned a %d-dim vector, want %d", len(vectors[i]), ix.Provider.Dims())
		}
		stored, err := ix.storeEmbeddingIfCurrent(ctx, p.artifact, p.sha, vectors[i])
		if err != nil {
			return err
		}
		if !stored {
			rep.Skipped++
			continue
		}
		if p.kind == "note" {
			rep.Notes++
		} else {
			rep.Turns++
		}
	}
	return nil
}

func (ix *Indexer) artifactsPage(ctx context.Context, kind, conversationID, after string) ([]artifact, error) {
	var q string
	alias := "t"
	args := []any{}
	if kind == "turn" {
		q = `SELECT t.id,t.conversation_id,t.content FROM turns t WHERE ` + memory.CurrentTurnPredicate("t")
	} else {
		alias = "n"
		q = `SELECT n.id,n.conversation_id,n.summary FROM notes n WHERE NOT ` + memory.NoteStalePredicate("n")
	}
	if conversationID != "" {
		q += ` AND ` + alias + `.conversation_id=?`
		args = append(args, conversationID)
	}
	if after != "" {
		q += ` AND ` + alias + `.id>?`
		args = append(args, after)
	}
	q += ` ORDER BY ` + alias + `.id LIMIT ?`
	args = append(args, embedBatch)
	rows, err := ix.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]artifact, 0, embedBatch)
	for rows.Next() {
		var a artifact
		a.kind = kind
		if err := rows.Scan(&a.id, &a.conv, &a.text); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Prune deletes embedding rows whose artifact no longer exists in turns or
// notes and returns the number of rows removed.
func (ix *Indexer) Prune(ctx context.Context) (int, error) {
	return ix.prune(ctx, "")
}

// PruneConversation prunes only the embedding rows of one conversation, so a
// scoped indexing run cleans up exactly what it touched.
func (ix *Indexer) PruneConversation(ctx context.Context, conversationID string) (int, error) {
	if strings.TrimSpace(conversationID) == "" {
		return ix.Prune(ctx)
	}
	return ix.prune(ctx, conversationID)
}

func (ix *Indexer) prune(ctx context.Context, conversationID string) (int, error) {
	if ix == nil || ix.DB == nil {
		return 0, errors.New("semantic: indexer database is required")
	}
	query := `DELETE FROM memory_embeddings
		WHERE ((kind='turn' AND NOT EXISTS(SELECT 1 FROM turns t WHERE t.id=memory_embeddings.artifact_id))
		    OR (kind='note' AND NOT EXISTS(SELECT 1 FROM notes n WHERE n.id=memory_embeddings.artifact_id)))`
	args := []any{}
	if conversationID != "" {
		query += ` AND conversation_id=?`
		args = append(args, conversationID)
	}
	res, err := ix.DB.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("semantic: prune: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("semantic: prune count: %w", err)
	}
	return int(n), nil
}

func (ix *Indexer) storeEmbeddingIfCurrent(ctx context.Context, a artifact, sum string, vector Vector) (bool, error) {
	table, contentColumn, currentPredicate := "", "", ""
	switch a.kind {
	case "note":
		table, contentColumn = "notes", "summary"
		currentPredicate = "NOT " + memory.NoteStalePredicate("source")
	case "turn":
		table, contentColumn = "turns", "content"
		currentPredicate = memory.CurrentTurnPredicate("source")
	default:
		return false, fmt.Errorf("semantic: unsupported artifact kind %q", a.kind)
	}
	query := `INSERT OR REPLACE INTO memory_embeddings(kind, artifact_id, conversation_id, content_sha256, provider_id, model_id, dims, vector, created_at)
			SELECT ?, ?, ?, ?, ?, ?, ?, ?, ? WHERE EXISTS(
				SELECT 1 FROM ` + table + ` source WHERE source.id=? AND source.conversation_id=? AND source.` + contentColumn + `=? AND ` + currentPredicate + `)`
	res, err := ix.DB.ExecContext(ctx, query,
		a.kind, a.id, a.conv, sum, ix.Provider.ID(), ix.Provider.Model(), ix.Provider.Dims(),
		Encode(vector), time.Now().UnixMilli(), a.id, a.conv, a.text,
	)
	if err != nil {
		return false, fmt.Errorf("semantic: store embedding: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("semantic: store embedding count: %w", err)
	}
	return n != 0, nil
}

func (ix *Indexer) embeddingExists(ctx context.Context, a artifact, sum string) (bool, error) {
	var one int
	err := ix.DB.QueryRowContext(ctx,
		`SELECT 1 FROM memory_embeddings WHERE kind=? AND artifact_id=? AND provider_id=? AND model_id=? AND dims=? AND content_sha256=? LIMIT 1`,
		a.kind, a.id, ix.Provider.ID(), ix.Provider.Model(), ix.Provider.Dims(), sum,
	).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("semantic: probe embedding: %w", err)
	}
	return true, nil
}

func sha256hex(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}
