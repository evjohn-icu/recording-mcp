package eval

import (
	"context"
	"errors"
	"testing"
	"time"

	"recordingmcp/internal/decision"
)

// stubEngine returns a fixed result and can simulate a slow or failing call.
type stubEngine struct {
	calls   int
	delay   time.Duration
	err     error
	states  []string
	answers map[string]decision.Answer
}

func (e *stubEngine) Evaluate(_ context.Context, state string, questions []decision.Question) (decision.Result, error) {
	e.calls++
	e.states = append(e.states, state)
	if e.delay > 0 {
		time.Sleep(e.delay)
	}
	if e.err != nil {
		return decision.Result{}, e.err
	}
	answers := map[string]decision.Answer{}
	for _, q := range questions {
		answers[q.ID] = decision.Answer{Type: q.Type, Choice: "other", Confidence: 1}
	}
	return decision.Result{Answers: answers}, nil
}

func TestEngineTimingForwardsTheResultUnchanged(t *testing.T) {
	inner := &stubEngine{}
	wrapped := NewEngineTiming(inner)
	questions := []decision.Question{{ID: "q1", Type: decision.TypeNoul}}
	result, err := wrapped.Evaluate(t.Context(), "state", questions)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if inner.calls != 1 || len(inner.states) != 1 || inner.states[0] != "state" {
		t.Fatalf("inner calls = %d states = %v", inner.calls, inner.states)
	}
	if _, ok := result.Answers["q1"]; !ok {
		t.Fatalf("answers = %v, want q1", result.Answers)
	}
}

func TestEngineTimingPropagatesErrorsAndStillCountsTheCall(t *testing.T) {
	want := errors.New("engine down")
	wrapped := NewEngineTiming(&stubEngine{err: want})
	if _, err := wrapped.Evaluate(t.Context(), "state", nil); !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
	calls, _ := wrapped.Snapshot()
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
}

func TestEngineTimingAccumulatesAndResets(t *testing.T) {
	wrapped := NewEngineTiming(&stubEngine{delay: 2 * time.Millisecond})
	for range 2 {
		if _, err := wrapped.Evaluate(t.Context(), "state", nil); err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
	}
	calls, total := wrapped.Snapshot()
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
	if total < 4*time.Millisecond {
		t.Errorf("total = %v, want at least the two simulated sleeps", total)
	}
	wrapped.Reset()
	if calls, total := wrapped.Snapshot(); calls != 0 || total != 0 {
		t.Errorf("after Reset calls = %d total = %v", calls, total)
	}
}
