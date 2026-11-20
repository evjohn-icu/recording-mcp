package backup

// Tests for plan step 3.3: a first-level profile directory must be archived
// even when it is named "cache", while its managed backups/cache/.cache
// subdirectories and root-level unrecognized cache directories stay excluded.
// Concurrent backups to the same output must not overwrite each other: exactly
// one Create succeeds and the loser reports archive_exists without leaving
// staging files behind.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// manifestFilePaths flattens the manifest file list into a set of archive paths.
func manifestFilePaths(manifest Manifest) map[string]bool {
	paths := map[string]bool{}
	for _, entry := range manifest.Files {
		paths[entry.Path] = true
	}
	return paths
}

// TestBackupCacheProfileArchivedAsProfile covers the 3.3 snapshot rule.
func TestBackupCacheProfileArchivedAsProfile(t *testing.T) {
	ctx := context.Background()

	t.Run("cache_profile_content_archived", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "profiles")
		seedProfile(t, root, "cache", false)
		// Managed subdirectories of the cache profile are not restorable state.
		for _, junk := range []string{"backups/old.tgz", "cache/junk.bin", ".cache/hidden.bin"} {
			path := filepath.Join(root, "cache", filepath.FromSlash(junk))
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("junk"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		// Adapter analysis staging under the profile's audio directory is
		// disposable working data and must never enter the archive.
		stagingPath := filepath.Join(root, "cache", "audio", ".analysis-tmp", "sentinel", "partial.bin")
		if err := os.MkdirAll(filepath.Dir(stagingPath), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(stagingPath, []byte("partial"), 0600); err != nil {
			t.Fatal(err)
		}

		archive := filepath.Join(t.TempDir(), "cache-profile.tgz")
		manifest, err := Create(ctx, CreateOptions{ProfilesRoot: root, Out: archive, ConfirmWritersStopped: true})
		if err != nil {
			t.Fatalf("Create with a profile named cache: %v", err)
		}
		foundCacheProfile := false
		for _, profile := range manifest.Profiles {
			if profile.ID == "cache" {
				foundCacheProfile = true
			}
		}
		if !foundCacheProfile {
			t.Fatalf("manifest profiles = %+v, want a cache entry", manifest.Profiles)
		}
		paths := manifestFilePaths(manifest)
		for _, required := range []string{"cache/db/context-memory.db", "cache/audio/rec-a.pcm"} {
			if !paths[required] {
				t.Fatalf("archive is missing %s; snapshot paths: %v", required, paths)
			}
		}
		for _, entry := range manifest.Files {
			for _, excluded := range []string{"cache/backups/", "cache/cache/", "cache/.cache/", "cache/audio/.analysis-tmp/"} {
				if strings.HasPrefix(entry.Path, excluded) {
					t.Fatalf("archive must exclude managed %s but contains %s", excluded, entry.Path)
				}
			}
		}

		dest := filepath.Join(t.TempDir(), "restored")
		if _, err := Restore(ctx, RestoreOptions{Archive: archive, Dest: dest, BarrierSource: root, ConfirmWritersStopped: true}); err != nil {
			t.Fatalf("Restore: %v", err)
		}
		restoredDB := filepath.Join(dest, "cache", "db", "context-memory.db")
		if _, err := os.Stat(restoredDB); err != nil {
			t.Fatalf("restored cache profile database missing: %v", err)
		}
		restoredPCM := filepath.Join(dest, "cache", "audio", "rec-a.pcm")
		if fileHash(t, restoredPCM) != fileHash(t, filepath.Join(root, "cache", "audio", "rec-a.pcm")) {
			t.Fatal("restored cache profile audio differs from the source")
		}
		report, err := Verify(ctx, VerifyOptions{ProfilesRoot: dest})
		if err != nil {
			t.Fatalf("Verify restored cache profile: %v", err)
		}
		if len(report.Profiles) != 1 || report.Profiles[0].ID != "cache" {
			t.Fatalf("verify report = %+v", report)
		}
	})

	t.Run("root_level_cache_without_db_excluded", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "profiles")
		seedProfile(t, root, "home", false)
		if err := os.MkdirAll(filepath.Join(root, "cache"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "cache", "junk.txt"), []byte("junk"), 0600); err != nil {
			t.Fatal(err)
		}

		archive := filepath.Join(t.TempDir(), "root-cache.tgz")
		manifest, err := Create(ctx, CreateOptions{ProfilesRoot: root, Out: archive, ConfirmWritersStopped: true})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		paths := manifestFilePaths(manifest)
		if paths["cache/junk.txt"] {
			t.Fatal("root-level cache directory without a profile database must stay excluded")
		}
		if !paths["home/db/context-memory.db"] {
			t.Fatalf("archive is missing home/db/context-memory.db; snapshot paths: %v", paths)
		}
	})
}

// TestBackupConcurrentArchiveNoOverwrite covers the 3.3 publish rule: two
// concurrent backups with the same output must not replace each other; exactly
// one succeeds, the loser reports archive_exists, and no staging file is left
// behind. The pre-fix rename publish makes overlapping runs both succeed, so
// the test releases both Creates simultaneously and repeats the race several
// times.
func TestBackupConcurrentArchiveNoOverwrite(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "profiles")
	for _, name := range []string{"p0", "p1", "p2", "p3"} {
		seedProfile(t, root, name, false)
	}
	remote := filepath.Join(t.TempDir(), "remote")

	for round := range 4 {
		t.Run(fmt.Sprintf("round%d", round), func(t *testing.T) {
			out := filepath.Join(remote, fmt.Sprintf("race-%d.tgz", round))
			start := make(chan struct{})
			type result struct {
				manifest Manifest
				err      error
			}
			results := make(chan result, 2)
			var wg sync.WaitGroup
			for range 2 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					manifest, err := Create(ctx, CreateOptions{ProfilesRoot: root, Out: out, ConfirmWritersStopped: true})
					results <- result{manifest: manifest, err: err}
				}()
			}
			close(start)
			wg.Wait()
			close(results)

			successes := 0
			for result := range results {
				if result.err == nil {
					successes++
					continue
				}
				if code := ErrorCode(result.err); code != CodeArchiveExists {
					t.Fatalf("losing backup must report archive_exists, got %q (err=%v)", code, result.err)
				}
			}
			if successes != 1 {
				t.Fatalf("exactly one concurrent backup may succeed, got %d", successes)
			}
			info, err := os.Lstat(out)
			if err != nil || !info.Mode().IsRegular() {
				t.Fatalf("published archive missing or not a regular file (stat err=%v)", err)
			}
			if _, err := extractArchive(out, t.TempDir()); err != nil {
				t.Fatalf("published archive is not a readable archive: %v", err)
			}
			leftovers, err := filepath.Glob(out + ".tmp-*")
			if err != nil {
				t.Fatalf("glob staging leftovers: %v", err)
			}
			if len(leftovers) != 0 {
				t.Fatalf("backup staging files left behind: %v", leftovers)
			}
		})
	}
}
