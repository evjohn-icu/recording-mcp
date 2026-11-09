// Command context-eval runs the deterministic synthetic evaluation harness for
// the context-building pipeline. It ingests fixtures/eval/corpus.json, drives
// extract and publish with the synthetic engines (no network, no model
// weights), applies the curated links, evaluates fixtures/eval/questions.json
// against contextbuild.Build and writes a JSON report.
//
// The database file is recreated on every run so the result is deterministic.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"recordingmcp/internal/contextbuild"
	"recordingmcp/internal/decision"
	"recordingmcp/internal/eval"
	"recordingmcp/internal/semantic"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "context-eval:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		dbPath                = flag.String("db", "", "SQLite database path (default: a fresh temp file). The file is removed and recreated on every run.")
		outPath               = flag.String("out", "", "JSON report path (default: stdout)")
		corpusPath            = flag.String("corpus", filepath.Join("fixtures", "eval", "corpus.json"), "corpus fixture path")
		questionsPath         = flag.String("questions", filepath.Join("fixtures", "eval", "questions.json"), "questions fixture path")
		linksPath             = flag.String("links", filepath.Join("fixtures", "eval", "links.json"), "curated links fixture path")
		retrPath              = flag.String("retrieval", filepath.Join("fixtures", "eval", "retrieval.json"), "retrieval questions fixture path (empty disables the hybrid/lexical retrieval pass)")
		projectionKey         = flag.String("projection", filepath.Join("fixtures", "eval", "projection.json"), "projection fixture path (empty disables the projection pass)")
		reportMD              = flag.String("report-md", "", "write a human-readable markdown summary to this path")
		maxItems              = flag.Int("max-items", 8, "contextbuild request MaxItems")
		maxBytes              = flag.Int("max-bytes", 16000, "contextbuild request MaxBytes")
		embedProvider         = flag.String("embed-provider", "hash", "embedding provider: hash|off|openai")
		embedDims             = flag.Int("embed-dims", semantic.DefaultHashDims, "embedding dimensions")
		embedEndpoint         = flag.String("embed-endpoint", "", "OpenAI-compatible embedding endpoint")
		embedModel            = flag.String("embed-model", "", "embedding model")
		embedKeyEnv           = flag.String("embed-key-env", "", "environment variable containing the embedding API key")
		embedInputType        = flag.Bool("embed-input-type", false, "send the non-standard input_type field (needed by the bundled Qwen embedding server)")
		retrievalVersion      = flag.String("retrieval-version", "v1", "retrieval implementation: v1|v2")
		rerankCandidateLimit  = flag.Int("rerank-candidate-limit", 64, "v2 candidate pool size before final budget (1..128)")
		rerankDecisionURL     = flag.String("rerank-decision-url", "", "optional Decision service endpoint for explicitly enabled reranking")
		rerankDecisionModel   = flag.String("rerank-decision-model", "", "Decision model name for reranking")
		assessorDecisionURL   = flag.String("assessor-decision-url", "", "optional Decision service endpoint for evidence sufficiency assessment")
		assessorDecisionModel = flag.String("assessor-decision-model", "", "Decision model name for evidence assessment")
		minSimilarity         = flag.Float64("min-similarity", -1, "optional semantic admission threshold; omit for default behavior")
		lexicalOnly           = flag.Bool("lexical-only", false, "run only the lexical retrieval pass")
		timeout               = flag.Duration("timeout", 5*time.Minute, "total evaluation timeout")
	)
	flag.Parse()

	db := *dbPath
	cleanup := func() {}
	if db == "" {
		dir, err := os.MkdirTemp("", "context-eval-*")
		if err != nil {
			return err
		}
		db = filepath.Join(dir, "eval.db")
		cleanup = func() { os.RemoveAll(dir) }
	} else if dir := filepath.Dir(db); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	defer cleanup()

	provider, err := semantic.ProviderFromConfig(semantic.Config{
		Provider:       *embedProvider,
		Dims:           *embedDims,
		Endpoint:       *embedEndpoint,
		Model:          *embedModel,
		KeyEnv:         *embedKeyEnv,
		InputTypeField: *embedInputType,
	})
	if err != nil {
		return err
	}
	if *minSimilarity > 1 {
		return fmt.Errorf("min-similarity must be in [0,1]")
	}
	if *retrievalVersion != "v1" && *retrievalVersion != "v2" {
		return fmt.Errorf("retrieval-version must be v1 or v2")
	}
	if *rerankCandidateLimit < 1 || *rerankCandidateLimit > 128 {
		return fmt.Errorf("rerank-candidate-limit must be in [1,128]")
	}
	if (*rerankDecisionURL == "") != (*rerankDecisionModel == "") {
		return fmt.Errorf("rerank-decision-url and rerank-decision-model must be configured together")
	}
	if (*assessorDecisionURL == "") != (*assessorDecisionModel == "") {
		return fmt.Errorf("assessor-decision-url and assessor-decision-model must be configured together")
	}
	var reranker contextbuild.Reranker
	if *rerankDecisionURL != "" {
		engine := &decision.SystemOneClient{Endpoint: strings.TrimSpace(*rerankDecisionURL), Model: strings.TrimSpace(*rerankDecisionModel)}
		reranker = &contextbuild.DecisionReranker{Engine: engine}
	}
	var assessor contextbuild.EvidenceAssessor
	if *assessorDecisionURL != "" {
		engine := &decision.SystemOneClient{Endpoint: strings.TrimSpace(*assessorDecisionURL), Model: strings.TrimSpace(*assessorDecisionModel)}
		assessor = contextbuild.DecisionEvidenceAssessor{Engine: engine}
	}
	threshold := (*float64)(nil)
	if *minSimilarity >= 0 {
		threshold = minSimilarity
	}
	if *timeout <= 0 {
		return fmt.Errorf("timeout must be positive")
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	report, err := eval.Run(ctx, eval.Options{
		DBPath:                     db,
		CorpusPath:                 *corpusPath,
		QuestionsPath:              *questionsPath,
		LinksPath:                  *linksPath,
		RetrievalPath:              *retrPath,
		ProjectionPath:             *projectionKey,
		MaxItems:                   *maxItems,
		MaxBytes:                   *maxBytes,
		SemanticProvider:           provider,
		SemanticProviderConfigured: true,
		MinimumSimilarity:          threshold,
		RetrievalVersion:           *retrievalVersion,
		RerankCandidateLimit:       *rerankCandidateLimit,
		Reranker:                   reranker,
		Assessor:                   assessor,
		LexicalOnly:                *lexicalOnly,
	})
	if err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	if *outPath == "" {
		_, err = os.Stdout.Write(encoded)
		if err != nil {
			return err
		}
	} else if err := os.WriteFile(*outPath, encoded, 0o644); err != nil {
		return err
	}
	if *reportMD != "" {
		return os.WriteFile(*reportMD, []byte(markdownReport(report)), 0o644)
	}
	return nil
}

// markdownReport renders the report as a compact human-readable summary.
func markdownReport(report eval.Report) string {
	m := report.Metrics
	var b strings.Builder
	fmt.Fprintf(&b, "# context-eval report\n\n")
	fmt.Fprintf(&b, "- corpus: %d segments / %d conversations\n", report.CorpusSegments, report.Conversations)
	fmt.Fprintf(&b, "- questions: %d classic, %d retrieval\n\n", report.Questions, report.RetrievalQuestions)
	fmt.Fprintf(&b, "| metric | value |\n|---|---|\n")
	fmt.Fprintf(&b, "| evidence_recall_at_5 | %.4f |\n", m.EvidenceRecallAt5)
	fmt.Fprintf(&b, "| lexical_recall_at_5 | %.4f |\n", m.LexicalRecallAt5)
	fmt.Fprintf(&b, "| hybrid_recall_at_5 | %.4f |\n", m.HybridRecallAt5)
	fmt.Fprintf(&b, "| retrieval_hybrid_mrr_at_10 | %.4f |\n", m.RetrievalHybridMRRAt10)
	fmt.Fprintf(&b, "| retrieval_lexical_mrr_at_10 | %.4f |\n", m.RetrievalLexicalMRRAt10)
	fmt.Fprintf(&b, "| retrieval_hybrid_precision_at_5 | %.4f |\n", m.RetrievalHybridPrecisionAt5)
	fmt.Fprintf(&b, "| retrieval_lexical_precision_at_5 | %.4f |\n", m.RetrievalLexicalPrecisionAt5)
	fmt.Fprintf(&b, "| retrieval_requests | %d |\n", m.RetrievalRequests)
	fmt.Fprintf(&b, "| semantic_query_embed_requests | %d |\n", m.SemanticQueryEmbedRequests)
	fmt.Fprintf(&b, "| semantic_index_embed_requests | %d |\n", m.SemanticIndexEmbedRequests)
	fmt.Fprintf(&b, "| semantic_index_embedded_items | %d |\n", m.SemanticIndexEmbeddedItems)
	fmt.Fprintf(&b, "| semantic_provider_embed_requests | %d |\n", m.SemanticProviderEmbedRequests)
	fmt.Fprintf(&b, "| semantic_provider_embed_items | %d |\n", m.SemanticProviderEmbedItems)
	fmt.Fprintf(&b, "| semantic_provider_embed_errors | %d |\n", m.SemanticProviderEmbedErrors)
	fmt.Fprintf(&b, "| semantic_index_duration_ms | %.3f |\n", m.SemanticIndexDurationMS)
	fmt.Fprintf(&b, "| retrieval_lexical_latency_ms_p50 | %.3f |\n", m.RetrievalLexicalLatencyMSP50)
	fmt.Fprintf(&b, "| retrieval_lexical_latency_ms_p95 | %.3f |\n", m.RetrievalLexicalLatencyMSP95)
	fmt.Fprintf(&b, "| retrieval_hybrid_latency_ms_p50 | %.3f |\n", m.RetrievalHybridLatencyMSP50)
	fmt.Fprintf(&b, "| retrieval_hybrid_latency_ms_p95 | %.3f |\n", m.RetrievalHybridLatencyMSP95)
	fmt.Fprintf(&b, "| semantic_assisted_recall | %.4f |\n", m.SemanticAssistedRecall)
	fmt.Fprintf(&b, "| unanswerable_questions | %d |\n", m.UnanswerableQuestions)
	fmt.Fprintf(&b, "| unanswerable_clean_rate | %.4f |\n", m.UnanswerableCleanRate)
	fmt.Fprintf(&b, "| unanswerable_false_positive_items | %d |\n", m.UnanswerableFalsePositiveItems)
	fmt.Fprintf(&b, "| unanswerable_lexical_false_positive_items | %d |\n", m.UnanswerableLexicalFalsePositiveItems)
	fmt.Fprintf(&b, "| unanswerable_hybrid_false_positive_items | %d |\n", m.UnanswerableHybridFalsePositiveItems)
	fmt.Fprintf(&b, "| turn_top1_accuracy | %.4f |\n", m.TurnTop1Accuracy)
	fmt.Fprintf(&b, "| stale_leakage | %.4f |\n", m.StaleLeakage)
	fmt.Fprintf(&b, "| ambient_leakage | %.4f |\n", m.AmbientLeakage)
	fmt.Fprintf(&b, "| scope_escape_count | %d |\n", m.ScopeEscapeCount)
	fmt.Fprintf(&b, "| invalid_evidence_refs | %d |\n", m.InvalidEvidenceRefs)
	fmt.Fprintf(&b, "| relation_assisted | %d |\n", m.RelationAssisted)
	fmt.Fprintf(&b, "| context_coverage | %.4f |\n", m.ContextCoverage)
	fmt.Fprintf(&b, "| model_calls | %d |\n", m.ModelCalls)
	if len(m.RetrievalBuckets) > 0 {
		keys := make([]string, 0, len(m.RetrievalBuckets))
		for key := range m.RetrievalBuckets {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		fmt.Fprintf(&b, "\n## retrieval buckets\n\n")
		fmt.Fprintf(&b, "| split / mode / bucket | questions | answerable | no-evidence | precision@5 | recall@5 | MRR@10 | false-positive rate | p50 ms | p95 ms |\n|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|\n")
		for _, key := range keys {
			bucket := m.RetrievalBuckets[key]
			fmt.Fprintf(&b, "| %s | %d | %d | %d | %.4f | %.4f | %.4f | %.4f | %.3f | %.3f |\n", key, bucket.Questions, bucket.AnswerableQuestions, bucket.NoEvidenceQuestions, bucket.PrecisionAt5, bucket.RecallAt5, bucket.MRRAt10, bucket.FalsePositiveRate, bucket.LatencyMSP50, bucket.LatencyMSP95)
		}
	}
	fmt.Fprintf(&b, "\n## unanswerable\n\n")
	fmt.Fprintf(&b, "| question | mode | returned_items | turn_top1_hit |\n|---|---|---:|---|\n")
	for _, result := range report.PerQuestion {
		if !result.Unanswerable {
			continue
		}
		top1 := "n/a"
		if result.TurnTop1Hit != nil {
			top1 = fmt.Sprintf("%t", *result.TurnTop1Hit)
		}
		fmt.Fprintf(&b, "| %s | %s | %d | %s |\n", result.ID, result.Mode, result.ReturnedItems, top1)
	}
	fmt.Fprintf(&b, "\n### turn top1\n\n")
	fmt.Fprintf(&b, "| question | mode | turn_top1_hit |\n|---|---|---|\n")
	for _, result := range report.PerQuestion {
		if result.TurnTop1Hit == nil {
			continue
		}
		fmt.Fprintf(&b, "| %s | %s | %t |\n", result.ID, result.Mode, *result.TurnTop1Hit)
	}
	p := m.Projection
	fmt.Fprintf(&b, "\n## projection\n\n")
	fmt.Fprintf(&b, "| metric | value |\n|---|---|\n")
	fmt.Fprintf(&b, "| ran | %t |\n", p.Ran)
	fmt.Fprintf(&b, "| evidence_valid | %.4f |\n", p.EvidenceValid)
	fmt.Fprintf(&b, "| stale_not_current | %.4f |\n", p.StaleNotCurrent)
	fmt.Fprintf(&b, "| superseded_not_current | %.4f |\n", p.SupersededNotCurrent)
	fmt.Fprintf(&b, "| conflicting_honest | %.4f |\n", p.ConflictingHonest)
	fmt.Fprintf(&b, "| deterministic | %t |\n", p.Deterministic)
	fmt.Fprintf(&b, "| objects | %d |\n", p.Objects)
	fmt.Fprintf(&b, "| conflicts | %d |\n", p.Conflicts)
	fmt.Fprintf(&b, "| unprojected | %d |\n", p.Unprojected)
	return b.String()
}
