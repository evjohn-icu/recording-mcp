// Package generate turns a rendered conversation state into draft memory
// candidates. Adapters (synthetic, OpenAI-compatible chat completions)
// implement Generator; the pipeline validates the returned drafts before they
// are persisted. Drafts are never trusted: callers must check the kind,
// summary and evidence turn IDs against the window they were generated for.
package generate

import "context"

// Kinds enumerates the candidate kinds accepted by the memory contract. A
// draft whose Kind is not in this list is rejected by the pipeline.
var Kinds = []string{"observation", "suggestion", "request", "constraint", "intention"}

// ValidKind reports whether kind is one of the accepted candidate kinds.
func ValidKind(kind string) bool {
	for _, k := range Kinds {
		if k == kind {
			return true
		}
	}
	return false
}

// Draft is one unvalidated memory candidate proposed by a Generator.
type Draft struct {
	Kind               string   `json:"kind"`
	Summary            string   `json:"summary"`
	TopicTitle         string   `json:"topic"`
	EvidenceTurnIDs    []string `json:"evidence_turn_ids"`
	InterpretationNote string   `json:"interpretation_note"`
}

// Generator proposes memory candidates for one rendered state. evidenceTurnIDs
// is the closed set of turn IDs the state was rendered from; every returned
// draft must cite a non-empty subset of it. The second return value is the
// model identifier that produced the drafts.
type Generator interface {
	Generate(ctx context.Context, state string, evidenceTurnIDs []string) ([]Draft, string, error)
}
