// Command context-decision is a local reference implementation of the
// Decision 2.0 "system_one" HTTP contract that internal/decision's
// SystemOneClient speaks. It answers typed judgment questions (choice / noul /
// score) with full distributions using the repository's deterministic
// synthetic engine — a contract-faithful sandbox so the processing pipeline
// (`context-worker --executor pipeline`) and build_context reranking run
// entirely offline. It is NOT a model: judgments are hash-derived and
// deterministic, and the response identifies itself as such through
// weights_version. A real Decision weights service can be swapped in behind
// the same endpoint.
package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"recordingmcp/internal/decision"
)

// Wire contract (mirrors internal/decision/systemone.go's client shape).

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

type rawAnswer struct {
	Type          string            `json:"type"`
	Choice        string            `json:"choice,omitempty"`
	Confidence    float64           `json:"confidence"`
	Noul          *float64          `json:"noul,omitempty"`
	Score         *float64          `json:"score,omitempty"`
	Probabilities json.RawMessage   `json:"probabilities,omitempty"`
	Legend        map[string]string `json:"legend,omitempty"`
}

type rawUsage struct {
	LatencyMS    int64 `json:"latency_ms"`
	InputTokens  int   `json:"input_tokens"`
	OutputTokens int   `json:"output_tokens"`
}

type systemOneResponse struct {
	Model          string               `json:"model"`
	WeightsVersion string               `json:"weights_version"`
	Answers        map[string]rawAnswer `json:"answers"`
	Usage          rawUsage             `json:"usage"`
}

type server struct {
	model          string
	weightsVersion string
	engine         decision.Engine
}

// ServeSystemOne implements the POST /v1/systemone endpoint.
func (s *server) ServeSystemOne(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	start := time.Now()
	var req requestBody
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.State) == "" {
		http.Error(w, "bad request: state is required", http.StatusBadRequest)
		return
	}
	ids := make([]string, 0, len(req.Questions))
	for id := range req.Questions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	questions := make([]decision.Question, 0, len(ids))
	for _, id := range ids {
		q := req.Questions[id]
		questions = append(questions, decision.Question{
			ID: id, Type: q.Type, Instructions: q.Instructions,
			Criteria: q.Criteria, Rubric: q.Rubric,
		})
	}
	result, err := s.engine.Evaluate(r.Context(), req.State, questions)
	if err != nil {
		var validation *decision.ValidationError
		if ok := asValidationError(err, &validation); ok {
			http.Error(w, "invalid judgment result: "+strings.Join(validation.Issues, "; "), http.StatusUnprocessableEntity)
			return
		}
		http.Error(w, "judgment failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	answers := make(map[string]rawAnswer, len(result.Answers))
	for id, a := range result.Answers {
		raw, err := toRawAnswer(a)
		if err != nil {
			http.Error(w, "encode answer: "+err.Error(), http.StatusInternalServerError)
			return
		}
		answers[id] = raw
	}
	out := systemOneResponse{
		Model:          s.model,
		WeightsVersion: s.weightsVersion,
		Answers:        answers,
		Usage:          rawUsage{LatencyMS: time.Since(start).Milliseconds()},
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(out); err != nil {
		log.Printf("encode response: %v", err)
	}
}

// toRawAnswer serializes one canonical answer into the service union shape.
func toRawAnswer(a decision.Answer) (rawAnswer, error) {
	probs, err := json.Marshal(a.Distribution)
	if err != nil {
		return rawAnswer{}, err
	}
	raw := rawAnswer{Type: a.Type, Confidence: a.Confidence, Probabilities: probs}
	switch a.Type {
	case decision.TypeChoice:
		raw.Choice = a.Choice
	case decision.TypeNoul:
		v := a.Noul
		raw.Noul = &v
	case decision.TypeScore:
		v := a.Score
		raw.Score = &v
	}
	return raw, nil
}

func asValidationError(err error, out **decision.ValidationError) bool {
	for err != nil {
		if v, ok := err.(*decision.ValidationError); ok {
			*out = v
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func main() {
	log.SetOutput(os.Stderr)
	addr := flag.String("addr", "127.0.0.1:8765", "listen address")
	model := flag.String("model", "jev-style-2b-v2", "model name reported to clients")
	weights := flag.String("weights-version", "sandbox-synthetic-v1", "weights version reported to clients")
	flag.Parse()
	if flag.NArg() != 0 {
		log.Fatalf("unexpected arguments: %v", flag.Args())
	}

	s := &server{model: *model, weightsVersion: *weights, engine: decision.Synthetic{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/systemone", s.ServeSystemOne)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	log.Printf("context-decision listening on http://%s/v1/systemone (model=%s weights=%s, deterministic sandbox judgments)", *addr, *model, *weights)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatal(err)
	}
}
