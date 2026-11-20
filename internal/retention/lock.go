// Package retention coordinates local audio reads/downloads and physical
// forgetting across processes. The database remains the authority for fences.
package retention

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// Lock returns a release function. Normal audio activity takes a shared lock;
// physical retention cleanup takes an exclusive lock after persisting its
// tombstone, so no old download can recreate a file after cleanup completes.
func Lock(ctx context.Context, directory string, exclusive bool) (func(), error) {
	f, err := LockFile(ctx, directory, exclusive)
	if err != nil || f == nil {
		return func() {}, err
	}
	return func() {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		_ = f.Close()
	}, nil
}

// LockFile acquires the retention flock and hands the open file to the
// caller; the lock is held until the returned file is closed and the last
// close wins. Analysis runs keep the handle for the whole run so a copy
// inherited by the adapter child keeps the lock after a killed worker.
// An empty directory stays the historical no-op: nil file, nil error.
func LockFile(ctx context.Context, directory string, exclusive bool) (*os.File, error) {
	if directory == "" {
		return nil, nil
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
			return f, nil
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

// AnalysisStagingRel returns the root-relative staging directory of one
// recording inside the managed audio root: .analysis-tmp/<sha256(recordingID)>.
// CleanupForget deletes exactly these directories; every adapter run stages
// its per-run copies beneath one of them.
func AnalysisStagingRel(recordingID string) string {
	sum := sha256.Sum256([]byte(recordingID))
	return filepath.Join(".analysis-tmp", hex.EncodeToString(sum[:]))
}
