package worker

import (
	"context"
	"time"

	"recordingmcp/internal/process"
)

// Synthetic is a deterministic Executor used for smoke tests and dry runs. It
// optionally waits for Delay and returns "synthetic:<jobID>".
type Synthetic struct {
	Delay time.Duration
}

// Execute implements Executor.
func (s Synthetic) Execute(ctx context.Context, job process.Job) (string, error) {
	if s.Delay > 0 {
		timer := time.NewTimer(s.Delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-timer.C:
		}
	}
	return "synthetic:" + job.ID, nil
}
