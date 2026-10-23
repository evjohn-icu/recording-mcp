package main

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"recordingmcp/internal/decision"
)

// Worldline inputs include private source excerpts. This first integration is
// explicitly local: no proxies, remote hosts or redirects carrying those inputs.
func localWorldlineDecision(endpoint, model string) (decision.Engine, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return nil, nil
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("worldline decision endpoint must be a loopback HTTP URL without credentials, query or fragment")
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil || !ip.IsLoopback() {
		return nil, errors.New("worldline decision endpoint requires a literal loopback IP (127.0.0.1 or ::1)")
	}
	if strings.TrimSpace(model) == "" {
		return nil, errors.New("worldline decision model is required")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	client := &http.Client{Transport: transport, Timeout: 45 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return &decision.SystemOneClient{Endpoint: endpoint, Model: model, HTTPClient: client}, nil
}
