// Package retention coordinates local audio reads/downloads and physical
// forgetting across processes. The database remains the authority for fences.
package retention

import (
	"context"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// Lock returns a release function. Normal audio activity takes a shared lock;
// physical retention cleanup takes an exclusive lock after persisting its
// tombstone, so no old download can recreate a file after cleanup completes.
func Lock(ctx context.Context, directory string, exclusive bool) (func(), error) {
	if directory == "" {
		return func() {}, nil
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := root.OpenFile(".retention.lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	op := unix.LOCK_SH | unix.LOCK_NB
	if exclusive {
		op = unix.LOCK_EX | unix.LOCK_NB
	}
	for {
		err = unix.Flock(int(f.Fd()), op)
		if err == nil {
			return func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN); _ = f.Close() }, nil
		}
		if err != unix.EWOULDBLOCK && err != unix.EAGAIN {
			f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}
