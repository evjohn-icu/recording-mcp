package eval

import (
	"context"
	"time"

	"recordingmcp/internal/decision"
)

// EngineTiming wraps a decision.Engine and records the wall time of every
// Evaluate call made since the last Reset.
//
// It exists for the offline profile only: the v3 latency decision needs the
// per-query time spent inside the assessor engine, which subtracting stage
// percentiles cannot produce. The wrapper forwards every call unchanged and
// never reorders, retries or caches. It is not used by a served surface.
type EngineTiming struct {
	Engine decision.Engine
	calls  int
	total  time.Duration
}

// NewEngineTiming wraps inner. A nil engine keeps the wrapper inert so callers
// can gate on the option without a nil check of their own.
func NewEngineTiming(inner decision.Engine) *EngineTiming {
	return &EngineTiming{Engine: inner}
}

// Evaluate forwards the call and adds its wall time to the accumulator.
func (t *EngineTiming) Evaluate(ctx context.Context, state string, questions []decision.Question) (decision.Result, error) {
	started := time.Now()
	result, err := t.Engine.Evaluate(ctx, state, questions)
	t.total += time.Since(started)
	t.calls++
	return result, err
}

// Reset clears the counters for the next query.
func (t *EngineTiming) Reset() {
	t.calls = 0
	t.total = 0
}

// Snapshot reports the accumulated calls and wall time since the last Reset.
func (t *EngineTiming) Snapshot() (int, time.Duration) {
	return t.calls, t.total
}
