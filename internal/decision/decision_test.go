package decision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func testQuestions() []Question {
	return []Question{
		{ID: "q-choice", Type: TypeChoice, Instructions: "pick a team", Criteria: map[string]string{"billing": "charges", "technical": "failures"}},
		{ID: "q-noul", Type: TypeNoul, Instructions: "does the sender ask for help today?"},
		{ID: "q-score", Type: TypeScore, Instructions: "relevance", Rubric: []string{"none", "partial", "full"}},
	}
}

func sumDist(t *testing.T, id string, dist map[string]float64) {
	t.Helper()
	var sum float64
	for _, v := range dist {
		sum += v
	}
	if math.Abs(sum-1) > 1e-9 {
		t.Fatalf("%s: distribution sums to %v, want 1", id, sum)
	}
}

func TestSyntheticDeterministic(t *testing.T) {
	ctx := context.Background()
	state := "I was charged twice and need help today."
	qs := testQuestions()

	first, err := Synthetic{}.Evaluate(ctx, state, qs)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	second, err := Synthetic{}.Evaluate(ctx, state, qs)
	if err != nil {
		t.Fatalf("evaluate again: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("synthetic not deterministic:\n%+v\n%+v", first, second)
	}
	if first.InputHash == "" || first.InputHash != inputHash(state, qs) {
		t.Fatalf("input hash = %q, want %q", first.InputHash, inputHash(state, qs))
	}
	if first.Model != "synthetic" {
		t.Fatalf("model = %q", first.Model)
	}
	if issues := validateAnswers(qs, first.Answers); len(issues) != 0 {
		t.Fatalf("synthetic produced invalid answers: %v", issues)
	}

	choice := first.Answers["q-choice"]
	if _, ok := qs[0].Criteria[choice.Choice]; !ok {
		t.Fatalf("choice %q not in criteria", choice.Choice)
	}
	sumDist(t, "q-choice", choice.Distribution)

	noul := first.Answers["q-noul"]
	if noul.Noul < 0 || noul.Noul > 1 {
		t.Fatalf("noul = %v", noul.Noul)
	}
	if math.Abs(noul.Distribution["true"]-noul.Noul) > 1e-12 {
		t.Fatalf("noul distribution mismatch: %v vs %v", noul.Distribution["true"], noul.Noul)
	}
	sumDist(t, "q-noul", noul.Distribution)

	score := first.Answers["q-score"]
	if len(score.Distribution) != len(qs[2].Rubric) {
		t.Fatalf("score distribution length = %d, rubric = %d", len(score.Distribution), len(qs[2].Rubric))
	}
	if score.Score < 0 || score.Score > float64(len(qs[2].Rubric)-1) {
		t.Fatalf("score = %v", score.Score)
	}
	sumDist(t, "q-score", score.Distribution)

	other, err := Synthetic{}.Evaluate(ctx, state+" different", qs)
	if err != nil {
		t.Fatalf("evaluate other: %v", err)
	}
	if other.InputHash == first.InputHash {
		t.Fatal("different state produced the same input hash")
	}
}

func TestInputHashCanonical(t *testing.T) {
	a := []Question{{ID: "q", Type: TypeChoice, Criteria: map[string]string{"x": "1", "y": "2"}}}
	b := []Question{{ID: "q", Type: TypeChoice, Criteria: map[string]string{"y": "2", "x": "1"}}}
	if inputHash("s", a) != inputHash("s", b) {
		t.Fatal("criteria map ordering changed the input hash")
	}
	if inputHash("s", a) == inputHash("s2", a) {
		t.Fatal("state did not affect the input hash")
	}
}

func TestSystemOneParsesAllTypes(t *testing.T) {
	qs := testQuestions()
	state := "I was charged twice."

	var gotBody requestBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("content type = %q", ct)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"model": "kai-0.6b",
			"weights_version": "w1",
			"answers": {
				"q-choice": {"type": "choice", "choice": "billing", "confidence": 0.8, "probabilities": {"billing": 0.8, "technical": 0.2}},
				"q-noul": {"type": "noul", "noul": 0.7, "confidence": 0.7},
				"q-score": {"type": "score", "score": 1.4, "confidence": 0.6, "probabilities": [0.1, 0.3, 0.6], "legend": {"a": "none", "b": "partial", "c": "full"}}
			},
			"usage": {"latency_ms": 12, "input_tokens": 100, "output_tokens": 20}
		}`)
	}))
	defer srv.Close()

	client := &SystemOneClient{Endpoint: srv.URL, Model: "kai-0.6b", HTTPClient: srv.Client()}
	res, err := client.Evaluate(context.Background(), state, qs)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if gotBody.Model != "kai-0.6b" || gotBody.State != state {
		t.Fatalf("request model/state = %q/%q", gotBody.Model, gotBody.State)
	}
	if len(gotBody.Questions) != 3 {
		t.Fatalf("request questions = %d", len(gotBody.Questions))
	}
	if gotBody.Questions["q-choice"].Criteria["billing"] != "charges" {
		t.Fatalf("choice criteria not sent: %+v", gotBody.Questions["q-choice"])
	}
	if !reflect.DeepEqual(gotBody.Questions["q-score"].Rubric, qs[2].Rubric) {
		t.Fatalf("score rubric not sent: %+v", gotBody.Questions["q-score"])
	}
	if res.Model != "kai-0.6b" || res.WeightsVersion != "w1" {
		t.Fatalf("model/weights = %q/%q", res.Model, res.WeightsVersion)
	}
	if res.InputHash != inputHash(state, qs) {
		t.Fatalf("input hash = %q", res.InputHash)
	}
	if res.Usage != (Usage{LatencyMS: 12, InputTokens: 100, OutputTokens: 20}) {
		t.Fatalf("usage = %+v", res.Usage)
	}

	choice := res.Answers["q-choice"]
	if choice.Choice != "billing" || choice.Confidence != 0.8 || len(choice.Distribution) != 2 {
		t.Fatalf("choice answer = %+v", choice)
	}
	noul := res.Answers["q-noul"]
	if noul.Noul != 0.7 || noul.Distribution["true"] != 0.7 || math.Abs(noul.Distribution["false"]-0.3) > 1e-9 {
		t.Fatalf("noul answer = %+v", noul)
	}
	score := res.Answers["q-score"]
	if score.Score != 1.4 || len(score.Distribution) != 3 {
		t.Fatalf("score answer = %+v", score)
	}
	if score.Distribution["a"] != 0.1 || score.Distribution["b"] != 0.3 || score.Distribution["c"] != 0.6 {
		t.Fatalf("score legend not applied: %+v", score.Distribution)
	}
	if issues := validateAnswers(qs, res.Answers); len(issues) != 0 {
		t.Fatalf("valid response rejected: %v", issues)
	}
}

func TestSystemOneRejectsInvalidResults(t *testing.T) {
	qs := testQuestions()
	valid := `"q-choice": {"type": "choice", "choice": "billing", "probabilities": {"billing": 0.8, "technical": 0.2}},
		"q-noul": {"type": "noul", "noul": 0.7},
		"q-score": {"type": "score", "score": 1.4, "probabilities": [0.1, 0.3, 0.6]}`

	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "missing answer",
			body: `{"answers": {"q-choice": {"type": "choice", "choice": "billing", "probabilities": {"billing": 0.8, "technical": 0.2}}}}`,
			want: "missing answer",
		},
		{
			name: "type mismatch",
			body: `{"answers": {"q-choice": {"type": "noul", "noul": 0.5}, "q-noul": {"type": "noul", "noul": 0.7}, "q-score": {"type": "score", "score": 1.4, "probabilities": [0.1, 0.3, 0.6]}}}`,
			want: "does not match",
		},
		{
			name: "choice keys differ from criteria",
			body: `{"answers": {"q-choice": {"type": "choice", "choice": "billing", "probabilities": {"billing": 0.5, "other": 0.5}}, "q-noul": {"type": "noul", "noul": 0.7}, "q-score": {"type": "score", "score": 1.4, "probabilities": [0.1, 0.3, 0.6]}}}`,
			want: "choice distribution",
		},
		{
			name: "choice probabilities do not sum to one",
			body: `{"answers": {"q-choice": {"type": "choice", "choice": "billing", "probabilities": {"billing": 0.5, "technical": 0.2}}, "q-noul": {"type": "noul", "noul": 0.7}, "q-score": {"type": "score", "score": 1.4, "probabilities": [0.1, 0.3, 0.6]}}}`,
			want: "sums to",
		},
		{
			name: "noul out of range",
			body: `{"answers": {"q-choice": {"type": "choice", "choice": "billing", "probabilities": {"billing": 0.8, "technical": 0.2}}, "q-noul": {"type": "noul", "noul": 1.5}, "q-score": {"type": "score", "score": 1.4, "probabilities": [0.1, 0.3, 0.6]}}}`,
			want: "outside [0,1]",
		},
		{
			name: "score length differs from rubric",
			body: `{"answers": {"q-choice": {"type": "choice", "choice": "billing", "probabilities": {"billing": 0.8, "technical": 0.2}}, "q-noul": {"type": "noul", "noul": 0.7}, "q-score": {"type": "score", "score": 0.5, "probabilities": [0.5, 0.5]}}}`,
			want: "rubric has 3",
		},
		{
			name: "score out of range",
			body: `{"answers": {"q-choice": {"type": "choice", "choice": "billing", "probabilities": {"billing": 0.8, "technical": 0.2}}, "q-noul": {"type": "noul", "noul": 0.7}, "q-score": {"type": "score", "score": 5, "probabilities": [0.1, 0.3, 0.6]}}}`,
			want: "outside [0,2]",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			client := &SystemOneClient{Endpoint: srv.URL, Model: "kai", HTTPClient: srv.Client()}
			_, err := client.Evaluate(context.Background(), "state", qs)
			if err == nil {
				t.Fatal("expected validation error, got nil")
			}
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("error = %T %v, want *ValidationError", err, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.want)
			}
			if len(ve.Issues) == 0 {
				t.Fatal("ValidationError has no issues")
			}
		})
	}

	// Sanity: the reference valid body is accepted.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"answers": {%s}}`, valid)
	}))
	defer srv.Close()
	client := &SystemOneClient{Endpoint: srv.URL, Model: "kai", HTTPClient: srv.Client()}
	if _, err := client.Evaluate(context.Background(), "state", qs); err != nil {
		t.Fatalf("valid body rejected: %v", err)
	}
}

func TestSystemOneUsageFloat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{
			"answers": {
				"q-choice": {"type": "choice", "choice": "billing", "probabilities": {"billing": 0.8, "technical": 0.2}},
				"q-noul": {"type": "noul", "noul": 0.7},
				"q-score": {"type": "score", "score": 1.4, "probabilities": [0.1, 0.3, 0.6]}
			},
			"usage": {"latency_ms": 815.3, "input_tokens": 10.6, "output_tokens": 3}
		}`)
	}))
	defer srv.Close()
	client := &SystemOneClient{Endpoint: srv.URL, Model: "kai", HTTPClient: srv.Client()}
	res, err := client.Evaluate(context.Background(), "state", testQuestions())
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if res.Usage.LatencyMS != 815 {
		t.Fatalf("latency_ms = %d, want 815", res.Usage.LatencyMS)
	}
	if res.Usage.InputTokens != 11 || res.Usage.OutputTokens != 3 {
		t.Fatalf("token usage = %+v", res.Usage)
	}
}

func TestSystemOneHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	client := &SystemOneClient{Endpoint: srv.URL, HTTPClient: srv.Client()}
	_, err := client.Evaluate(context.Background(), "state", testQuestions())
	if err == nil {
		t.Fatal("expected error")
	}
	var ve *ValidationError
	if errors.As(err, &ve) {
		t.Fatalf("transport failure should not be a ValidationError: %v", err)
	}
}
