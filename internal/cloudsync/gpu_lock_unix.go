//go:build unix

package cloudsync

import (
	"context"
	"time"

	"recordingmcp/internal/gpulock"
)

// acquireGPULock delegates to the single user-level GPU lock so local ASR,
// embedding and audio analysis cannot hold the card at the same time.
func acquireGPULock(ctx context.Context, path string, timeout time.Duration) (func(), error) {
	return gpulock.Acquire(ctx, path, timeout)
}
