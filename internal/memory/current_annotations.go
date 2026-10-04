package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// CurrentAnnotations returns the stored annotation snapshot of a conversation
// together with its version. A conversation that does not exist yet, or whose
// annotations_json is empty, yields the zero-value Annotations and an empty
// version so callers can merge into an empty baseline.
func (s *Store) CurrentAnnotations(ctx context.Context, conversationID string) (Annotations, string, error) {
	var version, raw string
	err := s.db.QueryRowContext(ctx,
		`SELECT annotation_version, annotations_json FROM conversations WHERE id = ?`,
		conversationID).Scan(&version, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return Annotations{}, "", nil
	}
	if err != nil {
		return Annotations{}, "", err
	}
	var a Annotations
	if strings.TrimSpace(raw) != "" {
		if err := json.Unmarshal([]byte(raw), &a); err != nil {
			return Annotations{}, "", fmt.Errorf("memory: decode annotations for %s: %w", conversationID, err)
		}
	}
	return a, version, nil
}
