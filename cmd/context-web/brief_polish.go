package main

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
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

// configuredPiBriefPolisher reads only startup-selected provider configuration.
// No Pi sessions, auth stores, commands or user source databases are accessed.
func configuredPiBriefPolisher(path, provider, model string) (contextbrief.Polisher, error) {
	if strings.TrimSpace(provider) == "" || strings.TrimSpace(model) == "" {
		return nil, errors.New("brief polish: Pi provider and model are required")
	}
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, errors.New("brief polish: Pi config location unavailable")
		}
		path = filepath.Join(home, ".pi", "agent", "models.json")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("brief polish: Pi models config unavailable")
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		return nil, errors.New("brief polish: invalid Pi config size")
	}
	var config struct {
		Providers map[string]struct {
			API     string `json:"api"`
			BaseURL string `json:"baseUrl"`
			APIKey  string `json:"apiKey"`
			Models  []struct {
				ID  string `json:"id"`
				API string `json:"api"`
			} `json:"models"`
		} `json:"providers"`
	}
	if json.Unmarshal(raw, &config) != nil {
		return nil, errors.New("brief polish: invalid Pi models config")
	}
	selected, ok := config.Providers[provider]
	if !ok {
		return nil, errors.New("brief polish: selected Pi provider not found")
	}
	api := selected.API
	found := false
	for _, m := range selected.Models {
		if m.ID == model {
			found = true
			if m.API != "" {
				api = m.API
			}
			break
		}
	}
	if !found {
		return nil, errors.New("brief polish: model not listed for selected Pi provider")
	}
	// Responses-labelled gateways may also expose chat completions; explicit
	// selection opts into this compatibility path, not a Responses implementation.
	if api != "openai-completions" && api != "openai-responses" {
		return nil, errors.New("brief polish: unsupported Pi API mode; chat-completions compatibility required")
	}
	key := strings.TrimSpace(selected.APIKey)
	if strings.HasPrefix(key, "!") {
		return nil, errors.New("brief polish: command-based keys are unsupported")
	}
	if value, exists := os.LookupEnv(key); exists {
		key = value
	} else if regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`).MatchString(key) {
		return nil, errors.New("brief polish: configured Pi key environment variable is unset")
	}
	if key == "" {
		return nil, errors.New("brief polish: Pi API key is empty")
	}
	u, err := url.Parse(selected.BaseURL)
	if err != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("brief polish: invalid Pi endpoint")
	}
	endpoint := strings.TrimRight(selected.BaseURL, "/") + "/chat/completions"
	p, err := configuredBriefPolisher(endpoint, model, "")
	if err != nil {
		return nil, err
	}
	p.(*contextbrief.HTTPPolisher).APIKey = key
	return p, nil
}
