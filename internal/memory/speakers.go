package memory

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Speaker binding statuses stored in speaker_bindings.status and the review
// actions recorded in speaker_binding_events.action. A binding starts as
// "proposed"; only a "confirmed" row contributes to SpeakerProjection, so a
// rejected or revoked binding immediately stops resolving the label.
const (
	speakerBindingProposed  = "proposed"
	speakerBindingConfirmed = "confirmed"
	speakerBindingRejected  = "rejected"
	speakerBindingRevoked   = "revoked"

	speakerActionPropose = "propose"
	speakerActionConfirm = "confirm"
	speakerActionReject  = "reject"
	speakerActionRevoke  = "revoke"
)

// SpeakerBinding is one reviewed identity for a speaker label inside a
// recording. SpeakerScopedID is the diarization label as it appears in the
// transcript, and it is only meaningful together with RecordingID: the same
// label in another recording is an unrelated speaker. PersonID is set once a
// reviewer confirms the proposal. EvidenceTurnIDs records the turns the
// proposal was based on.
type SpeakerBinding struct {
	ID                 string   `json:"id"`
	RecordingID        string   `json:"recording_id"`
	TranscriptRevision string   `json:"transcript_revision,omitempty"`
	SpeakerScopedID    string   `json:"speaker_scoped_id"`
	PersonID           string   `json:"person_id,omitempty"`
	Status             string   `json:"status"`
	EvidenceTurnIDs    []string `json:"evidence_turn_ids"`
	Reviewer           string   `json:"reviewer,omitempty"`
	Version            int      `json:"version"`
	CreatedAt          int64    `json:"created_at"`
	UpdatedAt          int64    `json:"updated_at"`
}

// speakerBindingID derives the deterministic id of one binding attempt for a
// (recording, speaker label, version) triple, following the repository's
// "kind:hash" convention. Version is the row's sequence number inside its
// (recording, label) group, so every new claim (for example after a revoke or
// for a different transcript revision) is a new immutable row while a
// refreshed draft keeps its id.
func speakerBindingID(recordingID, speakerScopedID string, version int) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{recordingID, speakerScopedID, strconv.Itoa(version)}, "|")))
	return "sb:" + hex.EncodeToString(sum[:])[:32]
}

// speakerBindingEventID derives the deterministic id of one review event from
// the binding, the action and the version the action applied to. A given
// transition therefore records exactly one event even if it is replayed.
func speakerBindingEventID(bindingID, action string, version int) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{bindingID, action, strconv.Itoa(version)}, "|")))
	return "sbe:" + hex.EncodeToString(sum[:])[:32]
}

// latestSpeakerBinding returns the highest-version row of one (recording,
// speaker label) group, or sql.ErrNoRows when the label has no binding yet.
func latestSpeakerBinding(ctx context.Context, tx *sql.Tx, recordingID, speakerScopedID string) (SpeakerBinding, error) {
	var (
		b        SpeakerBinding
		person   sql.NullString
		evidence string
	)
	err := tx.QueryRowContext(ctx, `SELECT id, recording_id, transcript_revision, speaker_scoped_id, person_id, status, evidence_json, reviewer, version, created_at, updated_at
		FROM speaker_bindings WHERE recording_id=? AND speaker_scoped_id=? ORDER BY version DESC LIMIT 1`, recordingID, speakerScopedID).Scan(
		&b.ID, &b.RecordingID, &b.TranscriptRevision, &b.SpeakerScopedID, &person, &b.Status,
		&evidence, &b.Reviewer, &b.Version, &b.CreatedAt, &b.UpdatedAt)
	if err != nil {
		return SpeakerBinding{}, err
	}
	if person.Valid {
		b.PersonID = person.String
	}
	if err := json.Unmarshal([]byte(evidence), &b.EvidenceTurnIDs); err != nil {
		return SpeakerBinding{}, fmt.Errorf("memory: decode speaker binding evidence: %w", err)
	}
	if b.EvidenceTurnIDs == nil {
		b.EvidenceTurnIDs = []string{}
	}
	return b, nil
}

// insertSpeakerBinding writes one new versioned binding row together with its
// "propose" event in the caller's transaction.
func insertSpeakerBinding(ctx context.Context, tx *sql.Tx, recordingID, speakerScopedID, transcriptRevision string, evidence []string, model string, version int, now int64) (SpeakerBinding, error) {
	raw, err := json.Marshal(evidence)
	if err != nil {
		return SpeakerBinding{}, fmt.Errorf("memory: propose speaker binding: encode evidence: %w", err)
	}
	id := speakerBindingID(recordingID, speakerScopedID, version)
	if _, err := tx.ExecContext(ctx, `INSERT INTO speaker_bindings(id, recording_id, transcript_revision, speaker_scoped_id, person_id, status, evidence_json, reviewer, version, created_at, updated_at)
		VALUES(?, ?, ?, ?, NULL, ?, ?, '', ?, ?, ?)`,
		id, recordingID, transcriptRevision, speakerScopedID, speakerBindingProposed, string(raw), version, now, now); err != nil {
		return SpeakerBinding{}, fmt.Errorf("memory: propose speaker binding: insert: %w", err)
	}
	if err := insertSpeakerBindingEvent(ctx, tx, id, speakerActionPropose, version, model, "", now); err != nil {
		return SpeakerBinding{}, err
	}
	return getSpeakerBinding(ctx, tx, id)
}

// getSpeakerBinding loads one binding by id using the caller's transaction so a
// mutation can read back the row it just wrote before committing.
func getSpeakerBinding(ctx context.Context, tx *sql.Tx, id string) (SpeakerBinding, error) {
	var (
		b        SpeakerBinding
		person   sql.NullString
		evidence string
	)
	err := tx.QueryRowContext(ctx, `SELECT id, recording_id, transcript_revision, speaker_scoped_id, person_id, status, evidence_json, reviewer, version, created_at, updated_at
		FROM speaker_bindings WHERE id=?`, id).Scan(
		&b.ID, &b.RecordingID, &b.TranscriptRevision, &b.SpeakerScopedID, &person, &b.Status,
		&evidence, &b.Reviewer, &b.Version, &b.CreatedAt, &b.UpdatedAt)
	if err != nil {
		return SpeakerBinding{}, err
	}
	if person.Valid {
		b.PersonID = person.String
	}
	if err := json.Unmarshal([]byte(evidence), &b.EvidenceTurnIDs); err != nil {
		return SpeakerBinding{}, fmt.Errorf("memory: decode speaker binding evidence: %w", err)
	}
	if b.EvidenceTurnIDs == nil {
		b.EvidenceTurnIDs = []string{}
	}
	return b, nil
}

// insertSpeakerBindingEvent appends one append-only event row. The id is
// derived from (binding, action, version), so replaying the same transition
// would reuse the id rather than duplicate the log entry.
func insertSpeakerBindingEvent(ctx context.Context, tx *sql.Tx, bindingID, action string, version int, actor, note string, now int64) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO speaker_binding_events(id, binding_id, action, actor, note, created_at) VALUES(?, ?, ?, ?, ?, ?)`,
		speakerBindingEventID(bindingID, action, version), bindingID, action, actor, note, now); err != nil {
		return fmt.Errorf("memory: record speaker binding %s event: %w", action, err)
	}
	return nil
}

// ProposeSpeakerBinding records a proposal that the speaker label
// speakerScopedID in recordingID denotes a person in transcript revision
// transcriptRevision, backed by evidenceTurnIDs. Bindings are revision-scoped:
// a diarization label may be reused across transcript revisions, so identity
// never carries over a revision boundary.
//
// Lifecycle policy (documented and tested):
//   - a still-proposed row is a reviewable draft and is refreshed in place;
//   - a confirmed row for the same recording, label and revision is returned
//     unchanged: the claim is already resolved, a replay is idempotent;
//   - every other verdict row (confirmed for a different revision, rejected,
//     revoked) is immutable history: a new proposal creates the next versioned
//     row. A revoked identity can therefore be re-proposed with new evidence,
//     and a rejection never blocks future claims while keeping its own verdict.
//
// The binding and its "propose" event are written in one transaction.
func (s *Store) ProposeSpeakerBinding(ctx context.Context, recordingID, speakerScopedID, transcriptRevision string, evidenceTurnIDs []string, model string) (SpeakerBinding, error) {
	if !validID(recordingID) || !validID(speakerScopedID) {
		return SpeakerBinding{}, fmt.Errorf("memory: propose speaker binding: recording id and speaker scoped id are required")
	}
	evidence := evidenceTurnIDs
	if evidence == nil {
		evidence = []string{}
	}
	now := time.Now().UnixMilli()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SpeakerBinding{}, fmt.Errorf("memory: propose speaker binding: begin: %w", err)
	}
	defer tx.Rollback()

	latest, err := latestSpeakerBinding(ctx, tx, recordingID, speakerScopedID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		b, err := insertSpeakerBinding(ctx, tx, recordingID, speakerScopedID, transcriptRevision, evidence, model, 1, now)
		if err != nil {
			return SpeakerBinding{}, err
		}
		if err := tx.Commit(); err != nil {
			return SpeakerBinding{}, fmt.Errorf("memory: propose speaker binding: commit: %w", err)
		}
		return b, nil
	case err != nil:
		return SpeakerBinding{}, fmt.Errorf("memory: propose speaker binding: read: %w", err)
	}

	if latest.Status == speakerBindingProposed {
		raw, err := json.Marshal(evidence)
		if err != nil {
			return SpeakerBinding{}, fmt.Errorf("memory: propose speaker binding: encode evidence: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE speaker_bindings SET transcript_revision=?, evidence_json=?, updated_at=? WHERE id=? AND status=?`,
			transcriptRevision, string(raw), now, latest.ID, speakerBindingProposed); err != nil {
			return SpeakerBinding{}, fmt.Errorf("memory: propose speaker binding: refresh: %w", err)
		}
		b, err := getSpeakerBinding(ctx, tx, latest.ID)
		if err != nil {
			return SpeakerBinding{}, fmt.Errorf("memory: propose speaker binding: read back: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return SpeakerBinding{}, fmt.Errorf("memory: propose speaker binding: commit: %w", err)
		}
		return b, nil
	}
	if latest.Status == speakerBindingConfirmed && latest.TranscriptRevision == transcriptRevision {
		return latest, nil
	}
	b, err := insertSpeakerBinding(ctx, tx, recordingID, speakerScopedID, transcriptRevision, evidence, model, latest.Version+1, now)
	if err != nil {
		return SpeakerBinding{}, err
	}
	if err := tx.Commit(); err != nil {
		return SpeakerBinding{}, fmt.Errorf("memory: propose speaker binding: commit: %w", err)
	}
	return b, nil
}

// ConfirmSpeakerBinding accepts a proposal: personID becomes the identity of
// the binding's speaker label for its transcript revision and the row moves to
// "confirmed", so only the accepted revision of the label resolves in
// SpeakerProjection. Version is the row's stable sequence number and is not
// changed by review. Confirming an already-confirmed binding with the same
// person is a no-op; any other source status is rejected. The update and its
// "confirm" event are written in one transaction.
func (s *Store) ConfirmSpeakerBinding(ctx context.Context, bindingID, personID, reviewer string) (SpeakerBinding, error) {
	if !validID(bindingID) || !validID(personID) {
		return SpeakerBinding{}, fmt.Errorf("memory: confirm speaker binding: binding id and person id are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SpeakerBinding{}, fmt.Errorf("memory: confirm speaker binding: begin: %w", err)
	}
	defer tx.Rollback()

	cur, err := getSpeakerBinding(ctx, tx, bindingID)
	if errors.Is(err, sql.ErrNoRows) {
		return SpeakerBinding{}, fmt.Errorf("memory: confirm speaker binding %q: %w", bindingID, sql.ErrNoRows)
	}
	if err != nil {
		return SpeakerBinding{}, fmt.Errorf("memory: confirm speaker binding: read: %w", err)
	}
	switch cur.Status {
	case speakerBindingConfirmed:
		if cur.PersonID == personID {
			return cur, nil
		}
		return SpeakerBinding{}, fmt.Errorf("memory: confirm speaker binding %q: already confirmed as %q", bindingID, cur.PersonID)
	case speakerBindingProposed:
	default:
		return SpeakerBinding{}, fmt.Errorf("memory: confirm speaker binding %q: cannot confirm from status %q", bindingID, cur.Status)
	}

	now := time.Now().UnixMilli()
	if _, err := tx.ExecContext(ctx, `UPDATE speaker_bindings SET person_id=?, status=?, reviewer=?, updated_at=? WHERE id=?`,
		personID, speakerBindingConfirmed, reviewer, now, bindingID); err != nil {
		return SpeakerBinding{}, fmt.Errorf("memory: confirm speaker binding: update: %w", err)
	}
	if err := insertSpeakerBindingEvent(ctx, tx, bindingID, speakerActionConfirm, cur.Version, reviewer, "", now); err != nil {
		return SpeakerBinding{}, err
	}
	b, err := getSpeakerBinding(ctx, tx, bindingID)
	if err != nil {
		return SpeakerBinding{}, fmt.Errorf("memory: confirm speaker binding: read back: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return SpeakerBinding{}, fmt.Errorf("memory: confirm speaker binding: commit: %w", err)
	}
	return b, nil
}

// RejectSpeakerBinding records a reviewer's refusal of a proposed or confirmed
// binding. The row moves to "rejected" and stops contributing to
// SpeakerProjection; rejecting an already-rejected binding is a no-op. The
// update and its "reject" event (carrying the reviewer's note) are written in
// one transaction.
func (s *Store) RejectSpeakerBinding(ctx context.Context, bindingID, reviewer, note string) (SpeakerBinding, error) {
	return s.reviewSpeakerBinding(ctx, bindingID, reviewer, note, speakerActionReject)
}

// RevokeSpeakerBinding withdraws a previously confirmed binding. The row moves
// to "revoked" and immediately disappears from SpeakerProjection, so a
// corrected identity stops resolving without deleting the audit trail.
// Revoking an already-revoked binding is a no-op. The update and its "revoke"
// event (carrying the reviewer's note) are written in one transaction.
func (s *Store) RevokeSpeakerBinding(ctx context.Context, bindingID, reviewer, note string) (SpeakerBinding, error) {
	return s.reviewSpeakerBinding(ctx, bindingID, reviewer, note, speakerActionRevoke)
}

// reviewSpeakerBinding implements the reject and revoke transitions, which
// differ only in their allowed source status and target status.
func (s *Store) reviewSpeakerBinding(ctx context.Context, bindingID, reviewer, note, action string) (SpeakerBinding, error) {
	if !validID(bindingID) {
		return SpeakerBinding{}, fmt.Errorf("memory: %s speaker binding: binding id is required", action)
	}
	target := speakerBindingRejected
	if action == speakerActionRevoke {
		target = speakerBindingRevoked
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SpeakerBinding{}, fmt.Errorf("memory: %s speaker binding: begin: %w", action, err)
	}
	defer tx.Rollback()

	cur, err := getSpeakerBinding(ctx, tx, bindingID)
	if errors.Is(err, sql.ErrNoRows) {
		return SpeakerBinding{}, fmt.Errorf("memory: %s speaker binding %q: %w", action, bindingID, sql.ErrNoRows)
	}
	if err != nil {
		return SpeakerBinding{}, fmt.Errorf("memory: %s speaker binding: read: %w", action, err)
	}
	// Both a rejection and a revocation may start from a confirmed binding; a
	// revocation requires one, because only an accepted identity can be
	// withdrawn.
	switch cur.Status {
	case target:
		return cur, nil
	case speakerBindingConfirmed:
	case speakerBindingProposed:
		if action == speakerActionRevoke {
			return SpeakerBinding{}, fmt.Errorf("memory: revoke speaker binding %q: only a confirmed binding can be revoked", bindingID)
		}
	default:
		return SpeakerBinding{}, fmt.Errorf("memory: %s speaker binding %q: cannot %s from status %q", action, bindingID, action, cur.Status)
	}

	now := time.Now().UnixMilli()
	if _, err := tx.ExecContext(ctx, `UPDATE speaker_bindings SET status=?, reviewer=?, updated_at=? WHERE id=?`, target, reviewer, now, bindingID); err != nil {
		return SpeakerBinding{}, fmt.Errorf("memory: %s speaker binding: update: %w", action, err)
	}
	if err := insertSpeakerBindingEvent(ctx, tx, bindingID, action, cur.Version, reviewer, note, now); err != nil {
		return SpeakerBinding{}, err
	}
	b, err := getSpeakerBinding(ctx, tx, bindingID)
	if err != nil {
		return SpeakerBinding{}, fmt.Errorf("memory: %s speaker binding: read back: %w", action, err)
	}
	if err := tx.Commit(); err != nil {
		return SpeakerBinding{}, fmt.Errorf("memory: %s speaker binding: commit: %w", action, err)
	}
	return b, nil
}

// SpeakerProjection resolves the confirmed speaker labels of one recording
// for one transcript revision to person ids. Bindings are revision-scoped: a
// diarization label may be reused across revisions, so a confirmed binding
// only resolves for the transcript revision it was reviewed against and never
// leaks identity into another revision. Only status='confirmed' rows
// contribute, so proposed, rejected and revoked bindings yield nothing and a
// revocation updates the projection immediately. Bindings are also scoped by
// recording id, so an identical label in another recording is unaffected.
func (s *Store) SpeakerProjection(ctx context.Context, recordingID, transcriptRevision string) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT speaker_scoped_id, person_id, version FROM speaker_bindings
		WHERE recording_id=? AND transcript_revision=? AND status=? AND person_id IS NOT NULL AND person_id<>''
		ORDER BY speaker_scoped_id, version`, recordingID, transcriptRevision, speakerBindingConfirmed)
	if err != nil {
		return nil, fmt.Errorf("memory: speaker projection: query: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var speaker, person string
		var version int
		if err := rows.Scan(&speaker, &person, &version); err != nil {
			return nil, fmt.Errorf("memory: speaker projection: scan: %w", err)
		}
		// Rows are ordered by version ascending; a later confirmation of the
		// same label wins.
		out[speaker] = person
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("memory: speaker projection: read: %w", err)
	}
	return out, nil
}
