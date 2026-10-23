package main

import "testing"

func TestWorldlineDecisionStaysLocal(t *testing.T) {
	for _, endpoint := range []string{"https://api.typesafe.ai/v1/systemone", "http://example.org/v1/systemone", "http://localhost/v1/systemone", "http://127.0.0.1@remote.invalid/", "http://127.0.0.1/?key=private"} {
		if _, err := localWorldlineDecision(endpoint, "model"); err == nil {
			t.Errorf("accepted non-local or unsafe endpoint %q", endpoint)
		}
	}
	for _, endpoint := range []string{"http://127.0.0.1:8799/v1/systemone", "http://[::1]:8799/v1/systemone"} {
		if _, err := localWorldlineDecision(endpoint, "model"); err != nil {
			t.Errorf("rejected loopback: %v", err)
		}
	}
	if engine, err := localWorldlineDecision("", ""); err != nil || engine != nil {
		t.Fatal("unconfigured engine must remain disabled")
	}
}
