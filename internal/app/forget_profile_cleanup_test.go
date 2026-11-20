package app

// Tests for plan step 3.2: the app-level retirement cleanup must record the
// durable cleanup proof only after every cleanup step succeeded, so the backup
// gate can trust profile_control.cleanup_done. The password-authorized retry
// route exercises the same service method.

import (
	"context"
	"testing"
)

func TestCleanupRetiredProfileRecordsCleanupProof(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.svc.BeginProfileRetirement(ctx); err != nil {
		t.Fatalf("BeginProfileRetirement: %v", err)
	}
	var cleaned int
	if err := f.store.DB().QueryRowContext(ctx,
		`SELECT cleanup_done FROM profile_control WHERE id=1 AND retired=1`).Scan(&cleaned); err != nil {
		t.Fatalf("read profile_control after fencing: %v", err)
	}
	if cleaned != 0 {
		t.Fatalf("cleanup_done = %d while cleanup is pending, want 0", cleaned)
	}

	if err := f.svc.CleanupRetiredProfile(ctx, ""); err != nil {
		t.Fatalf("CleanupRetiredProfile: %v", err)
	}
	if err := f.store.DB().QueryRowContext(ctx,
		`SELECT cleanup_done FROM profile_control WHERE id=1 AND retired=1`).Scan(&cleaned); err != nil {
		t.Fatalf("read profile_control after cleanup: %v", err)
	}
	if cleaned != 1 {
		t.Fatalf("cleanup_done = %d after completed cleanup, want 1", cleaned)
	}

	// Recording the proof again is idempotent.
	if err := f.store.MarkProfileRetirementCleaned(ctx); err != nil {
		t.Fatalf("MarkProfileRetirementCleaned again: %v", err)
	}
}

func TestMarkProfileRetirementCleanedRequiresRetirementFence(t *testing.T) {
	f := newFixture(t)
	if err := f.store.MarkProfileRetirementCleaned(context.Background()); err == nil {
		t.Fatal("MarkProfileRetirementCleaned without an active retirement fence must fail")
	}
}
