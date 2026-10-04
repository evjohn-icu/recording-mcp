package eval

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"recordingmcp/internal/pipeline"
)

// linkFile is the top-level shape of fixtures/eval/links.json.
type linkFile struct {
	Description string        `json:"description"`
	Links       []linkFixture `json:"links"`
}

// linkFixture is one curated topic link. Endpoints are authored as human turn
// refs; the harness resolves each to the topic the memory attached to that
// turn, so the fixture never hard-codes hash-derived topic ids. Confirm marks
// the link as human-reviewed; unconfirmed links stay "proposed" and must not
// influence assembly.
type linkFixture struct {
	ID               string    `json:"id"`
	FromTurnRef      TurnRef   `json:"from_turn_ref"`
	ToTurnRef        TurnRef   `json:"to_turn_ref"`
	Relation         string    `json:"relation"`
	Reason           string    `json:"reason"`
	EvidenceTurnRefs []TurnRef `json:"evidence_turn_refs"`
	Confidence       float64   `json:"confidence"`
	Confirm          bool      `json:"confirm"`
}

// AppliedLink is the outcome of inserting and reviewing one curated link.
type AppliedLink struct {
	FixtureID string `json:"fixture_id"`
	LinkID    string `json:"link_id"`
	FromTopic string `json:"from_topic"`
	ToTopic   string `json:"to_topic"`
	Relation  string `json:"relation"`
	Status    string `json:"status"`
	Outcome   string `json:"outcome"`
}

// LoadLinks reads the fixture. A missing file (path empty or not present)
// yields an empty set so the harness still runs without curated links.
func LoadLinks(path string) ([]linkFixture, error) {
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read links: %w", err)
	}
	var file linkFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("decode links: %w", err)
	}
	return file.Links, nil
}

// applyLinks resolves, inserts and (optionally) confirms every curated link.
func applyLinks(ctx context.Context, db *sql.DB, fixtures []linkFixture, now time.Time) ([]AppliedLink, error) {
	if len(fixtures) == 0 {
		return nil, nil
	}
	proposals := make([]pipeline.LinkProposal, 0, len(fixtures))
	ids := make([]string, 0, len(fixtures))
	for _, f := range fixtures {
		fromTopic, err := resolveTopicRef(ctx, db, f.FromTurnRef)
		if err != nil {
			return nil, fmt.Errorf("link %q from: %w", f.ID, err)
		}
		toTopic, err := resolveTopicRef(ctx, db, f.ToTurnRef)
		if err != nil {
			return nil, fmt.Errorf("link %q to: %w", f.ID, err)
		}
		var evidence []string
		for _, ref := range f.EvidenceTurnRefs {
			id, err := resolveTurnRef(ctx, db, ref)
			if err != nil {
				return nil, fmt.Errorf("link %q evidence: %w", f.ID, err)
			}
			evidence = appendUnique(evidence, id)
		}
		proposals = append(proposals, pipeline.LinkProposal{
			Kind:            "topic_link",
			FromTopicID:     fromTopic,
			ToTopicID:       toTopic,
			Relation:        f.Relation,
			Reason:          f.Reason,
			EvidenceTurnIDs: evidence,
			Confidence:      f.Confidence,
		})
		ids = append(ids, f.ID)
	}
	validated, err := pipeline.ValidateTopicLinks(ctx, db, proposals)
	if err != nil {
		return nil, fmt.Errorf("validate links: %w", err)
	}
	inserted, err := pipeline.InsertLinks(ctx, db, "eval-harness", validated, now)
	if err != nil {
		return nil, fmt.Errorf("insert links: %w", err)
	}
	// InsertLinks returns one outcome per input; rejected proposals are skipped
	// silently, so re-validate to fail loudly instead of losing a fixture.
	out := make([]AppliedLink, 0, len(fixtures))
	insertedByIndex := len(inserted) == len(fixtures)
	for i, f := range fixtures {
		if validated[i].Status != "validated" {
			return nil, fmt.Errorf("link %q rejected: %s", f.ID, validated[i].RejectionReason)
		}
		if !insertedByIndex {
			return nil, fmt.Errorf("link %q: unexpected insert outcome count %d for %d fixtures", f.ID, len(inserted), len(fixtures))
		}
		status := pipeline.LinkStatusProposed
		if f.Confirm {
			if _, err := pipeline.ReviewLink(ctx, db, inserted[i].ID, pipeline.ReviewConfirm); err != nil {
				return nil, fmt.Errorf("confirm link %q: %w", f.ID, err)
			}
			status = pipeline.LinkStatusConfirmed
		}
		out = append(out, AppliedLink{
			FixtureID: f.ID,
			LinkID:    inserted[i].ID,
			FromTopic: validated[i].FromTopicID,
			ToTopic:   validated[i].ToTopicID,
			Relation:  validated[i].Relation,
			Status:    status,
			Outcome:   string(inserted[i].Outcome),
		})
	}
	return out, nil
}
