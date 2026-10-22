package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"recordingmcp/internal/process"
)

// ValidationError reports a request that fails ApplyAnnotations' input
// contract. Its message is the exact literal the MCP tool returns, so typed
// callers can map it onto a 400 without changing the text.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

// AnnotationConflictError reports that expected_version does not match the
// stored annotation version. Current is the version the caller lost against.
type AnnotationConflictError struct{ Expected, Current string }

func (e *AnnotationConflictError) Error() string {
	return fmt.Sprintf("annotation version conflict: expected %s, current %s", e.Expected, e.Current)
}

// AnnotationVersionReusedError reports that annotation_version already exists
// in annotation_history, so replaying it would rewrite history.
type AnnotationVersionReusedError struct{ Version string }

func (e *AnnotationVersionReusedError) Error() string {
	return fmt.Sprintf("annotation version %s was already used", e.Version)
}

// AnnotationSnapshot is an immutable annotation revision. The current snapshot
// is included by GetAnnotationHistory along with previously archived versions.
type AnnotationSnapshot struct {
	Version     string      `json:"version"`
	Basis       string      `json:"basis"`
	Summary     string      `json:"summary"`
	Annotations Annotations `json:"annotations"`
	Current     bool        `json:"current"`
}

// ApplyAnnotations atomically replaces the derived memory view for a
// conversation. EvidenceTurnIDs and TurnTopics.TurnID use original transcript
// IDs, scoped to this conversation.
func (s *Store) ApplyAnnotations(ctx context.Context, conversationID, expectedVersion string, a Annotations) error {
	if !validID(conversationID) || (a.ConversationID != "" && a.ConversationID != conversationID) {
		return &ValidationError{Msg: "conversation_id, when provided, must match the target conversation"}
	}
	if (expectedVersion != "" && !validID(expectedVersion)) || !validID(a.AnnotationVersion) {
		return &ValidationError{Msg: "new annotation_version is required and expected_version must be valid"}
	}
	if strings.TrimSpace(a.Title) == "" {
		return &ValidationError{Msg: "title is required"}
	}
	topics := make(map[string]TopicAnnotation, len(a.Topics))
	for _, t := range a.Topics {
		if !validID(t.ID) || strings.TrimSpace(t.Title) == "" {
			return &ValidationError{Msg: fmt.Sprintf("invalid topic %q", t.ID)}
		}
		if _, exists := topics[t.ID]; exists {
			return &ValidationError{Msg: fmt.Sprintf("duplicate topic ID %q", t.ID)}
		}
		topics[t.ID] = t
	}
	notes := make(map[string]NoteAnnotation, len(a.Notes))
	for _, n := range a.Notes {
		if !validID(n.ID) || strings.TrimSpace(n.Summary) == "" {
			return &ValidationError{Msg: fmt.Sprintf("invalid note %q", n.ID)}
		}
		if _, exists := notes[n.ID]; exists {
			return &ValidationError{Msg: fmt.Sprintf("duplicate note ID %q", n.ID)}
		}
		if err := validNoteKind(n.Kind); err != nil {
			return err
		}
		if len(unique(n.TopicIDs)) != len(n.TopicIDs) || len(unique(n.EvidenceTurnIDs)) != len(n.EvidenceTurnIDs) {
			return &ValidationError{Msg: fmt.Sprintf("duplicate topic or evidence reference in note %s", n.ID)}
		}
		for _, tid := range n.TopicIDs {
			if _, ok := topics[tid]; !ok {
				return &ValidationError{Msg: fmt.Sprintf("note %s references unknown topic %s", n.ID, tid)}
			}
		}
		if len(n.EvidenceTurnIDs) == 0 {
			return &ValidationError{Msg: fmt.Sprintf("note %s needs evidence turns", n.ID)}
		}
		notes[n.ID] = n
	}
	links := make(map[string][]string)
	for _, tt := range a.TurnTopics {
		if !validID(tt.TurnID) || len(unique(tt.TopicIDs)) != len(tt.TopicIDs) {
			return &ValidationError{Msg: fmt.Sprintf("invalid or duplicate turn_topics reference %q", tt.TurnID)}
		}
		for _, tid := range tt.TopicIDs {
			if _, ok := topics[tid]; !ok {
				return &ValidationError{Msg: fmt.Sprintf("turn %s references unknown topic %s", tt.TurnID, tid)}
			}
		}
		links[tt.TurnID] = append(links[tt.TurnID], tt.TopicIDs...)
	}
	for _, n := range a.Notes {
		for _, id := range n.EvidenceTurnIDs {
			links[id] = append(links[id], n.TopicIDs...)
		}
	}
	for id, ts := range links {
		links[id] = unique(ts)
		sort.Strings(links[id])
	}
	raw, _, err := hashJSON(a)
	if err != nil {
		return err
	}
	basis := a.AnnotationBasis
	if basis == "" {
		basis = "人工解释的讨论脉络；非经核实事实，不代表承诺"
	}
	summary := a.Summary
	if summary == "" {
		summary = a.Title
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var current, oldBasis, oldSummary, oldRaw string
	err = tx.QueryRowContext(ctx, `SELECT annotation_version,annotation_basis,summary,annotations_json FROM conversations WHERE id=?`, conversationID).Scan(&current, &oldBasis, &oldSummary, &oldRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("conversation not found: %s", conversationID)
	}
	if err != nil {
		return err
	}
	if current != expectedVersion {
		if current == a.AnnotationVersion && oldRaw == raw {
			return tx.Commit()
		}
		return &AnnotationConflictError{Expected: expectedVersion, Current: current}
	}
	if a.AnnotationVersion == current {
		if oldRaw == raw {
			return tx.Commit()
		}
		return &ValidationError{Msg: "new annotation_version must differ from current version"}
	}
	var reused int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM annotation_history WHERE conversation_id=? AND annotation_version=?`, conversationID, a.AnnotationVersion).Scan(&reused); err != nil {
		return err
	}
	if reused != 0 {
		return &AnnotationVersionReusedError{Version: a.AnnotationVersion}
	}
	// Resolve original turn IDs within this conversation and reject excluded or
	// blank turns, as those never entered the immutable transcript store.
	turnIDs := make(map[string]string)
	originalIDs := make(map[string][]string)
	rows, err := tx.QueryContext(ctx, `SELECT original_turn_id,id FROM turns WHERE conversation_id=?`, conversationID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var original, dbid string
		if err = rows.Scan(&original, &dbid); err != nil {
			rows.Close()
			return err
		}
		turnIDs[dbid] = dbid
		originalIDs[original] = append(originalIDs[original], dbid)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err = rows.Close(); err != nil {
		return err
	}
	resolve := func(id string) (string, error) {
		if dbid, ok := turnIDs[id]; ok {
			return dbid, nil
		}
		candidates := originalIDs[id]
		if len(candidates) == 1 {
			return candidates[0], nil
		}
		if len(candidates) > 1 {
			return "", fmt.Errorf("ambiguous original turn ID %s; use full turn ID", id)
		}
		return "", fmt.Errorf("unknown, blank, excluded, or foreign turn %s", id)
	}
	resolved := make(map[string]string)
	for id := range links {
		dbid, e := resolve(id)
		if e != nil {
			return fmt.Errorf("annotation references %w", e)
		}
		resolved[id] = dbid
	}
	for _, n := range a.Notes {
		for _, id := range n.EvidenceTurnIDs {
			dbid, e := resolve(id)
			if e != nil {
				return fmt.Errorf("note %s references %w", n.ID, e)
			}
			resolved[id] = dbid
		}
	}
	// A forget or suppression can already have written the sanitized snapshot
	// for its new current version. Reuse it only when every stored field matches;
	// never overwrite different historical content under the same version key.
	var historyBasis, historySummary, historyRaw string
	err = tx.QueryRowContext(ctx, `SELECT annotation_basis,summary,annotations_json FROM annotation_history WHERE conversation_id=? AND annotation_version=?`, conversationID, current).Scan(&historyBasis, &historySummary, &historyRaw)
	if err == nil {
		if historyBasis != oldBasis || historySummary != oldSummary || historyRaw != oldRaw {
			return &AnnotationVersionReusedError{Version: current}
		}
	} else if errors.Is(err, sql.ErrNoRows) {
		if _, err = tx.ExecContext(ctx, `INSERT INTO annotation_history VALUES(?,?,?,?,?)`, conversationID, current, oldBasis, oldSummary, oldRaw); err != nil {
			return err
		}
	} else {
		return err
	}
	// Global topic IDs are stable: an existing ID can be reused only with its
	// existing title.
	for _, topic := range a.Topics {
		var title string
		e := tx.QueryRowContext(ctx, "SELECT title FROM topics WHERE id=?", topic.ID).Scan(&title)
		if e == nil && title != topic.Title {
			return fmt.Errorf("topic ID %s already has conflicting title", topic.ID)
		}
		if errors.Is(e, sql.ErrNoRows) {
			if _, err = tx.ExecContext(ctx, "INSERT INTO topics VALUES(?,?)", topic.ID, topic.Title); err != nil {
				return err
			}
		} else if e != nil {
			return e
		}
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM note_evidence WHERE note_id IN (SELECT id FROM notes WHERE conversation_id=?)", conversationID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM notes WHERE conversation_id=?", conversationID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM turn_topics WHERE turn_id IN (SELECT id FROM turns WHERE conversation_id=?)", conversationID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM topic_conversations WHERE conversation_id=?", conversationID); err != nil {
		return err
	}
	for _, topic := range a.Topics {
		if _, err = tx.ExecContext(ctx, "INSERT INTO topic_conversations VALUES(?,?)", topic.ID, conversationID); err != nil {
			return err
		}
	}
	for _, n := range a.Notes {
		id := noteKey(conversationID, n.ID)
		topicJSON, _ := json.Marshal(n.TopicIDs)
		if _, err = tx.ExecContext(ctx, "INSERT INTO notes VALUES(?,?,?,?,?,?,?,?)", id, conversationID, n.Kind, n.Summary, string(topicJSON), "inference", basis, a.AnnotationVersion); err != nil {
			return err
		}
		for i, original := range n.EvidenceTurnIDs {
			if _, err = tx.ExecContext(ctx, "INSERT INTO note_evidence VALUES(?,?,?)", id, resolved[original], i); err != nil {
				return err
			}
		}
	}
	if err = replaceNoteDerivedState(ctx, tx, conversationID, time.Now()); err != nil {
		return err
	}
	for original, topicIDs := range links {
		for _, topic := range topicIDs {
			if _, err = tx.ExecContext(ctx, "INSERT INTO turn_topics VALUES(?,?)", resolved[original], topic); err != nil {
				return err
			}
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE conversations SET title=?,summary=?,annotation_version=?,annotation_basis=?,annotations_json=? WHERE id=?`, a.Title, summary, a.AnnotationVersion, basis, raw, conversationID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM memory_embeddings WHERE kind='note' AND conversation_id=?`, conversationID); err != nil {
		return err
	}
	payload, err := json.Marshal(struct {
		ConversationID string `json:"conversation_id"`
	}{ConversationID: conversationID})
	if err != nil {
		return err
	}
	if _, _, err = process.EnqueueTx(ctx, tx, process.Spec{
		Stage: "embed", InputSnapshotID: conversationID,
		IdempotencyKey: "embed|" + conversationID + "|" + a.AnnotationVersion,
		PayloadJSON:    string(payload), MaxAttempts: 3,
	}, time.Now()); err != nil {
		return err
	}
	return tx.Commit()
}

func validNoteKind(kind string) error {
	switch kind {
	case "observation", "suggestion", "request", "constraint", "intention":
		return nil
	}
	return &ValidationError{Msg: fmt.Sprintf("unsupported note kind %q", kind)}
}

// replaceNoteDerivedState rebuilds the derived_artifacts staleness state for
// the live notes of one conversation after ApplyAnnotations replaced them
// wholesale. A note is stale exactly when any attachment backing its evidence
// turns is no longer active; legacy evidence without attachment rows keeps the
// note current with zero source rows. Runs inside the caller's transaction, so
// republishing a conversation leaves a state consistent with the new notes.
func replaceNoteDerivedState(ctx context.Context, tx *sql.Tx, conversationID string, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM derived_artifact_sources WHERE kind='note' AND artifact_id IN (SELECT artifact_id FROM derived_artifacts WHERE kind='note' AND conversation_id=?)`, conversationID); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT n.id, COALESCE(MAX(CASE WHEN a.active=0 THEN 1 ELSE 0 END),0)
		FROM notes n
		LEFT JOIN note_evidence ne ON ne.note_id=n.id
		LEFT JOIN attachment_turns at ON at.turn_id=ne.turn_id
		LEFT JOIN source_attachments a ON a.id=at.attachment_id
		WHERE n.conversation_id=?
		GROUP BY n.id`, conversationID)
	if err != nil {
		return err
	}
	noteIDs := map[string]bool{}
	staleIDs := map[string]bool{}
	for rows.Next() {
		var id string
		var stale int
		if err := rows.Scan(&id, &stale); err != nil {
			rows.Close()
			return err
		}
		noteIDs[id] = true
		if stale == 1 {
			staleIDs[id] = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for id := range noteIDs {
		reason := ""
		var staleAt any
		stale := 0
		if staleIDs[id] {
			stale, reason, staleAt = 1, staleReasonSuperseded, now.UnixMilli()
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO derived_artifacts(kind, artifact_id, conversation_id, stale, stale_reason, stale_at)
			VALUES('note', ?, ?, ?, ?, ?)
			ON CONFLICT(kind, artifact_id) DO UPDATE SET
				conversation_id=excluded.conversation_id,
				stale=CASE WHEN derived_artifacts.stale=1 THEN 1 ELSE excluded.stale END,
				stale_reason=CASE WHEN derived_artifacts.stale=1 THEN derived_artifacts.stale_reason ELSE excluded.stale_reason END,
				stale_at=CASE WHEN derived_artifacts.stale=1 THEN derived_artifacts.stale_at ELSE excluded.stale_at END`, id, conversationID, stale, reason, staleAt); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO derived_artifact_sources(kind, artifact_id, attachment_id)
		SELECT DISTINCT 'note', ne.note_id, at.attachment_id
		FROM note_evidence ne
		JOIN attachment_turns at ON at.turn_id=ne.turn_id
		JOIN notes n ON n.id=ne.note_id
		WHERE n.conversation_id=?`, conversationID); err != nil {
		return err
	}
	return nil
}

// GetAnnotationHistory returns archived revisions followed by the current one.
func (s *Store) GetAnnotationHistory(ctx context.Context, conversationID string) ([]AnnotationSnapshot, error) {
	var exists int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM conversations WHERE id=?", conversationID).Scan(&exists); err != nil {
		return nil, err
	}
	if exists == 0 {
		return nil, fmt.Errorf("conversation not found: %s", conversationID)
	}
	out := []AnnotationSnapshot{}
	rows, err := s.db.QueryContext(ctx, `SELECT annotation_version,annotation_basis,summary,annotations_json FROM annotation_history WHERE conversation_id=? ORDER BY rowid`, conversationID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var x AnnotationSnapshot
		var raw string
		if err = rows.Scan(&x.Version, &x.Basis, &x.Summary, &raw); err != nil {
			rows.Close()
			return nil, err
		}
		if err = json.Unmarshal([]byte(raw), &x.Annotations); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, x)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	var x AnnotationSnapshot
	var raw string
	err = s.db.QueryRowContext(ctx, `SELECT annotation_version,annotation_basis,summary,annotations_json FROM conversations WHERE id=?`, conversationID).Scan(&x.Version, &x.Basis, &x.Summary, &raw)
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal([]byte(raw), &x.Annotations); err != nil {
		return nil, err
	}
	x.Current = true
	out = append(out, x)
	return out, nil
}
