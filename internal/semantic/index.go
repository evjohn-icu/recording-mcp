package semantic

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
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
	Notes      int    `json:"notes"`
	Turns      int    `json:"turns"`
	Skipped    int    `json:"skipped"`
	Pruned     int    `json:"pruned"`
	ProviderID string `json:"provider_id"`
	Model      string `json:"model"`
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

	arts, err := ix.artifacts(ctx, conversationID)
	if err != nil {
		return rep, err
	}
	type pending struct {
		artifact
		sha string
	}
	todo := make([]pending, 0, len(arts))
	for _, a := range arts {
		sum := sha256hex(a.text)
		exists, err := ix.embeddingExists(ctx, a, sum)
		if err != nil {
			return rep, err
		}
		if exists {
			rep.Skipped++
			continue
		}
		todo = append(todo, pending{artifact: a, sha: sum})
	}
	for start := 0; start < len(todo); start += embedBatch {
		end := min(start+embedBatch, len(todo))
		texts := make([]string, 0, end-start)
		for _, p := range todo[start:end] {
			texts = append(texts, p.text)
		}
		vectors, err := ix.Provider.Embed(ctx, texts)
		if err != nil {
			return rep, err
		}
		if len(vectors) != len(texts) {
			return rep, fmt.Errorf("semantic: provider returned %d vectors for %d texts", len(vectors), len(texts))
		}
		for i, p := range todo[start:end] {
			if len(vectors[i]) != ix.Provider.Dims() {
				return rep, fmt.Errorf("semantic: provider returned a %d-dim vector, want %d", len(vectors[i]), ix.Provider.Dims())
			}
			if _, err := ix.DB.ExecContext(ctx,
				`INSERT OR REPLACE INTO memory_embeddings(kind, artifact_id, conversation_id, content_sha256, provider_id, model_id, dims, vector, created_at)
				 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				p.kind, p.id, p.conv, p.sha, ix.Provider.ID(), ix.Provider.Model(), ix.Provider.Dims(),
				Encode(vectors[i]), time.Now().UnixMilli(),
			); err != nil {
				return rep, fmt.Errorf("semantic: store embedding: %w", err)
			}
			switch p.kind {
			case "note":
				rep.Notes++
			case "turn":
				rep.Turns++
			}
		}
	}
	return rep, nil
}

// Prune deletes embedding rows whose artifact no longer exists in turns or
// notes and returns the number of rows removed.
func (ix *Indexer) Prune(ctx context.Context) (int, error) {
	if ix == nil || ix.DB == nil {
		return 0, errors.New("semantic: indexer database is required")
	}
	res, err := ix.DB.ExecContext(ctx, `DELETE FROM memory_embeddings
		WHERE (kind='turn' AND NOT EXISTS(SELECT 1 FROM turns t WHERE t.id=memory_embeddings.artifact_id))
		   OR (kind='note' AND NOT EXISTS(SELECT 1 FROM notes n WHERE n.id=memory_embeddings.artifact_id))`)
	if err != nil {
		return 0, fmt.Errorf("semantic: prune: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("semantic: prune count: %w", err)
	}
	return int(n), nil
}

func (ix *Indexer) artifacts(ctx context.Context, conversationID string) ([]artifact, error) {
	out := []artifact{}
	turnSQL := `SELECT t.id, t.conversation_id, t.content FROM turns t WHERE ` + memory.CurrentTurnPredicate("t")
	turnArgs := []any{}
	if conversationID != "" {
		turnSQL += ` AND t.conversation_id=?`
		turnArgs = append(turnArgs, conversationID)
	}
	turnSQL += ` ORDER BY t.id`
	rows, err := ix.DB.QueryContext(ctx, turnSQL, turnArgs...)
	if err != nil {
		return nil, fmt.Errorf("semantic: select turns: %w", err)
	}
	for rows.Next() {
		var a artifact
		a.kind = "turn"
		if err := rows.Scan(&a.id, &a.conv, &a.text); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	noteSQL := `SELECT n.id, n.conversation_id, n.summary FROM notes n WHERE NOT ` + memory.NoteStalePredicate("n")
	noteArgs := []any{}
	if conversationID != "" {
		noteSQL += ` AND n.conversation_id=?`
		noteArgs = append(noteArgs, conversationID)
	}
	noteSQL += ` ORDER BY n.id`
	nrows, err := ix.DB.QueryContext(ctx, noteSQL, noteArgs...)
	if err != nil {
		return nil, fmt.Errorf("semantic: select notes: %w", err)
	}
	defer nrows.Close()
	for nrows.Next() {
		var a artifact
		a.kind = "note"
		if err := nrows.Scan(&a.id, &a.conv, &a.text); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	if err := nrows.Err(); err != nil {
		return nil, err
	}
	return out, nil
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
