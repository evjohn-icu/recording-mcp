package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"recordingmcp/internal/contextbrief"
	"strings"
	"testing"
)

func TestBriefPolisherConfigurationIsExplicit(t *testing.T) {
	p, e := configuredBriefPolisher("", "", "")
	if e != nil || p != nil {
		t.Fatal("default enabled", e)
	}
	for _, endpoint := range []string{"file:///tmp/model", "http://example.com/model", "https://user:secret@example.com/model", "https://example.com/model#token"} {
		if _, e = configuredBriefPolisher(endpoint, "model", ""); e == nil {
			t.Errorf("unsafe endpoint accepted: %s", endpoint)
		}
	}
	if _, e = configuredBriefPolisher("http://127.0.0.1:9876/v1/chat/completions", "test", ""); e != nil {
		t.Fatal(e)
	}
	if _, e = configuredBriefPolisher("https://models.example.test/completions", "test", ""); e != nil {
		t.Fatal(e)
	}
	if _, e = configuredBriefPolisher("https://models.example.test/completions", "", ""); e == nil {
		t.Fatal("missing model allowed")
	}
	t.Setenv("BRIEF_TEST_EMPTY_KEY", "")
	if _, e = configuredBriefPolisher("https://models.example.test/completions", "test", "BRIEF_TEST_EMPTY_KEY"); e == nil {
		t.Fatal("missing key allowed")
	}
}

func TestBriefPolisherPiConfigUsesOnlyExplicitProvider(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	write := func(key string) {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"providers": map[string]any{"fixture": map[string]any{"api": "openai-responses", "baseUrl": "https://model.example.test/v1", "apiKey": key, "models": []map[string]string{{"id": "synthetic"}}}}})
		if e := os.WriteFile(path, raw, 0600); e != nil {
			t.Fatal(e)
		}
	}
	write("synthetic-secret")
	p, e := configuredPiBriefPolisher(path, "fixture", "synthetic")
	if e != nil {
		t.Fatal(e)
	}
	h := p.(*contextbrief.HTTPPolisher)
	if h.Endpoint != "https://model.example.test/v1/chat/completions" || h.APIKey != "synthetic-secret" || h.Model != "synthetic" {
		t.Fatal("wrong resolved configuration")
	}
	if _, e = configuredPiBriefPolisher(path, "missing", "synthetic"); e == nil {
		t.Fatal("missing provider allowed")
	}
	if _, e = configuredPiBriefPolisher(path, "fixture", "unlisted"); e == nil {
		t.Fatal("unlisted model allowed")
	}
	write("!touch should-never-execute")
	if _, e = configuredPiBriefPolisher(path, "fixture", "synthetic"); e == nil || strings.Contains(e.Error(), "touch") {
		t.Fatal("command key executed or exposed")
	}
	write("SYNTHETIC_PI_KEY_ENV")
	t.Setenv("SYNTHETIC_PI_KEY_ENV", "resolved-secret")
	p, e = configuredPiBriefPolisher(path, "fixture", "synthetic")
	if e != nil || p.(*contextbrief.HTTPPolisher).APIKey != "resolved-secret" {
		t.Fatal("env reference unresolved", e)
	}
	if e = os.WriteFile(path, []byte(`{"bad":"synthetic-secret"`), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = configuredPiBriefPolisher(path, "fixture", "synthetic"); e == nil || strings.Contains(e.Error(), "synthetic-secret") {
		t.Fatal("invalid config exposed")
	}
}

func TestBriefPolisherPiRejectsUnsupportedProtocol(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	for _, raw := range []string{
		`{"providers":{"p":{"api":"anthropic-messages","baseUrl":"https://example.test/v1","apiKey":"synthetic-secret","models":[{"id":"test"}]}}}`,
		`{"providers":{"p":{"api":"openai-completions","baseUrl":"https://example.test/v1","apiKey":"synthetic-secret","models":[{"id":"test","api":"anthropic-messages"}]}}}`,
	} {
		if e := os.WriteFile(path, []byte(raw), 0600); e != nil {
			t.Fatal(e)
		}
		if _, e := configuredPiBriefPolisher(path, "p", "test"); e == nil {
			t.Fatal("unsupported protocol accepted")
		}
	}
}
