package eval

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"recordingmcp/internal/contextbuild"
	"recordingmcp/internal/decision"
)

// recordingEngine wraps a decision.Engine and counts every Evaluate call so a
// test can observe the adapter requests the harness issues.
type recordingEngine struct {
	inner decision.Engine
	calls int
}

func (e *recordingEngine) Evaluate(ctx context.Context, state string, questions []decision.Question) (decision.Result, error) {
	e.calls++
	return e.inner.Evaluate(ctx, state, questions)
}

// echoAssessor answers with the configured status while citing exactly the
// evidence it received, which is always a valid final-package assessment.
type echoAssessor struct{ status string }

func (a echoAssessor) Assess(_ context.Context, _ string, evidence []contextbuild.EvidenceRef) (contextbuild.EvidenceAssessment, error) {
	refs := make([]string, 0, len(evidence))
	for _, ref := range evidence {
		refs = append(refs, ref.TurnID)
	}
	return contextbuild.EvidenceAssessment{Status: a.status, EvidenceRefs: refs}, nil
}

// hashAdmissionPolicy builds a calibration-phase v3 policy that matches the
// offline hash provider shipped with the harness.
func hashAdmissionPolicy() *contextbuild.AdmissionPolicy {
	return &contextbuild.AdmissionPolicy{
		SchemaVersion:    contextbuild.AdmissionPolicySchema,
		RetrievalVersion: "v3",
		ProviderID:       "hash-ngram-v1",
		ModelID:          "hash-ngram-v1",
		ModelRevision:    "hash-rev-1",
		AssessorModel:    "synthetic",
		FixtureSHA256:    strings.Repeat("a", 64),
		Threshold:        0,
		Phase:            "calibration",
	}
}

func writeRegressionsRetrievalFixture(t *testing.T, dir string, questions string) string {
	t.Helper()
	path := filepath.Join(dir, "retrieval.json")
	if err := os.WriteFile(path, []byte(`{"questions":[`+questions+`]}`), 0o600); err != nil {
		t.Fatalf("write retrieval fixture: %v", err)
	}
	return path
}

// TestFalseSufficientCountsDroppedRefUnderAuthoredSufficientLabel pins 7.1's
// counterexample: a question authored "sufficient" whose final package dropped
// one required ref, with the assessor still claiming sufficient, must count as
// false-sufficient. The package (not the authored label) decides.
func TestFalseSufficientCountsDroppedRefUnderAuthoredSufficientLabel(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	// The pinned conversation has real content; the gold turns live in another
	// conversation, so the final evidence can never contain them.
	const fixture = `{"id":"q-drop","query":"预算批准了吗？","purpose":"synonym","split":"legacy","bucket":"synonym","unanswerable":false,"scope":{"conversation_id":"conv-venue-rehearsal"},"gold_turn_refs":[{"segment_id":"seg-budget","transcript_revision":"rev-2","turn_id":"utt-1"},{"segment_id":"seg-budget","transcript_revision":"rev-1","turn_id":"utt-1"}],"expected_sufficiency":"sufficient","evidence_completeness":"complete"}`
	retrieval := writeRegressionsRetrievalFixture(t, dir, fixture)
	report, err := Run(ctx, Options{
		DBPath:           filepath.Join(dir, "eval.db"),
		CorpusPath:       filepath.Join("..", "..", "fixtures", "eval", "corpus.json"),
		QuestionsPath:    filepath.Join("..", "..", "fixtures", "eval", "questions.json"),
		LinksPath:        filepath.Join("..", "..", "fixtures", "eval", "links.json"),
		RetrievalPath:    retrieval,
		RetrievalVersion: "v2",
		Assessor:         echoAssessor{status: "sufficient"},
		MaxItems:         8,
		MaxBytes:         16000,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	row, ok := hybridRow(report, "q-drop")
	if !ok {
		t.Fatalf("missing hybrid row for q-drop: %+v", report.PerQuestion)
	}
	if row.AssessorCalls == 0 {
		t.Fatalf("the assessor never scored the package: %+v", row)
	}
	if row.FinalGoldEvidenceFound == row.GoldEvidenceCount {
		t.Fatalf("fixture drifted: gold evidence is visible in the final package: %+v", row)
	}
	m := report.Metrics
	// Both retrieval modes score the package, so the counterexample is
	// assessed twice and counted twice.
	if m.EvidenceAssessmentFalseSufficient != 2 {
		t.Errorf("false_sufficient = %d, want 2 (assessor said sufficient over dropped evidence)", m.EvidenceAssessmentFalseSufficient)
	}
	if m.EvidenceAssessmentMatches != 0 {
		t.Errorf("matches = %d, want 0: the recomputed expectation is insufficient", m.EvidenceAssessmentMatches)
	}
	if m.EvidenceAssessmentIncompletePackages != 2 {
		t.Errorf("incomplete_packages = %d, want 2", m.EvidenceAssessmentIncompletePackages)
	}
	if m.EvidenceAssessmentFalseSufficientRate == nil || *m.EvidenceAssessmentFalseSufficientRate != 1 {
		t.Errorf("false_sufficient_rate = %v, want 1", m.EvidenceAssessmentFalseSufficientRate)
	}
}

// TestAuthoredUnknownExpectationIsNotCountedAsAMatch pins the v2 legacy rule on
// the Go side: an explicit "unknown" expectation is never a sufficiency match,
// even when the assessor also reports unknown.
func TestAuthoredUnknownExpectationIsNotCountedAsAMatch(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	const fixture = `{"id":"q-legacy-unknown","query":"下一季度招多少人？","purpose":"synonym","split":"legacy","bucket":"synonym","unanswerable":true,"expected_sufficiency":"unknown"}`
	retrieval := writeRegressionsRetrievalFixture(t, dir, fixture)
	report, err := Run(ctx, Options{
		DBPath:           filepath.Join(dir, "eval.db"),
		CorpusPath:       filepath.Join("..", "..", "fixtures", "eval", "corpus.json"),
		QuestionsPath:    filepath.Join("..", "..", "fixtures", "eval", "questions.json"),
		LinksPath:        filepath.Join("..", "..", "fixtures", "eval", "links.json"),
		RetrievalPath:    retrieval,
		RetrievalVersion: "v2",
		Assessor:         echoAssessor{status: "unknown"},
		MaxItems:         8,
		MaxBytes:         16000,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	m := report.Metrics
	if m.EvidenceAssessmentUnknown != 2 {
		t.Errorf("unknown count = %d, want 2 (one assessment per mode)", m.EvidenceAssessmentUnknown)
	}
	if m.EvidenceAssessmentMatches != 0 {
		t.Errorf("matches = %d, want 0: an authored unknown is not a sufficiency match", m.EvidenceAssessmentMatches)
	}
	if m.EvidenceAssessmentFalseSufficient != 0 {
		t.Errorf("false_sufficient = %d, want 0", m.EvidenceAssessmentFalseSufficient)
	}
}

// TestV1AndV3ComputeRecallOverTheSameEligibleSet pins 7.1's denominator rule on
// the Go side: the lifecycle forget applies to both versions, so the
// recall-eligible question set — and therefore the recall/MRR denominators —
// must be identical for v1 and v3.
func TestV1AndV3ComputeRecallOverTheSameEligibleSet(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	lifecycle := filepath.Join(dir, "lifecycle.json")
	const lc = `{"schema":"retrieval-lifecycle-v1","sources":[{"segment_id":"seg-relocation","transcript_revision":"rev-1","turn_id":"utt-1","reason":"test forget"}]}`
	if err := os.WriteFile(lifecycle, []byte(lc), 0o600); err != nil {
		t.Fatalf("write lifecycle: %v", err)
	}
	const questions = `
{"id":"q-normal","query":"预算批准了吗？","purpose":"synonym","split":"legacy","bucket":"synonym","unanswerable":false,"gold_turn_refs":[{"segment_id":"seg-budget","transcript_revision":"rev-2","turn_id":"utt-1"}],"expected_sufficiency":"sufficient","evidence_completeness":"complete"},
{"id":"q-forgotten","query":"我们打算搬去哪里定居？","purpose":"revocation","split":"legacy","bucket":"revocation","unanswerable":false,"gold_turn_refs":[{"segment_id":"seg-relocation","transcript_revision":"rev-1","turn_id":"utt-1"}],"expected_sufficiency":"insufficient","evidence_completeness":"partial","expect_forgotten_excluded":true},
{"id":"q-flag-live","query":"预算改动了吗？","purpose":"revocation","split":"legacy","bucket":"revocation","unanswerable":false,"gold_turn_refs":[{"segment_id":"seg-budget","transcript_revision":"rev-2","turn_id":"utt-1"}],"expected_sufficiency":"sufficient","evidence_completeness":"complete","expect_forgotten_excluded":true}`
	retrieval := writeRegressionsRetrievalFixture(t, dir, questions)
	base := Options{
		DBPath:        filepath.Join(dir, "eval.db"),
		CorpusPath:    filepath.Join("..", "..", "fixtures", "eval", "corpus.json"),
		QuestionsPath: filepath.Join("..", "..", "fixtures", "eval", "questions.json"),
		LinksPath:     filepath.Join("..", "..", "fixtures", "eval", "links.json"),
		RetrievalPath: retrieval,
		LifecyclePath: lifecycle,
		MaxItems:      8,
		MaxBytes:      16000,
	}
	v1Report, err := Run(ctx, base)
	if err != nil {
		t.Fatalf("v1 Run: %v", err)
	}
	engine := &recordingEngine{inner: decision.Synthetic{}}
	v3Options := base
	v3Options.RetrievalVersion = "v3"
	v3Options.Assessor = echoAssessor{status: "sufficient"}
	v3Options.CandidateAssessor = contextbuild.DecisionCandidateAssessor{Engine: engine, MaxQuestionsPerBatch: 8}
	v3Options.AdmissionPolicy = hashAdmissionPolicy()
	v3Options.AllowCalibrationPolicy = true
	v3Report, err := Run(ctx, v3Options)
	if err != nil {
		t.Fatalf("v3 Run: %v", err)
	}
	want := map[string]bool{"q-normal": true, "q-forgotten": false, "q-flag-live": true}
	got := func(report Report) map[string]bool {
		out := map[string]bool{}
		for _, row := range report.PerQuestion {
			if row.Mode == "hybrid" {
				out[row.ID] = row.RecallEligible
			}
		}
		return out
	}
	v1Eligible, v3Eligible := got(v1Report), got(v3Report)
	if len(v1Eligible) != len(want) {
		t.Fatalf("v1 eligible rows = %v, want %v", v1Eligible, want)
	}
	for id, wantEligible := range want {
		if v1Eligible[id] != wantEligible || v3Eligible[id] != wantEligible {
			t.Errorf("recall eligibility drifted for %s: v1=%v v3=%v want %v",
				id, v1Eligible[id], v3Eligible[id], wantEligible)
		}
	}
	for _, version := range []struct {
		name   string
		report Report
	}{{"v1", v1Report}, {"v3", v3Report}} {
		m := version.report.Metrics
		if m.ForgottenLeakage != 0 {
			t.Errorf("%s forgotten_leakage = %d, want 0", version.name, m.ForgottenLeakage)
		}
		wantRecall := (hybridRowReport(version.report, "q-normal").RecallAt5 + hybridRowReport(version.report, "q-flag-live").RecallAt5) / 2
		if m.HybridRecallAt5 != wantRecall {
			t.Errorf("%s hybrid_recall_at_5 = %f, want the eligible-only average %f", version.name, m.HybridRecallAt5, wantRecall)
		}
	}
}

func hybridRow(report Report, id string) (QuestionResult, bool) {
	for _, row := range report.PerQuestion {
		if row.Mode == "hybrid" && row.ID == id {
			return row, true
		}
	}
	return QuestionResult{}, false
}

func hybridRowReport(report Report, id string) QuestionResult {
	row, _ := hybridRow(report, id)
	return row
}

// TestAdmissionEngineLatencyCoversEveryHybridRow pins 7.3: the admission
// engine p50/p95 is computed over every ModeHybrid row — including the rows
// that never reached the engine, reported as 0 — while the total call count is
// reported separately from the same rows.
func TestAdmissionEngineLatencyCoversEveryHybridRow(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	var specs []string
	for i := 1; i <= 20; i++ {
		specs = append(specs, fmt.Sprintf(
			`{"id":"q-out-%02d","query":"预算批准了吗？","purpose":"synonym","split":"legacy","bucket":"synonym","unanswerable":false,"scope":{"started_after":"2999-01-01T00:00:00Z"},"gold_turn_refs":[{"segment_id":"seg-budget","transcript_revision":"rev-2","turn_id":"utt-1"}],"expected_sufficiency":"sufficient","evidence_completeness":"complete"}`, i))
	}
	specs = append(specs, `{"id":"q-live","query":"预算批准了吗？","purpose":"synonym","split":"legacy","bucket":"synonym","unanswerable":false,"gold_turn_refs":[{"segment_id":"seg-budget","transcript_revision":"rev-2","turn_id":"utt-1"}],"expected_sufficiency":"sufficient","evidence_completeness":"complete"}`)
	retrieval := writeRegressionsRetrievalFixture(t, dir, strings.Join(specs, ","))
	recording := &recordingEngine{inner: decision.Synthetic{}}
	timing := NewEngineTiming(recording)
	report, err := Run(ctx, Options{
		DBPath:                 filepath.Join(dir, "eval.db"),
		CorpusPath:             filepath.Join("..", "..", "fixtures", "eval", "corpus.json"),
		QuestionsPath:          filepath.Join("..", "..", "fixtures", "eval", "questions.json"),
		LinksPath:              filepath.Join("..", "..", "fixtures", "eval", "links.json"),
		RetrievalPath:          retrieval,
		RetrievalVersion:       "v3",
		Assessor:               contextbuild.DecisionEvidenceAssessor{Engine: timing},
		CandidateAssessor:      contextbuild.DecisionCandidateAssessor{Engine: timing, MaxQuestionsPerBatch: 8},
		AdmissionPolicy:        hashAdmissionPolicy(),
		AllowCalibrationPolicy: true,
		EngineTiming:           timing,
		MaxItems:               8,
		MaxBytes:               16000,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	latencies := make([]float64, 0, 21)
	zeroRows := 0
	for _, row := range report.PerQuestion {
		if row.Mode != "hybrid" {
			continue
		}
		latencies = append(latencies, row.AdmissionEngineLatencyMS)
		if row.AdmissionEngineCalls == 0 {
			zeroRows++
		}
	}
	if len(latencies) != 21 {
		t.Fatalf("hybrid rows = %d, want 21", len(latencies))
	}
	if zeroRows != 20 {
		t.Fatalf("rows with zero engine calls = %d, want 20 (out-of-scope rows must not reach the engine)", zeroRows)
	}
	totalCalls := 0
	for _, row := range report.PerQuestion {
		totalCalls += row.AdmissionEngineCalls
	}
	if totalCalls == 0 {
		t.Fatal("the unscoped question never reached the engine")
	}
	if report.Metrics.AdmissionEngineCalls != totalCalls {
		t.Errorf("admission_engine_calls = %d, want the per-row sum %d", report.Metrics.AdmissionEngineCalls, totalCalls)
	}
	wantP95 := percentile(latencies, .95)
	if report.Metrics.AdmissionEngineLatencyMSP95 != wantP95 {
		t.Errorf("admission_engine_latency_ms_p95 = %f, want %f over all %d hybrid rows",
			report.Metrics.AdmissionEngineLatencyMSP95, wantP95, len(latencies))
	}
	if wantP95 != 0 {
		t.Fatalf("p95 over 20 zero rows and one measured row must be 0, got %f", wantP95)
	}
	if report.Metrics.AdmissionEngineLatencyMSP50 != 0 {
		t.Errorf("admission_engine_latency_ms_p50 = %f, want 0", report.Metrics.AdmissionEngineLatencyMSP50)
	}
}

// TestWarmupFixtureRunsOneTrainQuestionThroughTheAdapter pins the warmup
// contract at the harness level: a single-question train fixture is executed
// end to end, the adapter observes the request, and no other question runs.
func TestWarmupFixtureRunsOneTrainQuestionThroughTheAdapter(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	retrieval := writeRegressionsRetrievalFixture(t, dir, `{"id":"warm-1","query":"预算批准了吗？","purpose":"synonym","split":"train","bucket":"synonym","unanswerable":false,"gold_turn_refs":[{"segment_id":"seg-budget","transcript_revision":"rev-2","turn_id":"utt-1"}],"expected_sufficiency":"sufficient","evidence_completeness":"complete"}`)
	recording := &recordingEngine{inner: decision.Synthetic{}}
	timing := NewEngineTiming(recording)
	report, err := Run(ctx, Options{
		DBPath:                 filepath.Join(dir, "eval.db"),
		CorpusPath:             filepath.Join("..", "..", "fixtures", "eval", "corpus.json"),
		QuestionsPath:          filepath.Join("..", "..", "fixtures", "eval", "questions.json"),
		LinksPath:              filepath.Join("..", "..", "fixtures", "eval", "links.json"),
		RetrievalPath:          retrieval,
		RetrievalVersion:       "v3",
		Assessor:               contextbuild.DecisionEvidenceAssessor{Engine: timing},
		CandidateAssessor:      contextbuild.DecisionCandidateAssessor{Engine: timing, MaxQuestionsPerBatch: 8},
		AdmissionPolicy:        hashAdmissionPolicy(),
		AllowCalibrationPolicy: true,
		EngineTiming:           timing,
		MaxItems:               8,
		MaxBytes:               16000,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.RetrievalQuestions != 1 {
		t.Fatalf("retrieval questions = %d, want the single train question", report.RetrievalQuestions)
	}
	ids := map[string]int{}
	for _, row := range report.PerQuestion {
		if row.Split != "" {
			ids[row.ID]++
		}
	}
	if len(ids) != 1 || ids["warm-1"] != 2 {
		t.Fatalf("warmup fixture ran %v, want only warm-1 in both modes", ids)
	}
	if recording.calls == 0 {
		t.Fatal("the adapter never observed a request")
	}
}
