package app

import (
	"context"
	"time"

	"recordingmcp/internal/plane"
)

// appendWorldlineMutation places a version guard and the mutation in one event
// plane transaction. A unique guard ID makes expected_version a cross-process
// compare-and-swap for every client using this helper.
func (s *WorldlineService) appendWorldlineMutation(ctx context.Context, scope, expectedVersion, payloadHash string, event plane.Event) error {
	now := time.Now().UTC()
	guard := plane.Event{ID: "wl-mutation-guard-" + shortHash(scope+"\x00"+expectedVersion), Type: "worldline.mutation_guard", Timestamp: now, ObservedAt: now, Source: "local-web", Content: "worldline mutation version guard", Confidence: 1, StructuredData: map[string]any{"scope": scope, "origin": "worldline", "expected_version": expectedVersion, "payload_hash": payloadHash}}
	_, err := s.Time.Events.Ingest(ctx, []plane.Event{guard, event})
	return err
}
