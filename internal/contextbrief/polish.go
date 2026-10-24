package contextbrief

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	polishMaxEntries    = 40
	polishMaxTextBytes  = 8192
	polishMaxTotalBytes = 64 << 10
)

type PolishCandidate struct {
	EntryID        string   `json:"entry_id"`
	Text           string   `json:"text"`
	EvidenceIDs    []string `json:"evidence_ids"`
	AssertionState string   `json:"assertion_state"`
}

type PolishResult struct {
	SnapshotID string            `json:"snapshot_id"`
	Candidates []PolishCandidate `json:"candidates"`
	Degraded   bool              `json:"degraded"`
	Reason     string            `json:"reason,omitempty"`
}

type Polisher interface {
	Polish(context.Context, Result) ([]PolishCandidate, error)
}

// ValidatePolishCandidates checks candidates against the exact deterministic brief.
func ValidatePolishCandidates(base Result, candidates []PolishCandidate) error {
	if len(candidates) > polishMaxEntries {
		return errors.New("polish: too many candidates")
	}
	entries := map[string]Entry{}
	for _, sec := range [][]Entry{base.Sections.Background, base.Sections.PreviousDiscussions, base.Sections.FollowUpCandidates, base.Sections.ChangesConflicts, base.Sections.Unknowns} {
		for _, e := range sec {
			entries[e.ID] = e
		}
	}
	evidence := make(map[string]bool, len(base.Evidence))
	for _, e := range base.Evidence {
		evidence[e.ID] = e.SourceState == "current"
	}
	seen := map[string]bool{}
	total := 0
	for _, c := range candidates {
		e, ok := entries[c.EntryID]
		if !ok || seen[c.EntryID] || e.SourceState != "current" {
			return errors.New("polish: unknown or duplicate entry")
		}
		seen[c.EntryID] = true
		if c.AssertionState != "proposed" {
			return errors.New("polish: invalid assertion state")
		}
		if strings.TrimSpace(c.Text) == "" || len(c.Text) > polishMaxTextBytes {
			return errors.New("polish: invalid text length")
		}
		total += len(c.Text)
		if total > polishMaxTotalBytes {
			return errors.New("polish: total text limit exceeded")
		}
		if !sameStringSet(c.EvidenceIDs, e.EvidenceIDs) {
			return errors.New("polish: evidence mismatch")
		}
		if len(c.EvidenceIDs) == 0 {
			return errors.New("polish: evidence required")
		}
		for _, id := range c.EvidenceIDs {
			if !evidence[id] {
				return errors.New("polish: evidence unavailable")
			}
		}
	}
	return nil
}

func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]string(nil), a...)
	y := append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if i > 0 && (x[i] == x[i-1] || y[i] == y[i-1]) {
			return false
		}
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

// OpenAICompatPolisher calls a single explicitly configured chat-completions endpoint.
// It never follows redirects, bounds input/output and returns sanitized failures.
type OpenAICompatPolisher struct {
	Endpoint   string
	APIKey     string
	Model      string
	HTTPClient *http.Client
}

// HTTPPolisher is the concise name used by optional application wiring.
type HTTPPolisher = OpenAICompatPolisher

type polishRequest struct {
	Model    string          `json:"model"`
	Messages []polishMessage `json:"messages"`
}
type polishMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}
type polishWire struct {
	Candidates []polishWireCandidate `json:"candidates"`
}
type polishWireCandidate struct {
	EntryID     string   `json:"entry_id"`
	Text        string   `json:"text"`
	EvidenceIDs []string `json:"evidence_ids"`
}

func (p *OpenAICompatPolisher) Polish(ctx context.Context, base Result) ([]PolishCandidate, error) {
	if p == nil {
		return nil, errors.New("polish: unavailable")
	}
	u, err := url.Parse(strings.TrimSpace(p.Endpoint))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, errors.New("polish: invalid configured endpoint")
	}
	inputs := make([]Entry, 0)
	for _, sec := range [][]Entry{base.Sections.Background, base.Sections.PreviousDiscussions, base.Sections.FollowUpCandidates, base.Sections.ChangesConflicts, base.Sections.Unknowns} {
		inputs = append(inputs, sec...)
	}
	if len(inputs) > polishMaxEntries {
		return nil, errors.New("polish: input entry limit exceeded")
	}
	in, err := json.Marshal(inputs)
	if err != nil || len(in) > polishMaxTotalBytes {
		return nil, errors.New("polish: input size limit exceeded")
	}
	prompt := "Polish wording only. Return JSON {\"candidates\":[{\"entry_id\":string,\"text\":string,\"evidence_ids\":[string]}]}. Include at most one candidate per entry. Preserve meaning; cite exactly the entry's evidence_ids. Do not output other keys. Input entries:\n" + string(in)
	body, err := json.Marshal(polishRequest{Model: p.Model, Messages: []polishMessage{{Role: "system", Content: "You edit wording without adding facts. Output strict JSON only."}, {Role: "user", Content: prompt}}})
	if err != nil || len(body) > polishMaxTotalBytes {
		return nil, errors.New("polish: request size limit exceeded")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("polish: request unavailable")
	}
	req.Header.Set("Content-Type", "application/json")
	if strings.TrimSpace(p.APIKey) != "" {
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
	}
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if p.HTTPClient != nil {
		client.Transport = p.HTTPClient.Transport
		if p.HTTPClient.Timeout > 0 && p.HTTPClient.Timeout < client.Timeout {
			client.Timeout = p.HTTPClient.Timeout
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("polish: endpoint unavailable")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, polishMaxTotalBytes+1))
	if err != nil || len(data) > polishMaxTotalBytes {
		return nil, errors.New("polish: invalid response size")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, errors.New("polish: endpoint returned an error")
	}
	var outer struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(data, &outer) != nil || len(outer.Choices) == 0 {
		return nil, errors.New("polish: invalid response")
	}
	dec := json.NewDecoder(strings.NewReader(outer.Choices[0].Message.Content))
	dec.DisallowUnknownFields()
	var parsed polishWire
	if dec.Decode(&parsed) != nil {
		return nil, errors.New("polish: invalid candidates")
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return nil, errors.New("polish: invalid candidates")
	}
	candidates := make([]PolishCandidate, 0, len(parsed.Candidates))
	for _, c := range parsed.Candidates {
		candidates = append(candidates, PolishCandidate{EntryID: c.EntryID, Text: c.Text, EvidenceIDs: c.EvidenceIDs, AssertionState: "proposed"})
	}
	if err := ValidatePolishCandidates(base, candidates); err != nil {
		return nil, errors.New("polish: candidate validation failed")
	}
	return candidates, nil
}
