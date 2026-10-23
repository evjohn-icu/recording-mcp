package decision

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ModelIdentity reads the pinned model card from the same trusted loopback
// provider used for pair inference. It follows no proxy or redirect and does
// not send input data. A provider without this health contract must be probed
// by the inference path before any cached result is reused.
func (c *SystemOneClient) ModelIdentity(ctx context.Context) (string, string, error) {
	if c == nil {
		return "", "", errors.New("decision: client is nil")
	}
	u, err := url.Parse(strings.TrimSpace(c.Endpoint))
	if err != nil || u == nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", "", errors.New("decision: model identity requires a loopback HTTP endpoint")
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil || !ip.IsLoopback() {
		return "", "", errors.New("decision: model identity requires a loopback HTTP endpoint")
	}
	health := &url.URL{Scheme: "http", Host: u.Host, Path: "/healthz"}
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, health.String(), nil)
	if err != nil {
		return "", "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", errors.New("decision: model identity endpoint unavailable")
	}
	var card struct {
		Model          string `json:"model"`
		WeightsVersion string `json:"weights_version"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<10)).Decode(&card); err != nil {
		return "", "", err
	}
	model, weights := strings.TrimSpace(card.Model), strings.TrimSpace(card.WeightsVersion)
	if model == "" || weights == "" {
		return "", "", errors.New("decision: healthz response has no model identity")
	}
	if c.WeightsVersion != "" && strings.TrimSpace(c.WeightsVersion) != weights {
		return "", "", errors.New("decision: healthz weights do not match configured weights")
	}
	return model, weights, nil
}
