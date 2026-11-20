package retention

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// LockFile is the raw lock handle the audio analysis runner holds while a
// worker analyses PCM: an *os.File whose close releases the flock. The tests
// pin that it shares reads, blocks exclusive cleanup, interoperates with the
// historical Lock helper on the same lock file and is a no-op for an empty
// directory.
func TestLockFileEmptyDirectoryIsNoOp(t *testing.T) {
	f, err := LockFile(context.Background(), "", false)
	if err != nil {
		t.Fatalf("LockFile on empty directory: %v", err)
	}
	if f != nil {
		t.Fatalf("LockFile on empty directory returned %v, want nil", f)
	}
}

func TestLockFileCreatesMissingDirectory(t *testing.T) {
	root := t.TempDir()
	missing := root + "/nested/audio"
	f, err := LockFile(context.Background(), missing, false)
	if err != nil {
		t.Fatalf("LockFile on missing directory: %v", err)
	}
	if f == nil {
		t.Fatal("LockFile on missing directory returned nil")
	}
	f.Close()
	info, err := os.Stat(missing)
	if err != nil || !info.IsDir() {
		t.Fatalf("missing directory not created: %v", err)
	}
}

func TestLockFileSharesReadsAndBlocksExclusive(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()
	a, err := LockFile(ctx, root, false)
	if err != nil {
		t.Fatalf("first shared lock: %v", err)
	}
	b, err := LockFile(ctx, root, false)
	if err != nil {
		t.Fatalf("second shared lock: %v", err)
	}
	exclusiveCtx, cancel := context.WithTimeout(ctx, 120*time.Millisecond)
	defer cancel()
	if release, err := LockFile(exclusiveCtx, root, true); err == nil {
		release.Close()
		t.Fatal("exclusive lock overrode shared readers")
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("exclusive lock error = %v, want deadline exceeded", err)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("close first shared lock: %v", err)
	}
	// The second shared handle still blocks cleanup.
	exclusiveCtx2, cancel2 := context.WithTimeout(ctx, 120*time.Millisecond)
	defer cancel2()
	if release, err := LockFile(exclusiveCtx2, root, true); err == nil {
		release.Close()
		t.Fatal("exclusive lock acquired while another shared handle is open")
	}
	if err := b.Close(); err != nil {
		t.Fatalf("close second shared lock: %v", err)
	}
	last, err := LockFile(ctx, root, true)
	if err != nil {
		t.Fatalf("exclusive lock after readers closed: %v", err)
	}
	last.Close()
}

func TestLockFileInteropWithLockHelper(t *testing.T) {
	// The runner (LockFile) and the forget cleanup (Lock) must contend on the
	// same lock file: whichever form holds the audio root blocks the other.
	root := t.TempDir()
	ctx := context.Background()
	release, err := Lock(ctx, root, false)
	if err != nil {
		t.Fatalf("Lock shared: %v", err)
	}
	blockCtx, cancel := context.WithTimeout(ctx, 120*time.Millisecond)
	defer cancel()
	if f, err := LockFile(blockCtx, root, true); err == nil {
		f.Close()
		t.Fatal("LockFile exclusive ignored the Lock helper's shared lock")
	}
	release()

	f, err := LockFile(ctx, root, true)
	if err != nil {
		t.Fatalf("LockFile exclusive: %v", err)
	}
	blockCtx2, cancel2 := context.WithTimeout(ctx, 120*time.Millisecond)
	defer cancel2()
	if rel, err := Lock(blockCtx2, root, true); err == nil {
		rel()
		t.Fatal("Lock exclusive ignored the LockFile handle")
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close LockFile handle: %v", err)
	}
	again, err := LockFile(ctx, root, true)
	if err != nil {
		t.Fatalf("LockFile exclusive after close: %v", err)
	}
	again.Close()
}

// TestAnalysisStagingRelLayout pins the documented staging path contract:
// .analysis-tmp/<sha256(recordingID)> with a 64 hex digest, stable per
// recording and distinct between recordings. CleanupForget relies on it to
// target staging deletions.
func TestAnalysisStagingRelLayout(t *testing.T) {
	want := ".analysis-tmp/" + hex.EncodeToString(func() []byte { s := sha256.Sum256([]byte("rec:abc")); return s[:] }())
	if got := AnalysisStagingRel("rec:abc"); got != want {
		t.Fatalf("AnalysisStagingRel = %q, want %q", got, want)
	}
	if got := AnalysisStagingRel("rec:abc"); got != want {
		t.Fatal("AnalysisStagingRel is not stable for the same recording id")
	}
	other := AnalysisStagingRel("rec:other")
	if other == want || other == "" {
		t.Fatalf("AnalysisStagingRel collides or is empty: %q", other)
	}
	if !strings.HasPrefix(other, ".analysis-tmp/") || len(strings.TrimPrefix(other, ".analysis-tmp/")) != 64 {
		t.Fatalf("AnalysisStagingRel layout wrong: %q", other)
	}
}
