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
	"strings"

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
		dbPath        = flag.String("db", "", "SQLite database path (default: a fresh temp file). The file is removed and recreated on every run.")
		outPath       = flag.String("out", "", "JSON report path (default: stdout)")
		corpusPath    = flag.String("corpus", filepath.Join("fixtures", "eval", "corpus.json"), "corpus fixture path")
		questionsPath = flag.String("questions", filepath.Join("fixtures", "eval", "questions.json"), "questions fixture path")
		linksPath     = flag.String("links", filepath.Join("fixtures", "eval", "links.json"), "curated links fixture path")
		retrPath      = flag.String("retrieval", filepath.Join("fixtures", "eval", "retrieval.json"), "retrieval questions fixture path (empty disables the hybrid/lexical retrieval pass)")
		projectionKey = flag.String("projection", filepath.Join("fixtures", "eval", "projection.json"), "projection fixture path (empty disables the projection pass)")
		reportMD      = flag.String("report-md", "", "write a human-readable markdown summary to this path")
		maxItems      = flag.Int("max-items", 8, "contextbuild request MaxItems")
		maxBytes      = flag.Int("max-bytes", 16000, "contextbuild request MaxBytes")
		embedProvider = flag.String("embed-provider", "hash", "embedding provider: hash|off|openai")
		embedDims     = flag.Int("embed-dims", semantic.DefaultHashDims, "embedding dimensions")
		embedEndpoint = flag.String("embed-endpoint", "", "OpenAI-compatible embedding endpoint")
		embedModel    = flag.String("embed-model", "", "embedding model")
		embedKeyEnv   = flag.String("embed-key-env", "", "environment variable containing the embedding API key")
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
		Provider: *embedProvider,
		Dims:     *embedDims,
		Endpoint: *embedEndpoint,
		Model:    *embedModel,
		KeyEnv:   *embedKeyEnv,
	})
	if err != nil {
		return err
	}
	report, err := eval.Run(context.Background(), eval.Options{
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
