//go:build !unix

package gpulock

import (
	"context"
	"errors"
	"time"
)

// ErrBusy reports an unavailable lock on platforms without flock.
var ErrBusy = errors.New("gpulock: lock is held by another process")

// DefaultPath is unavailable on non-unix builds.
func DefaultPath() (string, error) {
	return "", errors.New("gpulock: file locks require a unix platform")
}

// Acquire refuses to pretend it serialised inference on platforms without
// file locks.
func Acquire(ctx context.Context, path string, timeout time.Duration) (func(), error) {
	return nil, errors.New("gpulock: file locks require a unix platform")
}
