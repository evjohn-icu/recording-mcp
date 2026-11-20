package backup

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	_ "modernc.org/sqlite"

	"recordingmcp/internal/migrations"
)

// BarrierSet is the identity of the permanent-forget state of a profiles
// root. IDs are stable, sorted strings; Digest is the SHA-256 of their
// canonical JSON encoding. Incomplete names tombstones whose cleanup has not
// finished, which makes the root unusable as a barrier source.
type BarrierSet struct {
	Profiles   []string `json:"profiles"`
	IDs        []string `json:"ids"`
	Digest     string   `json:"digest"`
	Incomplete []string `json:"incomplete,omitempty"`
}

// profileDBRel is the canonical database path relative to a profile root.
var profileDBRel = filepath.Join("db", "context-memory.db")

// ReadBarriers collects the forget barriers of every profile below root.
//
// The barrier identity set is:
//   - every tombstone id,
//   - every provider replay fence (tombstone, provider recording, device),
//   - the profile_control retirement marker and tombstone,
//   - every name in the trusted retired-profiles catalog.
//
// Cleanup progress (forget_files.done, redaction_done) is deliberately not part
// of the digest: it can only move forward, and a changed cleanup state must not
// look like a different forget history. Completeness is reported separately.
func ReadBarriers(ctx context.Context, root string) (BarrierSet, error) {
	profiles, err := profileNames(root)
	if err != nil {
		return BarrierSet{}, err
	}
	set := BarrierSet{Profiles: profiles, IDs: []string{}, Incomplete: []string{}}
	for _, name := range profiles {
		if err := ctx.Err(); err != nil {
			return BarrierSet{}, err
		}
		profileRoot := filepath.Join(root, name)
		db, cleanup, err := openProfileSnapshot(ctx, profileRoot)
		if err != nil {
			return BarrierSet{}, err
		}
		ids, incomplete, err := readProfileBarriers(ctx, db, name)
		cleanup()
		if err != nil {
			return BarrierSet{}, err
		}
		set.IDs = append(set.IDs, ids...)
		set.Incomplete = append(set.Incomplete, incomplete...)
	}
	catalog, err := readRetiredCatalog(root)
	if err != nil {
		return BarrierSet{}, err
	}
	for _, name := range catalog {
		set.IDs = append(set.IDs, "catalog:"+name)
	}
	sort.Strings(set.IDs)
	encoded, err := json.Marshal(set.IDs)
	if err != nil {
		return BarrierSet{}, fmt.Errorf("backup: encode barrier set: %w", err)
	}
	sum := sha256.Sum256(encoded)
	set.Digest = hex.EncodeToString(sum[:])
	return set, nil
}

// readProfileBarriers reads one profile's forget rows.
func readProfileBarriers(ctx context.Context, db *sql.DB, profile string) (ids, incomplete []string, err error) {
	rows, err := db.QueryContext(ctx, `SELECT id FROM forget_tombstones ORDER BY id`)
	if err != nil {
		return nil, nil, fmt.Errorf("backup: read tombstones for %s: %w", profile, err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, nil, err
		}
		ids = append(ids, "tombstone:"+id)
	}
	if err := rows.Close(); err != nil {
		return nil, nil, err
	}
	rows, err = db.QueryContext(ctx, `SELECT tombstone_id,provider_recording_id,device_id FROM forget_provider_fences ORDER BY tombstone_id,provider_recording_id,device_id`)
	if err != nil {
		return nil, nil, fmt.Errorf("backup: read provider fences for %s: %w", profile, err)
	}
	for rows.Next() {
		var tombstone, provider, device string
		if err := rows.Scan(&tombstone, &provider, &device); err != nil {
			rows.Close()
			return nil, nil, err
		}
		ids = append(ids, "fence:"+tombstone+"|"+provider+"|"+device)
	}
	if err := rows.Close(); err != nil {
		return nil, nil, err
	}
	var retired int
	var controlTombstone string
	err = db.QueryRowContext(ctx, `SELECT retired,tombstone_id FROM profile_control WHERE id=1`).Scan(&retired, &controlTombstone)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// A profile created before retirement existed has no control row.
	case err != nil:
		return nil, nil, fmt.Errorf("backup: read profile control for %s: %w", profile, err)
	default:
		ids = append(ids, fmt.Sprintf("profile_control:%s:%d:%s", profile, retired, controlTombstone))
	}
	rows, err = db.QueryContext(ctx, `SELECT id FROM forget_tombstones WHERE redaction_done<>1 OR status<>'complete' ORDER BY id`)
	if err != nil {
		return nil, nil, fmt.Errorf("backup: read unfinished tombstones for %s: %w", profile, err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, nil, err
		}
		incomplete = append(incomplete, id)
	}
	if err := rows.Close(); err != nil {
		return nil, nil, err
	}
	rows, err = db.QueryContext(ctx, `SELECT DISTINCT tombstone_id FROM forget_files WHERE done=0 ORDER BY tombstone_id`)
	if err != nil {
		return nil, nil, fmt.Errorf("backup: read pending forget files for %s: %w", profile, err)
	}
	seen := map[string]bool{}
	for _, id := range incomplete {
		seen[id] = true
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, nil, err
		}
		if !seen[id] {
			seen[id] = true
			incomplete = append(incomplete, id)
		}
	}
	if err := rows.Close(); err != nil {
		return nil, nil, err
	}
	sort.Strings(incomplete)
	return ids, incomplete, nil
}

// readRetiredCatalog reads the trusted root-level retirement catalog.
func readRetiredCatalog(root string) ([]string, error) {
	path := filepath.Join(root, "retired-profiles.json")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("backup: read retired catalog: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("backup: retired catalog must be a regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("backup: read retired catalog: %w", err)
	}
	var names []string
	if err := json.Unmarshal(data, &names); err != nil {
		return nil, fmt.Errorf("backup: retired catalog is corrupt")
	}
	sort.Strings(names)
	return names, nil
}

// profileNames lists profile directories that hold a canonical database.
func profileNames(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("backup: list profiles root: %w", err)
	}
	names := []string{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dbPath := filepath.Join(root, entry.Name(), profileDBRel)
		info, err := os.Lstat(dbPath)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("backup: profile %s database is not a regular file", entry.Name())
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names, nil
}

// openProfileSnapshot copies the database and its WAL/SHM sidecars to a
// temporary directory and opens the copy read-write, so SQLite can replay the
// WAL without touching the original. The returned cleanup removes the copy.
func openProfileSnapshot(ctx context.Context, profileRoot string) (*sql.DB, func(), error) {
	dir, err := os.MkdirTemp("", "backup-db-")
	if err != nil {
		return nil, nil, fmt.Errorf("backup: create database staging: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	source := filepath.Join(profileRoot, profileDBRel)
	target := filepath.Join(dir, "context-memory.db")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := copyIfExists(source+suffix, target+suffix); err != nil {
			cleanup()
			return nil, nil, err
		}
	}
	dsn := "file:" + target + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("backup: open database copy: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		db.Close()
		cleanup()
		return nil, nil, fmt.Errorf("backup: replay write-ahead log: %w", err)
	}
	combined := func() {
		_ = db.Close()
		cleanup()
	}
	return db, combined, nil
}

func copyIfExists(source, target string) error {
	info, err := os.Lstat(source)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("backup: %s is not a regular file", filepath.Base(source))
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if _, err := copyBuffer(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// compareBarriers enforces the restore policy: the archive must contain every
// barrier of the trusted root, no barrier the root does not know, the same
// profile set, and finished cleanup on both sides.
func compareBarriers(archive, source BarrierSet) error {
	if len(source.Profiles) == 0 {
		return newError(CodeForgetBarrierUnverifiable, "the trusted barrier source has no readable profile database")
	}
	if strings.Join(archive.Profiles, "\x00") != strings.Join(source.Profiles, "\x00") {
		return newError(CodeForgetBarrierUnverifiable, "archive profiles %v do not match the trusted source %v", archive.Profiles, source.Profiles)
	}
	if len(source.Incomplete) > 0 {
		return newError(CodeForgetBarrierUnverifiable, "the trusted source has unfinished forget cleanup; finish it before restoring")
	}
	if len(archive.Incomplete) > 0 {
		return newError(CodeForgetBarrierUnverifiable, "the archive has unfinished forget cleanup")
	}
	sourceIDs := indexIDs(source.IDs)
	archiveIDs := indexIDs(archive.IDs)
	missing := []string{}
	for id := range sourceIDs {
		if !archiveIDs[id] {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return newError(CodeBackupPredatesForget, "the archive predates %d permanent-forget barrier(s), for example %s; restore a backup taken after the latest forget", len(missing), missing[0])
	}
	extra := []string{}
	for id := range archiveIDs {
		if !sourceIDs[id] {
			extra = append(extra, id)
		}
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		return newError(CodeForgetBarrierUnverifiable, "the archive contains %d barrier(s) the trusted source does not have, for example %s; the barrier source is stale or belongs to another root", len(extra), extra[0])
	}
	return nil
}

func indexIDs(ids []string) map[string]bool {
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out
}

// schemaVersion reads the applied migration version of a profile database.
func schemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	version, err := migrations.Version(ctx, db)
	if err != nil {
		return 0, fmt.Errorf("backup: read schema version: %w", err)
	}
	return version, nil
}
