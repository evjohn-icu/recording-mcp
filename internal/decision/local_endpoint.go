package decision

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// NewLocalSystemOneClient creates the private-input-safe local JEV transport.
// It rejects proxies, redirects, remote hosts and URL credentials/query data.
func NewLocalSystemOneClient(endpoint, model string) (*SystemOneClient, error) {
	endpoint = strings.TrimSpace(endpoint)
	u, err := url.Parse(endpoint)
	if endpoint == "" || err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("decision endpoint must be a loopback HTTP URL without credentials, query or fragment")
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil || !ip.IsLoopback() {
		return nil, errors.New("decision endpoint requires a literal loopback IP (127.0.0.1 or ::1)")
	}
	if strings.TrimSpace(model) == "" {
		return nil, errors.New("decision model is required")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	client := &http.Client{Transport: transport, Timeout: 45 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &SystemOneClient{Endpoint: endpoint, Model: model, HTTPClient: client}, nil
}
