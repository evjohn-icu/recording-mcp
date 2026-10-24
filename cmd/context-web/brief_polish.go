package main

import (
	"errors"
	"net"
	"net/url"
	"os"
	"strings"

	"recordingmcp/internal/contextbrief"
)

// Model destinations are trusted startup configuration, never HTTP request data.
// Plain HTTP is restricted to literal loopback addresses or localhost.
func configuredBriefPolisher(endpoint, model, keyEnv string) (contextbrief.Polisher, error) {
	endpoint = strings.TrimSpace(endpoint)
	model = strings.TrimSpace(model)
	keyEnv = strings.TrimSpace(keyEnv)
	if endpoint == "" {
		if model != "" || keyEnv != "" {
			return nil, errors.New("brief polish: endpoint is required when model settings are supplied")
		}
		return nil, nil
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, errors.New("brief polish: invalid endpoint configuration")
	}
	if u.Scheme == "http" {
		ip := net.ParseIP(u.Hostname())
		if u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return nil, errors.New("brief polish: remote endpoint requires HTTPS")
		}
	}
	if model == "" {
		return nil, errors.New("brief polish: model is required")
	}
	key := ""
	if keyEnv != "" {
		key = os.Getenv(keyEnv)
		if strings.TrimSpace(key) == "" {
			return nil, errors.New("brief polish: configured key environment variable is empty")
		}
	}
	return &contextbrief.HTTPPolisher{Endpoint: endpoint, Model: model, APIKey: key}, nil
}
