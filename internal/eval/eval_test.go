package eval

import (
	"path/filepath"
	"testing"
)

// TestRunOnShippedFixtures is the regression gate for the round: hybrid
// retrieval must not regress the lexical baseline, the semantic branch must
// add recall, ambient/stale/scope invariants must hold, and the read-time
// projection must be deterministic and honest about conflicts.
func TestRunOnShippedFixtures(t *testing.T) {
	report, err := Run(t.Context(), Options{
		DBPath:         filepath.Join(t.TempDir(), "eval.db"),
		CorpusPath:     filepath.Join("..", "..", "fixtures", "eval", "corpus.json"),
		QuestionsPath:  filepath.Join("..", "..", "fixtures", "eval", "questions.json"),
		LinksPath:      filepath.Join("..", "..", "fixtures", "eval", "links.json"),
		RetrievalPath:  filepath.Join("..", "..", "fixtures", "eval", "retrieval.json"),
		ProjectionPath: filepath.Join("..", "..", "fixtures", "eval", "projection.json"),
		MaxItems:       8,
		MaxBytes:       16000,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	m := report.Metrics
	if m.StaleLeakage != 0 {
		t.Errorf("stale_leakage = %f, want 0", m.StaleLeakage)
	}
	if m.AmbientLeakage != 0 {
		t.Errorf("ambient_leakage = %f, want 0", m.AmbientLeakage)
	}
	if m.ScopeEscapeCount != 0 {
		t.Errorf("scope_escape_count = %d, want 0", m.ScopeEscapeCount)
	}
	if m.InvalidEvidenceRefs != 0 {
		t.Errorf("invalid_evidence_refs = %d, want 0", m.InvalidEvidenceRefs)
	}
	if m.HybridRecallAt5 < m.LexicalRecallAt5 {
		t.Errorf("hybrid_recall_at_5 = %f regressed below lexical %f", m.HybridRecallAt5, m.LexicalRecallAt5)
	}
	if m.SemanticAssistedRecall <= 0 {
		t.Errorf("semantic_assisted_recall = %f, want > 0", m.SemanticAssistedRecall)
	}
	if m.EvidenceRecallAt5 <= 0 {
		t.Errorf("evidence_recall_at_5 = %f, want > 0", m.EvidenceRecallAt5)
	}

	p := m.Projection
	if !p.Deterministic {
		t.Error("projection.deterministic = false, want true")
	}
	if p.StaleNotCurrent != 1 {
		t.Errorf("projection.stale_not_current = %f, want 1", p.StaleNotCurrent)
	}
	if p.ConflictingHonest != 1 {
		t.Errorf("projection.conflicting_honest = %f, want 1", p.ConflictingHonest)
	}
	if p.EvidenceValid != 1 {
		t.Errorf("projection.evidence_valid = %f, want 1", p.EvidenceValid)
	}
	if p.Objects == 0 {
		t.Errorf("projection produced objects=%d, want > 0", p.Objects)
	}
	// Conflicts are legitimately empty since commitments project as proposed
	// (unconfirmed): the supersede-cue rule only applies to current commitments,
	// and no fixture or rule emits one. The pair invariant is still enforced as
	// a hard error inside the harness whenever a conflict does appear.
	if p.Conflicts != 0 {
		t.Errorf("projection produced conflicts=%d, want 0: no source yields a current commitment", p.Conflicts)
	}
	if report.RetrievalQuestions == 0 {
		t.Error("retrieval pass did not run")
	}
}

// TestRunLexicalOnlySkipsHybrid proves the LexicalOnly option really disables
// the semantic pass: no index is built and no hybrid metrics are produced.
func TestRunLexicalOnlySkipsHybrid(t *testing.T) {
	report, err := Run(t.Context(), Options{
		DBPath:         filepath.Join(t.TempDir(), "eval-lex.db"),
		CorpusPath:     filepath.Join("..", "..", "fixtures", "eval", "corpus.json"),
		QuestionsPath:  filepath.Join("..", "..", "fixtures", "eval", "questions.json"),
		LinksPath:      filepath.Join("..", "..", "fixtures", "eval", "links.json"),
		RetrievalPath:  filepath.Join("..", "..", "fixtures", "eval", "retrieval.json"),
		ProjectionPath: "",
		LexicalOnly:    true,
		MaxItems:       8,
		MaxBytes:       16000,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Metrics.HybridRecallAt5 != 0 || report.Metrics.SemanticAssistedRecall != 0 {
		t.Errorf("lexical-only run produced hybrid metrics: %+v", report.Metrics)
	}
	if report.RetrievalQuestions == 0 {
		t.Error("retrieval questions not loaded")
	}
}
