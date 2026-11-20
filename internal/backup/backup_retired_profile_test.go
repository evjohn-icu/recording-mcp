package backup

// Tests for plan step 3.2: a retired profile must gate archiving on the
// profile-retirement cleanup proof. The barrier-written, catalog-written and
// cleanup-midway states must refuse backup; only the cleanup-complete state
// allows archiving. Old-schema profile_control tables without the cleanup
// column must refuse for retired profiles and stay readable for unretired
// profiles.

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"recordingmcp/internal/memory"
)

// retirementTombstone mirrors the tombstone id the app-level retirement flow
// uses for every retry.
const retirementTombstone = "profile-retirement-v1"

// retireProfile fences the profile with the real retirement API.
func retireProfile(t *testing.T, root, name string) {
	t.Helper()
	store, err := memory.Open(filepath.Join(root, name, profileDBRel))
	if err != nil {
		t.Fatalf("memory.Open(%s): %v", name, err)
	}
	defer store.Close()
	if err := store.BeginProfileRetirement(context.Background(), retirementTombstone); err != nil {
		t.Fatalf("BeginProfileRetirement(%s): %v", name, err)
	}
}

// purgeRetiredProfile runs the real resumable purge, the heavy half of cleanup
// that leaves only the profile_control marker behind.
func purgeRetiredProfile(t *testing.T, root, name string) {
	t.Helper()
	store, err := memory.Open(filepath.Join(root, name, profileDBRel))
	if err != nil {
		t.Fatalf("memory.Open(%s): %v", name, err)
	}
	defer store.Close()
	if err := store.PurgeRetiredProfile(context.Background()); err != nil {
		t.Fatalf("PurgeRetiredProfile(%s): %v", name, err)
	}
}

// writeRetiredCatalog writes the trusted root-level retirement catalog, as the
// retirement flow does once the profile is fenced.
func writeRetiredCatalog(t *testing.T, root string, names ...string) {
	t.Helper()
	path := filepath.Join(root, "retired-profiles.json")
	entries := []string{}
	if raw, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(raw, &entries); err != nil {
			t.Fatalf("parse existing catalog: %v", err)
		}
	}
	entries = append(entries, names...)
	encoded, err := json.Marshal(entries)
	if err != nil {
		t.Fatalf("encode catalog: %v", err)
	}
	if err := os.WriteFile(path, encoded, 0600); err != nil {
		t.Fatalf("write catalog: %v", err)
	}
}

// profileControlColumns returns the column names of profile_control as they
// exist in the current database file.
func profileControlColumns(t *testing.T, db *sql.DB) map[string]bool {
	t.Helper()
	rows, err := db.Query(`PRAGMA table_info(profile_control)`)
	if err != nil {
		t.Fatalf("table_info(profile_control): %v", err)
	}
	defer rows.Close()
	columns := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			t.Fatalf("scan table_info: %v", err)
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("table_info rows: %v", err)
	}
	return columns
}

// rebuildProfileControlWithoutCleanupColumn turns a migrated database into the
// pre-cleanup-proof shape by dropping the cleanup_done column. When the schema
// has no cleanup column yet this is a no-op, so the helper produces the same
// old-schema state before and after the production change. Dropping a single
// column keeps the dependent retirement triggers valid; a table rebuild would
// break them, so ALTER TABLE ... DROP COLUMN is used whenever the engine
// supports it for this column shape.
func rebuildProfileControlWithoutCleanupColumn(t *testing.T, dbPath string) {
	t.Helper()
	db, err := openSQL(dbPath)
	if err != nil {
		t.Fatalf("open db for schema rebuild: %v", err)
	}
	defer db.Close()
	if !profileControlColumns(t, db)["cleanup_done"] {
		return
	}
	if _, err := db.Exec(`ALTER TABLE profile_control DROP COLUMN cleanup_done`); err != nil {
		t.Fatalf("drop cleanup_done column: %v", err)
	}
}

// markRetirementCleanupDone records the cleanup proof the way the production
// cleanup step does, when the schema can hold it.
func markRetirementCleanupDone(t *testing.T, root, name string) {
	t.Helper()
	db, err := openSQL(filepath.Join(root, name, profileDBRel))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	if !profileControlColumns(t, db)["cleanup_done"] {
		t.Log("cleanup_done column absent; cleanup proof cannot be recorded yet")
		return
	}
	execSQL(t, db, `UPDATE profile_control SET cleanup_done=1 WHERE id=1 AND retired=1`)
}

// TestBackupRetiredProfileArchiveGate covers the 3.2 gate: of the four
// retirement states only the cleanup-complete one may be archived.
func TestBackupRetiredProfileArchiveGate(t *testing.T) {
	states := []struct {
		name  string
		setup func(t *testing.T, root, profile string)
	}{
		{"barrier_written", func(t *testing.T, root, profile string) {
			retireProfile(t, root, profile)
		}},
		{"catalog_written", func(t *testing.T, root, profile string) {
			retireProfile(t, root, profile)
			writeRetiredCatalog(t, root, profile)
		}},
		{"cleanup_midway", func(t *testing.T, root, profile string) {
			retireProfile(t, root, profile)
			purgeRetiredProfile(t, root, profile)
			writeRetiredCatalog(t, root, profile)
		}},
		{"cleanup_complete", func(t *testing.T, root, profile string) {
			retireProfile(t, root, profile)
			purgeRetiredProfile(t, root, profile)
			writeRetiredCatalog(t, root, profile)
			markRetirementCleanupDone(t, root, profile)
		}},
	}
	for _, state := range states {
		state := state
		t.Run(state.name, func(t *testing.T) {
			ctx := context.Background()
			root := filepath.Join(t.TempDir(), "profiles")
			seedProfile(t, root, "home", false)
			seedProfile(t, root, "retired", false)
			state.setup(t, root, "retired")

			db, err := openSQL(filepath.Join(root, "retired", profileDBRel))
			if err != nil {
				t.Fatalf("open retired db: %v", err)
			}
			hasProofColumn := profileControlColumns(t, db)["cleanup_done"]
			db.Close()

			_, err = Create(ctx, CreateOptions{ProfilesRoot: root, Out: filepath.Join(t.TempDir(), "gate.tgz"), ConfirmWritersStopped: true})
			switch {
			case state.name == "cleanup_complete" && hasProofColumn:
				if err != nil {
					t.Fatalf("Create with completed retirement cleanup: %v", err)
				}
			default:
				// Barrier written, catalog written, cleanup midway, and any
				// retired profile without a cleanup proof must refuse to
				// archive.
				if code := ErrorCode(err); code != CodeForgetUnfinished {
					t.Fatalf("Create code = %q (err=%v), want %q", code, err, CodeForgetUnfinished)
				}
			}
		})
	}
}

// TestBackupOldSchemaProfileControlReadability covers the old-schema rule: a
// profile_control table without the cleanup column must refuse archiving for a
// retired profile (including the restore side against such a trusted root) and
// must stay fully readable for an unretired profile.
func TestBackupOldSchemaProfileControlReadability(t *testing.T) {
	ctx := context.Background()

	t.Run("retired_old_schema_refused", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "profiles")
		seedProfile(t, root, "home", false)
		seedProfile(t, root, "retired", false)
		archive := filepath.Join(t.TempDir(), "before-retirement.tgz")
		if _, err := Create(ctx, CreateOptions{ProfilesRoot: root, Out: archive, ConfirmWritersStopped: true}); err != nil {
			t.Fatalf("Create before retirement: %v", err)
		}
		retireProfile(t, root, "retired")
		rebuildProfileControlWithoutCleanupColumn(t, filepath.Join(root, "retired", profileDBRel))

		if _, err := Create(ctx, CreateOptions{ProfilesRoot: root, Out: filepath.Join(t.TempDir(), "after.tgz"), ConfirmWritersStopped: true}); ErrorCode(err) != CodeForgetUnfinished {
			t.Fatalf("Create with old-schema retired profile code = %q (err=%v), want %q", ErrorCode(err), err, CodeForgetUnfinished)
		}
		if _, err := Restore(ctx, RestoreOptions{
			Archive:               archive,
			Dest:                  filepath.Join(t.TempDir(), "restored"),
			BarrierSource:         root,
			ConfirmWritersStopped: true,
		}); ErrorCode(err) != CodeForgetBarrierUnverifiable {
			t.Fatalf("Restore against old-schema retired root code = %q (err=%v), want %q", ErrorCode(err), err, CodeForgetBarrierUnverifiable)
		}
	})

	t.Run("unretired_old_schema_readable", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "profiles")
		seedProfile(t, root, "home", false)
		rebuildProfileControlWithoutCleanupColumn(t, filepath.Join(root, "home", profileDBRel))
		db, err := openSQL(filepath.Join(root, "home", profileDBRel))
		if err != nil {
			t.Fatalf("open db: %v", err)
		}
		execSQL(t, db, `INSERT OR REPLACE INTO profile_control(id,retired,tombstone_id,created_at) VALUES(1,0,'',0)`)
		db.Close()

		archive := filepath.Join(t.TempDir(), "unretired.tgz")
		if _, err := Create(ctx, CreateOptions{ProfilesRoot: root, Out: archive, ConfirmWritersStopped: true}); err != nil {
			t.Fatalf("Create with unretired old-schema profile: %v", err)
		}
		dest := filepath.Join(t.TempDir(), "restored")
		if _, err := Restore(ctx, RestoreOptions{Archive: archive, Dest: dest, BarrierSource: root, ConfirmWritersStopped: true}); err != nil {
			t.Fatalf("Restore: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dest, "home", profileDBRel)); err != nil {
			t.Fatalf("restored database missing: %v", err)
		}
	})
}
