package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"recordingmcp/internal/decision"
	"recordingmcp/internal/plane"
)

const maxWorldlineSemanticPairs = 128
const worldlineSemanticRubricVersion = "worldline-semantic-v2"
const worldlineSemanticBatchSize = 128

type WorldlineSemanticSuggestion struct {
	Status                string              `json:"status"`
	Relation              string              `json:"relation"`
	Confidence            float64             `json:"confidence"`
	Distribution          map[string]float64  `json:"distribution"`
	Model                 string              `json:"model"`
	WeightsVersion        string              `json:"weights_version"`
	InputHash             string              `json:"input_hash"`
	SourceSnapshotVersion string              `json:"source_snapshot_version"`
	SourceClaimIDs        []string            `json:"source_claim_ids"`
	SourceEvidence        []EvidenceReference `json:"source_evidence"`
	EarlierSummary        string              `json:"earlier_summary"`
	LaterSummary          string              `json:"later_summary"`
	EarlierEvidence       []WorldlineEvidence `json:"earlier_evidence"`
	LaterEvidence         []WorldlineEvidence `json:"later_evidence"`
	IdentityStatus        string              `json:"identity_status"`
	CandidatePairID       string              `json:"candidate_pair_id"`
	Reason                string              `json:"reason"`
	RequiresReview        bool                `json:"requires_review"`
	ReviewStatus          string              `json:"review_status,omitempty"`
	SuggestionID          string              `json:"suggestion_id"`
	RecordedAt            *time.Time          `json:"recorded_at,omitempty"`
}

type worldlineSemanticSummary struct {
	Status             string
	CandidatePairCount int
	PairCount          int
	SuggestionCount    int
	Truncated          bool
	Model              string
	WeightsVersion     string
	CompletedPairCount int
	RemainingPairCount int
	FailedPairCount    int
}

type semanticPair struct {
	ID                string               `json:"id"`
	Title             string               `json:"source_topic_title"`
	IdentityStatus    string               `json:"identity_status"`
	ConversationCount int                  `json:"-"`
	MentionedAt       *time.Time           `json:"-"`
	Earlier           semanticPairEvidence `json:"earlier"`
	Later             semanticPairEvidence `json:"later"`
}

type semanticPairEvidence struct {
	EntryID            string               `json:"entry_id"`
	ClaimID            string               `json:"claim_id"`
	Summary            string               `json:"summary"`
	EvidenceCount      int                  `json:"evidence_count"`
	SourceRefs         []EvidenceReference  `json:"source_refs"`
	SourceNoteVersions []string             `json:"source_note_versions,omitempty"`
	SourceSubjects     map[string]string    `json:"source_subjects,omitempty"`
	ExcerptTruncated   bool                 `json:"excerpt_truncated"`
	Sources            []semanticSourceText `json:"source_excerpts"`
}

type semanticSourceText struct {
	TurnID    string `json:"turn_id"`
	Revision  string `json:"revision"`
	Text      string `json:"text"`
	Truncated bool   `json:"truncated"`
	SubjectID string `json:"subject_id,omitempty"`
}

func (s *WorldlineService) evaluateWorldlineSemantics(ctx context.Context, scope string, topics []WorldlineTopic) (worldlineSemanticSummary, error) {
	if s.DecisionEngine == nil {
		return worldlineSemanticSummary{Status: "not_configured"}, nil
	}
	pairs, totalCandidates := collectWorldlineSemanticPairs(topics, maxWorldlineSemanticPairs)
	if len(pairs) == 0 {
		return worldlineSemanticSummary{Status: "no_candidates", CandidatePairCount: totalCandidates}, nil
	}
	sourceSnapshotVersion := worldlineSourceFingerprint(topics)
	criteria := worldlineSemanticCriteria()
	type evaluated struct {
		pair                      semanticPair
		answer                    decision.Answer
		inputHash, model, weights string
	}
	results := make([]evaluated, 0, len(pairs))
	model, weights := "", ""
	callCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	for _, pair := range pairs {
		state, err := json.Marshal(struct {
			Scope                 string       `json:"scope"`
			SourceSnapshotVersion string       `json:"source_snapshot_version"`
			Pair                  semanticPair `json:"pair"`
		}{scope, sourceSnapshotVersion, pair})
		if err != nil {
			return worldlineSemanticSummary{}, err
		}
		result, err := s.DecisionEngine.Evaluate(callCtx, string(state), []decision.Question{worldlineSemanticQuestion(pair.ID)})
		if err != nil {
			return worldlineSemanticSummary{}, fmt.Errorf("worldline semantic evaluation: %w", err)
		}
		actualModel, actualWeights := strings.TrimSpace(result.Model), strings.TrimSpace(result.WeightsVersion)
		if actualModel == "" || actualWeights == "" || strings.Contains(strings.ToLower(actualModel), "synthetic") || strings.Contains(strings.ToLower(actualWeights), "synthetic") || strings.Contains(strings.ToLower(actualModel), "sandbox") || strings.Contains(strings.ToLower(actualWeights), "sandbox") {
			return worldlineSemanticSummary{}, errors.New("worldline semantic evaluation requires an identified non-synthetic JEV model and weights version")
		}
		if len(result.InputHash) != 64 {
			return worldlineSemanticSummary{}, errors.New("worldline semantic engine returned an invalid input hash")
		}
		if _, err := hex.DecodeString(result.InputHash); err != nil {
			return worldlineSemanticSummary{}, errors.New("worldline semantic engine returned an invalid input hash")
		}
		if len(result.Answers) != 1 {
			return worldlineSemanticSummary{}, errors.New("worldline semantic engine returned an incomplete pair result")
		}
		answer, ok := result.Answers[pair.ID]
		if !ok {
			return worldlineSemanticSummary{}, fmt.Errorf("worldline semantic result missing pair %s", pair.ID)
		}
		if err := validateWorldlineSemanticAnswer(answer, criteria); err != nil {
			return worldlineSemanticSummary{}, err
		}
		if model == "" {
			model, weights = actualModel, actualWeights
		} else if model != actualModel || weights != actualWeights {
			return worldlineSemanticSummary{}, errors.New("worldline semantic model identity changed during one generation")
		}
		results = append(results, evaluated{pair: pair, answer: answer, inputHash: result.InputHash, model: actualModel, weights: actualWeights})
	}
	// Re-check the entire source/speaker snapshot after all inference. A slow
	// local model must never append advice computed from a superseded revision.
	currentTopics, _, _, err := s.project(ctx, scope)
	if err != nil {
		return worldlineSemanticSummary{}, err
	}
	if worldlineSourceFingerprint(currentTopics) != sourceSnapshotVersion {
		return worldlineSemanticSummary{}, errors.New("worldline source snapshot changed during semantic evaluation")
	}
	currentEvents, err := s.Time.Events.History(ctx, scope)
	if err != nil {
		return worldlineSemanticSummary{}, err
	}
	existing := map[string]bool{}
	for _, event := range currentEvents {
		existing[event.ID] = true
	}
	events := make([]plane.Event, 0, len(results))
	suggestions := 0
	for _, item := range results {
		earlier, later := findSemanticPairEntries(topics, item.pair)
		if earlier == nil || later == nil {
			return worldlineSemanticSummary{}, errors.New("worldline semantic pair no longer exists in snapshot")
		}
		refs, notes, versions := semanticSources(*earlier, *later)
		if len(refs) == 0 {
			return worldlineSemanticSummary{}, errors.New("worldline semantic pair has no source evidence")
		}
		refsValidated, _, err := s.Time.validateEvidence(ctx, refs)
		if err != nil {
			return worldlineSemanticSummary{}, err
		}
		earlierRefs, earlierNotes, earlierVersions := semanticSourcesForClaim(earlier.Claim)
		laterRefs, laterNotes, laterVersions := semanticSourcesForClaim(later.Claim)
		subjects := semanticSourceSubjects(earlier.Claim, later.Claim)
		id := semanticEventID(scope, item.pair.ID, item.inputHash, item.model, item.weights, item.answer)
		if existing[id] {
			suggestions++
			continue
		}
		distribution := map[string]float64{}
		for k, v := range item.answer.Distribution {
			distribution[k] = v
		}
		now := time.Now().UTC()
		semanticStatus := "review_required"
		if item.answer.Choice == "insufficient_evidence" {
			semanticStatus = "insufficient_evidence"
		}
		structured := map[string]any{"scope": scope, "origin": "worldline", "semantic_status": semanticStatus, "relation": item.answer.Choice, "confidence": item.answer.Confidence, "distribution": distribution, "model": item.model, "weights_version": item.weights, "input_hash": item.inputHash, "source_snapshot_version": sourceSnapshotVersion, "identity_status": item.pair.IdentityStatus, "candidate_pair_id": item.pair.ID, "source_subjects": subjects, "thread_id": item.pair.ThreadID(), "entry_id": item.pair.Later.EntryID, "source_claim_ids": []string{item.pair.Earlier.ClaimID, item.pair.Later.ClaimID}, "source_note_ids": notes, "source_note_versions": versions, "references": refsValidated, "earlier_claim_id": item.pair.Earlier.ClaimID, "later_claim_id": item.pair.Later.ClaimID, "earlier_references": evidenceStrings(earlierRefs), "later_references": evidenceStrings(laterRefs), "earlier_source_note_ids": earlierNotes, "later_source_note_ids": laterNotes, "earlier_source_note_versions": earlierVersions, "later_source_note_versions": laterVersions, "reason": "decision_engine_pairwise_relation_suggestion; human review required"}
		events = append(events, plane.Event{ID: id, Type: "worldline.semantic", Timestamp: now, ObservedAt: now, Source: "local-web", Content: "semantic relation suggestion; review required", Confidence: item.answer.Confidence, StructuredData: structured})
		suggestions++
	}
	if len(events) > 0 {
		if err := appendSemanticEventsWithCheck(ctx, events,
			func(ctx context.Context, events []plane.Event) error {
				_, err := s.Time.Events.Ingest(ctx, events)
				return err
			},
			func(ctx context.Context) error {
				return s.verifySemanticSources(ctx, scope, sourceSnapshotVersion, events)
			},
			func(ctx context.Context, ids []string) error { return s.Time.PurgeContent(ctx, ids) }); err != nil {
			return worldlineSemanticSummary{}, err
		}
	}
	return worldlineSemanticSummary{Status: "evaluated", CandidatePairCount: totalCandidates, PairCount: len(pairs), SuggestionCount: suggestions, Truncated: totalCandidates > len(pairs), Model: model, WeightsVersion: weights}, nil
}

type worldlineModelIdentity interface {
	ModelIdentity(context.Context) (model, weights string, err error)
}

type cachedWorldlinePair struct {
	Pair      semanticPair    `json:"pair"`
	Answer    decision.Answer `json:"answer"`
	InputHash string          `json:"input_hash"`
	Model     string          `json:"model"`
	Weights   string          `json:"weights_version"`
}

// runDurableWorldlinePairs evaluates a frozen finite candidate list and writes
// each answer to the event plane before moving to the next pair. A new worker
// can therefore reuse completed results after a lease expires or process exits.
func (s *WorldlineService) runDurableWorldlinePairs(ctx context.Context, scope, jobID string, attempt int, pairs []semanticPair) (worldlineSemanticSummary, error) {
	return s.runDurableWorldlinePairsGuarded(ctx, scope, jobID, attempt, pairs, nil)
}

func (s *WorldlineService) runDurableWorldlinePairsGuarded(ctx context.Context, scope, jobID string, attempt int, pairs []semanticPair, guard func(context.Context) error) (worldlineSemanticSummary, error) {
	checkLease := func() error {
		if guard == nil {
			return nil
		}
		return guard(ctx)
	}
	if err := checkLease(); err != nil {
		return worldlineSemanticSummary{}, err
	}
	if s == nil || s.Time == nil || s.Time.Events == nil {
		return worldlineSemanticSummary{}, errors.New("worldline service unavailable")
	}
	if len(pairs) == 0 {
		return worldlineSemanticSummary{Status: "no_candidates"}, nil
	}
	if s.DecisionEngine == nil {
		return worldlineSemanticSummary{Status: "not_configured", CandidatePairCount: len(pairs), RemainingPairCount: len(pairs)}, nil
	}
	currentTopics, _, _, err := s.project(ctx, scope)
	if err != nil {
		return worldlineSemanticSummary{}, err
	}
	currentNotes, currentSubjects := currentWorldlineSources(currentTopics)
	events, err := s.Time.History(ctx, scope)
	if err != nil {
		return worldlineSemanticSummary{}, err
	}
	caches := map[string]cachedWorldlinePair{}
	for _, event := range events {
		if event.Type != "worldline.semantic_cache" || event.SourceStatus != "current" {
			continue
		}
		key, _ := event.StructuredData["cache_key"].(string)
		if key == "" {
			continue
		}
		raw, e := json.Marshal(event.StructuredData["result"])
		if e != nil {
			continue
		}
		var result cachedWorldlinePair
		if json.Unmarshal(raw, &result) == nil {
			caches[key] = result
		}
	}
	model, weights, known := "", "", false
	if identity, ok := s.DecisionEngine.(worldlineModelIdentity); ok {
		if m, w, e := identity.ModelIdentity(ctx); e == nil {
			model, weights, known = m, w, validCacheIdentity(m, w)
		}
	}
	criteria := worldlineSemanticCriteria()
	completed, failed, suggestions := 0, 0, 0
	for batchStart := 0; batchStart < len(pairs); batchStart += worldlineSemanticBatchSize {
		batchEnd := min(batchStart+worldlineSemanticBatchSize, len(pairs))
		batchCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
		for index := batchStart; index < batchEnd; index++ {
			if err := checkLease(); err != nil {
				cancel()
				return worldlineSemanticSummary{}, err
			}
			pair := pairs[index]
			refs, subjects, sourceErr := s.validateDurablePairSources(batchCtx, scope, pair, currentNotes, currentSubjects)
			if sourceErr != nil {
				failed++
				if err := checkLease(); err != nil {
					cancel()
					return worldlineSemanticSummary{}, err
				}
				if err := s.recordDurablePairFailure(ctx, scope, jobID, attempt, index, pair, "stale_source"); err != nil {
					cancel()
					return worldlineSemanticSummary{}, err
				}
				continue
			}
			var answer decision.Answer
			inputHash := ""
			cacheHit := false
			if known {
				key := worldlinePairCacheKey(pair, model, weights)
				if cached, ok := caches[key]; ok {
					answer, inputHash, cacheHit = cached.Answer, cached.InputHash, true
				}
			}
			if !cacheHit {
				state, e := worldlinePairState(scope, pair)
				if e != nil {
					cancel()
					return worldlineSemanticSummary{}, e
				}
				result, e := s.DecisionEngine.Evaluate(batchCtx, state, []decision.Question{worldlineSemanticQuestion(pair.ID)})
				if e != nil {
					_ = s.recordDurablePairFailure(ctx, scope, jobID, attempt, index, pair, "evaluation_failed")
					cancel()
					return worldlineSemanticSummary{}, fmt.Errorf("worldline semantic evaluation: %w", e)
				}
				actualModel, actualWeights := strings.TrimSpace(result.Model), strings.TrimSpace(result.WeightsVersion)
				if actualModel == "" || actualWeights == "" || strings.Contains(strings.ToLower(actualModel), "synthetic") || strings.Contains(strings.ToLower(actualWeights), "synthetic") || strings.Contains(strings.ToLower(actualModel), "sandbox") || strings.Contains(strings.ToLower(actualWeights), "sandbox") {
					cancel()
					return worldlineSemanticSummary{}, errors.New("worldline semantic evaluation requires an identified non-synthetic JEV model and weights version")
				}
				if model == "" {
					model, weights = actualModel, actualWeights
				} else if model != actualModel || weights != actualWeights {
					cancel()
					return worldlineSemanticSummary{}, errors.New("worldline semantic model identity changed during one generation")
				}
				if len(result.InputHash) != 64 {
					cancel()
					return worldlineSemanticSummary{}, errors.New("worldline semantic engine returned an invalid input hash")
				}
				if _, e := hex.DecodeString(result.InputHash); e != nil {
					cancel()
					return worldlineSemanticSummary{}, errors.New("worldline semantic engine returned an invalid input hash")
				}
				if len(result.Answers) != 1 {
					cancel()
					return worldlineSemanticSummary{}, errors.New("worldline semantic engine returned an incomplete pair result")
				}
				gotAnswer, ok := result.Answers[pair.ID]
				if !ok {
					cancel()
					return worldlineSemanticSummary{}, fmt.Errorf("worldline semantic result missing pair %s", pair.ID)
				}
				answer = gotAnswer
				if e := validateWorldlineSemanticAnswer(answer, criteria); e != nil {
					cancel()
					return worldlineSemanticSummary{}, e
				}
				inputHash = result.InputHash
				known = true
				if cached, ok := caches[worldlinePairCacheKey(pair, model, weights)]; ok {
					answer, inputHash, cacheHit = cached.Answer, cached.InputHash, true
				}
			}
			if err := validateWorldlineSemanticAnswer(answer, criteria); err != nil {
				cancel()
				return worldlineSemanticSummary{}, err
			}
			if !cacheHit {
				key := worldlinePairCacheKey(pair, model, weights)
				if err := checkLease(); err != nil {
					cancel()
					return worldlineSemanticSummary{}, err
				}
				result := cachedWorldlinePair{Pair: pair, Answer: answer, InputHash: inputHash, Model: model, Weights: weights}
				if err := s.persistDurablePairResult(ctx, scope, key, result, refs, subjects, guard); err != nil {
					cancel()
					return worldlineSemanticSummary{}, err
				}
				caches[key] = result
			}
			completed++
			suggestions++
		}
		cancel()
	}
	if err := checkLease(); err != nil {
		return worldlineSemanticSummary{}, err
	}
	status := "evaluated"
	if failed > 0 {
		status = "partial"
	}
	return worldlineSemanticSummary{Status: status, CandidatePairCount: len(pairs), PairCount: len(pairs), SuggestionCount: suggestions, Model: model, WeightsVersion: weights, CompletedPairCount: completed, RemainingPairCount: len(pairs) - completed - failed, FailedPairCount: failed}, nil
}

func validCacheIdentity(model, weights string) bool {
	model, weights = strings.ToLower(strings.TrimSpace(model)), strings.ToLower(strings.TrimSpace(weights))
	return model != "" && weights != "" && !strings.Contains(model, "synthetic") && !strings.Contains(weights, "synthetic") && !strings.Contains(model, "sandbox") && !strings.Contains(weights, "sandbox")
}

func worldlinePairState(scope string, pair semanticPair) (string, error) {
	state, err := json.Marshal(struct {
		Scope         string       `json:"scope"`
		RubricVersion string       `json:"rubric_version"`
		Pair          semanticPair `json:"pair"`
	}{scope, worldlineSemanticRubricVersion, pair})
	return string(state), err
}

func worldlinePairCacheKey(pair semanticPair, model, weights string) string {
	return worldlinePairCacheKeyForRubric(pair, model, weights, worldlineSemanticRubricVersion)
}

func worldlinePairCacheKeyForRubric(pair semanticPair, model, weights, rubric string) string {
	body, _ := json.Marshal(struct {
		Rubric  string       `json:"rubric"`
		Model   string       `json:"model"`
		Weights string       `json:"weights"`
		Pair    semanticPair `json:"pair"`
	}{rubric, model, weights, pair})
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func currentWorldlineSources(topics []WorldlineTopic) (map[string]string, map[string]string) {
	notes, subjects := map[string]string{}, map[string]string{}
	for _, topic := range topics {
		for _, lane := range topic.Lanes {
			for _, claim := range lane.Claims {
				if id, ok := claim.Metadata["source_note_id"].(string); ok {
					version, _ := claim.Metadata["source_note_version"].(string)
					notes[id] = version
				}
				for _, ev := range claim.Evidence {
					subjects[ev.TurnID+"@"+ev.Revision] = ev.SubjectID
				}
			}
		}
	}
	return notes, subjects
}

func (s *WorldlineService) validateDurablePairSources(ctx context.Context, scope string, pair semanticPair, currentNotes, currentSubjects map[string]string) ([]string, map[string]string, error) {
	refs := uniqueEvidenceRefs(append(append([]EvidenceReference{}, pair.Earlier.SourceRefs...), pair.Later.SourceRefs...))
	if len(refs) == 0 {
		return nil, nil, errors.New("worldline pair has no source evidence")
	}
	validated, _, err := s.Time.validateEvidence(ctx, refs)
	if err != nil {
		return nil, nil, err
	}
	for _, raw := range append(append([]string{}, pair.Earlier.SourceNoteVersions...), pair.Later.SourceNoteVersions...) {
		at := strings.LastIndex(raw, "@")
		if at <= 0 || at == len(raw)-1 || currentNotes[raw[:at]] != raw[at+1:] {
			return nil, nil, errors.New("worldline source note version is stale")
		}
	}
	subjects := map[string]string{}
	for _, ref := range refs {
		key := ref.TurnID + "@" + ref.Revision
		subject, ok := currentSubjects[key]
		if !ok {
			return nil, nil, errors.New("worldline source evidence is no longer projected")
		}
		subjects[key] = subject
	}
	return validated, subjects, nil
}

func (s *WorldlineService) persistDurablePairResult(ctx context.Context, scope, key string, result cachedWorldlinePair, refs []string, subjects map[string]string, guard func(context.Context) error) error {
	cacheID := "wl-pair-cache-" + shortHash(scope+"\x00"+key)
	refsCopy := append([]string{}, refs...)
	notes := map[string]string{}
	for _, raw := range append(append([]string{}, result.Pair.Earlier.SourceNoteVersions...), result.Pair.Later.SourceNoteVersions...) {
		at := strings.LastIndex(raw, "@")
		if at > 0 {
			notes[raw[:at]] = raw[at+1:]
		}
	}
	now := time.Now().UTC()
	cacheData := map[string]any{"scope": scope, "origin": "worldline", "cache_key": key, "result": map[string]any{"answer": result.Answer, "input_hash": result.InputHash, "model": result.Model, "weights_version": result.Weights}, "references": refsCopy, "source_note_ids": mapKeys(notes), "source_note_versions": notes}
	cacheEvent := plane.Event{ID: cacheID, Type: "worldline.semantic_cache", Timestamp: now, ObservedAt: now, Source: "local-web", Content: "durable pair result", Confidence: 1, StructuredData: cacheData}
	answer := result.Answer
	status := "review_required"
	if answer.Choice == "insufficient_evidence" {
		status = "insufficient_evidence"
	}
	structure := map[string]any{"scope": scope, "origin": "worldline", "semantic_status": status, "relation": answer.Choice, "confidence": answer.Confidence, "distribution": answer.Distribution, "model": result.Model, "weights_version": result.Weights, "input_hash": result.InputHash, "rubric_version": worldlineSemanticRubricVersion, "identity_status": result.Pair.IdentityStatus, "candidate_pair_id": result.Pair.ID, "thread_id": result.Pair.ThreadID(), "entry_id": result.Pair.Later.EntryID, "source_claim_ids": []string{result.Pair.Earlier.ClaimID, result.Pair.Later.ClaimID}, "source_note_ids": mapKeys(notes), "source_note_versions": notes, "references": refsCopy, "source_subjects": subjects, "earlier_claim_id": result.Pair.Earlier.ClaimID, "later_claim_id": result.Pair.Later.ClaimID, "earlier_references": evidenceStrings(result.Pair.Earlier.SourceRefs), "later_references": evidenceStrings(result.Pair.Later.SourceRefs), "reason": "decision_engine_pairwise_relation_suggestion; human review required"}
	suggestionID := "wl-semantic-" + shortHash(scope+"\x00"+key+"\x00"+semanticEventID(scope, result.Pair.ID, result.InputHash, result.Model, result.Weights, answer))
	suggestion := plane.Event{ID: suggestionID, Type: "worldline.semantic", Timestamp: now, ObservedAt: now, Source: "local-web", Content: "semantic relation suggestion; review required", Confidence: answer.Confidence, StructuredData: structure}
	return appendSemanticEventsWithCheck(ctx, []plane.Event{cacheEvent, suggestion}, func(c context.Context, es []plane.Event) error { _, err := s.Time.Events.Ingest(c, es); return err }, func(c context.Context) error {
		if guard != nil {
			if err := guard(c); err != nil {
				return err
			}
		}
		return s.verifyDurablePairSources(c, result.Pair)
	}, func(c context.Context, ids []string) error { return s.Time.PurgeContent(c, ids) })
}

func (s *WorldlineService) verifyDurablePairSources(ctx context.Context, pair semanticPair) error {
	refs := uniqueEvidenceRefs(append(append([]EvidenceReference{}, pair.Earlier.SourceRefs...), pair.Later.SourceRefs...))
	if _, _, err := s.Time.validateEvidence(ctx, refs); err != nil {
		return err
	}
	for _, raw := range append(append([]string{}, pair.Earlier.SourceNoteVersions...), pair.Later.SourceNoteVersions...) {
		at := strings.LastIndex(raw, "@")
		if at <= 0 {
			return errors.New("invalid source note version")
		}
		id, want := raw[:at], raw[at+1:]
		var annotationVersion, kind, summary, topics string
		err := s.Time.Store.DB().QueryRowContext(ctx, `SELECT n.annotation_version,n.kind,n.summary,n.topic_ids_json FROM notes n LEFT JOIN derived_artifacts da ON da.kind='note' AND da.artifact_id=n.id WHERE n.id=? AND COALESCE(da.stale,0)=0`, id).Scan(&annotationVersion, &kind, &summary, &topics)
		if err != nil {
			return errors.New("worldline source note changed or was removed")
		}
		got := annotationVersion + "-" + shortHash(kind+"\x00"+summary+"\x00"+topics)
		if got != want {
			return errors.New("worldline source note version changed")
		}
	}
	for _, ref := range refs {
		key := ref.TurnID + "@" + ref.Revision
		expected, hasExpected := pair.Earlier.SourceSubjects[key]
		if later, ok := pair.Later.SourceSubjects[key]; ok {
			if hasExpected && expected != later {
				return errors.New("pair contains conflicting source speaker identities")
			}
			expected, hasExpected = later, true
		}
		if !hasExpected {
			continue
		}
		var recording, scoped string
		err := s.Time.Store.DB().QueryRowContext(ctx, `SELECT t.recording_id,COALESCE(t.speaker_scoped_id,'') FROM turns t JOIN attachment_turns at ON at.turn_id=t.id JOIN source_attachments a ON a.id=at.attachment_id AND a.active=1 WHERE t.id=? AND a.transcript_revision=?`, ref.TurnID, ref.Revision).Scan(&recording, &scoped)
		if err != nil {
			return err
		}
		current := ""
		if scoped != "" {
			projection, e := s.Time.Store.SpeakerProjection(ctx, recording, ref.Revision)
			if e != nil {
				return e
			}
			current = projection[scoped]
		}
		if current != expected {
			return errors.New("worldline speaker identity changed")
		}
	}
	return nil
}

func (s *WorldlineService) recordDurablePairFailure(ctx context.Context, scope, jobID string, attempt, index int, pair semanticPair, kind string) error {
	refs := evidenceStrings(append(append([]EvidenceReference{}, pair.Earlier.SourceRefs...), pair.Later.SourceRefs...))
	event := plane.Event{ID: fmt.Sprintf("wl-pair-failure-%s-%d-%d", shortHash(jobID), attempt, index), Type: "worldline.semantic_failure", Timestamp: time.Now().UTC(), Source: "local-web", Content: "pair processing failed", Confidence: 1, StructuredData: map[string]any{"scope": scope, "origin": "worldline", "job_id": jobID, "pair_id": pair.ID, "failure_kind": kind, "references": refs, "source_note_ids": []string{}}}
	_, err := s.Time.Events.Ingest(ctx, []plane.Event{event})
	return err
}

// appendSemanticEventsWithCheck closes the race where source deletion or
// revision replacement lands after the pre-ingest validation. The injected
// functions keep the cleanup guarantee directly testable without real data.
func appendSemanticEventsWithCheck(ctx context.Context, events []plane.Event, ingest func(context.Context, []plane.Event) error, verify func(context.Context) error, purge func(context.Context, []string) error) error {
	ids := make([]string, 0, len(events))
	for _, event := range events {
		ids = append(ids, event.ID)
	}
	if err := ingest(ctx, events); err != nil {
		return err
	}
	if err := verify(ctx); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if purgeErr := purge(cleanupCtx, ids); purgeErr != nil {
			return fmt.Errorf("semantic source changed after event append (%v); purge failed: %w", err, purgeErr)
		}
		return fmt.Errorf("semantic source changed before event append completed: %w", err)
	}
	return nil
}

func (s *WorldlineService) verifySemanticSources(ctx context.Context, scope, expectedFingerprint string, events []plane.Event) error {
	topics, _, _, err := s.project(ctx, scope)
	if err != nil {
		return err
	}
	if got := worldlineSourceFingerprint(topics); got != expectedFingerprint {
		return errors.New("worldline source notes, references or speaker bindings changed during semantic event append")
	}
	refs := []EvidenceReference{}
	for _, event := range events {
		values, _ := sourceReferences(event)
		for _, value := range values {
			at := strings.LastIndex(value, "@")
			if at <= 0 || at == len(value)-1 {
				return errors.New("semantic event contains an invalid source reference")
			}
			refs = append(refs, EvidenceReference{TurnID: value[:at], Revision: value[at+1:]})
		}
	}
	if len(refs) == 0 {
		return errors.New("semantic events contain no source references")
	}
	if _, _, err := s.Time.validateEvidence(ctx, refs); err != nil {
		return fmt.Errorf("semantic source references are no longer current: %w", err)
	}
	return nil
}

func worldlineSourceFingerprint(topics []WorldlineTopic) string {
	type sourceClaim struct {
		ID, Version, TopicTitle, SubjectKey string
		Evidence                            []string
		Notes                               []string
	}
	claims := []sourceClaim{}
	for _, topic := range topics {
		for _, lane := range topic.Lanes {
			for _, claim := range lane.Claims {
				item := sourceClaim{ID: claim.ID, Version: claim.Version, TopicTitle: claim.SourceTopicTitle, SubjectKey: claim.SubjectKey}
				for _, ev := range claim.Evidence {
					item.Evidence = append(item.Evidence, ev.TurnID+"@"+ev.Revision+"="+ev.SubjectID)
				}
				_, notes, versions := semanticSourcesForClaim(claim)
				for _, note := range notes {
					item.Notes = append(item.Notes, note+"="+versions[note])
				}
				sort.Strings(item.Evidence)
				sort.Strings(item.Notes)
				claims = append(claims, item)
			}
		}
	}
	sort.Slice(claims, func(i, j int) bool { return claims[i].ID < claims[j].ID })
	body, _ := json.Marshal(claims)
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func worldlineSemanticCriteria() map[string]string {
	return map[string]string{
		"text_similarity":       "The supplied text has substantially similar content; this does not establish same speaker or same real-world fact.",
		"complementary_context": "The text adds context without a supported contradiction.",
		"review_required":       "The supplied text may conflict; human review is required and no reality change is asserted.",
		"unrelated":             "The excerpts concern unrelated content despite the exact same source topic title.",
		"insufficient_evidence": "The supplied evidence is clipped, missing, ambiguous, or otherwise insufficient to judge.",
	}
}

func worldlineSemanticQuestion(id string) decision.Question {
	return decision.Question{ID: id, Type: decision.TypeChoice, Criteria: worldlineSemanticCriteria(), Instructions: "Compare only the single pair whose id matches this question id. Both items have the same normalized source-topic title. Identity status is stated per pair. If identity is unconfirmed, judge text resemblance only and never imply the same speaker or same real-world fact. Use only supplied summaries and explicitly marked transcript excerpts. Some excerpts may be clipped; all source references are listed separately. review_required is a human-review hint only; never assert truth, change dates, merge identities, or merge claims. Choose insufficient_evidence whenever the supplied material does not support a clear judgment; do not turn uncertainty into complementary_context."}
}

func validateWorldlineSemanticAnswer(answer decision.Answer, criteria map[string]string) error {
	if answer.Type != decision.TypeChoice {
		return errors.New("worldline semantic engine returned a non-choice answer")
	}
	if _, ok := criteria[answer.Choice]; !ok {
		return fmt.Errorf("worldline semantic result has unsupported relation %q", answer.Choice)
	}
	if !finiteWorldlineProbability(answer.Confidence) {
		return errors.New("worldline semantic engine returned invalid confidence")
	}
	if len(answer.Distribution) != len(criteria) {
		return errors.New("worldline semantic engine returned incomplete distribution")
	}
	sum := 0.0
	for label := range criteria {
		p, ok := answer.Distribution[label]
		if !ok || !finiteWorldlineProbability(p) {
			return fmt.Errorf("worldline semantic engine returned invalid distribution for %s", label)
		}
		sum += p
	}
	if math.Abs(sum-1) > 1e-6 {
		return errors.New("worldline semantic engine returned non-normalized distribution")
	}
	return nil
}

func finiteWorldlineProbability(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= 1
}

func collectWorldlineSemanticPairs(topics []WorldlineTopic, limit int) ([]semanticPair, int) {
	built := buildWorldlineThreadDetails(topics)
	out := []semanticPair{}
	total := 0
	for _, thread := range built {
		for i, entry := range thread.entries {
			if entry.Category != "facts" {
				continue
			}
			var prior *WorldlineThreadEntry
			identityStatus := ""
			for j := i - 1; j >= 0; j-- {
				candidate := thread.entries[j]
				if candidate.Category != "facts" || !entriesSpanConversations(candidate, entry) {
					continue
				}
				knownA := candidate.SubjectKey != "" && !strings.HasPrefix(candidate.SubjectKey, "unknown:")
				knownB := entry.SubjectKey != "" && !strings.HasPrefix(entry.SubjectKey, "unknown:")
				if knownA && knownB {
					if candidate.SubjectKey != entry.SubjectKey {
						continue
					}
					identityStatus = "same_confirmed_subject"
				} else {
					identityStatus = "unconfirmed_identity_text_only"
				}
				copy := candidate
				prior = &copy
				break
			}
			if prior == nil {
				continue
			}
			total++
			pairID := "pair-" + shortHash(thread.thread.ID+"\x00"+prior.ID+"\x00"+entry.ID)
			out = append(out, semanticPair{ID: pairID, Title: thread.thread.Title, IdentityStatus: identityStatus, ConversationCount: thread.thread.ConversationCount, MentionedAt: entry.MentionedAt, Earlier: semanticPairEntry(*prior), Later: semanticPairEntry(entry)})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.ConversationCount != b.ConversationCount {
			return a.ConversationCount > b.ConversationCount
		}
		if a.MentionedAt == nil && b.MentionedAt != nil {
			return false
		}
		if a.MentionedAt != nil && b.MentionedAt == nil {
			return true
		}
		if a.MentionedAt != nil && b.MentionedAt != nil && !a.MentionedAt.Equal(*b.MentionedAt) {
			return a.MentionedAt.After(*b.MentionedAt)
		}
		if a.Title != b.Title {
			return normalizeTopicTitle(a.Title) < normalizeTopicTitle(b.Title)
		}
		return a.ID < b.ID
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, total
}

func semanticPairEntry(entry WorldlineThreadEntry) semanticPairEvidence {
	sources := []semanticSourceText{}
	refs := []EvidenceReference{}
	subjects := map[string]string{}
	truncated := len(entry.Evidence) > 3
	for i, ev := range entry.Evidence {
		refs = append(refs, EvidenceReference{TurnID: ev.TurnID, Revision: ev.Revision})
		subjects[ev.TurnID+"@"+ev.Revision] = ev.SubjectID
		if i >= 3 {
			continue
		}
		text := clipSemanticText(ev.Text, 800)
		wasTruncated := len([]rune(strings.TrimSpace(ev.Text))) > 800
		truncated = truncated || wasTruncated
		sources = append(sources, semanticSourceText{TurnID: ev.TurnID, Revision: ev.Revision, Text: text, Truncated: wasTruncated, SubjectID: ev.SubjectID})
	}
	summary := clipSemanticText(entry.Summary, 800)
	truncated = truncated || len([]rune(strings.TrimSpace(entry.Summary))) > 800
	return semanticPairEvidence{EntryID: entry.ID, ClaimID: entry.ClaimID, Summary: summary, EvidenceCount: len(entry.Evidence), SourceRefs: uniqueEvidenceRefs(refs), SourceNoteVersions: claimSourceNoteVersions(entry.Claim), SourceSubjects: subjects, ExcerptTruncated: truncated, Sources: sources}
}

func claimSourceNoteVersions(claim WorldlineClaim) []string {
	if id, ok := claim.Metadata["source_note_id"].(string); ok && strings.TrimSpace(id) != "" {
		version, _ := claim.Metadata["source_note_version"].(string)
		return []string{id + "@" + version}
	}
	return []string{}
}

func entriesSpanConversations(a, b WorldlineThreadEntry) bool {
	for _, x := range a.ConversationIDs {
		for _, y := range b.ConversationIDs {
			if x != y {
				return true
			}
		}
	}
	return false
}

func clipSemanticText(text string, maxRunes int) string {
	r := []rune(strings.TrimSpace(text))
	if len(r) > maxRunes {
		return string(r[:maxRunes]) + "…"
	}
	return string(r)
}

func (p semanticPair) ThreadID() string { return "thread-" + shortHash(normalizeTopicTitle(p.Title)) }

func findSemanticPairEntries(topics []WorldlineTopic, pair semanticPair) (*WorldlineThreadEntry, *WorldlineThreadEntry) {
	var a, b *WorldlineThreadEntry
	for _, topic := range topics {
		for _, lane := range topic.Lanes {
			for i := range lane.Claims {
				claim := lane.Claims[i]
				if a == nil && claim.ID == pair.Earlier.ClaimID {
					entry := WorldlineThreadEntry{ID: pair.Earlier.EntryID, ClaimID: claim.ID, Claim: claim}
					a = &entry
				}
				if b == nil && claim.ID == pair.Later.ClaimID {
					entry := WorldlineThreadEntry{ID: pair.Later.EntryID, ClaimID: claim.ID, Claim: claim}
					b = &entry
				}
			}
		}
	}
	return a, b
}

func semanticSources(a, b WorldlineThreadEntry) ([]EvidenceReference, []string, map[string]string) {
	refsA, notesA, versionsA := semanticSourcesForClaim(a.Claim)
	refsB, notesB, versionsB := semanticSourcesForClaim(b.Claim)
	versions := map[string]string{}
	for id, v := range versionsA {
		versions[id] = v
	}
	for id, v := range versionsB {
		versions[id] = v
	}
	return uniqueEvidenceRefs(append(refsA, refsB...)), uniqueSorted(append(notesA, notesB...)), versions
}

func semanticSourcesForClaim(claim WorldlineClaim) ([]EvidenceReference, []string, map[string]string) {
	refs, notes, versions := []EvidenceReference{}, []string{}, map[string]string{}
	for _, ev := range claim.Evidence {
		refs = append(refs, EvidenceReference{TurnID: ev.TurnID, Revision: ev.Revision})
	}
	if id, ok := claim.Metadata["source_note_id"].(string); ok {
		notes = append(notes, id)
		if version, ok := claim.Metadata["source_note_version"].(string); ok {
			versions[id] = version
		}
	}
	for _, id := range stringSetToSlice(claim.Metadata["source_note_ids"]) {
		notes = append(notes, id)
	}
	for id, version := range stringMap(claim.Metadata["source_note_versions"]) {
		versions[id] = version
	}
	return uniqueEvidenceRefs(refs), uniqueSorted(notes), versions
}

func semanticSourceSubjects(a, b WorldlineClaim) map[string]string {
	result := map[string]string{}
	for _, claim := range []WorldlineClaim{a, b} {
		for _, ev := range claim.Evidence {
			result[ev.TurnID+"@"+ev.Revision] = ev.SubjectID
		}
	}
	return result
}

func uniqueEvidenceRefs(refs []EvidenceReference) []EvidenceReference {
	m := map[string]EvidenceReference{}
	for _, r := range refs {
		if r.TurnID != "" && r.Revision != "" {
			m[r.TurnID+"@"+r.Revision] = r
		}
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]EvidenceReference, 0, len(keys))
	for _, k := range keys {
		out = append(out, m[k])
	}
	return out
}

func semanticEventID(scope, pairID, inputHash, model, weights string, answer decision.Answer) string {
	body, _ := json.Marshal(struct {
		Choice       string
		Confidence   float64
		Distribution map[string]float64
	}{answer.Choice, answer.Confidence, answer.Distribution})
	sum := sha256.Sum256(body)
	return "wl-semantic-" + shortHash(scope+"\x00"+pairID+"\x00"+inputHash+"\x00"+model+"\x00"+weights+"\x00"+hex.EncodeToString(sum[:]))
}

func (s *WorldlineService) applySemanticSuggestions(ctx context.Context, scope string, built []threadBuild) error {
	if s == nil || s.Time == nil || s.Time.Events == nil {
		return nil
	}
	currentNotes := map[string]string{}
	byEntry := map[string]*WorldlineThreadEntry{}
	claimsByID := map[string]WorldlineClaim{}
	currentSubjects := map[string]string{}
	for ti := range built {
		for i := range built[ti].entries {
			e := &built[ti].entries[i]
			byEntry[e.ID] = e
			addCurrentNoteVersions(currentNotes, e.Claim)
			claimsByID[e.Claim.ID] = e.Claim
			for _, ev := range e.Claim.Evidence {
				currentSubjects[ev.TurnID+"@"+ev.Revision] = ev.SubjectID
			}
		}
		for i := range built[ti].unknown {
			e := &built[ti].unknown[i]
			byEntry[e.ID] = e
			addCurrentNoteVersions(currentNotes, e.Claim)
			claimsByID[e.Claim.ID] = e.Claim
			for _, ev := range e.Claim.Evidence {
				currentSubjects[ev.TurnID+"@"+ev.Revision] = ev.SubjectID
			}
		}
	}
	events, err := s.Time.History(ctx, scope)
	if err != nil {
		return err
	}
	latest := map[string]plane.Event{}
	reviews := map[string]plane.Event{}
	for _, event := range events {
		if event.Type == "worldline.semantic_review" && event.SourceStatus == "current" {
			sid, _ := event.StructuredData["suggestion_id"].(string)
			prior, ok := reviews[sid]
			if sid != "" && (!ok || eventRecordedAfter(event, prior)) {
				reviews[sid] = event
			}
			continue
		}
		if event.Type != "worldline.semantic" || event.SourceStatus != "current" || !feedbackSourcesCurrent(event, currentNotes) || !semanticSourceSubjectsCurrent(event, currentSubjects) {
			continue
		}
		entryID, _ := event.StructuredData["entry_id"].(string)
		entry := byEntry[entryID]
		if entry == nil {
			continue
		}
		prior, ok := latest[entryID]
		if !ok || eventRecordedAfter(event, prior) {
			latest[entryID] = event
		}
	}
	for entryID, event := range latest {
		entry := byEntry[entryID]
		suggestion := semanticSuggestionFromEvent(event, claimsByID)
		if review, ok := reviews[event.ID]; ok {
			status, _ := review.StructuredData["decision"].(string)
			suggestion.ReviewStatus = status
			suggestion.SuggestionID = event.ID
			suggestion.RequiresReview = semanticReviewRequiresReview(status)
		}
		entry.SemanticSuggestion = suggestion
	}
	return nil
}

func semanticReviewRequiresReview(status string) bool { return status == "reopen" }

func semanticSourceSubjectsCurrent(event plane.Event, current map[string]string) bool {
	stored := stringMap(event.StructuredData["source_subjects"])
	refs, _ := sourceReferences(event)
	if len(stored) == 0 || len(refs) == 0 || len(stored) != len(refs) {
		return false
	}
	for _, ref := range refs {
		value, ok := stored[ref]
		if !ok {
			return false
		}
		actual, exists := current[ref]
		if !exists || actual != value {
			return false
		}
	}
	return true
}

func addCurrentNoteVersions(out map[string]string, claim WorldlineClaim) {
	if id, ok := claim.Metadata["source_note_id"].(string); ok {
		version, _ := claim.Metadata["source_note_version"].(string)
		out[id] = version
	}
	for _, id := range stringSetToSlice(claim.Metadata["source_note_ids"]) {
		versions := stringMap(claim.Metadata["source_note_versions"])
		out[id] = versions[id]
	}
}

func eventRecordedAfter(a, b plane.Event) bool {
	if a.RecordedAt != nil && b.RecordedAt != nil {
		return a.RecordedAt.After(*b.RecordedAt)
	}
	return a.ID > b.ID
}

func semanticSuggestionFromEvent(event plane.Event, claimsByID map[string]WorldlineClaim) *WorldlineSemanticSuggestion {
	data := event.StructuredData
	relation, _ := data["relation"].(string)
	status, _ := data["semantic_status"].(string)
	model, _ := data["model"].(string)
	weights, _ := data["weights_version"].(string)
	inputHash, _ := data["input_hash"].(string)
	snapshot, _ := data["source_snapshot_version"].(string)
	reason, _ := data["reason"].(string)
	confidence, _ := data["confidence"].(float64)
	distribution := floatMap(data["distribution"])
	claimIDs := stringSlice(data["source_claim_ids"])
	refs, _ := sourceReferences(event)
	evidence := []EvidenceReference{}
	for _, ref := range refs {
		at := strings.LastIndex(ref, "@")
		if at > 0 {
			evidence = append(evidence, EvidenceReference{TurnID: ref[:at], Revision: ref[at+1:]})
		}
	}
	var recorded *time.Time
	if event.RecordedAt != nil {
		v := *event.RecordedAt
		recorded = &v
	}
	identity, _ := data["identity_status"].(string)
	pairID, _ := data["candidate_pair_id"].(string)
	suggestion := &WorldlineSemanticSuggestion{Status: status, Relation: relation, Confidence: confidence, Distribution: distribution, Model: model, WeightsVersion: weights, InputHash: inputHash, SourceSnapshotVersion: snapshot, SourceClaimIDs: claimIDs, SourceEvidence: evidence, IdentityStatus: identity, CandidatePairID: pairID, Reason: reason, RequiresReview: true, SuggestionID: event.ID, RecordedAt: recorded}
	if len(claimIDs) >= 2 {
		if claim, ok := claimsByID[claimIDs[0]]; ok {
			suggestion.EarlierSummary = claim.Summary
			suggestion.EarlierEvidence = append([]WorldlineEvidence{}, claim.Evidence...)
		}
		if claim, ok := claimsByID[claimIDs[1]]; ok {
			suggestion.LaterSummary = claim.Summary
			suggestion.LaterEvidence = append([]WorldlineEvidence{}, claim.Evidence...)
		}
	}
	return suggestion
}

func stringSlice(v any) []string {
	if values, ok := v.([]string); ok {
		return append([]string{}, values...)
	}
	if values, ok := v.([]any); ok {
		out := []string{}
		for _, x := range values {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
func floatMap(v any) map[string]float64 {
	out := map[string]float64{}
	if values, ok := v.(map[string]any); ok {
		for k, x := range values {
			switch n := x.(type) {
			case float64:
				out[k] = n
			case int:
				out[k] = float64(n)
			}
		}
	}
	if values, ok := v.(map[string]float64); ok {
		for k, x := range values {
			out[k] = x
		}
	}
	return out
}
