package main

import (
	"strings"

	"recordingmcp/internal/decision"
)

// Worldline inputs include private source excerpts. This first integration is
// explicitly local: no proxies, remote hosts or redirects carrying those inputs.
func localWorldlineDecision(endpoint, model string) (decision.Engine, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return nil, nil
	}
	return decision.NewLocalSystemOneClient(endpoint, model)
}
