package eval

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"recordingmcp/internal/memory"
	"recordingmcp/internal/projection"
)

// projectionNoteFixture is one note the projection pass seeds directly into the
// evaluation database. Evidence turn refs reuse the human turn-ref convention;
// the note's conversation is derived from its evidence turns.
type projectionNoteFixture struct {
	ID               string    `json:"id"`
	ConversationRef  string    `json:"conversation_ref"`
	Kind             string    `json:"kind"`
	Summary          string    `json:"summary"`
	TopicTitles      []string  `json:"topic_titles"`
	EvidenceTurnRefs []TurnRef `json:"evidence_turn_refs"`
	Stale            bool      `json:"stale"`
}

// projectionBindingFixture is one speaker binding the projection pass records
// through the real store API (propose, optionally confirm).
type projectionBindingFixture struct {
	ConversationRef    string    `json:"conversation_ref"`
	SpeakerScopedID    string    `json:"speaker_scoped_id"`
	TranscriptRevision string    `json:"transcript_revision"`
	PersonID           string    `json:"person_id"`
	Confirm            bool      `json:"confirm"`
	EvidenceTurnRefs   []TurnRef `json:"evidence_turn_refs"`
}

type projectionExpectation struct {
	Kind               string `json:"kind"`
	SubjectNoteID      string `json:"subject_note_id,omitempty"`
	SubjectTopicTitle  string `json:"subject_topic_title,omitempty"`
	PersonID           string `json:"person_id,omitempty"`
	Status             string `json:"status"`
	ConflictWithNoteID string `json:"conflict_with_note_id,omitempty"`
}

type projectionUnprojectedFixture struct {
	NoteID string `json:"note_id"`
	Reason string `json:"reason"`
}

type projectionFixture struct {
	Description       string                         `json:"description"`
	Notes             []projectionNoteFixture        `json:"notes"`
	Bindings          []projectionBindingFixture     `json:"bindings"`
	Expectations      []projectionExpectation        `json:"expectations"`
	ExpectUnprojected []projectionUnprojectedFixture `json:"expect_unprojected"`
}

// loadProjectionFixture reads the projection fixture. A missing file disables
// the projection pass.
func loadProjectionFixture(path string) (*projectionFixture, error) {
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read projection fixture: %w", err)
	}
	var fixture projectionFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		return nil, fmt.Errorf("decode projection fixture: %w", err)
	}
	if len(fixture.Notes) == 0 && len(fixture.Bindings) == 0 {
		return nil, errors.New("projection fixture has no notes or bindings")
	}
	return &fixture, nil
}

// seedProjectionFixture writes the fixture notes (via SQL, like a curator
// would) and records the bindings through the real store API. Seeding is
// idempotent per fixture id.
func seedProjectionFixture(ctx context.Context, db *sql.DB, store *memory.Store, fixture *projectionFixture) error {
	for _, note := range fixture.Notes {
		if len(note.EvidenceTurnRefs) == 0 {
			return fmt.Errorf("projection note %s has no evidence turn refs", note.ID)
		}
		turnIDs := make([]string, 0, len(note.EvidenceTurnRefs))
		conversationID := ""
		for _, ref := range note.EvidenceTurnRefs {
			turnID, err := resolveTurnRef(ctx, db, ref)
			if err != nil {
				return fmt.Errorf("projection note %s: %w", note.ID, err)
			}
			turnIDs = append(turnIDs, turnID)
			if conversationID == "" {
				if err := db.QueryRowContext(ctx, `SELECT conversation_id FROM turns WHERE id=?`, turnID).Scan(&conversationID); err != nil {
					return fmt.Errorf("projection note %s: conversation of %s: %w", note.ID, turnID, err)
				}
			}
		}
		topicIDs := make([]string, 0, len(note.TopicTitles))
		for _, title := range note.TopicTitles {
			topicID, err := ensureTopic(ctx, db, title, conversationID)
			if err != nil {
				return err
			}
			topicIDs = append(topicIDs, topicID)
		}
		topicsJSON, err := json.Marshal(topicIDs)
		if err != nil {
			return err
		}
		if _, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO notes(id,conversation_id,kind,summary,topic_ids_json,claim_mode,interpretation_note,annotation_version)
 VALUES(?,?,?,?,?,'inference','','projection-fixture-v1')`, note.ID, conversationID, note.Kind, note.Summary, string(topicsJSON)); err != nil {
			return fmt.Errorf("projection note %s: %w", note.ID, err)
		}
		for i, turnID := range turnIDs {
			if _, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO note_evidence(note_id,turn_id,ordinal) VALUES(?,?,?)`, note.ID, turnID, i); err != nil {
				return fmt.Errorf("projection note %s evidence: %w", note.ID, err)
			}
		}
		if note.Stale {
			if _, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO derived_artifacts(kind,artifact_id,conversation_id,stale,stale_reason,stale_at)
 VALUES('note',?,?,1,'projection fixture: superseded evidence',NULL)`, note.ID, conversationID); err != nil {
				return fmt.Errorf("projection note %s staleness: %w", note.ID, err)
			}
		}
	}
	for _, binding := range fixture.Bindings {
		if len(binding.EvidenceTurnRefs) == 0 {
			return fmt.Errorf("projection binding for %s has no evidence turn refs", binding.PersonID)
		}
		turnIDs := make([]string, 0, len(binding.EvidenceTurnRefs))
		for _, ref := range binding.EvidenceTurnRefs {
			turnID, err := resolveTurnRef(ctx, db, ref)
			if err != nil {
				return fmt.Errorf("projection binding %s: %w", binding.PersonID, err)
			}
			turnIDs = append(turnIDs, turnID)
		}
		var recordingID string
		if err := db.QueryRowContext(ctx, `SELECT recording_id FROM turns WHERE id=?`, turnIDs[0]).Scan(&recordingID); err != nil {
			return fmt.Errorf("projection binding %s: recording: %w", binding.PersonID, err)
		}
		proposed, err := store.ProposeSpeakerBinding(ctx, recordingID, binding.SpeakerScopedID, binding.TranscriptRevision, turnIDs, "projection-fixture")
		if err != nil {
			return fmt.Errorf("projection binding %s: propose: %w", binding.PersonID, err)
		}
		if binding.Confirm {
			if _, err := store.ConfirmSpeakerBinding(ctx, proposed.ID, binding.PersonID, "projection-fixture"); err != nil {
				return fmt.Errorf("projection binding %s: confirm: %w", binding.PersonID, err)
			}
		}
	}
	return nil
}

// ensureTopic mints the same deterministic topic id publish uses and records
// the topic plus its conversation membership.
func ensureTopic(ctx context.Context, db *sql.DB, title, conversationID string) (string, error) {
	sum := sha256.Sum256([]byte(title))
	topicID := "topic:auto:" + hex.EncodeToString(sum[:])[:16]
	if _, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO topics(id,title) VALUES(?,?)`, topicID, title); err != nil {
		return "", fmt.Errorf("projection topic %q: %w", title, err)
	}
	if conversationID != "" {
		if _, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO topic_conversations(topic_id,conversation_id) VALUES(?,?)`, topicID, conversationID); err != nil {
			return "", fmt.Errorf("projection topic %q membership: %w", title, err)
		}
	}
	return topicID, nil
}

// runProjectionPass builds the projection, checks its determinism and scores
// the honesty metrics against the fixture expectations. The evaluation builds
// use the maximum limit so every derived object is scored, not just the first
// page a consumer would see.
func runProjectionPass(ctx context.Context, db *sql.DB, store *memory.Store, fixture *projectionFixture) (ProjectionMetrics, error) {
	var metrics ProjectionMetrics
	first, err := projection.Build(ctx, store, db, projection.Request{Limit: 200})
	if err != nil {
		return metrics, fmt.Errorf("projection build: %w", err)
	}
	second, err := projection.Build(ctx, store, db, projection.Request{Limit: 200})
	if err != nil {
		return metrics, fmt.Errorf("projection build: %w", err)
	}
	if _, err := projection.Build(ctx, store, db, projection.Request{Limit: 200, Audit: true}); err != nil {
		return metrics, fmt.Errorf("projection audit build: %w", err)
	}
	a, err := json.Marshal(first)
	if err != nil {
		return metrics, err
	}
	b, err := json.Marshal(second)
	if err != nil {
		return metrics, err
	}
	metrics.Deterministic = string(a) == string(b)
	metrics.Objects = len(first.Objects)
	metrics.Conflicts = len(first.Conflicts)
	metrics.Unprojected = first.Coverage.NotesUnprojected

	// Evidence validity: every emitted object's evidence resolves to a current,
	// non-ambient turn.
	valid, total := 0, 0
	for _, obj := range first.Objects {
		total++
		ok, err := objectEvidenceCurrent(ctx, db, obj)
		if err != nil {
			return metrics, err
		}
		if ok {
			valid++
		}
	}
	if total > 0 {
		metrics.EvidenceValid = float64(valid) / float64(total)
	}

	// Stale notes must never surface as objects of the default projection.
	staleTotal, staleExcluded := 0, 0
	for _, note := range fixture.Notes {
		if !note.Stale {
			continue
		}
		staleTotal++
		if !hasObjectID(first.Objects, note.ID) {
			staleExcluded++
		}
	}
	if staleTotal > 0 {
		metrics.StaleNotCurrent = float64(staleExcluded) / float64(staleTotal)
	}

	// Every reported conflict must mark the earlier object superseded and the
	// later one current.
	if len(first.Conflicts) > 0 {
		honest := 0
		for _, conflict := range first.Conflicts {
			earlier, okE := objectByID(first.Objects, conflict.EarlierID)
			later, okL := objectByID(first.Objects, conflict.LaterID)
			if okE && okL && earlier.Status == projection.StatusSuperseded && later.Status == projection.StatusCurrent {
				honest++
			}
		}
		metrics.SupersededNotCurrent = float64(honest) / float64(len(first.Conflicts))
	} else {
		metrics.SupersededNotCurrent = 1
	}

	// Fixture expectations: every one must be satisfiable, otherwise the
	// fixture and the rules disagree and the run fails loudly.
	supersededTotal, supersededHonest := 0, 0
	conflictTotal, conflictHonest := 0, 0
	for _, exp := range fixture.Expectations {
		obj, ok := findExpectationObject(first, exp)
		if !ok {
			return metrics, fmt.Errorf("projection expectation unsatisfied: no %s object for %+v", exp.Kind, exp)
		}
		if string(obj.Kind) != exp.Kind {
			return metrics, fmt.Errorf("projection expectation: object %s kind = %s, want %s", obj.ID, obj.Kind, exp.Kind)
		}
		if string(obj.Status) != exp.Status {
			return metrics, fmt.Errorf("projection expectation: object %s status = %s, want %s", obj.ID, obj.Status, exp.Status)
		}
		if exp.Status == "superseded" {
			supersededTotal++
			if obj.Status == projection.StatusSuperseded {
				supersededHonest++
			}
		}
		if exp.ConflictWithNoteID != "" {
			conflictTotal++
			if containsString(obj.ConflictWith, exp.ConflictWithNoteID) {
				conflictHonest++
			} else {
				return metrics, fmt.Errorf("projection expectation: object %s conflict_with = %v, want %s", obj.ID, obj.ConflictWith, exp.ConflictWithNoteID)
			}
		}
	}
	if supersededTotal > 0 {
		metrics.SupersededNotCurrent = float64(supersededHonest) / float64(supersededTotal)
	}
	if conflictTotal > 0 {
		metrics.ConflictingHonest = float64(conflictHonest) / float64(conflictTotal)
	} else {
		metrics.ConflictingHonest = 1
	}

	for _, want := range fixture.ExpectUnprojected {
		found := false
		for _, u := range first.Unresolved {
			if u.Subject == want.NoteID && u.Reason == want.Reason {
				found = true
				break
			}
		}
		if !found {
			return metrics, fmt.Errorf("projection fixture: note %s expected unprojected with reason %s", want.NoteID, want.Reason)
		}
	}
	return metrics, nil
}

// objectEvidenceCurrent reports whether every evidence turn of the object is a
// current, non-ambient turn row.
func objectEvidenceCurrent(ctx context.Context, db *sql.DB, obj projection.Object) (bool, error) {
	for _, turnID := range obj.EvidenceTurnIDs {
		var ok bool
		err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM turns t WHERE t.id=? AND `+
			memory.CurrentTurnPredicate("t")+
			` AND NOT EXISTS(SELECT 1 FROM turn_classes tc WHERE tc.turn_id=t.id AND tc.class='ambient'))`, turnID).Scan(&ok)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

func hasObjectID(objects []projection.Object, id string) bool {
	_, ok := objectByID(objects, id)
	return ok
}

func objectByID(objects []projection.Object, id string) (projection.Object, bool) {
	for _, obj := range objects {
		if obj.ID == id {
			return obj, true
		}
	}
	return projection.Object{}, false
}

// findExpectationObject locates the object an expectation talks about, by note
// id, topic title or person id.
func findExpectationObject(result projection.Result, exp projectionExpectation) (projection.Object, bool) {
	switch {
	case exp.SubjectNoteID != "":
		return objectByID(result.Objects, exp.SubjectNoteID)
	case exp.SubjectTopicTitle != "":
		for _, obj := range result.Objects {
			if obj.Kind == projection.KindTopic && obj.Title == exp.SubjectTopicTitle {
				return obj, true
			}
		}
		return projection.Object{}, false
	case exp.PersonID != "":
		return objectByID(result.Objects, exp.PersonID)
	default:
		return projection.Object{}, false
	}
}

func containsString(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
