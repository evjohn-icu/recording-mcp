package retention

import (
	"context"
	"testing"
	"time"
)

func TestCleanupWaitsForReadersAndDownloads(t *testing.T) {
	root := t.TempDir()
	a, err := Lock(context.Background(), root, false)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Lock(context.Background(), root, false)
	if err != nil {
		t.Fatal(err)
	}
	b()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if release, err := Lock(ctx, root, true); err == nil {
		release()
		t.Fatal("exclusive cleanup overlapped download")
	}
	a()
	release, err := Lock(context.Background(), root, true)
	if err != nil {
		t.Fatal(err)
	}
	release()
}
