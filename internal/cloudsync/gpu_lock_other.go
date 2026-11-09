//go:build !unix

package cloudsync

import (
	"context"
	"errors"
	"time"
)

// acquireGPULock refuses to pretend it serialized inference on platforms without
// flock. Callers only configure a lock path on hosts that support it.
func acquireGPULock(ctx context.Context, path string, timeout time.Duration) (func(), error) {
	return nil, errors.New("cloudsync: GPU file locking is unsupported on this platform")
}
