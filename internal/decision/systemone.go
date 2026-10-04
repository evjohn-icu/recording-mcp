package decision

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
)

// maxResponseBytes bounds how much of a decision-service response is read.
const maxResponseBytes = 4 << 20

// SystemOneClient talks to a local Decision 2.0-style service that exposes the
// model card's system_one interface. It is not a chat-completions endpoint.
type SystemOneClient struct {
	Endpoint       string
	Model          string
	HTTPClient     *http.Client
	WeightsVersion string
}

// requestQuestion is the wire form of a Question sent to the service.
type requestQuestion struct {
	Type         string            `json:"type"`
	Instructions string            `json:"instructions"`
	Criteria     map[string]string `json:"criteria,omitempty"`
	Rubric       []string          `json:"rubric,omitempty"`
}

type requestBody struct {
	Model     string                     `json:"model"`
	State     string                     `json:"state"`
	Questions map[string]requestQuestion `json:"questions"`
}

// rawAnswer is the service-shaped union answer before normalization.
type rawAnswer struct {
	Type          string            `json:"type"`
	Choice        string            `json:"choice"`
	Confidence    *float64          `json:"confidence"`
	Noul          *float64          `json:"noul"`
	Score         *float64          `json:"score"`
	Probabilities json.RawMessage   `json:"probabilities"`
	Legend        map[string]string `json:"legend"`
}

// rawUsage decodes usage tolerantly: local services may report latency as a
// float (e.g. 2484.4) while the contract exposes integer milliseconds.
type rawUsage struct {
	LatencyMS    json.Number `json:"latency_ms"`
	InputTokens  json.Number `json:"input_tokens"`
	OutputTokens json.Number `json:"output_tokens"`
}

type systemOneResponse struct {
	Model          string               `json:"model"`
	WeightsVersion string               `json:"weights_version"`
	Answers        map[string]rawAnswer `json:"answers"`
	Usage          rawUsage             `json:"usage"`
}

// usageFromRaw converts the tolerant wire usage into the contract Usage,
// rounding fractional values to the nearest integer.
func usageFromRaw(raw rawUsage) Usage {
	return Usage{
		LatencyMS:    roundToInt64(raw.LatencyMS),
		InputTokens:  int(roundToInt64(raw.InputTokens)),
		OutputTokens: int(roundToInt64(raw.OutputTokens)),
	}
}

func roundToInt64(n json.Number) int64 {
	if n == "" {
		return 0
	}
	f, err := n.Float64()
	if err != nil {
		return 0
	}
	return int64(math.Round(f))
}

// Evaluate implements Engine. Any contract violation rejects the whole result
// with a *ValidationError; transport and decoding failures return plain errors.
func (c *SystemOneClient) Evaluate(ctx context.Context, state string, questions []Question) (Result, error) {
	if c == nil {
		return Result{}, errors.New("decision: nil SystemOneClient")
	}
	endpoint := strings.TrimSpace(c.Endpoint)
	if endpoint == "" {
		return Result{}, errors.New("decision: empty endpoint")
	}
	reqQuestions := make(map[string]requestQuestion, len(questions))
	for _, q := range questions {
		reqQuestions[q.ID] = requestQuestion{
			Type:         q.Type,
			Instructions: q.Instructions,
			Criteria:     q.Criteria,
			Rubric:       q.Rubric,
		}
	}
	body, err := json.Marshal(requestBody{Model: c.Model, State: state, Questions: reqQuestions})
	if err != nil {
		return Result{}, fmt.Errorf("decision: encode request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return Result{}, fmt.Errorf("decision: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return Result{}, fmt.Errorf("decision: request: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return Result{}, fmt.Errorf("decision: read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Result{}, fmt.Errorf("decision: service returned %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}

	var out systemOneResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return Result{}, fmt.Errorf("decision: decode response: %w", err)
	}
	if len(out.Answers) == 0 {
		// Accept a bare {question_id: answer} body as well.
		var bare map[string]rawAnswer
		if err := json.Unmarshal(data, &bare); err == nil && len(bare) > 0 {
			out.Answers = bare
		}
	}

	var issues []string
	answers := make(map[string]Answer, len(questions))
	for _, q := range questions {
		raw, ok := out.Answers[q.ID]
		if !ok {
			continue // validateAnswers reports the missing answer.
		}
		a, aIssues := normalizeAnswer(q, raw)
		issues = append(issues, aIssues...)
		answers[q.ID] = a
	}
	issues = append(issues, validateAnswers(questions, answers)...)
	if err := newValidationError(issues); err != nil {
		return Result{}, err
	}

	model := out.Model
	if model == "" {
		model = c.Model
	}
	weights := out.WeightsVersion
	if weights == "" {
		weights = c.WeightsVersion
	}
	return Result{
		Model:          model,
		WeightsVersion: weights,
		InputHash:      inputHash(state, questions),
		Answers:        answers,
		Usage:          usageFromRaw(out.Usage),
	}, nil
}

// normalizeAnswer converts the service union shape into the canonical Answer.
func normalizeAnswer(q Question, raw rawAnswer) (Answer, []string) {
	var issues []string
	a := Answer{Type: q.Type}
	if raw.Confidence != nil {
		a.Confidence = *raw.Confidence
	}
	if raw.Type != "" && raw.Type != q.Type {
		issues = append(issues, fmt.Sprintf("question %q: service type %q does not match question type %q", q.ID, raw.Type, q.Type))
	}
	switch q.Type {
	case TypeChoice:
		dist, err := decodeProbMap(raw.Probabilities)
		if err != nil {
			issues = append(issues, fmt.Sprintf("question %q: %v", q.ID, err))
		}
		a.Distribution = dist
		a.Choice = raw.Choice
		if raw.Confidence == nil {
			a.Confidence = maxDist(dist)
		}
	case TypeNoul:
		if raw.Noul == nil {
			issues = append(issues, fmt.Sprintf("question %q: missing noul", q.ID))
			a.Noul = math.NaN()
		} else {
			a.Noul = *raw.Noul
		}
		a.Distribution = map[string]float64{"true": a.Noul, "false": 1 - a.Noul}
		if raw.Confidence == nil && finite(a.Noul) {
			a.Confidence = math.Max(a.Noul, 1-a.Noul)
		}
	case TypeScore:
		if raw.Score == nil {
			issues = append(issues, fmt.Sprintf("question %q: missing score", q.ID))
			a.Score = math.NaN()
		} else {
			a.Score = *raw.Score
		}
		dist, err := decodeScoreDist(raw.Probabilities, raw.Legend)
		if err != nil {
			issues = append(issues, fmt.Sprintf("question %q: %v", q.ID, err))
		}
		a.Distribution = dist
		if raw.Confidence == nil {
			a.Confidence = maxDist(dist)
		}
	}
	return a, issues
}

// decodeProbMap decodes an object-shaped probability map.
func decodeProbMap(raw json.RawMessage) (map[string]float64, error) {
	if len(raw) == 0 {
		return nil, errors.New("missing probabilities")
	}
	var dist map[string]float64
	if err := json.Unmarshal(raw, &dist); err != nil {
		return nil, fmt.Errorf("probabilities: %w", err)
	}
	return dist, nil
}

// decodeScoreDist decodes score probabilities, which are either an ordered
// array (paired with rubric indices or legend keys) or an object map.
func decodeScoreDist(raw json.RawMessage, legend map[string]string) (map[string]float64, error) {
	if len(raw) == 0 {
		return nil, errors.New("missing probabilities")
	}
	var arr []float64
	if err := json.Unmarshal(raw, &arr); err == nil {
		dist := make(map[string]float64, len(arr))
		keys := indexKeys(len(arr))
		if len(legend) == len(arr) {
			keys = legendKeys(legend)
		}
		for i, v := range arr {
			dist[keys[i]] = v
		}
		return dist, nil
	}
	var obj map[string]float64
	if err := json.Unmarshal(raw, &obj); err == nil {
		return obj, nil
	}
	return nil, errors.New("probabilities: not an array or object")
}

func indexKeys(n int) []string {
	keys := make([]string, n)
	for i := range keys {
		keys[i] = scoreKey(i)
	}
	return keys
}

func legendKeys(legend map[string]string) []string {
	keys := make([]string, 0, len(legend))
	for k := range legend {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func maxDist(dist map[string]float64) float64 {
	var best float64
	for _, v := range dist {
		if v > best {
			best = v
		}
	}
	return best
}
