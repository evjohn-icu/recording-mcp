package contextbrief

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func polishFixture() Result {
	return Result{SnapshotID: "snap-1", Sections: Sections{Background: []Entry{{ID: "e1", Text: "原文", EvidenceIDs: []string{"ev1", "ev2"}, AssertionState: "unverified", SourceState: "current"}}}, Evidence: []Evidence{{ID: "ev1", SourceState: "current"}, {ID: "ev2", SourceState: "current"}}}
}

func TestValidatePolishCandidatesKeepsEntryAndEvidenceScope(t *testing.T) {
	base := polishFixture()
	good := []PolishCandidate{{EntryID: "e1", Text: "清晰表述", EvidenceIDs: []string{"ev2", "ev1"}, AssertionState: "proposed"}}
	if err := ValidatePolishCandidates(base, good); err != nil {
		t.Fatalf("valid candidate rejected: %v", err)
	}
	bad := []PolishCandidate{{EntryID: "e1", Text: "扩写", EvidenceIDs: []string{"ev1"}, AssertionState: "proposed"}}
	if err := ValidatePolishCandidates(base, bad); err == nil {
		t.Fatal("missing citation accepted")
	}
	bad = []PolishCandidate{{EntryID: "e1", Text: "扩写", EvidenceIDs: []string{"ev1", "ev2", "other"}, AssertionState: "proposed"}}
	if err := ValidatePolishCandidates(base, bad); err == nil {
		t.Fatal("widened citation accepted")
	}
	bad = []PolishCandidate{{EntryID: "e1", Text: "扩写", EvidenceIDs: []string{"ev1", "ev2"}, AssertionState: "rejected"}}
	if err := ValidatePolishCandidates(base, bad); err == nil {
		t.Fatal("state override accepted")
	}
	bad = []PolishCandidate{{EntryID: "e1", Text: "扩写", EvidenceIDs: []string{"ev1", "ev2"}, AssertionState: "proposed"}}
	bad = append(bad, bad[0])
	if err := ValidatePolishCandidates(base, bad); err == nil {
		t.Fatal("duplicate entry accepted")
	}
	bad = []PolishCandidate{{EntryID: "missing", Text: "text", EvidenceIDs: []string{"ev1", "ev2"}, AssertionState: "proposed"}}
	if err := ValidatePolishCandidates(base, bad); err == nil {
		t.Fatal("unknown entry accepted")
	}
	bad = []PolishCandidate{{EntryID: "e1", Text: "text", EvidenceIDs: []string{"ev1", "ev1"}, AssertionState: "proposed"}}
	if err := ValidatePolishCandidates(base, bad); err == nil {
		t.Fatal("duplicate evidence citation accepted")
	}
	missingEvidence := base
	missingEvidence.Evidence = base.Evidence[:1]
	if err := ValidatePolishCandidates(missingEvidence, good); err == nil {
		t.Fatal("evidence absent from current brief accepted")
	}
	bad = []PolishCandidate{{EntryID: "e1", Text: strings.Repeat("x", polishMaxTextBytes+1), EvidenceIDs: []string{"ev1", "ev2"}, AssertionState: "proposed"}}
	if err := ValidatePolishCandidates(base, bad); err == nil {
		t.Fatal("oversized text accepted")
	}
	if err := ValidatePolishCandidates(base, make([]PolishCandidate, polishMaxEntries+1)); err == nil {
		t.Fatal("candidate count limit not enforced")
	}
}

func TestOpenAICompatPolisherStrictWireAndSafeFailure(t *testing.T) {
	base := polishFixture()
	originalJSON, _ := json.Marshal(base)
	var posted map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&posted); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"candidates\":[{\"entry_id\":\"e1\",\"text\":\"整理后\",\"evidence_ids\":[\"ev1\",\"ev2\"]}] }"}}]}`))
	}))
	defer srv.Close()
	p := &OpenAICompatPolisher{Endpoint: srv.URL + "/v1/chat/completions", Model: "test", HTTPClient: srv.Client()}
	cands, err := p.Polish(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidatePolishCandidates(base, cands); err != nil {
		t.Fatal(err)
	}
	if cands[0].AssertionState != "proposed" {
		t.Fatalf("state was not forced to proposed: %#v", cands[0])
	}
	if cands[0].Text != "整理后" {
		t.Fatalf("unexpected candidate: %#v", cands[0])
	}
	if len(posted) == 0 {
		t.Fatal("request was not sent")
	}
	afterJSON, _ := json.Marshal(base)
	if !reflect.DeepEqual(originalJSON, afterJSON) {
		t.Fatal("polisher mutated base brief")
	}

	badServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"candidates\":[{\"entry_id\":\"e1\",\"text\":\"伪造\",\"evidence_ids\":[\"ev1\"],\"assertion_state\":\"confirmed\",\"state\":\"accepted\"}] }"}}]}`))
	}))
	defer badServer.Close()
	p.Endpoint = badServer.URL
	_, err = p.Polish(context.Background(), base)
	if err == nil {
		t.Fatal("invalid response accepted")
	}
	if strings.Contains(err.Error(), "伪造") || strings.Contains(err.Error(), "state") {
		t.Fatalf("error leaked response content: %v", err)
	}
}

func TestHTTPPolisherBoundsRedirectsAndCanceledContext(t *testing.T) {
	base := polishFixture()
	t.Run("response limit", func(t *testing.T) {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(strings.Repeat("x", polishMaxTotalBytes+1)))
		}))
		defer s.Close()
		p := &HTTPPolisher{Endpoint: s.URL, HTTPClient: s.Client()}
		if _, err := p.Polish(context.Background(), base); err == nil {
			t.Fatal("oversized response accepted")
		}
	})
	t.Run("no redirect", func(t *testing.T) {
		called := false
		dst := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true; w.WriteHeader(500) }))
		defer dst.Close()
		src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, dst.URL, http.StatusFound) }))
		defer src.Close()
		p := &HTTPPolisher{Endpoint: src.URL, HTTPClient: src.Client()}
		if _, err := p.Polish(context.Background(), base); err == nil {
			t.Fatal("redirect response accepted")
		}
		if called {
			t.Fatal("redirect was followed")
		}
	})
	t.Run("canceled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		p := &HTTPPolisher{Endpoint: "http://127.0.0.1:1", HTTPClient: &http.Client{}}
		if _, err := p.Polish(ctx, base); err == nil {
			t.Fatal("canceled request succeeded")
		}
	})
}

func TestPolishRejectsSupersededOrUnavailableEvidence(t *testing.T) {
	for _, state := range []string{"superseded", "unavailable", ""} {
		base := polishFixture()
		base.Evidence[0].SourceState = state
		drafts := []PolishCandidate{{EntryID: "e1", Text: "candidate", EvidenceIDs: []string{"ev1", "ev2"}, AssertionState: "proposed"}}
		if ValidatePolishCandidates(base, drafts) == nil {
			t.Fatalf("accepted evidence state %q", state)
		}
	}
	base := polishFixture()
	base.Sections.Background[0].SourceState = "superseded"
	if ValidatePolishCandidates(base, []PolishCandidate{{EntryID: "e1", Text: "candidate", EvidenceIDs: []string{"ev1", "ev2"}, AssertionState: "proposed"}}) == nil {
		t.Fatal("accepted superseded entry")
	}
}
