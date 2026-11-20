package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// CodeNoProfiles marks a root that holds no readable profile database.
const CodeNoProfiles = "no_profiles"

// inspectProfiles opens each profile database copy, proves its integrity and
// foreign keys, and validates every managed audio reference.
func inspectProfiles(ctx context.Context, root string) ([]ProfileEntry, error) {
	names, err := profileNames(root)
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, newError(CodeNoProfiles, "no profile database found below the profiles root")
	}
	profiles := make([]ProfileEntry, 0, len(names))
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		profileRoot := filepath.Join(root, name)
		db, cleanup, err := openProfileSnapshot(ctx, profileRoot)
		if err != nil {
			// A database that cannot even be opened or replayed is damaged;
			// report it as an integrity failure rather than a generic error.
			if ErrorCode(err) == "" {
				return nil, newError(CodeIntegrityFailed, "profile %s: %v", name, err)
			}
			return nil, err
		}
		entry, err := inspectProfile(ctx, db, name, profileRoot)
		cleanup()
		if err != nil {
			return nil, err
		}
		profiles = append(profiles, entry)
	}
	return profiles, nil
}

func inspectProfile(ctx context.Context, db *sql.DB, name, profileRoot string) (ProfileEntry, error) {
	if err := integrityCheck(ctx, db, name); err != nil {
		return ProfileEntry{}, err
	}
	if err := foreignKeyCheck(ctx, db, name); err != nil {
		return ProfileEntry{}, err
	}
	version, err := schemaVersion(ctx, db)
	if err != nil {
		return ProfileEntry{}, err
	}
	audio, err := profileAudioStatus(ctx, db, profileRoot)
	if err != nil {
		return ProfileEntry{}, err
	}
	return ProfileEntry{ID: name, Schema: version, Audio: audio}, nil
}

// validateRoot applies the restore-time checks to a freshly extracted root.
func validateRoot(ctx context.Context, root string) error {
	_, err := inspectProfiles(ctx, root)
	return err
}

// integrityCheck requires PRAGMA integrity_check to return exactly one "ok"
// row. Any other row, or a second row, is a failure.
func integrityCheck(ctx context.Context, db *sql.DB, profile string) error {
	rows, err := db.QueryContext(ctx, `PRAGMA integrity_check`)
	if err != nil {
		return newError(CodeIntegrityFailed, "profile %s: integrity check could not run", profile)
	}
	defer rows.Close()
	results := []string{}
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return newError(CodeIntegrityFailed, "profile %s: integrity check result is unreadable", profile)
		}
		results = append(results, line)
	}
	if err := rows.Err(); err != nil {
		return newError(CodeIntegrityFailed, "profile %s: integrity check failed", profile)
	}
	if len(results) != 1 || results[0] != "ok" {
		detail := "unexpected result"
		if len(results) > 0 {
			detail = firstLine(results[0])
		}
		return newError(CodeIntegrityFailed, "profile %s: %s", profile, detail)
	}
	return nil
}

// foreignKeyCheck requires PRAGMA foreign_key_check to return no rows.
func foreignKeyCheck(ctx context.Context, db *sql.DB, profile string) error {
	rows, err := db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return newError(CodeForeignKeyFailed, "profile %s: foreign key check could not run", profile)
	}
	defer rows.Close()
	violations := 0
	first := ""
	for rows.Next() {
		var table string
		var rowid sql.NullInt64
		var parent string
		var fkid int
		if err := rows.Scan(&table, &rowid, &parent, &fkid); err != nil {
			return newError(CodeForeignKeyFailed, "profile %s: foreign key check result is unreadable", profile)
		}
		if violations == 0 {
			first = table + " -> " + parent
		}
		violations++
	}
	if err := rows.Err(); err != nil {
		return newError(CodeForeignKeyFailed, "profile %s: foreign key check failed", profile)
	}
	if violations > 0 {
		return newError(CodeForeignKeyFailed, "profile %s: %d violation(s), first %s", profile, violations, first)
	}
	return nil
}

// profileAudioStatus validates the managed audio references of one profile.
// A recording declared deleted is not a missing file; every other referenced
// stitched file must exist inside the profile's audio directory, and every
// recorded chunk must still match its stored hash and size.
func profileAudioStatus(ctx context.Context, db *sql.DB, profileRoot string) (AudioStatus, error) {
	status := AudioStatus{}
	audioRoot := filepath.Join(profileRoot, "audio")
	rows, err := db.QueryContext(ctx, `SELECT state,stitched_path FROM sync_recordings ORDER BY recording_id`)
	if err != nil {
		// A profile that predates cloud sync has no sync tables.
		if strings.Contains(err.Error(), "no such table") {
			return status, nil
		}
		return status, fmt.Errorf("backup: read sync recordings: %w", err)
	}
	for rows.Next() {
		var state string
		var stitched sql.NullString
		if err := rows.Scan(&state, &stitched); err != nil {
			rows.Close()
			return status, err
		}
		if state == "deleted" {
			status.DeclaredUnavailable++
			continue
		}
		if !stitched.Valid || strings.TrimSpace(stitched.String) == "" {
			continue
		}
		ref := resolveAudioRef(audioRoot, stitched.String)
		countAudioRef(&status, ref)
	}
	if err := rows.Close(); err != nil {
		return status, err
	}
	rows, err = db.QueryContext(ctx, `SELECT sha256,bytes,file_path FROM sync_chunks ORDER BY recording_id,seq`)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return status, nil
		}
		return status, fmt.Errorf("backup: read sync chunks: %w", err)
	}
	for rows.Next() {
		var expectedHash string
		var expectedSize int64
		var filePath string
		if err := rows.Scan(&expectedHash, &expectedSize, &filePath); err != nil {
			rows.Close()
			return status, err
		}
		resolved := resolveAudioRef(audioRoot, filePath)
		if resolved != audioRefAvailable {
			countAudioRef(&status, resolved)
			continue
		}
		path := resolvedPath(audioRoot, filePath)
		if path == "" {
			countAudioRef(&status, audioRefMissing)
			continue
		}
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() != expectedSize {
			countAudioRef(&status, audioRefHashMismatch)
			continue
		}
		hash, err := sha256File(path)
		if err != nil || !strings.EqualFold(hash, expectedHash) {
			countAudioRef(&status, audioRefHashMismatch)
			continue
		}
		status.Available++
	}
	if err := rows.Close(); err != nil {
		return status, err
	}
	return status, nil
}

type audioRefState int

const (
	audioRefAvailable audioRefState = iota
	audioRefMissing
	audioRefOutOfBounds
	audioRefHashMismatch
)

func countAudioRef(status *AudioStatus, state audioRefState) {
	switch state {
	case audioRefAvailable:
		status.Available++
	case audioRefOutOfBounds:
		status.OutOfBounds++
	case audioRefHashMismatch:
		status.HashMismatch++
	default:
		status.Missing++
	}
}

// resolveAudioRef locates a stored audio path under the current audio root.
//
// Paths are stored as absolute paths from the root that produced them. A
// restore into a different absolute root keeps the relative shape but not the
// prefix, so the lookup first accepts a path inside the current root, then
// re-anchors the part after the last "/audio/" segment (the managed layout is
// <audio-root>/<recording>.pcm and <audio-root>/<recording>/chunk-*.pcm), and
// finally falls back to the basename.
func resolveAudioRef(audioRoot, stored string) audioRefState {
	if resolvedPath(audioRoot, stored) != "" {
		return audioRefAvailable
	}
	if info, err := os.Lstat(stored); err == nil {
		if info.Mode().IsRegular() {
			// The stored file exists but outside the managed audio root.
			return audioRefOutOfBounds
		}
		return audioRefOutOfBounds
	}
	return audioRefMissing
}

// resolvedPath returns the on-disk path of stored audio under audioRoot, or ""
// when no candidate exists.
func resolvedPath(audioRoot, stored string) string {
	for _, candidate := range audioCandidates(audioRoot, stored) {
		info, err := os.Lstat(candidate)
		if err == nil && info.Mode().IsRegular() {
			return candidate
		}
	}
	return ""
}

func audioCandidates(audioRoot, stored string) []string {
	out := []string{}
	if inside(audioRoot, stored) {
		out = append(out, stored)
	}
	marker := string(os.PathSeparator) + "audio" + string(os.PathSeparator)
	if idx := strings.LastIndex(stored, marker); idx >= 0 {
		out = append(out, filepath.Join(audioRoot, filepath.FromSlash(stored[idx+len(marker):])))
	}
	if base := filepath.Base(stored); base != "" && base != "." && base != string(os.PathSeparator) {
		out = append(out, filepath.Join(audioRoot, base))
	}
	return out
}

func firstLine(value string) string {
	if idx := strings.IndexByte(value, '\n'); idx >= 0 {
		value = value[:idx]
	}
	if len(value) > 200 {
		value = value[:200]
	}
	return value
}

// isNotExist reports whether err is a missing-file error.
func isNotExist(err error) bool { return errors.Is(err, os.ErrNotExist) }
