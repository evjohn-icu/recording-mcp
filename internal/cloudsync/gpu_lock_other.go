//go:build !unix

package cloudsync

import (
	"context"
	"time"

	"recordingmcp/internal/gpulock"
)

// acquireGPULock refuses to pretend it serialized inference on platforms
// without file locks.
func acquireGPULock(ctx context.Context, path string, timeout time.Duration) (func(), error) {
	return gpulock.Acquire(ctx, path, timeout)
}
