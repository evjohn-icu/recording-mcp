package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"recordingmcp/internal/decision"
)

// TestSystemOneContractRoundTrip proves the sandbox answers the exact wire
// contract SystemOneClient expects, for every question type, and that the
// client's strict validation accepts the result.
func TestSystemOneContractRoundTrip(t *testing.T) {
	s := &server{model: "test-model", weightsVersion: "sandbox-test", engine: decision.Synthetic{}}
	srv := httptest.NewServer(http.HandlerFunc(s.ServeSystemOne))
	t.Cleanup(srv.Close)

	client := &decision.SystemOneClient{Endpoint: srv.URL, Model: "test-model"}
	questions := []decision.Question{
		{ID: "pick", Type: decision.TypeChoice, Instructions: "choose", Criteria: map[string]string{"a": "when a", "b": "when b"}},
		{ID: "yesno", Type: decision.TypeNoul, Instructions: "is it"},
		{ID: "rate", Type: decision.TypeScore, Instructions: "rate it", Rubric: []string{"low", "mid", "high"}},
	}
	result, err := client.Evaluate(t.Context(), "some state", questions)
	if err != nil {
		t.Fatalf("client rejected sandbox response: %v", err)
	}
	if result.Model != "test-model" || result.WeightsVersion != "sandbox-test" {
		t.Fatalf("identity not reported: %+v", result)
	}
	if len(result.Answers) != 3 {
		t.Fatalf("answers = %v", result.Answers)
	}
	if a := result.Answers["pick"]; a.Choice == "" || len(a.Distribution) != 2 {
		t.Fatalf("choice answer = %+v", a)
	}
	if a := result.Answers["yesno"]; a.Noul < 0 || a.Noul > 1 || len(a.Distribution) != 2 {
		t.Fatalf("noul answer = %+v", a)
	}
	if a := result.Answers["rate"]; len(a.Distribution) != 3 {
		t.Fatalf("score answer = %+v", a)
	}
}

// TestSystemOneRejectsMalformedRequests pins the error surface.
func TestSystemOneRejectsMalformedRequests(t *testing.T) {
	s := &server{model: "test-model", weightsVersion: "sandbox-test", engine: decision.Synthetic{}}
	srv := httptest.NewServer(http.HandlerFunc(s.ServeSystemOne))
	t.Cleanup(srv.Close)

	if resp, err := http.Get(srv.URL); err != nil || resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET = %v %v, want 405", resp, err)
	}
	resp, err := http.Post(srv.URL, "application/json", strings.NewReader(`{"state":"","questions":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty state = %d, want 400", resp.StatusCode)
	}
	resp, err = http.Post(srv.URL, "application/json", strings.NewReader(`{"state":"x","questions":{"q":{"type":"nope","instructions":"i"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("unsupported type = %d, want 422", resp.StatusCode)
	}
	// Unknown request fields are rejected loudly, mirroring the client's strictness.
	resp, err = http.Post(srv.URL, "application/json", strings.NewReader(`{"state":"x","questions":{},"bogus":1}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown field = %d, want 400", resp.StatusCode)
	}
}

// TestSystemOneWireShape checks the serialized union fields directly so a
// refactor cannot silently change the documented service shape.
func TestSystemOneWireShape(t *testing.T) {
	s := &server{model: "m", weightsVersion: "w", engine: decision.Synthetic{}}
	srv := httptest.NewServer(http.HandlerFunc(s.ServeSystemOne))
	t.Cleanup(srv.Close)

	body := `{"model":"m","state":"st","questions":{"q":{"type":"noul","instructions":"i"}}}`
	resp, err := http.Post(srv.URL, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var wire struct {
		Model          string `json:"model"`
		WeightsVersion string `json:"weights_version"`
		Answers        map[string]struct {
			Type          string          `json:"type"`
			Noul          *float64        `json:"noul"`
			Confidence    float64         `json:"confidence"`
			Probabilities json.RawMessage `json:"probabilities"`
		} `json:"answers"`
		Usage struct {
			LatencyMS int64 `json:"latency_ms"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&wire); err != nil {
		t.Fatal(err)
	}
	a := wire.Answers["q"]
	if a.Type != "noul" || a.Noul == nil || a.Confidence <= 0 || len(a.Probabilities) == 0 {
		t.Fatalf("wire answer shape = %+v", a)
	}
	if wire.Model != "m" || wire.WeightsVersion != "w" {
		t.Fatalf("wire identity = %q/%q", wire.Model, wire.WeightsVersion)
	}
}
