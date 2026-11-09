package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"recordingmcp/internal/app"
	"recordingmcp/internal/asrinput"
	"recordingmcp/internal/memory"
	"recordingmcp/internal/plane"
)

type decisionHTTPServer struct {
	server   *Server
	http     *httptest.Server
	cookie   *http.Cookie
	csrf     string
	store    *memory.Store
	events   *plane.Service
	password string
}

func newDecisionHTTPServer(t *testing.T, name, password string) *decisionHTTPServer {
	t.Helper()
	dir := t.TempDir()
	store, err := memory.Open(filepath.Join(dir, "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	audioDir := filepath.Join(dir, "audio")
	start := int64(0)
	end := int64(2000)
	result := asrinput.Result{
		ContractVersion: asrinput.ContractVersion,
		Source: asrinput.Source{
			Provider: "provider-x", ProviderRequestID: "req-seg-a", DeviceID: "dev-1",
			RecordingID: "rec-1", ConversationID: "conv-1", SegmentID: "seg-a",
			SegmentOffsetMS: 0, SegmentDurationMS: 3000, TranscriptRevision: "rev-1",
			CompletionStatus: "final",
		},
		Turns: []asrinput.Turn{{ID: "t-1", OriginalIndex: 1, Text: "先讨论方案", LocalStartMS: &start, LocalEndMS: &end, GlobalStartMS: &start, GlobalEndMS: &end}},
	}
	report, err := store.IngestTranscript(context.Background(), result)
	if err != nil {
		t.Fatal(err)
	}
	ann := memory.Annotations{
		Version: 1, ConversationID: report.ConversationID, Title: "决策测试", RecordingID: report.RecordingID,
		AnnotationVersion: "v1", AnnotationBasis: "人工解释；未核实", Summary: "决策",
		Topics: []memory.TopicAnnotation{{ID: "topic:a", Title: "Atlas 项目"}},
		Notes: []memory.NoteAnnotation{{
			ID: "note:one", Kind: "suggestion", TopicIDs: []string{"topic:a"}, Summary: "最终决定采用方案 A",
			EvidenceTurnIDs: []string{"seg-a/rev-1/t-1"},
		}},
	}
	if err := store.ApplyAnnotations(context.Background(), report.ConversationID, "", ann); err != nil {
		t.Fatal(err)
	}
	events, err := plane.Open(filepath.Join(dir, "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { events.Close() })
	timeSvc := &app.TimeService{Store: store, Events: events}
	worldline := &app.WorldlineService{Time: timeSvc}
	server, err := New(Config{
		Service:     app.New(store, nil, nil, audioDir),
		TimeService: timeSvc, Worldline: worldline,
		DecisionTrail:         &app.DecisionTrailService{Worldline: worldline, Time: timeSvc},
		ProfileName:           name,
		VerifyProfilePassword: func(candidate string) bool { return candidate == password },
	})
	if err != nil {
		t.Fatal(err)
	}
	env := &decisionHTTPServer{server: server, http: httptest.NewServer(server.Handler()), store: store, events: events, password: password}
	t.Cleanup(env.http.Close)

	resp := env.raw(t, http.MethodPost, "/api/login", `{"password":"`+password+`"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("login: %d %s", resp.StatusCode, body)
	}
	var body struct {
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	env.csrf = body.CSRFToken
	for _, cookie := range resp.Cookies() {
		if cookie.Name == sessionCookieName {
			env.cookie = cookie
		}
	}
	if env.cookie == nil {
		t.Fatal("no session cookie")
	}
	return env
}

func (e *decisionHTTPServer) raw(t *testing.T, method, path, body string) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, e.http.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Origin", e.http.URL)
	if e.csrf != "" {
		req.Header.Set("X-CSRF-Token", e.csrf)
	}
	if e.cookie != nil {
		req.AddCookie(e.cookie)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func (e *decisionHTTPServer) decisions(t *testing.T) app.DecisionTrail {
	t.Helper()
	resp := e.raw(t, http.MethodGet, "/api/decisions", "")
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("decisions: %d %s", resp.StatusCode, data)
	}
	var trail app.DecisionTrail
	if err := json.Unmarshal(data, &trail); err != nil {
		t.Fatal(err)
	}
	return trail
}

func (e *decisionHTTPServer) review(t *testing.T, body map[string]any) (int, []byte) {
	t.Helper()
	raw, _ := json.Marshal(body)
	resp := e.raw(t, http.MethodPost, "/api/decisions/review", string(raw))
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

func TestDecisionTrailProfileIsolation(t *testing.T) {
	alpha := newDecisionHTTPServer(t, "alpha", "alpha-password-1")
	beta := newDecisionHTTPServer(t, "beta", "beta-password-1")

	trail := alpha.decisions(t)
	if len(trail.Topics) != 1 || trail.Topics[0].CurrentState != app.DecisionCandidate {
		t.Fatalf("alpha initial trail: %+v", trail)
	}
	topic := trail.Topics[0]
	var candidate string
	for _, record := range topic.History {
		if record.State == app.DecisionCandidate {
			candidate = record.CandidateID
		}
	}
	if candidate == "" {
		t.Fatalf("no candidate: %+v", topic.History)
	}
	status, data := alpha.review(t, map[string]any{
		"expected_version": trail.Version, "candidate_id": candidate, "action": "confirm",
		"request_id": "alpha-request-1", "reviewer": "alpha-user",
	})
	if status != http.StatusOK {
		t.Fatalf("alpha review: %d %s", status, data)
	}
	if got := alpha.decisions(t); got.Topics[0].CurrentState != app.DecisionConfirmed {
		t.Fatalf("alpha current=%s", got.Topics[0].CurrentState)
	}

	// Beta has its own database and event plane: alpha's confirmation must not
	// appear at all.
	betaTrail := beta.decisions(t)
	if len(betaTrail.Topics) != 1 {
		t.Fatalf("beta topics=%d", len(betaTrail.Topics))
	}
	if betaTrail.Topics[0].CurrentState != app.DecisionCandidate || betaTrail.Topics[0].Counts.Confirmed != 0 {
		t.Fatalf("beta saw alpha's review: %+v", betaTrail.Topics[0])
	}
	for _, record := range betaTrail.Topics[0].History {
		if record.Reviewer == "alpha-user" {
			t.Fatalf("alpha review leaked into beta: %+v", record)
		}
	}
}

func TestDecisionReviewHTTPErrors(t *testing.T) {
	env := newDecisionHTTPServer(t, "alpha", "alpha-password-1")
	trail := env.decisions(t)
	topic := trail.Topics[0]
	var candidate string
	for _, record := range topic.History {
		if record.State == app.DecisionCandidate {
			candidate = record.CandidateID
		}
	}
	// Stale version maps to 409 with an action.
	status, data := env.review(t, map[string]any{
		"expected_version": "wl-stale", "candidate_id": candidate, "action": "confirm", "request_id": "req-stale",
	})
	if status != http.StatusConflict || !bytes.Contains(data, []byte("decision_version_conflict")) {
		t.Fatalf("stale version: %d %s", status, data)
	}
	// Unknown candidate maps to 404.
	status, data = env.review(t, map[string]any{
		"expected_version": trail.Version, "candidate_id": "decision-candidate-missing", "action": "confirm", "request_id": "req-missing",
	})
	if status != http.StatusNotFound || !bytes.Contains(data, []byte("decision_not_found")) {
		t.Fatalf("unknown candidate: %d %s", status, data)
	}
	// Unknown fields are rejected.
	resp := env.raw(t, http.MethodPost, "/api/decisions/review", `{"candidate_id":"x","unexpected":true}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown field: %d", resp.StatusCode)
	}
}
