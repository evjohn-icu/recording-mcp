package contextbuild

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"

	"recordingmcp/internal/decision"
	"recordingmcp/internal/memory"
)

// Evidence admission (retrieval v3).
//
// v3 adds one explicit gate between ranked retrieval and the answer budget: a
// candidate may only become an item when an assessor judges that it provides
// substantive support or refutation for the query and its own cited evidence is
// still current, in scope and complete. The gate is opt-in, pinned by a policy
// and never silently falls back to v1/v2: when the policy or assessor is
// unavailable the package carries unknown sufficiency and no items.

// AdmissionPolicySchema identifies the policy document shape.
const AdmissionPolicySchema = "evidence-admission-policy-v1"

// Fixed source status values reported per candidate and evidence ref.
const (
	SourceStatusRawCurrent      = "raw_current"
	SourceStatusDerivedProposed = "derived_proposed"
	SourceStatusNavigationOnly  = "navigation_only"
)

// Relations a candidate may have to the query. supports/refutes describe
// substantive support or refutation, not merely sharing a topic.
const (
	RelationSupports = "supports"
	RelationRefutes  = "refutes"
	RelationRelated  = "related"
	RelationUnknown  = "unknown"
)

// relationPriority is the fixed tie order used to validate the assessor's
// argmax choice.
var relationPriority = []string{RelationSupports, RelationRefutes, RelationRelated, RelationUnknown}

// AdmissionPolicy pins who assessed the candidates and at what threshold. A
// policy in phase "calibration" is accepted only by an explicit calibration
// run; served requests require "selected".
type AdmissionPolicy struct {
	SchemaVersion    string  `json:"schema_version"`
	RetrievalVersion string  `json:"retrieval_version"`
	ProviderID       string  `json:"provider_id"`
	ModelID          string  `json:"model_id"`
	ModelRevision    string  `json:"model_revision"`
	AssessorModel    string  `json:"assessor_model"`
	FixtureSHA256    string  `json:"fixture_sha256"`
	Threshold        float64 `json:"threshold"`
	Phase            string  `json:"phase"`
}

// Validate checks the policy document. allowCalibration permits the
// calibration phase, which only the offline runner passes.
func (p *AdmissionPolicy) Validate(allowCalibration bool) error {
	if p == nil {
		return errors.New("contextbuild: admission policy is required for v3")
	}
	if p.SchemaVersion != AdmissionPolicySchema {
		return fmt.Errorf("contextbuild: admission policy schema must be %s", AdmissionPolicySchema)
	}
	if p.RetrievalVersion != "v3" {
		return errors.New("contextbuild: admission policy retrieval_version must be v3")
	}
	for label, value := range map[string]string{
		"provider_id": p.ProviderID, "model_id": p.ModelID, "model_revision": p.ModelRevision,
		"assessor_model": p.AssessorModel, "fixture_sha256": p.FixtureSHA256,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("contextbuild: admission policy %s is required", label)
		}
	}
	if raw, err := hex.DecodeString(p.FixtureSHA256); err != nil || len(raw) != 32 {
		return errors.New("contextbuild: admission policy fixture_sha256 must be 64 hex characters")
	}
	if math.IsNaN(p.Threshold) || math.IsInf(p.Threshold, 0) || p.Threshold < 0 || p.Threshold > 1 {
		return errors.New("contextbuild: admission policy threshold must be within [0,1]")
	}
	switch p.Phase {
	case "selected":
	case "calibration":
		if !allowCalibration {
			return errors.New("contextbuild: calibration admission policies are not accepted by served requests")
		}
	default:
		return errors.New("contextbuild: admission policy phase must be calibration or selected")
	}
	return nil
}

// LoadAdmissionPolicy reads and decodes one policy document. Validation is the
// caller's responsibility so it can distinguish a malformed file from a policy
// phase the served surface must reject.
func LoadAdmissionPolicy(path string) (*AdmissionPolicy, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("contextbuild: read admission policy: %w", err)
	}
	var policy AdmissionPolicy
	if err := json.Unmarshal(data, &policy); err != nil {
		return nil, fmt.Errorf("contextbuild: decode admission policy: %w", err)
	}
	return &policy, nil
}

// MatchesIdentity compares the policy's embedding identity with the configured
// retriever. Only provider and model are observable through the retriever
// boundary; the revision is pinned by the policy document and reported.
func (p *AdmissionPolicy) MatchesIdentity(describe string) bool {
	provider, model := describeIdentity(describe)
	return provider == p.ProviderID && model == p.ModelID
}

// describeIdentity parses the stable "provider/model@dims" summary.
func describeIdentity(describe string) (string, string) {
	trimmed := strings.TrimSpace(describe)
	if trimmed == "" {
		return "", ""
	}
	if idx := strings.Index(trimmed, "@"); idx >= 0 {
		trimmed = trimmed[:idx]
	}
	if idx := strings.Index(trimmed, "/"); idx >= 0 {
		return trimmed[:idx], trimmed[idx+1:]
	}
	return trimmed, ""
}

// AdmissionCandidate is one ranked candidate presented to the assessor. It
// carries the candidate's complete own evidence, never a truncated excerpt and
// never a neighbour standing in for a missing citation.
type AdmissionCandidate struct {
	Item         memory.Item
	Evidence     []EvidenceRef
	Sources      []string
	Scores       map[string]float64
	SourceStatus string
}

// AdmissionDecision is the assessor's verdict about one candidate.
type AdmissionDecision struct {
	ItemID      string
	Relation    string
	Probability float64
}

// CandidateAssessor judges whether candidates provide substantive support or
// refutation for the query. It must return one decision per candidate, in
// order; a count mismatch is a failure, never a silent default.
type CandidateAssessor interface {
	AssessCandidates(ctx context.Context, query string, candidates []AdmissionCandidate) ([]AdmissionDecision, error)
}

// admissionRubric defines the four relations the engine may choose.
var admissionRubric = map[string]string{
	RelationSupports: "The candidate's own evidence substantively supports the query's answer, including required qualifiers",
	RelationRefutes:  "The candidate's own evidence substantively refutes the query's premise or answer",
	RelationRelated:  "The candidate shares a topic or entity with the query but does not answer it",
	RelationUnknown:  "The candidate cannot be judged against the query",
}

// DecisionCandidateAssessor adapts the existing choice engine: one question per
// candidate with an explicit supports/refutes/related/unknown distribution.
type DecisionCandidateAssessor struct {
	Engine decision.Engine
	// MaxQuestionsPerBatch bounds one assessor request. The shipped local
	// Jev-Style adapter scores questions sequentially over the full state, so
	// a large batch multiplies the prompt cost. One candidate per request
	// keeps each state small; zero uses the default.
	MaxQuestionsPerBatch int
	// Stats optionally collects per-request accounting. It is nil for served
	// requests; only the offline profile sets it, and the assessor emits no
	// telemetry of its own.
	Stats *AdmissionRequestStats
}

// AdmissionRequestStats is offline profile accounting: how many assessor
// requests were issued, for how many candidates, and how many request bytes the
// 64 KiB cap measured.
type AdmissionRequestStats struct {
	Requests   int
	Candidates int
	Bytes      int
}

// admissionEnvelopeOverhead is the byte count of the JSON envelope around the
// escaped state and the joined questions: {"state":"…","questions":[…]}.
const admissionEnvelopeOverhead = len(`{"state":"`) + len(`","questions":[`) + len(`]}`)

// admissionRequest holds everything one assessment pass needs, built once per
// candidate: the rendered block, the fixed question and their encoded sizes.
// The cap check and the submitted request therefore share the same bytes
// instead of re-marshalling the whole batch for every candidate.
type admissionRequest struct {
	itemIDs       []string
	prefix        string
	prefixBytes   int
	blocks        []string
	blockBytes    []int
	questions     []decision.Question
	questionBytes []int
}

// newAdmissionRequest renders every candidate block and question exactly once.
func newAdmissionRequest(query string, candidates []AdmissionCandidate) (*admissionRequest, error) {
	request := &admissionRequest{
		itemIDs:       make([]string, len(candidates)),
		prefix:        admissionStatePrefix(query),
		blocks:        make([]string, len(candidates)),
		blockBytes:    make([]int, len(candidates)),
		questions:     make([]decision.Question, len(candidates)),
		questionBytes: make([]int, len(candidates)),
	}
	size, err := escapedStringLen(request.prefix)
	if err != nil {
		return nil, err
	}
	request.prefixBytes = size
	for i, candidate := range candidates {
		request.itemIDs[i] = candidate.Item.ID
		request.blocks[i] = renderAdmissionBlock(query, i, candidate)
		if request.blockBytes[i], err = escapedStringLen(request.blocks[i]); err != nil {
			return nil, err
		}
		request.questions[i] = admissionQuestion(i)
		encoded, err := json.Marshal(request.questions[i])
		if err != nil {
			return nil, err
		}
		request.questionBytes[i] = len(encoded)
	}
	return request, nil
}

// size reports the request bytes for one batch. It equals json.Marshal of the
// whole {state, questions} document because JSON string escaping is per
// character and the envelope is fixed.
func (r *admissionRequest) size(batch []int) int {
	total := admissionEnvelopeOverhead + r.prefixBytes
	for position, index := range batch {
		total += r.blockBytes[index] + r.questionBytes[index]
		if position > 0 {
			total++
		}
	}
	return total
}

// fits reports whether a batch stays under the decision input cap.
func (r *admissionRequest) fits(batch []int, limit int) bool {
	return r.size(batch) <= limit
}

// state concatenates the query head and the selected candidate blocks.
func (r *admissionRequest) state(batch []int) string {
	var b strings.Builder
	b.WriteString(r.prefix)
	for _, index := range batch {
		b.WriteString(r.blocks[index])
	}
	return b.String()
}

// questionsFor returns the fixed questions of one batch in batch order.
func (r *admissionRequest) questionsFor(batch []int) []decision.Question {
	out := make([]decision.Question, len(batch))
	for position, index := range batch {
		out[position] = r.questions[index]
	}
	return out
}

// AssessCandidates evaluates every candidate in stable order. Candidates that
// cannot fit the 64 KiB request cap are not submitted and receive relation
// unknown with probability 0, which the gate treats as rejected.
func (a DecisionCandidateAssessor) AssessCandidates(ctx context.Context, query string, candidates []AdmissionCandidate) ([]AdmissionDecision, error) {
	if a.Engine == nil {
		return nil, errors.New("contextbuild: admission assessor decision engine is required")
	}
	if len(candidates) == 0 {
		return []AdmissionDecision{}, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	decisions := make([]AdmissionDecision, len(candidates))
	for i, candidate := range candidates {
		decisions[i] = AdmissionDecision{ItemID: candidate.Item.ID, Relation: RelationUnknown}
	}
	request, err := newAdmissionRequest(query, candidates)
	if err != nil {
		return nil, err
	}
	if a.MaxQuestionsPerBatch < 1 {
		a.MaxQuestionsPerBatch = 1
	}
	const maxQuestionsPerBatch = 32
	batchLimit := a.MaxQuestionsPerBatch
	if batchLimit > maxQuestionsPerBatch {
		batchLimit = maxQuestionsPerBatch
	}
	batch := []int{}
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		state := request.state(batch)
		questions := request.questionsFor(batch)
		if err := a.assessBatch(ctx, request, state, questions, batch, decisions); err != nil {
			return err
		}
		batch = batch[:0]
		return nil
	}
	for i := range candidates {
		candidate := append(append([]int(nil), batch...), i)
		if len(batch) >= batchLimit || !request.fits(candidate, maxRerankInputBytes) {
			if len(batch) == 0 {
				// A single candidate whose complete evidence exceeds the cap is
				// rejected rather than truncated.
				continue
			}
			if err := flush(); err != nil {
				return nil, err
			}
			if !request.fits([]int{i}, maxRerankInputBytes) {
				continue
			}
		}
		batch = append(batch, i)
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return decisions, nil
}

// assessBatch evaluates one batch and fills the matching decisions.
func (a DecisionCandidateAssessor) assessBatch(ctx context.Context, request *admissionRequest, state string,
	questions []decision.Question, batch []int, decisions []AdmissionDecision) error {
	if a.Stats != nil {
		a.Stats.Requests++
		a.Stats.Candidates += len(batch)
		a.Stats.Bytes += request.size(batch)
	}
	result, err := a.Engine.Evaluate(ctx, state, questions)
	if err != nil {
		return err
	}
	for k, idx := range batch {
		answer, ok := result.Answers[questions[k].ID]
		if !ok {
			return fmt.Errorf("contextbuild: admission assessor returned no answer for %s", questions[k].ID)
		}
		probability, err := validateAdmissionAnswer(answer)
		if err != nil {
			return fmt.Errorf("contextbuild: admission assessor answer %s: %w", questions[k].ID, err)
		}
		decisions[idx] = AdmissionDecision{ItemID: request.itemIDs[idx], Relation: answer.Choice, Probability: probability}
	}
	return nil
}

// admissionQuestion is the fixed question asked about one candidate.
func admissionQuestion(index int) decision.Question {
	return decision.Question{
		ID:           fmt.Sprintf("admission_%d", index),
		Type:         decision.TypeChoice,
		Instructions: fmt.Sprintf("Decide whether candidate_%d provides substantive support or refutation for the query in the state. A shared topic is related, not support. Use only the candidate's own cited evidence.", index),
		Criteria:     admissionRubric,
	}
}

// admissionStatePrefix is the constant head of the assessor state.
func admissionStatePrefix(query string) string {
	var b strings.Builder
	b.WriteString("query: ")
	b.WriteString(query)
	b.WriteString("\n")
	if numbers := queryNumbers(query); len(numbers) > 0 {
		b.WriteString("query_numbers: ")
		b.WriteString(strings.Join(numbers, ","))
		b.WriteString("\n")
	}
	return b.String()
}

// escapedStringLen reports the JSON string payload length of value: the encoded
// form without its surrounding quotes.
func escapedStringLen(value string) (int, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return 0, err
	}
	return len(data) - 2, nil
}

// validateAdmissionAnswer enforces the choice contract and returns
// P(supports)+P(refutes).
func validateAdmissionAnswer(answer decision.Answer) (float64, error) {
	if answer.Type != decision.TypeChoice {
		return 0, errors.New("answer is not a choice")
	}
	if answer.Choice != RelationSupports && answer.Choice != RelationRefutes && answer.Choice != RelationRelated && answer.Choice != RelationUnknown {
		return 0, fmt.Errorf("invalid choice %q", answer.Choice)
	}
	if len(answer.Distribution) != len(relationPriority) {
		return 0, errors.New("distribution must cover supports, refutes, related and unknown")
	}
	total := 0.0
	probabilities := map[string]float64{}
	for _, label := range relationPriority {
		value, ok := answer.Distribution[label]
		if !ok || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1 {
			return 0, fmt.Errorf("distribution value for %s is invalid", label)
		}
		probabilities[label] = value
		total += value
	}
	if math.Abs(total-1) > 1e-6 {
		return 0, errors.New("distribution does not sum to 1")
	}
	// The choice must be the argmax under the fixed tie order.
	for _, label := range relationPriority {
		if probabilities[label] > probabilities[answer.Choice]+1e-9 {
			return 0, fmt.Errorf("choice %q is not the distribution argmax", answer.Choice)
		}
		if math.Abs(probabilities[label]-probabilities[answer.Choice]) <= 1e-9 && label == answer.Choice {
			break
		}
	}
	return probabilities[RelationSupports] + probabilities[RelationRefutes], nil
}

// renderAdmissionBlock serializes one candidate's complete own evidence.
func renderAdmissionBlock(query string, index int, candidate AdmissionCandidate) string {
	var b strings.Builder
	fmt.Fprintf(&b, "candidate_%d id=%s kind=%s source_status=%s\n", index, candidate.Item.ID, candidate.Item.Kind, candidate.SourceStatus)
	fmt.Fprintf(&b, "current=%t\n", !candidate.Item.Stale)
	if len(candidate.Sources) > 0 {
		fmt.Fprintf(&b, "sources=%s\n", strings.Join(candidate.Sources, ","))
	}
	if len(candidate.Scores) > 0 {
		keys := make([]string, 0, len(candidate.Scores))
		for key := range candidate.Scores {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			parts = append(parts, fmt.Sprintf("%s=%.6f", key, candidate.Scores[key]))
		}
		fmt.Fprintf(&b, "scores=%s\n", strings.Join(parts, ","))
	}
	if candidate.SourceStatus == SourceStatusNavigationOnly {
		b.WriteString("evidence: none (navigation only; cannot answer the query)\n")
	}
	for _, ref := range candidate.Evidence {
		fmt.Fprintf(&b, "evidence turn=%s revision=%s recording=%s started_at=%s speaker=%s time=%s\n",
			ref.TurnID, orUnknown(ref.TranscriptRevision), orUnknown(ref.RecordingID), orUnknown(ref.RecordingStartedAt),
			speakerLabel(ref), timeLabel(ref.GlobalStartMS, ref.GlobalEndMS))
		fmt.Fprintf(&b, "text: %s\n", ref.Text)
	}
	return b.String()
}

func speakerLabel(ref EvidenceRef) string {
	if ref.SpeakerPersonID != nil && *ref.SpeakerPersonID != "" {
		return "confirmed:" + *ref.SpeakerPersonID
	}
	return "unknown"
}

func timeLabel(start, end *int64) string {
	if start == nil || end == nil {
		return "unknown"
	}
	return fmt.Sprintf("%d-%dms", *start, *end)
}

func orUnknown(value string) string {
	if strings.TrimSpace(value) == "" {
		return "unknown"
	}
	return value
}

// queryNumbers extracts distinct numeric tokens from the query so the assessor
// state carries the literal constraints.
func queryNumbers(query string) []string {
	seen := map[string]bool{}
	out := []string{}
	current := strings.Builder{}
	flush := func() {
		if current.Len() == 0 {
			return
		}
		value := current.String()
		current.Reset()
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	for _, r := range query {
		if (r >= '0' && r <= '9') || r == '.' || r == '%' {
			current.WriteRune(r)
			continue
		}
		flush()
	}
	flush()
	sort.Strings(out)
	return out
}

// fixtureSHA256 returns the canonical fixture digest used by a policy.
func fixtureSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
