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
)

// Options configures one harness run.
type Options struct {
	DBPath        string
	CorpusPath    string
	QuestionsPath string
	LinksPath     string
	MaxItems      int
	MaxBytes      int
}

// Report is the machine-readable evaluation report.
type Report struct {
	CorpusSegments int              `json:"corpus_segments"`
	Conversations  int              `json:"conversations"`
	Questions      int              `json:"questions"`
	Links          []AppliedLink    `json:"links"`
	Metrics        Metrics          `json:"metrics"`
	PerQuestion    []QuestionResult `json:"per_question"`
}

// Metrics are the aggregate evaluation numbers.
type Metrics struct {
	// EvidenceRecallAt5 is the macro-average over questions of the fraction of
	// authored gold evidence turns that appear in the first five items.
	EvidenceRecallAt5 float64 `json:"evidence_recall_at_5"`
	// StaleLeakage is sum(stale items) / sum(items) across every question. An
	// item counts as stale when the builder marks it stale or when all of its
	// evidence turns come from a superseded transcript revision.
	StaleLeakage float64 `json:"stale_leakage"`
	// InvalidEvidenceRefs counts distinct returned evidence ids that do not
	// resolve to any row in the turns table.
	InvalidEvidenceRefs int `json:"invalid_evidence_refs"`
	// RelationAssisted counts gold evidence hits whose item is reachable only
	// through a confirmed topic link.
	RelationAssisted int `json:"relation_assisted"`
	// ContextCoverage is the macro-average topic coverage over questions that
	// author gold topics.
	ContextCoverage float64 `json:"context_coverage"`
	LatencyMSAvg    float64 `json:"latency_ms_avg"`
	LatencyMSMax    int64   `json:"latency_ms_max"`
	LatencyMSTotal  int64   `json:"latency_ms_total"`
	// ModelCalls is the number of rerank invocations; the harness configures no
	// reranker, so the deterministic assembly runs untouched.
	ModelCalls int `json:"model_calls"`
}

// QuestionResult is one question's evaluation.
type QuestionResult struct {
	ID                  string   `json:"id"`
	Purpose             string   `json:"purpose"`
	Query               string   `json:"query"`
	Hits                int      `json:"hits"`
	GoldEvidenceCount   int      `json:"gold_evidence_count"`
	GoldEvidenceFound   int      `json:"gold_evidence_found"`
	RecallAt5           float64  `json:"recall_at_5"`
	StaleLeakage        float64  `json:"stale_leakage"`
	RelationAssisted    int      `json:"relation_assisted"`
	ContextCoverage     *float64 `json:"context_coverage,omitempty"`
	LatencyMS           int64    `json:"latency_ms"`
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

	report.PerQuestion = make([]QuestionResult, 0, len(questions))
	totalItems, totalStale := 0, 0
	recallSum := 0.0
	coverageSum, coverageQuestions := 0.0, 0
	var latencyTotal, latencyMax int64
	for _, q := range questions {
		started := time.Now()
		pkg, err := contextbuild.Build(ctx, store, db, nil, contextbuild.Request{
			Purpose:            q.Purpose,
			Query:              q.Query,
			MaxItems:           maxItems,
			MaxBytes:           maxBytes,
			IncludeRawEvidence: true,
		})
		latency := time.Since(started).Milliseconds()
		if err != nil {
			return Report{}, fmt.Errorf("question %q: build: %w", q.ID, err)
		}
		evaluated := evaluateQuestion(q, pkg, staleTurns, knownTurns, noteEvidence)
		res := QuestionResult{
			ID:                  q.ID,
			Purpose:             q.Purpose,
			Query:               q.Query,
			Hits:                evaluated.hits,
			GoldEvidenceCount:   len(q.GoldEvidenceTurnIDs),
			GoldEvidenceFound:   evaluated.goldFound,
			RecallAt5:           evaluated.recall,
			StaleLeakage:        evaluated.staleLeakage,
			RelationAssisted:    evaluated.relationAssisted,
			ContextCoverage:     evaluated.coverage,
			LatencyMS:           latency,
			ExpectStaleExcluded: q.ExpectStaleExcluded,
		}
		report.PerQuestion = append(report.PerQuestion, res)

		recallSum += res.RecallAt5
		totalItems += res.Hits
		totalStale += int(res.StaleLeakage * float64(res.Hits))
		report.Metrics.InvalidEvidenceRefs += evaluated.invalidRefs
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
	if n := len(questions); n > 0 {
		report.Metrics.EvidenceRecallAt5 = recallSum / float64(n)
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
	return report, nil
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
