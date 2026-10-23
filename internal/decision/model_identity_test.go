package decision

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestModelIdentityReadsTrustedLoopbackHealthCard(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("health method=%s", r.Method)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"model": "local-model", "weights_version": "weights-7"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	client := &SystemOneClient{Endpoint: srv.URL + "/v1/systemone", Model: "decision-v3-alias"}
	model, weights, err := client.ModelIdentity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if model != "local-model" || weights != "weights-7" {
		t.Fatalf("identity=%q/%q", model, weights)
	}
}

func TestModelIdentityDoesNotProbeRemoteEndpoint(t *testing.T) {
	client := &SystemOneClient{Endpoint: "http://example.com/v1/systemone", Model: "remote"}
	if _, _, err := client.ModelIdentity(context.Background()); err == nil {
		t.Fatal("remote endpoint identity probe was accepted")
	}
}
