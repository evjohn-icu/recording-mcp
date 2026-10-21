package eval

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"recordingmcp/internal/contextbuild"
	"recordingmcp/internal/decision"
	"recordingmcp/internal/generate"
	"recordingmcp/internal/memory"
	"recordingmcp/internal/pipeline"
	"recordingmcp/internal/process"
	"recordingmcp/internal/semantic"
)

// Options configures one harness run.
type Options struct {
	DBPath        string
	CorpusPath    string
	QuestionsPath string
	LinksPath     string
	MaxItems      int
	MaxBytes      int
	// RetrievalPath enables the hybrid/lexical retrieval pass over a separate
	// question fixture. Empty disables the pass.
	RetrievalPath string
	// ProjectionPath enables the projection pass over its fixture. Empty
	// disables the pass.
	ProjectionPath string
	// SemanticProvider overrides the default offline hashing embedder.
	SemanticProvider semantic.Provider
	// SemanticProviderConfigured distinguishes an explicit off provider from
	// the zero-value options default, which remains the offline hash provider.
	SemanticProviderConfigured bool
	// LexicalOnly skips semantic indexing and the hybrid pass.
	LexicalOnly bool
}

// Report is the machine-readable evaluation report.
type Report struct {
	CorpusSegments     int              `json:"corpus_segments"`
	Conversations      int              `json:"conversations"`
	Questions          int              `json:"questions"`
	RetrievalQuestions int              `json:"retrieval_questions"`
	Links              []AppliedLink    `json:"links"`
	Metrics            Metrics          `json:"metrics"`
	PerQuestion        []QuestionResult `json:"per_question"`
}

// Metrics are the aggregate evaluation numbers.
type Metrics struct {
	// EvidenceRecallAt5 is the macro-average over questions of the fraction of
	// authored gold evidence turns that appear in the first five items.
	EvidenceRecallAt5 float64 `json:"evidence_recall_at_5"`
	// StaleLeakage is sum(stale items) / sum(items) across every classic
	// question. An item counts as stale when the builder marks it stale or when
	// any of its evidence turns come from a superseded transcript revision.
	StaleLeakage float64 `json:"stale_leakage"`
	// InvalidEvidenceRefs is the aggregate of invalid reference occurrences
	// reported by classic and retrieval evaluations. The category fields retain
	// the same counts by evaluation family (and retrieval mode).
	InvalidEvidenceRefs                 int `json:"invalid_evidence_refs"`
	ClassicInvalidEvidenceRefs          int `json:"classic_invalid_evidence_refs"`
	RetrievalInvalidEvidenceRefs        int `json:"retrieval_invalid_evidence_refs"`
	RetrievalLexicalInvalidEvidenceRefs int `json:"retrieval_lexical_invalid_evidence_refs"`
	RetrievalHybridInvalidEvidenceRefs  int `json:"retrieval_hybrid_invalid_evidence_refs"`
	// RelationAssisted counts gold evidence hits whose item is reachable only
	// through a confirmed topic link.
	RelationAssisted int `json:"relation_assisted"`
	// ContextCoverage is the macro-average topic coverage over questions that
	// author gold topics.
	ContextCoverage float64 `json:"context_coverage"`
	LatencyMSAvg    float64 `json:"latency_ms_avg"`
	LatencyMSMax    float64 `json:"latency_ms_max"`
	LatencyMSTotal  float64 `json:"latency_ms_total"`
	// Retrieval latency is tracked separately for each retrieval mode. Values
	// are fractional milliseconds so fast deterministic builds are not rounded
	// down to zero.
	RetrievalLexicalLatencyMSAvg   float64 `json:"retrieval_lexical_latency_ms_avg"`
	RetrievalLexicalLatencyMSMax   float64 `json:"retrieval_lexical_latency_ms_max"`
	RetrievalLexicalLatencyMSTotal float64 `json:"retrieval_lexical_latency_ms_total"`
	RetrievalHybridLatencyMSAvg    float64 `json:"retrieval_hybrid_latency_ms_avg"`
	RetrievalHybridLatencyMSMax    float64 `json:"retrieval_hybrid_latency_ms_max"`
	RetrievalHybridLatencyMSTotal  float64 `json:"retrieval_hybrid_latency_ms_total"`
	// ModelCalls is the number of rerank invocations; the harness configures no
	// reranker, so the deterministic assembly runs untouched.
	ModelCalls int `json:"model_calls"`
	// LexicalRecallAt5 is the macro-average recall@5 of the lexical-only pass
	// over the retrieval questions.
	LexicalRecallAt5 float64 `json:"lexical_recall_at_5"`
	// HybridRecallAt5 is the macro-average recall@5 of the hybrid pass over
	// the retrieval questions.
	HybridRecallAt5 float64 `json:"hybrid_recall_at_5"`
	// SemanticAssistedRecall is the macro-average of |gold found in hybrid but
	// not lexical| / |gold| over the retrieval questions.
	SemanticAssistedRecall float64 `json:"semantic_assisted_recall"`
	// UnanswerableQuestions counts distinct unanswerable fixture ids that ran.
	UnanswerableQuestions int `json:"unanswerable_questions"`
	// UnanswerableCleanRate is the fraction of unanswerable evaluations with no returned items.
	UnanswerableCleanRate float64 `json:"unanswerable_clean_rate"`
	// UnanswerableFalsePositiveItems counts returned items from unanswerable evaluations.
	UnanswerableFalsePositiveItems        int `json:"unanswerable_false_positive_items"`
	UnanswerableLexicalFalsePositiveItems int `json:"unanswerable_lexical_false_positive_items"`
	UnanswerableHybridFalsePositiveItems  int `json:"unanswerable_hybrid_false_positive_items"`
	// The per-mode rates count an unanswerable evaluation as a false positive
	// when that mode returns at least one item. Item counts above remain useful
	// for diagnosing how much extra context each mode returned.
	UnanswerableLexicalFalsePositiveRate float64 `json:"unanswerable_lexical_false_positive_rate"`
	UnanswerableHybridFalsePositiveRate  float64 `json:"unanswerable_hybrid_false_positive_rate"`
	// TurnTop1Accuracy checks the first turn item for retrieval questions with an authored top1.
	TurnTop1Accuracy float64 `json:"turn_top1_accuracy"`
	// AmbientLeakage is sum(ambient items/evidence) / sum(returned) across
	// every build of the run. It must stay 0.
	AmbientLeakage float64 `json:"ambient_leakage"`
	// ScopeEscapeCount counts returned items and evidence refs that violate
	// their question's explicit scope. It must stay 0.
	ScopeEscapeCount int `json:"scope_escape_count"`
	// Projection carries the projection-pass metrics.
	Projection ProjectionMetrics `json:"projection"`
}

// ProjectionMetrics scores the projection pass. Ran reports whether the pass
// executed at all (a disabled pass leaves the other fields at their zero value
// rather than claiming the projection is nondeterministic). The honesty
// metrics are fractions in [0,1] where 1 means the projection behaved;
// Deterministic reports two identical builds.
type ProjectionMetrics struct {
	Ran                  bool    `json:"ran"`
	EvidenceValid        float64 `json:"evidence_valid"`
	StaleNotCurrent      float64 `json:"stale_not_current"`
	SupersededNotCurrent float64 `json:"superseded_not_current"`
	ConflictingHonest    float64 `json:"conflicting_honest"`
	Deterministic        bool    `json:"deterministic"`
	Objects              int     `json:"objects"`
	Conflicts            int     `json:"conflicts"`
	Unprojected          int     `json:"unprojected"`
}

// QuestionResult is one question's evaluation.
type QuestionResult struct {
	ID                  string   `json:"id"`
	Purpose             string   `json:"purpose"`
	Query               string   `json:"query"`
	Mode                string   `json:"mode,omitempty"`
	Hits                int      `json:"hits"`
	Unanswerable        bool     `json:"unanswerable,omitempty"`
	ReturnedItems       int      `json:"returned_items"`
	TurnTop1Hit         *bool    `json:"turn_top1_hit,omitempty"`
	GoldEvidenceCount   int      `json:"gold_evidence_count"`
	GoldEvidenceFound   int      `json:"gold_evidence_found"`
	RecallAt5           float64  `json:"recall_at_5"`
	StaleLeakage        float64  `json:"stale_leakage"`
	RelationAssisted    int      `json:"relation_assisted"`
	AmbientItems        int      `json:"ambient_items,omitempty"`
	ScopeEscapes        int      `json:"scope_escapes,omitempty"`
	ContextCoverage     *float64 `json:"context_coverage,omitempty"`
	LatencyMS           float64  `json:"latency_ms"`
	ExpectStaleExcluded bool     `json:"expect_stale_excluded"`
}

// Run ingests the corpus, drives the pipeline to completion, applies curated
// links, evaluates every question and returns the report.
func Run(ctx context.Context, opts Options) (Report, error) {
	if opts.DBPath == "" {
		return Report{}, errors.New("eval: db path is required")
	}
	corpus, err := LoadCorpus(opts.CorpusPath)
	if err != nil {
		return Report{}, err
	}
	if err := resetDatabase(opts.DBPath); err != nil {
		return Report{}, err
	}
	store, err := memory.Open(opts.DBPath)
	if err != nil {
		return Report{}, fmt.Errorf("open store: %w", err)
	}
	defer store.Close()
	db := store.DB()

	if err := ingestCorpus(ctx, store, corpus); err != nil {
		return Report{}, err
	}
	if err := drainPipeline(ctx, db, store); err != nil {
		return Report{}, err
	}
	linkFixtures, err := LoadLinks(opts.LinksPath)
	if err != nil {
		return Report{}, err
	}
	applied, err := applyLinks(ctx, db, linkFixtures, time.Now())
	if err != nil {
		return Report{}, err
	}
	questions, err := LoadQuestions(ctx, db, opts.QuestionsPath)
	if err != nil {
		return Report{}, err
	}
	maxItems := opts.MaxItems
	if maxItems <= 0 {
		maxItems = 8
	}
	maxBytes := opts.MaxBytes
	if maxBytes <= 0 {
		maxBytes = 16000
	}
	report := Report{
		CorpusSegments: corpus.SegmentCount(),
		Conversations:  len(corpus.Conversations),
		Questions:      len(questions),
		Links:          applied,
	}
	if report.Links == nil {
		report.Links = []AppliedLink{}
	}
	staleTurns, err := staleTurnSet(ctx, db)
	if err != nil {
		return Report{}, err
	}
	knownTurns, err := knownTurnSet(ctx, db)
	if err != nil {
		return Report{}, err
	}
	noteEvidence, err := loadNoteEvidence(ctx, db)
	if err != nil {
		return Report{}, err
	}

	// Semantic backend for the hybrid pass. The default is the offline hashing
	// embedder; LexicalOnly skips indexing and the hybrid pass entirely, so an
	// injected provider is ignored rather than half-wired.
	// The projection fixture is seeded first (through the real write paths) so
	// its notes are part of the same current state the retrieval passes see.
	projectionFixture, err := loadProjectionFixture(opts.ProjectionPath)
	if err != nil {
		return Report{}, err
	}
	if projectionFixture != nil {
		if err := seedProjectionFixture(ctx, db, store, projectionFixture); err != nil {
			return Report{}, err
		}
	}
	provider := opts.SemanticProvider
	if opts.LexicalOnly {
		provider = nil
	} else if provider == nil && !opts.SemanticProviderConfigured {
		provider = semantic.NewHashProvider(semantic.DefaultHashDims)
	}
	var retriever semantic.SemanticRetriever
	if provider != nil {
		indexer := &semantic.Indexer{DB: db, Provider: provider}
		if _, err := indexer.IndexAll(ctx); err != nil {
			return Report{}, fmt.Errorf("semantic index: %w", err)
		}
		retriever = &semantic.SQLiteRetriever{DB: db, Provider: provider}
	}
	retrievalQuestions, err := LoadRetrievalQuestions(ctx, db, opts.RetrievalPath)
	if err != nil {
		return Report{}, err
	}
	report.RetrievalQuestions = len(retrievalQuestions)

	ambientTurns, err := ambientTurnSet(ctx, db)
	if err != nil {
		return Report{}, err
	}
	ambientNotes, err := ambientNoteSet(ctx, db)
	if err != nil {
		return Report{}, err
	}
	trackAmbient := func(pkg contextbuild.Package) (ambient, total int) {
		for _, item := range pkg.Items {
			total++
			if isAmbientItem(item, noteEvidence, ambientTurns, ambientNotes) {
				ambient++
			}
			// A turn item IS its own evidence, so only notes contribute a
			// separate evidence set; counting turns twice would inflate the
			// denominator and understate the ratio.
			if item.Kind == "turn" {
				continue
			}
			for _, ev := range evidenceIDs(item, noteEvidence) {
				total++
				if ambientTurns[ev] {
					ambient++
				}
			}
		}
		for _, ref := range pkg.Evidence {
			total++
			if ambientTurns[ref.TurnID] {
				ambient++
			}
		}
		return ambient, total
	}

	report.PerQuestion = make([]QuestionResult, 0, len(questions))
	totalItems, totalStale := 0, 0
	recallSum, recallQuestions := 0.0, 0
	coverageSum, coverageQuestions := 0.0, 0
	ambientHitsTotal, ambientReturnedTotal := 0, 0
	var latencyTotal, latencyMax float64
	for _, q := range questions {
		started := time.Now()
		// The classic pass keeps the pre-hybrid behavior exactly: lexical mode,
		// raw evidence windows on. Hybrid retrieval is measured separately
		// over the retrieval questions.
		pkg, err := contextbuild.Build(ctx, contextbuild.Deps{Store: store, DB: db, Semantic: retriever}, contextbuild.Request{
			Purpose:            q.Purpose,
			Query:              q.Query,
			MaxItems:           maxItems,
			MaxBytes:           maxBytes,
			IncludeRawEvidence: true,
			Mode:               contextbuild.ModeLexical,
		})
		latency := float64(time.Since(started)) / float64(time.Millisecond)
		if err != nil {
			return Report{}, fmt.Errorf("question %q: build: %w", q.ID, err)
		}
		evaluated := evaluateQuestion(q, pkg, staleTurns, knownTurns, noteEvidence)
		ambient, returned := trackAmbient(pkg)
		ambientHitsTotal += ambient
		ambientReturnedTotal += returned
		res := QuestionResult{
			ID:                  q.ID,
			Purpose:             q.Purpose,
			Query:               q.Query,
			Mode:                contextbuild.ModeLexical,
			Hits:                evaluated.hits,
			Unanswerable:        q.Unanswerable,
			ReturnedItems:       evaluated.hits,
			GoldEvidenceCount:   len(q.GoldEvidenceTurnIDs),
			GoldEvidenceFound:   evaluated.goldFound,
			RecallAt5:           evaluated.recall,
			StaleLeakage:        evaluated.staleLeakage,
			RelationAssisted:    evaluated.relationAssisted,
			AmbientItems:        ambient,
			ContextCoverage:     evaluated.coverage,
			LatencyMS:           latency,
			ExpectStaleExcluded: q.ExpectStaleExcluded,
		}
		if !q.Unanswerable {
			recallSum += res.RecallAt5
			recallQuestions++
		}
		report.PerQuestion = append(report.PerQuestion, res)

		totalItems += res.Hits
		totalStale += evaluated.staleItems
		report.Metrics.InvalidEvidenceRefs += evaluated.invalidRefs
		report.Metrics.ClassicInvalidEvidenceRefs += evaluated.invalidRefs
		report.Metrics.RelationAssisted += res.RelationAssisted
		if res.ContextCoverage != nil {
			coverageSum += *res.ContextCoverage
			coverageQuestions++
		}
		latencyTotal += latency
		if latency > latencyMax {
			latencyMax = latency
		}
	}
	if recallQuestions > 0 {
		report.Metrics.EvidenceRecallAt5 = recallSum / float64(recallQuestions)
	}
	if totalItems > 0 {
		report.Metrics.StaleLeakage = float64(totalStale) / float64(totalItems)
	}
	if coverageQuestions > 0 {
		report.Metrics.ContextCoverage = coverageSum / float64(coverageQuestions)
	}
	report.Metrics.LatencyMSTotal = latencyTotal
	report.Metrics.LatencyMSMax = latencyMax
	if len(questions) > 0 {
		report.Metrics.LatencyMSAvg = float64(latencyTotal) / float64(len(questions))
	}

	// Retrieval pass: every retrieval question runs twice, once lexical-only
	// and once hybrid, so the semantic branch is measured against the same
	// deterministic lexical baseline.
	lexicalRecallSum, hybridRecallSum, assistedSum := 0.0, 0.0, 0.0
	lexicalRecallQuestions, hybridRecallQuestions, assistedQuestions := 0, 0, 0
	top1Hits, top1Questions := 0, 0
	var lexicalLatencyTotal, lexicalLatencyMax float64
	var hybridLatencyTotal, hybridLatencyMax float64
	lexicalLatencyQuestions, hybridLatencyQuestions := 0, 0
	startMemo := map[string]*time.Time{}
	for _, q := range retrievalQuestions {
		modes := []struct {
			mode     string
			semantic semantic.SemanticRetriever
		}{{contextbuild.ModeLexical, retriever}}
		if !opts.LexicalOnly {
			modes = append(modes, struct {
				mode     string
				semantic semantic.SemanticRetriever
			}{"hybrid", retriever})
		}
		asQuestion := Question{
			ID:                  q.ID,
			Query:               q.Query,
			Purpose:             q.Purpose,
			Notes:               q.Notes,
			GoldEvidenceTurnIDs: q.GoldEvidenceTurnIDs,
			GoldTopicIDs:        q.GoldTopicIDs,
			Unanswerable:        q.Unanswerable,
			GoldTop1TurnID:      q.GoldTop1TurnID,
			ExpectStaleExcluded: q.ExpectStaleExcluded,
		}
		var lexicalFound, hybridFound map[string]bool
		for _, m := range modes {
			started := time.Now()
			pkg, err := contextbuild.Build(ctx, contextbuild.Deps{Store: store, DB: db, Semantic: m.semantic}, contextbuild.Request{
				Purpose:        q.Purpose,
				Query:          q.Query,
				ConversationID: q.ScopeConversationID,
				TopicIDs:       topicScope(q.ScopeTopicID),
				StartedAfter:   q.ScopeStartedAfter,
				StartedBefore:  q.ScopeStartedBefore,
				MaxItems:       maxItems,
				MaxBytes:       maxBytes,
				Mode:           m.mode,
			})
			latency := float64(time.Since(started)) / float64(time.Millisecond)
			if err != nil {
				return Report{}, fmt.Errorf("retrieval question %q (%s): build: %w", q.ID, m.mode, err)
			}
			evaluated := evaluateQuestion(asQuestion, pkg, staleTurns, knownTurns, noteEvidence)
			report.Metrics.InvalidEvidenceRefs += evaluated.invalidRefs
			report.Metrics.RetrievalInvalidEvidenceRefs += evaluated.invalidRefs
			switch m.mode {
			case contextbuild.ModeLexical:
				report.Metrics.RetrievalLexicalInvalidEvidenceRefs += evaluated.invalidRefs
			default:
				report.Metrics.RetrievalHybridInvalidEvidenceRefs += evaluated.invalidRefs
			}
			ambient, returned := trackAmbient(pkg)
			ambientHitsTotal += ambient
			ambientReturnedTotal += returned
			escapes, err := countScopeEscapes(ctx, store, q, pkg, startMemo)
			if err != nil {
				return Report{}, err
			}
			report.Metrics.ScopeEscapeCount += escapes
			top1Hit := (*bool)(nil)
			if !q.Unanswerable {
				top1Hit = firstTurnTop1Hit(q.GoldTop1TurnID, pkg, noteEvidence)
			}
			report.PerQuestion = append(report.PerQuestion, QuestionResult{
				ID:                  q.ID,
				Purpose:             q.Purpose,
				Query:               q.Query,
				Mode:                m.mode,
				Hits:                evaluated.hits,
				Unanswerable:        q.Unanswerable,
				ReturnedItems:       evaluated.hits,
				TurnTop1Hit:         top1Hit,
				GoldEvidenceCount:   len(q.GoldEvidenceTurnIDs),
				GoldEvidenceFound:   evaluated.goldFound,
				RecallAt5:           evaluated.recall,
				StaleLeakage:        evaluated.staleLeakage,
				RelationAssisted:    evaluated.relationAssisted,
				AmbientItems:        ambient,
				ScopeEscapes:        escapes,
				LatencyMS:           latency,
				ExpectStaleExcluded: q.ExpectStaleExcluded,
			})
			found := foundGoldSet(asQuestion, pkg, noteEvidence)
			switch m.mode {
			case contextbuild.ModeLexical:
				if !q.Unanswerable {
					lexicalRecallSum += evaluated.recall
					lexicalRecallQuestions++
				}
				lexicalLatencyTotal += latency
				lexicalLatencyQuestions++
				if latency > lexicalLatencyMax {
					lexicalLatencyMax = latency
				}
				lexicalFound = found
			default:
				if !q.Unanswerable {
					hybridRecallSum += evaluated.recall
					hybridRecallQuestions++
				}
				hybridLatencyTotal += latency
				hybridLatencyQuestions++
				if latency > hybridLatencyMax {
					hybridLatencyMax = latency
				}
				hybridFound = found
			}
			if !q.Unanswerable && q.GoldTop1TurnID != "" && ((opts.LexicalOnly && m.mode == contextbuild.ModeLexical) || (!opts.LexicalOnly && (m.mode == contextbuild.ModeHybrid || m.mode == "hybrid"))) {
				top1Questions++
				if top1Hit != nil && *top1Hit {
					top1Hits++
				}
			}
		}
		if !q.Unanswerable {
			if n := len(q.GoldEvidenceTurnIDs); n > 0 && hybridFound != nil {
				assisted := 0
				for _, gold := range q.GoldEvidenceTurnIDs {
					if hybridFound[gold] && !lexicalFound[gold] {
						assisted++
					}
				}
				assistedSum += float64(assisted) / float64(n)
				assistedQuestions++
			}
		}
	}
	if lexicalRecallQuestions > 0 {
		report.Metrics.LexicalRecallAt5 = lexicalRecallSum / float64(lexicalRecallQuestions)
	}
	if hybridRecallQuestions > 0 {
		report.Metrics.HybridRecallAt5 = hybridRecallSum / float64(hybridRecallQuestions)
	}
	if assistedQuestions > 0 {
		report.Metrics.SemanticAssistedRecall = assistedSum / float64(assistedQuestions)
	}
	if top1Questions > 0 {
		report.Metrics.TurnTop1Accuracy = float64(top1Hits) / float64(top1Questions)
	}
	if lexicalLatencyQuestions > 0 {
		report.Metrics.RetrievalLexicalLatencyMSTotal = lexicalLatencyTotal
		report.Metrics.RetrievalLexicalLatencyMSMax = lexicalLatencyMax
		report.Metrics.RetrievalLexicalLatencyMSAvg = lexicalLatencyTotal / float64(lexicalLatencyQuestions)
	}
	if hybridLatencyQuestions > 0 {
		report.Metrics.RetrievalHybridLatencyMSTotal = hybridLatencyTotal
		report.Metrics.RetrievalHybridLatencyMSMax = hybridLatencyMax
		report.Metrics.RetrievalHybridLatencyMSAvg = hybridLatencyTotal / float64(hybridLatencyQuestions)
	}
	if ambientReturnedTotal > 0 {
		report.Metrics.AmbientLeakage = float64(ambientHitsTotal) / float64(ambientReturnedTotal)
	}
	unanswerableIDs := map[string]struct{}{}
	unanswerableEvaluations, unanswerableClean := 0, 0
	lexicalUnanswerableEvaluations, lexicalUnanswerableFalsePositives := 0, 0
	hybridUnanswerableEvaluations, hybridUnanswerableFalsePositives := 0, 0
	for _, result := range report.PerQuestion {
		if !result.Unanswerable {
			continue
		}
		unanswerableIDs[result.ID] = struct{}{}
		unanswerableEvaluations++
		if result.ReturnedItems == 0 {
			unanswerableClean++
		}
		report.Metrics.UnanswerableFalsePositiveItems += result.ReturnedItems
		switch result.Mode {
		case contextbuild.ModeLexical:
			lexicalUnanswerableEvaluations++
			if result.ReturnedItems > 0 {
				lexicalUnanswerableFalsePositives++
			}
			report.Metrics.UnanswerableLexicalFalsePositiveItems += result.ReturnedItems
		case contextbuild.ModeHybrid, "hybrid":
			hybridUnanswerableEvaluations++
			if result.ReturnedItems > 0 {
				hybridUnanswerableFalsePositives++
			}
			report.Metrics.UnanswerableHybridFalsePositiveItems += result.ReturnedItems
		}
	}
	report.Metrics.UnanswerableQuestions = len(unanswerableIDs)
	if unanswerableEvaluations > 0 {
		report.Metrics.UnanswerableCleanRate = float64(unanswerableClean) / float64(unanswerableEvaluations)
	}
	if lexicalUnanswerableEvaluations > 0 {
		report.Metrics.UnanswerableLexicalFalsePositiveRate = float64(lexicalUnanswerableFalsePositives) / float64(lexicalUnanswerableEvaluations)
	}
	if hybridUnanswerableEvaluations > 0 {
		report.Metrics.UnanswerableHybridFalsePositiveRate = float64(hybridUnanswerableFalsePositives) / float64(hybridUnanswerableEvaluations)
	}

	// Projection pass: score the read-time projection against the fixture
	// expectations (notes were seeded before the retrieval passes).
	if projectionFixture != nil {
		metrics, err := runProjectionPass(ctx, db, store, projectionFixture)
		if err != nil {
			return Report{}, err
		}
		report.Metrics.Projection = metrics
	}
	return report, nil
}

// topicScope wraps an optional pinned topic id into the request shape.
func topicScope(topicID string) []string {
	if topicID == "" {
		return nil
	}
	return []string{topicID}
}

// foundGoldSet returns the gold evidence ids present in the first five items
// of the package.
func foundGoldSet(q Question, pkg contextbuild.Package, noteEvidence map[string][]string) map[string]bool {
	first := pkg.Items
	if len(first) > 5 {
		first = first[:5]
	}
	found := map[string]bool{}
	for _, item := range first {
		for _, id := range evidenceIDs(item, noteEvidence) {
			found[id] = true
		}
	}
	return found
}

// countScopeEscapes counts returned items and raw evidence references that
// violate the question's explicit scope: the pinned conversation, the pinned
// topic or the time bounds (an unknown recording start fails any set bound).
func countScopeEscapes(ctx context.Context, store *memory.Store, q RetrievalQuestion, pkg contextbuild.Package, memo map[string]*time.Time) (int, error) {
	if q.ScopeConversationID == "" && q.ScopeTopicID == "" && q.ScopeStartedAfter == nil && q.ScopeStartedBefore == nil {
		return 0, nil
	}
	if memo == nil {
		memo = map[string]*time.Time{}
	}
	escapes := 0
	for _, item := range pkg.Items {
		if q.ScopeConversationID != "" && item.ConversationID != q.ScopeConversationID {
			escapes++
			continue
		}
		if q.ScopeTopicID != "" && !containsString(item.TopicIDs, q.ScopeTopicID) {
			escapes++
			continue
		}
		if q.ScopeStartedAfter != nil || q.ScopeStartedBefore != nil {
			started, ok := memo[item.ConversationID]
			if !ok {
				var err error
				started, err = store.ConversationStartedAt(ctx, item.ConversationID)
				if err != nil {
					return 0, fmt.Errorf("retrieval question %q: conversation start: %w", q.ID, err)
				}
				memo[item.ConversationID] = started
			}
			if !memory.WithinStartedRange(started, q.ScopeStartedAfter, q.ScopeStartedBefore) {
				escapes++
			}
		}
	}
	for _, ref := range pkg.Evidence {
		escaped, err := evidenceScopeEscape(ctx, store, q, ref.TurnID, memo)
		if err != nil {
			return 0, err
		}
		if escaped {
			escapes++
		}
	}
	return escapes, nil
}

// evidenceScopeEscape resolves a raw evidence turn back to its source
// conversation and topics before checking the same hard scope as an item.
// An unknown turn is not a scope escape: evaluateQuestion reports it as an
// invalid reference, and one bad id must not make the scope query fail.
func evidenceScopeEscape(ctx context.Context, store *memory.Store, q RetrievalQuestion, turnID string, memo map[string]*time.Time) (bool, error) {
	var conversationID string
	err := store.DB().QueryRowContext(ctx, `SELECT conversation_id FROM turns WHERE id=?`, turnID).Scan(&conversationID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("retrieval question %q: evidence %q: %w", q.ID, turnID, err)
	}
	if q.ScopeConversationID != "" && conversationID != q.ScopeConversationID {
		return true, nil
	}
	if q.ScopeTopicID != "" {
		var inTopic bool
		if err := store.DB().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM turn_topics WHERE turn_id=? AND topic_id=?)`, turnID, q.ScopeTopicID).Scan(&inTopic); err != nil {
			return false, fmt.Errorf("retrieval question %q: evidence %q topic: %w", q.ID, turnID, err)
		}
		if !inTopic {
			return true, nil
		}
	}
	if q.ScopeStartedAfter != nil || q.ScopeStartedBefore != nil {
		started, ok := memo[conversationID]
		if !ok {
			var err error
			started, err = store.ConversationStartedAt(ctx, conversationID)
			if err != nil {
				return false, fmt.Errorf("retrieval question %q: evidence %q conversation start: %w", q.ID, turnID, err)
			}
			memo[conversationID] = started
		}
		if !memory.WithinStartedRange(started, q.ScopeStartedAfter, q.ScopeStartedBefore) {
			return true, nil
		}
	}
	return false, nil
}

// isAmbientItem applies the store's ambient rule to one returned item: a turn
// classified ambient, or a note whose primary evidence turn is.
func isAmbientItem(item memory.Item, noteEvidence map[string][]string, ambientTurns, ambientNotes map[string]bool) bool {
	if item.Kind == "turn" {
		return ambientTurns[item.ID]
	}
	if item.Kind == "conversation" {
		return false
	}
	if ambientNotes[item.ID] {
		return true
	}
	// A note is ambient when its primary (ordinal 0) evidence turn is; the
	// ambientNoteSet captures that. Fallback for views without evidence rows.
	ev := evidenceIDs(item, noteEvidence)
	return len(ev) > 0 && ambientTurns[ev[0]]
}

// ambientTurnSet returns every turn id classified ambient.
func ambientTurnSet(ctx context.Context, db *sql.DB) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT turn_id FROM turn_classes WHERE class='ambient'`)
	if err != nil {
		return nil, fmt.Errorf("ambient turns: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// ambientNoteSet returns every note whose primary (ordinal 0) evidence turn is
// classified ambient, matching the store's note-ambient rule.
func ambientNoteSet(ctx context.Context, db *sql.DB) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT ne.note_id FROM note_evidence ne
JOIN turn_classes tc ON tc.turn_id=ne.turn_id WHERE ne.ordinal=0 AND tc.class='ambient'`)
	if err != nil {
		return nil, fmt.Errorf("ambient notes: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// ingestCorpus replays every segment snapshot in corpus order.
func ingestCorpus(ctx context.Context, store *memory.Store, corpus Corpus) error {
	for _, conv := range corpus.Conversations {
		for _, seg := range conv.Segments {
			report, err := store.IngestTranscript(ctx, seg)
			if err != nil {
				return fmt.Errorf("ingest %s/%s/%s: %w",
					seg.Source.ConversationID, seg.Source.SegmentID, seg.Source.TranscriptRevision, err)
			}
			if report.ConversationID == "" {
				return fmt.Errorf("ingest %s/%s/%s returned no conversation id",
					seg.Source.ConversationID, seg.Source.SegmentID, seg.Source.TranscriptRevision)
			}
		}
	}
	return nil
}

// drainPipeline claims and executes extract and publish jobs until the queue is
// empty, completing each job so its lease is released. Extract may enqueue a
// publish job, so both stages are drained together.
func drainPipeline(ctx context.Context, db *sql.DB, store *memory.Store) error {
	extract := &pipeline.ExtractHandler{
		DB:               db,
		Engine:           decision.Synthetic{},
		Endpoint:         "synthetic://eval/extract",
		ProcessorVersion: "extract-v1",
		Generator:        generate.Synthetic{},
	}
	publish := &pipeline.PublishHandler{DB: db, Store: store, ProcessorVersion: "extract-v1"}
	const maxJobs = 100000
	for range maxJobs {
		job, err := claimAny(ctx, db, "extract", "publish")
		if err != nil {
			return err
		}
		if job == nil {
			return nil
		}
		var ref string
		switch job.Stage {
		case "extract":
			ref, err = extract.Execute(ctx, *job)
		case "publish":
			ref, err = publish.Execute(ctx, *job)
		default:
			return fmt.Errorf("eval: unexpected stage %q", job.Stage)
		}
		if err != nil {
			return fmt.Errorf("eval: %s job %s: %w", job.Stage, job.ID, err)
		}
		if err := process.Complete(ctx, db, job.ID, job.LeaseToken, ref, time.Now()); err != nil {
			return fmt.Errorf("eval: complete %s job %s: %w", job.Stage, job.ID, err)
		}
	}
	return fmt.Errorf("eval: pipeline drain exceeded %d jobs", maxJobs)
}

// claimAny claims the oldest queued job of any requested stage, returning nil
// when the queue is empty.
func claimAny(ctx context.Context, db *sql.DB, stages ...string) (*process.Job, error) {
	job, err := process.Claim(ctx, db, process.ClaimOptions{
		WorkerID: "context-eval",
		Stages:   stages,
		Lease:    time.Minute,
		Now:      time.Now(),
	})
	if errors.Is(err, process.ErrNoJob) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("eval: claim: %w", err)
	}
	return job, nil
}

// resetDatabase removes any previous database files so every run starts from a
// clean, deterministic snapshot.
func resetDatabase(path string) error {
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		if err := removeIfExists(path + suffix); err != nil {
			return err
		}
	}
	return nil
}
