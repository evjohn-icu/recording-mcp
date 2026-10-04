package decision

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"math"
	"sort"
	"strconv"
)

// Synthetic is a deterministic Engine used for tests and shadow wiring. It
// needs no network or model weights: every distribution is derived from
// sha256(state | question ID | option), so identical inputs always produce
// identical results.
type Synthetic struct{}

// Evaluate implements Engine.
func (Synthetic) Evaluate(_ context.Context, state string, questions []Question) (Result, error) {
	answers := make(map[string]Answer, len(questions))
	for _, q := range questions {
		switch q.Type {
		case TypeChoice:
			answers[q.ID] = syntheticChoice(state, q)
		case TypeNoul:
			answers[q.ID] = syntheticNoul(state, q)
		case TypeScore:
			answers[q.ID] = syntheticScore(state, q)
		default:
			// Leave the answer absent so validation reports the unsupported type.
		}
	}
	if err := newValidationError(validateAnswers(questions, answers)); err != nil {
		return Result{}, err
	}
	return Result{
		Model:          "synthetic",
		WeightsVersion: "synthetic-v1",
		InputHash:      inputHash(state, questions),
		Answers:        answers,
	}, nil
}

func syntheticChoice(state string, q Question) Answer {
	keys := make([]string, 0, len(q.Criteria))
	for k := range q.Criteria {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	dist := make(map[string]float64, len(keys))
	var total float64
	for _, k := range keys {
		w := unitHash(state, q.ID, "choice", k)
		dist[k] = w
		total += w
	}
	best := ""
	var bestW float64
	for _, k := range keys {
		dist[k] /= total
		if dist[k] > bestW {
			bestW = dist[k]
			best = k
		}
	}
	return Answer{Type: TypeChoice, Choice: best, Confidence: bestW, Distribution: dist}
}

func syntheticNoul(state string, q Question) Answer {
	p := unitHash(state, q.ID, "noul")
	return Answer{
		Type:         TypeNoul,
		Noul:         p,
		Confidence:   math.Max(p, 1-p),
		Distribution: map[string]float64{"true": p, "false": 1 - p},
	}
}

func syntheticScore(state string, q Question) Answer {
	dist := make(map[string]float64, len(q.Rubric))
	var total, expected float64
	for i := range q.Rubric {
		w := unitHash(state, q.ID, "score", q.Rubric[i])
		dist[scoreKey(i)] = w
		total += w
	}
	var best float64
	for i := range q.Rubric {
		k := scoreKey(i)
		dist[k] /= total
		expected += float64(i) * dist[k]
		if dist[k] > best {
			best = dist[k]
		}
	}
	return Answer{Type: TypeScore, Score: expected, Confidence: best, Distribution: dist}
}

func scoreKey(i int) string {
	return strconv.Itoa(i)
}

// unitHash maps sha256(parts joined by '|') into [0,1].
func unitHash(parts ...string) float64 {
	h := sha256.New()
	for i, p := range parts {
		if i > 0 {
			h.Write([]byte{'|'})
		}
		h.Write([]byte(p))
	}
	sum := h.Sum(nil)
	return float64(binary.BigEndian.Uint64(sum[:8])) / float64(math.MaxUint64)
}
