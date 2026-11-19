//go:build unix

// Package gpulock is the single cross-process GPU lock used by every local GPU
// consumer: local ASR, embedding services, audio analysis and CUDA probes. One
// user-level lock path means a second process cannot start a model while the
// first still holds the card.
package gpulock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// ErrBusy reports that another process holds the lock past the timeout.
var ErrBusy = errors.New("gpulock: lock is held by another process")

// DefaultPath returns the user-level lock path shared by every process of this
// user. It creates the parent directory with 0700 when missing.
//
// The path is deliberately independent of any profile: two profiles on the
// same machine still share one GPU.
func DefaultPath() (string, error) {
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("gpulock: resolve home: %w", err)
		}
		base = filepath.Join(home, ".local", "state")
	}
	dir := filepath.Join(base, "recording-mcp")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("gpulock: create lock directory: %w", err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return "", fmt.Errorf("gpulock: secure lock directory: %w", err)
	}
	return filepath.Join(dir, "recording-mcp-gpu.lock"), nil
}

// Acquire takes the exclusive lock, polling until timeout or ctx cancellation.
// The returned release function unlocks and closes the file.
func Acquire(ctx context.Context, path string, timeout time.Duration) (func(), error) {
	if path == "" {
		defaultPath, err := DefaultPath()
		if err != nil {
			return nil, err
		}
		path = defaultPath
	}
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("gpulock: create lock directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("gpulock: open lock: %w", err)
	}
	if err := file.Chmod(0600); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("gpulock: secure lock file: %w", err)
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
			return nil, fmt.Errorf("gpulock: acquire: %w", err)
		}
		if time.Now().After(deadline) {
			_ = file.Close()
			return nil, fmt.Errorf("%w (waited %s)", ErrBusy, timeout)
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}
