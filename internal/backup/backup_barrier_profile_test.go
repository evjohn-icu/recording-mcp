package backup

// Tests for plan step 3.1: tombstone and provider-fence barrier identities must
// include the owning profile, restore must recompute barriers from the unpacked
// SQLite (never from the manifest's barrier metadata), and same-version
// archives with an old-format manifest must stay readable.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// seedSharedSourceProfile seeds two profiles with byte-identical synthetic
// source state: the same recording, turn, provider recording id and device, so
// a forget of the same source in both profiles produces colliding tombstone and
// provider-fence rows exactly like the real deterministic forget locator does.
func seedSharedSourceProfiles(t *testing.T, root string) {
	t.Helper()
	seedProfile(t, root, "alpha", false)
	seedProfile(t, root, "beta", false)
}

// TestBackupBarrierProfileIsolation covers the 3.1 scenario: profiles alpha and
// beta share the same synthetic source. Alpha forgets, a backup is taken, then
// beta forgets the same source. Restoring that stale archive must be refused
// with backup_predates_forget (today the unscoped tombstone/fence identities
// collapse across profiles and the restore resurrects beta's redacted turn).
// An archive taken after beta's forget restores normally and keeps the
// profile-scoped barrier identity of the live root.
func TestBackupBarrierProfileIsolation(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "profiles")
	seedSharedSourceProfiles(t, root)

	remote := filepath.Join(t.TempDir(), "remote")
	stale := filepath.Join(remote, "alpha-forget.tgz")

	// Alpha forgets the shared source; the archive knows only alpha's forget.
	completedForget(t, root, "alpha", "forget:shared-source")
	if _, err := Create(ctx, CreateOptions{ProfilesRoot: root, Out: stale, ConfirmWritersStopped: true}); err != nil {
		t.Fatalf("Create after alpha forget: %v", err)
	}

	// Beta forgets the same source afterwards; the stale archive now predates it.
	completedForget(t, root, "beta", "forget:shared-source")

	dest := filepath.Join(t.TempDir(), "restored-stale")
	_, err := Restore(ctx, RestoreOptions{
		Archive:               stale,
		Dest:                  dest,
		BarrierSource:         root,
		ConfirmWritersStopped: true,
	})
	if code := ErrorCode(err); code != CodeBackupPredatesForget {
		t.Fatalf("Restore of stale archive code = %q (err=%v), want %q", code, err, CodeBackupPredatesForget)
	}
	if _, statErr := os.Lstat(dest); !os.IsNotExist(statErr) {
		t.Fatalf("stale restore must not publish the destination (stat err=%v)", statErr)
	}

	// An archive taken after both forgets restores normally.
	fresh := filepath.Join(remote, "both-forgot.tgz")
	if _, err := Create(ctx, CreateOptions{ProfilesRoot: root, Out: fresh, ConfirmWritersStopped: true}); err != nil {
		t.Fatalf("Create after both forgets: %v", err)
	}
	destFresh := filepath.Join(t.TempDir(), "restored-fresh")
	restored, err := Restore(ctx, RestoreOptions{
		Archive:               fresh,
		Dest:                  destFresh,
		BarrierSource:         root,
		ConfirmWritersStopped: true,
	})
	if err != nil {
		t.Fatalf("Restore of fresh archive: %v", err)
	}
	live, err := ReadBarriers(ctx, root)
	if err != nil {
		t.Fatalf("ReadBarriers(live): %v", err)
	}
	if restored.BarrierDigest != live.Digest {
		t.Fatalf("restored manifest barrier digest = %q, want live %q", restored.BarrierDigest, live.Digest)
	}
	// The restored root must carry the same profile-scoped barrier identity as
	// the live root; recomputing from its unpacked SQLite must not lose the
	// per-profile forget history.
	restoredSet, err := ReadBarriers(ctx, destFresh)
	if err != nil {
		t.Fatalf("ReadBarriers(restored): %v", err)
	}
	if restoredSet.Digest != live.Digest {
		t.Fatalf("restored root barrier digest = %q, want live %q (ids %v vs %v)", restoredSet.Digest, live.Digest, restoredSet.IDs, live.IDs)
	}
	for _, profile := range []string{"alpha", "beta"} {
		if _, statErr := os.Lstat(filepath.Join(destFresh, profile, profileDBRel)); statErr != nil {
			t.Fatalf("restored profile %s database missing: %v", profile, statErr)
		}
	}
}

// TestBackupBarrierOldManifestFormatRestores pins the compatibility rule: the
// manifest's barrier_ids are not load-bearing for restore authorization. An
// archive whose manifest still carries the legacy unscoped barrier id list
// (today's format, without profile scoping) must keep restoring after the
// identity becomes profile-scoped; barriers are recomputed from the unpacked
// SQLite instead.
func TestBackupBarrierOldManifestFormatRestores(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "profiles")
	seedSharedSourceProfiles(t, root)
	completedForget(t, root, "alpha", "forget:shared-source")
	completedForget(t, root, "beta", "forget:shared-source")

	remote := filepath.Join(t.TempDir(), "remote")
	archive := filepath.Join(remote, "legacy-manifest.tgz")
	created, err := Create(ctx, CreateOptions{ProfilesRoot: root, Out: archive, ConfirmWritersStopped: true})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(created.BarrierIDs) == 0 {
		t.Fatal("created manifest has no barrier ids to replace")
	}

	// Rewrite the manifest with a legacy, unprofile-scoped barrier id list and
	// repack the archive. extractArchive does not materialise manifest.json in
	// the staging tree, so the legacy manifest is derived from the Create
	// result; the file list is unchanged because the extracted bytes are not
	// touched.
	work := t.TempDir()
	if _, err := extractArchive(archive, work); err != nil {
		t.Fatalf("extractArchive: %v", err)
	}
	legacy := created
	legacy.BarrierIDs = []string{
		"tombstone:forget:shared-source",
		"fence:forget:shared-source|provider-rec|device",
	}
	files, err := snapshot(work)
	if err != nil {
		t.Fatalf("snapshot work dir: %v", err)
	}
	repacked := filepath.Join(remote, "legacy-repacked.tgz")
	if err := writeArchive(repacked, work, files, legacy); err != nil {
		t.Fatalf("writeArchive: %v", err)
	}

	dest := filepath.Join(t.TempDir(), "restored-legacy")
	if _, err := Restore(ctx, RestoreOptions{
		Archive:               repacked,
		Dest:                  dest,
		BarrierSource:         root,
		ConfirmWritersStopped: true,
	}); err != nil {
		t.Fatalf("Restore of old-format manifest archive: %v", err)
	}
}
