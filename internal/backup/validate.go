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

// audioReferenceError reports the first profile whose managed audio references
// are broken. Create, Verify and Restore share this exact judgment so a restore
// never publishes what a backup or verification would refuse.
func audioReferenceError(profiles []ProfileEntry) error {
	for _, profile := range profiles {
		if profile.Audio.Missing+profile.Audio.OutOfBounds+profile.Audio.HashMismatch > 0 {
			return newError(CodeAudioMissing, "profile %s has %d missing and %d out-of-bounds audio reference(s)", profile.ID, profile.Audio.Missing, profile.Audio.OutOfBounds)
		}
	}
	return nil
}

// validateRoot applies the restore-time checks to a freshly extracted root.
// The managed audio references are validated with the same
// missing/out-of-bounds/hash-mismatch judgment create and verify use, so a
// broken stored reference is rejected before the destination is published.
func validateRoot(ctx context.Context, root string) error {
	profiles, err := inspectProfiles(ctx, root)
	if err != nil {
		return err
	}
	return audioReferenceError(profiles)
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
// recorded chunk must still match its stored hash and size. A completed turn
// forget deliberately erases the hashes of retained chunks so unrelated turns
// stay playable without disclosing pre-redaction content; such blank hashes
// are only tolerated when the recording has a turn-scoped provider fence for
// its device backed by a tombstone that is complete and fully redacted, and
// the file existence, path and byte count are still validated.
func profileAudioStatus(ctx context.Context, db *sql.DB, profileRoot string) (AudioStatus, error) {
	status := AudioStatus{}
	audioRoot := filepath.Join(profileRoot, "audio")
	rows, err := db.QueryContext(ctx, `SELECT recording_id,state,stitched_path,device_id FROM sync_recordings ORDER BY recording_id`)
	if err != nil {
		// A profile that predates cloud sync has no sync tables.
		if strings.Contains(err.Error(), "no such table") {
			return status, nil
		}
		return status, fmt.Errorf("backup: read sync recordings: %w", err)
	}
	deviceByRecording := map[string]string{}
	for rows.Next() {
		var recordingID string
		var state string
		var stitched sql.NullString
		var deviceID string
		if err := rows.Scan(&recordingID, &state, &stitched, &deviceID); err != nil {
			rows.Close()
			return status, err
		}
		deviceByRecording[recordingID] = deviceID
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
	retainedBlankSHA, err := completedTurnForgetRecordings(ctx, db, deviceByRecording)
	if err != nil {
		return status, err
	}
	rows, err = db.QueryContext(ctx, `SELECT recording_id,sha256,bytes,file_path FROM sync_chunks ORDER BY recording_id,seq`)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return status, nil
		}
		return status, fmt.Errorf("backup: read sync chunks: %w", err)
	}
	for rows.Next() {
		var recordingID string
		var expectedHash string
		var expectedSize int64
		var filePath string
		if err := rows.Scan(&recordingID, &expectedHash, &expectedSize, &filePath); err != nil {
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
		info, lstatErr := os.Lstat(path)
		if lstatErr != nil || !info.Mode().IsRegular() {
			countAudioRef(&status, audioRefHashMismatch)
			continue
		}
		if expectedHash == "" {
			// Retained chunk of a completed turn forget: only the hash
			// comparison is skipped; existence, path and byte count above and
			// below still apply.
			if !retainedBlankSHA[recordingID] || info.Size() != expectedSize {
				countAudioRef(&status, audioRefHashMismatch)
				continue
			}
			status.Available++
			continue
		}
		if info.Size() != expectedSize {
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

// completedTurnForgetRecordings maps provider recording ids whose retained
// chunks may carry erased hashes: a turn-scoped provider fence must exist for
// exactly the device the recording is synced under, backed by a tombstone
// whose status is complete and whose database redaction is done.
func completedTurnForgetRecordings(ctx context.Context, db *sql.DB, deviceByRecording map[string]string) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `
SELECT f.provider_recording_id, f.device_id, t.scope, t.status, t.redaction_done
FROM forget_provider_fences f
JOIN forget_tombstones t ON t.id = f.tombstone_id`)
	if err != nil {
		return nil, fmt.Errorf("backup: read forget fences for blank chunk hashes: %w", err)
	}
	defer rows.Close()
	allowed := map[string]bool{}
	for rows.Next() {
		var recordingID, deviceID, scope, forgetStatus string
		var redactionDone int
		if err := rows.Scan(&recordingID, &deviceID, &scope, &forgetStatus, &redactionDone); err != nil {
			return nil, err
		}
		if scope != "turn" || forgetStatus != "complete" || redactionDone != 1 {
			continue
		}
		if deviceID != deviceByRecording[recordingID] {
			continue
		}
		allowed[recordingID] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return allowed, nil
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
