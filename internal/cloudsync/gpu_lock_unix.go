//go:build unix

package cloudsync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// acquireGPULock takes an exclusive advisory lock on path. Inference processes
// that share one GPU must not run concurrently on an 8 GB card, so callers hold
// this for the whole model session. It polls because flock has no timeout and a
// context-aware wait must stay cancellable.
func acquireGPULock(ctx context.Context, path string, timeout time.Duration) (func(), error) {
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("cloudsync: open GPU lock %s: %w", path, err)
	}
	deadline := time.Now().Add(timeout)
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
				_ = file.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = file.Close()
			return nil, fmt.Errorf("cloudsync: GPU lock %s: %w", path, err)
		}
		if time.Now().After(deadline) {
			_ = file.Close()
			return nil, fmt.Errorf("cloudsync: GPU lock %s is held by another inference process (waited %s)", path, timeout)
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}
