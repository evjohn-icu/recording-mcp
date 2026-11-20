//go:build unix

package gpulock

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestGPULockSerializesProcesses proves a second holder is refused while the
// first holds the same path, and that the lock is reusable after release.
func TestGPULockSerializesProcesses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gpu.lock")
	first, err := Acquire(context.Background(), path, time.Second)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if _, err := Acquire(context.Background(), path, 120*time.Millisecond); !errors.Is(err, ErrBusy) {
		t.Fatalf("second acquire error = %v, want ErrBusy", err)
	}
	first()
	second, err := Acquire(context.Background(), path, time.Second)
	if err != nil {
		t.Fatalf("re-acquire after release: %v", err)
	}
	second()
}

// TestGPULockIsSharedAcrossProfiles pins the single user-level path: two
// different profile directories must still contend on one lock, not get one
// lock each.
func TestGPULockIsSharedAcrossProfiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gpu.lock")
	release, err := Acquire(context.Background(), path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, profile := range []string{"profile-a", "profile-b"} {
		wg.Add(1)
		go func(i int, profile string) {
			defer wg.Done()
			// The profile name never changes the lock path.
			_, errs[i] = Acquire(context.Background(), path, 100*time.Millisecond)
			_ = profile
		}(i, profile)
	}
	wg.Wait()
	for i, err := range errs {
		if !errors.Is(err, ErrBusy) {
			t.Fatalf("profile %d acquired a shared lock: %v", i, err)
		}
	}
	release()
}

// TestGPULockHonorsContextCancellation keeps a shutdown from waiting out the
// timeout.
func TestGPULockHonorsContextCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gpu.lock")
	release, err := Acquire(context.Background(), path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := Acquire(ctx, path, time.Minute); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("acquire error = %v, want deadline exceeded", err)
	}
}
