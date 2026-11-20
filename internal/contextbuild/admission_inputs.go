package contextbuild

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"unicode/utf8"

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

// buildAdmissionCandidates hydrates every candidate's canonical own evidence
// and sorts it by transcript time and turn id. Note candidates re-read their
// raw note_evidence refs in one batch, so a scope-trimmed in-memory subset or
// a detail-evidence fallback can never stand in for a citation; turn
// candidates cite their own canonical row id; conversation candidates are
// navigation-only and are submitted only when their recording source still
// exists. Candidates whose required refs are missing, out of scope or not
// valid UTF-8 are rejected whole and never submitted.
func buildAdmissionCandidates(ctx context.Context, store *memory.Store, db *sql.DB, scope *requestScope, ordered []candidate) ([]AdmissionCandidate, []AdmissionRejection, error) {
	candidates := make([]AdmissionCandidate, 0, len(ordered))
	rejections := []AdmissionRejection{}
	noteRefs, err := hydrateEvidence(ctx, db, candidateItems(ordered), true)
	if err != nil {
		return nil, nil, err
	}
	convSources, err := conversationSourcesExist(ctx, db, scope, admissionConversationIDs(ordered))
	if err != nil {
		return nil, nil, err
	}
	for _, cand := range ordered {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		status := sourceStatusForKind(cand.item.Kind)
		if cand.item.Kind == "conversation" {
			if !convSources[cand.item.ID] {
				rejections = append(rejections, AdmissionRejection{ItemID: cand.item.ID, Reason: "no_source_evidence"})
				continue
			}
			// A conversation is navigation only: it is submitted without
			// evidence refs and can never claim raw or derived content.
			candidates = append(candidates, AdmissionCandidate{
				Item:         cand.item,
				Evidence:     []EvidenceRef{},
				Sources:      sortedSourceNames(cand.sources),
				Scores:       cloneScores(cand.sources),
				SourceStatus: status,
			})
			continue
		}
		ids := admissionRequiredRefs(cand, noteRefs)
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
		reason := ""
		for _, row := range rows {
			if !utf8.ValidString(row.Text) {
				reason = "invalid_utf8_evidence"
				break
			}
			allowed, err := scope.allowsEvidence(ctx, db, row.ID)
			if err != nil {
				return nil, nil, err
			}
			if !allowed {
				reason = "filtered_evidence"
				break
			}
			ref := evidenceRefFromRow(row, metadata[row.ID])
			ref.UsedByItemIDs = []string{cand.item.ID}
			refs = append(refs, ref)
		}
		if reason != "" {
			rejections = append(rejections, AdmissionRejection{ItemID: cand.item.ID, Reason: reason})
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

// admissionRequiredRefs returns the candidate's canonical required refs:
// notes cite their stored note_evidence rows, turns cite their own canonical
// row id. The in-memory candidate evidence — which the scope pre-filter may
// have trimmed — is never a substitute.
func admissionRequiredRefs(cand candidate, noteRefs map[string][]string) []string {
	if isNoteKind(cand.item.Kind) {
		return noteRefs[cand.item.ID]
	}
	return evidenceOf(cand.item)
}

// admissionConversationIDs lists the conversation candidates in pool order.
func admissionConversationIDs(ordered []candidate) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, cand := range ordered {
		if cand.item.Kind != "conversation" || cand.item.ID == "" || seen[cand.item.ID] {
			continue
		}
		seen[cand.item.ID] = true
		out = append(out, cand.item.ID)
	}
	return out
}

// conversationSourcesExist reports which conversations still have a valid
// existing source: their recording row exists and at least one current,
// readable turn inside the requested scope remains (an ambient-only or fully
// superseded conversation is not a legal navigation candidate). The check is
// existence-only, so a navigation-only candidate never carries text and no
// filter-excluded content can leak through it.
func conversationSourcesExist(ctx context.Context, db *sql.DB, scope *requestScope, ids []string) (map[string]bool, error) {
	out := make(map[string]bool, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	query := `SELECT c.id FROM conversations c JOIN recordings r ON r.id=c.recording_id
 WHERE c.id IN (` + placeholders(len(ids)) + `)
 AND EXISTS(SELECT 1 FROM turns t WHERE t.conversation_id=c.id AND ` + memory.CurrentSpeechTurnPredicate("t")
	args := append([]any(nil), stringsToAny(ids)...)
	if scope.topicScoped() {
		topics := sortedKeys(scope.topicIDs)
		query += ` AND EXISTS(SELECT 1 FROM turn_topics tt WHERE tt.turn_id=t.id AND tt.topic_id IN (` + placeholders(len(topics)) + `))`
		args = append(args, stringsToAny(topics)...)
	}
	query += `)`
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// assembleAdmittedEvidence builds the v3 evidence from the refs each selected
// item presented at admission, within the remaining byte budget. Every ref is
// revalidated against the current store and the request scope and must still
// match the revision and text the assessor reviewed; any failed ref drops the
// whole item's evidence and records it as missing. An item's refs enter the
// budget atomically — nothing of an over-budget item is charged and later
// candidates are still tried — a ref shared with an earlier item is charged
// once and merges UsedByItemIDs, and at most maxEvidenceItems selected items
// provide evidence. The returned refs keep the reviewed content ordered by
// transcript time and turn id.
func assembleAdmittedEvidence(ctx context.Context, store *memory.Store, db *sql.DB, scope *requestScope, selected []candidate, admitted map[string][]EvidenceRef, maxBytes, bytesUsed int) ([]EvidenceRef, int, []string, bool, bool, error) {
	evidence := []EvidenceRef{}
	index := map[string]int{}
	skipped := []string{}
	budgetExceeded := false
	missing := false
	providers := 0
	for _, c := range selected {
		if providers >= maxEvidenceItems {
			break
		}
		refs := admitted[candidateKey(c.item)]
		if len(refs) == 0 {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, bytesUsed, nil, false, false, err
		}
		ids := make([]string, 0, len(refs))
		for _, ref := range refs {
			ids = append(ids, ref.TurnID)
		}
		rows, gone, err := store.GetCurrentTurns(ctx, ids)
		if err != nil {
			return nil, bytesUsed, nil, false, false, err
		}
		if len(gone) > 0 {
			for _, id := range gone {
				skipped = appendUnique(skipped, id)
			}
			missing = true
			continue
		}
		byID := make(map[string]memory.CurrentTurnRow, len(rows))
		for _, row := range rows {
			byID[row.ID] = row
		}
		type reviewedRef struct {
			ref     EvidenceRef
			textLen int
			deduped bool
		}
		reviewed := make([]reviewedRef, 0, len(refs))
		needs := 0
		brokenRef := ""
		for _, ref := range refs {
			row, ok := byID[ref.TurnID]
			if !ok || !utf8.ValidString(row.Text) || row.Text != ref.Text || row.TranscriptRevision != ref.TranscriptRevision {
				brokenRef = ref.TurnID
				break
			}
			allowed, err := scope.allowsEvidence(ctx, db, row.ID)
			if err != nil {
				return nil, bytesUsed, nil, false, false, err
			}
			if !allowed {
				brokenRef = ref.TurnID
				break
			}
			entry := reviewedRef{ref: ref, textLen: len(row.Text)}
			if _, seen := index[ref.TurnID]; seen {
				entry.deduped = true
			} else {
				needs += entry.textLen
			}
			reviewed = append(reviewed, entry)
		}
		if brokenRef != "" {
			skipped = appendUnique(skipped, brokenRef)
			missing = true
			continue
		}
		if bytesUsed+needs > maxBytes {
			for _, entry := range reviewed {
				skipped = appendUnique(skipped, entry.ref.TurnID)
			}
			budgetExceeded = true
			continue
		}
		providers++
		for _, entry := range reviewed {
			if entry.deduped {
				shared := &evidence[index[entry.ref.TurnID]]
				shared.UsedByItemIDs = appendUnique(shared.UsedByItemIDs, c.item.ID)
				continue
			}
			ref := entry.ref
			ref.UsedByItemIDs = appendUnique(nil, c.item.ID)
			index[ref.TurnID] = len(evidence)
			evidence = append(evidence, ref)
		}
		bytesUsed += needs
	}
	sortEvidenceRefs(evidence)
	return evidence, bytesUsed, skipped, budgetExceeded, missing, nil
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
