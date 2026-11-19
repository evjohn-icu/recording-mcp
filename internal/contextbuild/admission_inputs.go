package contextbuild

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"

	"recordingmcp/internal/memory"
)

// admissionBlocked returns a v3 package whose items and evidence are empty
// because the gate could not run. It never falls back to v1/v2 output.
func admissionBlocked(pkg Package, degradeReasons []string, code, detail string) Package {
	pkg.Items = []memory.Item{}
	pkg.Evidence = []EvidenceRef{}
	pkg.Truncated = false
	pkg.Degraded = true
	pkg.Gaps = append(pkg.Gaps, Gap{Kind: code, Detail: detail})
	degradeReasons = append(degradeReasons, detail)
	pkg.DegradeReason = stringsJoin(degradeReasons)
	pkg.Sufficiency = &EvidenceAssessment{Status: "unknown", EvidenceRefs: []string{}}
	pkg.Trace.Budget = BudgetTrace{}
	pkg.Coverage.Conversations = []string{}
	pkg.Coverage.TopicIDs = []string{}
	pkg.Coverage.Branches = Branches{}
	pkg.Ranking = []ItemRanking{}
	if pkg.Trace.Admission != nil {
		pkg.Trace.Admission.Admitted = 0
	}
	return pkg
}

// stringsJoin keeps the degrade-reason rendering in one place.
func stringsJoin(parts []string) string {
	out := ""
	for i, part := range parts {
		if i > 0 {
			out += "; "
		}
		out += part
	}
	return out
}

// sourceStatusForKind maps an item kind onto the fixed v3 source status.
func sourceStatusForKind(kind string) string {
	switch {
	case kind == "conversation":
		return SourceStatusNavigationOnly
	case isNoteKind(kind):
		return SourceStatusDerivedProposed
	default:
		return SourceStatusRawCurrent
	}
}

// buildAdmissionCandidates hydrates every candidate's own current evidence and
// sorts it by transcript time and turn id. Candidates whose evidence is
// missing, filtered or empty are returned as rejections and never submitted.
func buildAdmissionCandidates(ctx context.Context, store *memory.Store, db *sql.DB, scope *requestScope, ordered []candidate) ([]AdmissionCandidate, []AdmissionRejection, error) {
	candidates := make([]AdmissionCandidate, 0, len(ordered))
	rejections := []AdmissionRejection{}
	for _, cand := range ordered {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		status := sourceStatusForKind(cand.item.Kind)
		ids := candidateEvidenceIDs(cand)
		if len(ids) == 0 {
			rejections = append(rejections, AdmissionRejection{ItemID: cand.item.ID, Reason: "no_source_evidence"})
			continue
		}
		rows, missing, err := store.GetCurrentTurns(ctx, ids)
		if err != nil {
			return nil, nil, err
		}
		if len(missing) > 0 {
			rejections = append(rejections, AdmissionRejection{ItemID: cand.item.ID, Reason: "missing_evidence"})
			continue
		}
		metadata, err := hydrateTurnMetadata(ctx, store, rows)
		if err != nil {
			return nil, nil, err
		}
		refs := make([]EvidenceRef, 0, len(rows))
		filtered := false
		for _, row := range rows {
			allowed, err := scope.allowsEvidence(ctx, db, row.ID)
			if err != nil {
				return nil, nil, err
			}
			if !allowed {
				filtered = true
				break
			}
			ref := evidenceRefFromRow(row, metadata[row.ID])
			ref.UsedByItemIDs = []string{cand.item.ID}
			refs = append(refs, ref)
		}
		if filtered {
			rejections = append(rejections, AdmissionRejection{ItemID: cand.item.ID, Reason: "filtered_evidence"})
			continue
		}
		sortEvidenceRefs(refs)
		candidates = append(candidates, AdmissionCandidate{
			Item:         cand.item,
			Evidence:     refs,
			Sources:      sortedSourceNames(cand.sources),
			Scores:       cloneScores(cand.sources),
			SourceStatus: status,
		})
	}
	return candidates, rejections, nil
}

// sortedSourceNames returns the retrieval branch names in stable order.
func sortedSourceNames(sources map[string]float64) []string {
	names := make([]string, 0, len(sources))
	for name := range sources {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func cloneScores(sources map[string]float64) map[string]float64 {
	out := make(map[string]float64, len(sources))
	for key, value := range sources {
		out[key] = value
	}
	return out
}

// turnMetadata is the hydrated provenance of one current turn.
type turnMetadata struct {
	RecordingID        string
	ConversationID     string
	TranscriptRevision string
	RecordingStartedAt string
	SpeakerPersonID    *string
}

// hydrateTurnMetadata resolves recording start time and the confirmed speaker
// projection. Unknown start time stays empty, and an unconfirmed binding never
// yields a person id.
func hydrateTurnMetadata(ctx context.Context, store *memory.Store, rows []memory.CurrentTurnRow) (map[string]turnMetadata, error) {
	out := make(map[string]turnMetadata, len(rows))
	recordingCache := map[string]struct {
		startedAt string
		known     bool
	}{}
	projectionCache := map[string]map[string]string{}
	for _, row := range rows {
		meta := turnMetadata{RecordingID: row.RecordingID, ConversationID: row.ConversationID, TranscriptRevision: row.TranscriptRevision}
		if _, ok := recordingCache[row.RecordingID]; !ok {
			var started sql.NullString
			var known int
			err := store.DB().QueryRowContext(ctx, `SELECT recording_started_at,absolute_time_known FROM recordings WHERE id=?`, row.RecordingID).Scan(&started, &known)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
			value := struct {
				startedAt string
				known     bool
			}{}
			if started.Valid {
				value.startedAt = started.String
			}
			value.known = known == 1
			recordingCache[row.RecordingID] = value
		}
		recording := recordingCache[row.RecordingID]
		if recording.known {
			meta.RecordingStartedAt = recording.startedAt
		}
		if row.TranscriptRevision != "" && row.SpeakerScopedID != nil {
			key := row.RecordingID + "\x00" + row.TranscriptRevision
			projection, ok := projectionCache[key]
			if !ok {
				var err error
				projection, err = store.SpeakerProjection(ctx, row.RecordingID, row.TranscriptRevision)
				if err != nil {
					return nil, err
				}
				projectionCache[key] = projection
			}
			if person, ok := projection[*row.SpeakerScopedID]; ok && person != "" {
				value := person
				meta.SpeakerPersonID = &value
			}
		}
		out[row.ID] = meta
	}
	return out, nil
}

// evidenceRefFromRow builds one ref with its provenance metadata.
func evidenceRefFromRow(row memory.CurrentTurnRow, meta turnMetadata) EvidenceRef {
	ref := EvidenceRef{
		TurnID:             row.ID,
		Text:               row.Text,
		SpeakerScopedID:    row.SpeakerScopedID,
		GlobalStartMS:      row.GlobalStartMS,
		GlobalEndMS:        row.GlobalEndMS,
		RecordingID:        meta.RecordingID,
		ConversationID:     meta.ConversationID,
		TranscriptRevision: meta.TranscriptRevision,
		SourceStatus:       SourceStatusRawCurrent,
		RecordingStartedAt: meta.RecordingStartedAt,
	}
	if meta.SpeakerPersonID != nil {
		ref.SpeakerConfirmed = true
		ref.SpeakerPersonID = meta.SpeakerPersonID
	}
	return ref
}

// sortEvidenceRefs orders refs by transcript time and turn id; unknown time
// sorts last so it never fabricates an ordering.
func sortEvidenceRefs(refs []EvidenceRef) {
	sort.SliceStable(refs, func(i, j int) bool {
		left, right := refs[i], refs[j]
		if (left.GlobalStartMS == nil) != (right.GlobalStartMS == nil) {
			return left.GlobalStartMS != nil
		}
		if left.GlobalStartMS != nil && *left.GlobalStartMS != *right.GlobalStartMS {
			return *left.GlobalStartMS < *right.GlobalStartMS
		}
		return left.TurnID < right.TurnID
	})
}

// hydrateFinalEvidence fills provenance metadata on the budgeted evidence. A
// turn that disappeared between assembly and hydration keeps the ref (it was
// already accepted) but carries no fabricated metadata.
func hydrateFinalEvidence(ctx context.Context, store *memory.Store, refs []EvidenceRef) error {
	if len(refs) == 0 {
		return nil
	}
	ids := make([]string, 0, len(refs))
	for _, ref := range refs {
		ids = append(ids, ref.TurnID)
	}
	rows, _, err := store.GetCurrentTurns(ctx, ids)
	if err != nil {
		return err
	}
	metadata, err := hydrateTurnMetadata(ctx, store, rows)
	if err != nil {
		return err
	}
	byID := make(map[string]memory.CurrentTurnRow, len(rows))
	for _, row := range rows {
		byID[row.ID] = row
	}
	for i := range refs {
		row, ok := byID[refs[i].TurnID]
		if !ok {
			continue
		}
		meta := metadata[row.ID]
		refs[i].RecordingID = meta.RecordingID
		refs[i].ConversationID = meta.ConversationID
		refs[i].TranscriptRevision = meta.TranscriptRevision
		refs[i].RecordingStartedAt = meta.RecordingStartedAt
		refs[i].SourceStatus = SourceStatusRawCurrent
		if meta.SpeakerPersonID != nil {
			refs[i].SpeakerConfirmed = true
			refs[i].SpeakerPersonID = meta.SpeakerPersonID
		} else {
			refs[i].SpeakerConfirmed = false
			refs[i].SpeakerPersonID = nil
		}
	}
	return nil
}

// validateAdmissionDecisions enforces the assessor contract: one decision per
// candidate, in order, with a known relation and a finite probability.
func validateAdmissionDecisions(candidates []AdmissionCandidate, decisions []AdmissionDecision, threshold float64) error {
	if len(decisions) != len(candidates) {
		return fmt.Errorf("admission assessor returned %d decisions for %d candidates", len(decisions), len(candidates))
	}
	if math.IsNaN(threshold) || math.IsInf(threshold, 0) || threshold < 0 || threshold > 1 {
		return errors.New("admission threshold must be within [0,1]")
	}
	for i, decision := range decisions {
		if decision.ItemID != candidates[i].Item.ID {
			return fmt.Errorf("admission decision %d is for %q, want %q", i, decision.ItemID, candidates[i].Item.ID)
		}
		switch decision.Relation {
		case RelationSupports, RelationRefutes, RelationRelated, RelationUnknown:
		default:
			return fmt.Errorf("admission decision %q has invalid relation %q", decision.ItemID, decision.Relation)
		}
		if math.IsNaN(decision.Probability) || math.IsInf(decision.Probability, 0) || decision.Probability < 0 || decision.Probability > 1 {
			return fmt.Errorf("admission decision %q has invalid probability", decision.ItemID)
		}
	}
	return nil
}
