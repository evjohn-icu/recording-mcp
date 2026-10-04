// Package decision defines the versioned decision-engine contract used by the
// processing pipeline: typed questions (choice / yes-no / score) are evaluated
// against a single state string and must yield a fully specified, validated
// distribution per question. Adapters (synthetic, local Decision 2.0 service)
// implement Engine; invalid results are rejected as a whole via ValidationError.
package decision

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

// Question types supported by the contract.
const (
	TypeChoice = "choice"
	TypeNoul   = "noul"
	TypeScore  = "score"
)

// probEpsilon is the tolerance used when checking that a distribution sums to
// one and that scores fall inside their rubric range.
const probEpsilon = 1e-6

// Question is a single typed judgment about a state.
//
//   - choice: Criteria maps option -> "when this option applies"; the answer
//     must carry the full option distribution.
//   - noul: yes/no; no options are required.
//   - score: Rubric is the ordered list of levels; the answer must carry the
//     full ordered distribution.
type Question struct {
	ID           string            `json:"id"`
	Type         string            `json:"type"`
	Instructions string            `json:"instructions"`
	Criteria     map[string]string `json:"criteria,omitempty"`
	Rubric       []string          `json:"rubric,omitempty"`
}

// Answer is the normalized answer to one Question. Distribution is always the
// canonical representation: option -> probability for choice, {"true","false"}
// for noul, and rubric-level key -> probability for score.
type Answer struct {
	Type         string             `json:"type"`
	Choice       string             `json:"choice,omitempty"`
	Noul         float64            `json:"noul,omitempty"`
	Score        float64            `json:"score,omitempty"`
	Confidence   float64            `json:"confidence"`
	Distribution map[string]float64 `json:"distribution,omitempty"`
}

// Usage reports per-evaluation cost as observed by the adapter.
type Usage struct {
	LatencyMS    int64 `json:"latency_ms"`
	InputTokens  int   `json:"input_tokens"`
	OutputTokens int   `json:"output_tokens"`
}

// Result is the complete, versioned outcome of one Evaluate call.
type Result struct {
	Model          string            `json:"model"`
	WeightsVersion string            `json:"weights_version"`
	InputHash      string            `json:"input_hash"`
	Answers        map[string]Answer `json:"answers"`
	Usage          Usage             `json:"usage"`
}

// Engine evaluates a set of questions against one state.
type Engine interface {
	Evaluate(ctx context.Context, state string, questions []Question) (Result, error)
}

// ValidationError reports every contract violation found in one evaluation.
// The whole result is rejected; a missing answer is never treated as "no".
type ValidationError struct {
	Issues []string
}

func (e *ValidationError) Error() string {
	if e == nil {
		return "decision: invalid result"
	}
	return "decision: invalid result: " + strings.Join(e.Issues, "; ")
}

// inputHash returns sha256 over the canonical JSON encoding of {state,
// questions}. Map keys are emitted in sorted order and HTML escaping is
// disabled, so identical inputs always hash identically across processes.
func inputHash(state string, questions []Question) string {
	payload, err := canonicalJSON(struct {
		State     string     `json:"state"`
		Questions []Question `json:"questions"`
	}{State: state, Questions: questions})
	if err != nil {
		payload = []byte(state)
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// canonicalJSON marshals v deterministically: struct fields in declaration
// order, map keys sorted by encoding/json, and no HTML escaping.
func canonicalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// validateAnswers enforces the full contract and returns every issue found in
// deterministic order. An empty slice means the result is valid.
func validateAnswers(questions []Question, answers map[string]Answer) []string {
	var issues []string
	seen := make(map[string]bool, len(questions))
	for _, q := range questions {
		seen[q.ID] = true
		a, ok := answers[q.ID]
		if !ok {
			issues = append(issues, fmt.Sprintf("question %q: missing answer", q.ID))
			continue
		}
		if a.Type != q.Type {
			issues = append(issues, fmt.Sprintf("question %q: answer type %q does not match question type %q", q.ID, a.Type, q.Type))
			continue
		}
		if !finite(a.Confidence) {
			issues = append(issues, fmt.Sprintf("question %q: confidence is not finite", q.ID))
		}
		for k, v := range a.Distribution {
			if !finite(v) {
				issues = append(issues, fmt.Sprintf("question %q: distribution value for %q is not finite", q.ID, k))
			}
		}
		switch q.Type {
		case TypeChoice:
			issues = append(issues, validateChoice(q, a)...)
		case TypeNoul:
			issues = append(issues, validateNoul(q, a)...)
		case TypeScore:
			issues = append(issues, validateScore(q, a)...)
		default:
			issues = append(issues, fmt.Sprintf("question %q: unsupported type %q", q.ID, q.Type))
		}
	}
	extras := make([]string, 0)
	for id := range answers {
		if !seen[id] {
			extras = append(extras, id)
		}
	}
	sort.Strings(extras)
	for _, id := range extras {
		issues = append(issues, fmt.Sprintf("answer %q: not requested", id))
	}
	return issues
}

func validateChoice(q Question, a Answer) []string {
	var issues []string
	if len(q.Criteria) == 0 {
		issues = append(issues, fmt.Sprintf("question %q: choice question has no criteria", q.ID))
		return issues
	}
	if len(a.Distribution) != len(q.Criteria) {
		issues = append(issues, fmt.Sprintf("question %q: choice distribution has %d options, criteria has %d", q.ID, len(a.Distribution), len(q.Criteria)))
	}
	for k := range q.Criteria {
		if _, ok := a.Distribution[k]; !ok {
			issues = append(issues, fmt.Sprintf("question %q: choice distribution missing option %q", q.ID, k))
		}
	}
	for k := range a.Distribution {
		if _, ok := q.Criteria[k]; !ok {
			issues = append(issues, fmt.Sprintf("question %q: choice distribution has unknown option %q", q.ID, k))
		}
	}
	issues = append(issues, checkSum(q.ID, a.Distribution)...)
	if a.Choice == "" {
		issues = append(issues, fmt.Sprintf("question %q: choice answer is empty", q.ID))
	} else if _, ok := q.Criteria[a.Choice]; !ok {
		issues = append(issues, fmt.Sprintf("question %q: choice %q is not a criterion", q.ID, a.Choice))
	}
	return issues
}

func validateNoul(q Question, a Answer) []string {
	var issues []string
	if !finite(a.Noul) || a.Noul < 0 || a.Noul > 1 {
		issues = append(issues, fmt.Sprintf("question %q: noul %v is outside [0,1]", q.ID, a.Noul))
	}
	if len(a.Distribution) != 2 {
		issues = append(issues, fmt.Sprintf("question %q: noul distribution must have true/false, got %d entries", q.ID, len(a.Distribution)))
		return issues
	}
	for _, k := range []string{"true", "false"} {
		if _, ok := a.Distribution[k]; !ok {
			issues = append(issues, fmt.Sprintf("question %q: noul distribution missing key %q", q.ID, k))
		}
	}
	issues = append(issues, checkSum(q.ID, a.Distribution)...)
	if p, ok := a.Distribution["true"]; ok && finite(a.Noul) && math.Abs(p-a.Noul) > probEpsilon {
		issues = append(issues, fmt.Sprintf("question %q: noul %v disagrees with distribution true=%v", q.ID, a.Noul, p))
	}
	return issues
}

func validateScore(q Question, a Answer) []string {
	var issues []string
	n := len(q.Rubric)
	if n == 0 {
		issues = append(issues, fmt.Sprintf("question %q: score question has no rubric", q.ID))
		return issues
	}
	if len(a.Distribution) != n {
		issues = append(issues, fmt.Sprintf("question %q: score distribution has %d entries, rubric has %d", q.ID, len(a.Distribution), n))
	}
	issues = append(issues, checkSum(q.ID, a.Distribution)...)
	if !finite(a.Score) || a.Score < -probEpsilon || a.Score > float64(n-1)+probEpsilon {
		issues = append(issues, fmt.Sprintf("question %q: score %v is outside [0,%d]", q.ID, a.Score, n-1))
	}
	return issues
}

func checkSum(id string, dist map[string]float64) []string {
	if len(dist) == 0 {
		return []string{fmt.Sprintf("question %q: empty distribution", id)}
	}
	var sum float64
	for _, v := range dist {
		sum += v
	}
	if math.Abs(sum-1) > probEpsilon {
		return []string{fmt.Sprintf("question %q: distribution sums to %v, want 1", id, sum)}
	}
	return nil
}

func finite(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0)
}

func newValidationError(issues []string) error {
	if len(issues) == 0 {
		return nil
	}
	return &ValidationError{Issues: issues}
}
