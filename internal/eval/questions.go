package eval

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// TurnRef is a human-authored reference to one ASR turn. The harness resolves
// it to the stored turn row through the turns.original_turn_id key, which is
// exactly "<segment_id>/<transcript_revision>/<turn_id>".
type TurnRef struct {
	SegmentID          string `json:"segment_id"`
	TranscriptRevision string `json:"transcript_revision"`
	TurnID             string `json:"turn_id"`
}

// OriginalTurnID renders the stable key ingest uses for turns.original_turn_id.
func (r TurnRef) OriginalTurnID() string {
	return r.SegmentID + "/" + r.TranscriptRevision + "/" + r.TurnID
}

func (r TurnRef) String() string { return r.OriginalTurnID() }

// authoredQuestion is the on-disk fixture shape. Gold answers are authored by
// content first as human turn refs (gold_turn_refs / gold_topic_refs); the
// harness resolves them to stored ids at load time. The explicit *_ids fields
// exist for ids that are already stable and known.
type authoredQuestion struct {
	ID                  string    `json:"id"`
	Query               string    `json:"query"`
	Purpose             string    `json:"purpose"`
	GoldTurnRefs        []TurnRef `json:"gold_turn_refs"`
	GoldTopicRefs       []TurnRef `json:"gold_topic_refs"`
	GoldEvidenceTurnIDs []string  `json:"gold_evidence_turn_ids"`
	GoldTopicIDs        []string  `json:"gold_topic_ids"`
	ExpectStaleExcluded bool      `json:"expect_stale_excluded"`
	Notes               string    `json:"notes"`
}

type questionFile struct {
	Description string             `json:"description"`
	Questions   []authoredQuestion `json:"questions"`
}

// Question is one resolved evaluation question.
type Question struct {
	ID                  string
	Query               string
	Purpose             string
	Notes               string
	GoldEvidenceTurnIDs []string
	GoldTopicIDs        []string
	ExpectStaleExcluded bool
}

// LoadQuestions reads the fixture and resolves every human reference against
// the ingested database.
func LoadQuestions(ctx context.Context, db *sql.DB, path string) ([]Question, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read questions: %w", err)
	}
	var file questionFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("decode questions: %w", err)
	}
	if len(file.Questions) == 0 {
		return nil, errors.New("questions fixture is empty")
	}
	seen := map[string]bool{}
	out := make([]Question, 0, len(file.Questions))
	for i, aq := range file.Questions {
		if aq.ID == "" {
			return nil, fmt.Errorf("question %d has no id", i)
		}
		if seen[aq.ID] {
			return nil, fmt.Errorf("duplicate question id %q", aq.ID)
		}
		seen[aq.ID] = true
		if aq.Query == "" {
			return nil, fmt.Errorf("question %q has no query", aq.ID)
		}
		q := Question{
			ID:                  aq.ID,
			Query:               aq.Query,
			Purpose:             aq.Purpose,
			Notes:               aq.Notes,
			GoldEvidenceTurnIDs: append([]string(nil), aq.GoldEvidenceTurnIDs...),
			GoldTopicIDs:        append([]string(nil), aq.GoldTopicIDs...),
			ExpectStaleExcluded: aq.ExpectStaleExcluded,
		}
		for _, ref := range aq.GoldTurnRefs {
			id, err := resolveTurnRef(ctx, db, ref)
			if err != nil {
				return nil, fmt.Errorf("question %q: %w", aq.ID, err)
			}
			q.GoldEvidenceTurnIDs = appendUnique(q.GoldEvidenceTurnIDs, id)
		}
		for _, ref := range aq.GoldTopicRefs {
			id, err := resolveTopicRef(ctx, db, ref)
			if err != nil {
				return nil, fmt.Errorf("question %q: %w", aq.ID, err)
			}
			q.GoldTopicIDs = appendUnique(q.GoldTopicIDs, id)
		}
		if len(q.GoldEvidenceTurnIDs) == 0 {
			return nil, fmt.Errorf("question %q: no gold evidence turns resolved", aq.ID)
		}
		out = append(out, q)
	}
	return out, nil
}

// resolveTurnRef maps a human turn reference to the stored turn row id. The
// original_turn_id key is unique per recording; the corpus uses globally
// unique segment ids, so a single match is required.
func resolveTurnRef(ctx context.Context, db *sql.DB, ref TurnRef) (string, error) {
	if ref.SegmentID == "" || ref.TranscriptRevision == "" || ref.TurnID == "" {
		return "", fmt.Errorf("incomplete turn ref %q", ref.OriginalTurnID())
	}
	rows, err := db.QueryContext(ctx, `SELECT id FROM turns WHERE original_turn_id=? ORDER BY id`, ref.OriginalTurnID())
	if err != nil {
		return "", fmt.Errorf("resolve turn ref %q: %w", ref.OriginalTurnID(), err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	switch len(ids) {
	case 0:
		return "", fmt.Errorf("turn ref %q not found", ref.OriginalTurnID())
	case 1:
		return ids[0], nil
	default:
		return "", fmt.Errorf("turn ref %q matched %d turns", ref.OriginalTurnID(), len(ids))
	}
}

// resolveTopicRef maps a human turn reference to the topic the derived memory
// attached to that turn. Topics are produced by publish from turn_topics, so
// the smallest topic id is chosen deterministically when a turn carries more
// than one.
func resolveTopicRef(ctx context.Context, db *sql.DB, ref TurnRef) (string, error) {
	turnID, err := resolveTurnRef(ctx, db, ref)
	if err != nil {
		return "", err
	}
	var topic string
	err = db.QueryRowContext(ctx, `SELECT topic_id FROM turn_topics WHERE turn_id=? ORDER BY topic_id LIMIT 1`, turnID).Scan(&topic)
	if err == nil {
		return topic, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("resolve topic ref %q: %w", ref.OriginalTurnID(), err)
	}
	// Fall back to the topic of a note citing the turn.
	err = db.QueryRowContext(ctx, `SELECT json_extract(n.topic_ids_json,'$[0]') FROM note_evidence ne JOIN notes n ON n.id=ne.note_id WHERE ne.turn_id=? ORDER BY n.id LIMIT 1`, turnID).Scan(&topic)
	if err != nil {
		return "", fmt.Errorf("topic ref %q has no topic: %w", ref.OriginalTurnID(), err)
	}
	return topic, nil
}

func appendUnique(in []string, v string) []string {
	for _, x := range in {
		if x == v {
			return in
		}
	}
	return append(in, v)
}
