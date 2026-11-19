// Package backup implements stop-write backup, restore and verification for a
// recording-mcp profiles root.
//
// A backup is only valid when no writer is running: the caller asserts it with
// ConfirmWritersStopped and the implementation proves it by hashing the
// complete file set before and after the copy. A restore refuses to publish an
// archive that predates the permanent-forget barriers of the trusted live root
// it is compared against, so a forgotten source cannot be resurrected from an
// older copy.
package backup

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ManifestSchema identifies the archive format. A change to the manifest shape
// requires a new schema value.
const ManifestSchema = "backup-archive-manifest-v1"

// Error codes. They are stable identifiers a caller can branch on; Detail is a
// human-readable explanation with no source text.
const (
	CodeWritersActive             = "writers_active"
	CodeBackupPredatesForget      = "backup_predates_forget"
	CodeForgetBarrierUnverifiable = "forget_barrier_unverifiable"
	CodeForgetUnfinished          = "forget_unfinished"
	CodeInvalidArchive            = "invalid_archive"
	CodeArchiveExists             = "archive_exists"
	CodeDestinationNotEmpty       = "destination_not_empty"
	CodeIntegrityFailed           = "integrity_check_failed"
	CodeForeignKeyFailed          = "foreign_key_check_failed"
	CodeAudioMissing              = "audio_reference_missing"
	CodeAudioOutOfBounds          = "audio_reference_out_of_bounds"
	CodeAudioHashMismatch         = "audio_reference_hash_mismatch"
	CodeProfileUnreadable         = "profile_unreadable"
	CodeNotADirectory             = "not_a_directory"
)

// Error is one structured backup failure.
type Error struct {
	Code   string `json:"code"`
	Detail string `json:"detail,omitempty"`
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Detail == "" {
		return e.Code
	}
	return e.Code + ": " + e.Detail
}

func newError(code, format string, args ...any) error {
	return &Error{Code: code, Detail: fmt.Sprintf(format, args...)}
}

// ErrorCode returns the stable error code of err, or "" when err is not a
// structured backup error.
func ErrorCode(err error) string {
	var target *Error
	if errors.As(err, &target) {
		return target.Code
	}
	return ""
}

// FileEntry is one archived regular file.
type FileEntry struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// AudioStatus summarises the managed-audio references of one profile.
type AudioStatus struct {
	Available           int `json:"available"`
	DeclaredUnavailable int `json:"declared_unavailable"`
	Missing             int `json:"missing"`
	OutOfBounds         int `json:"out_of_bounds"`
	HashMismatch        int `json:"hash_mismatch"`
}

// ProfileEntry is one archived profile.
type ProfileEntry struct {
	ID     string      `json:"id"`
	Schema int         `json:"schema"`
	Audio  AudioStatus `json:"audio"`
}

// Manifest is the archive index written as manifest.json.
type Manifest struct {
	Schema        string         `json:"schema"`
	CreatedAt     string         `json:"created_at"`
	Profiles      []ProfileEntry `json:"profiles"`
	Files         []FileEntry    `json:"files"`
	BarrierDigest string         `json:"barrier_digest"`
	BarrierIDs    []string       `json:"barrier_ids"`
}

// CreateOptions configures a stop-write backup.
type CreateOptions struct {
	ProfilesRoot string
	Out          string
	// ConfirmWritersStopped is the operator's explicit assertion. Without it
	// the command refuses to run: an advisory lock cannot stop a process that
	// does not participate in it.
	ConfirmWritersStopped bool
}

// Create writes a consistent archive of ProfilesRoot to Out and returns the
// manifest that was written. Out must not exist and must be outside the
// profiles root.
func Create(ctx context.Context, opts CreateOptions) (Manifest, error) {
	if !opts.ConfirmWritersStopped {
		return Manifest{}, newError(CodeWritersActive, "all writers must be stopped; pass --confirm-writers-stopped")
	}
	root, err := requireRealDir(opts.ProfilesRoot)
	if err != nil {
		return Manifest{}, err
	}
	out, err := filepath.Abs(opts.Out)
	if err != nil {
		return Manifest{}, fmt.Errorf("backup: resolve output: %w", err)
	}
	if inside(root, out) {
		return Manifest{}, newError(CodeInvalidArchive, "archive output must be outside the profiles root")
	}
	if _, err := os.Lstat(out); err == nil {
		return Manifest{}, newError(CodeArchiveExists, "archive already exists: %s", filepath.Base(out))
	} else if !errors.Is(err, os.ErrNotExist) {
		return Manifest{}, err
	}
	if err := os.MkdirAll(filepath.Dir(out), 0700); err != nil {
		return Manifest{}, fmt.Errorf("backup: create output directory: %w", err)
	}

	before, err := snapshot(root)
	if err != nil {
		return Manifest{}, err
	}
	barriers, err := ReadBarriers(ctx, root)
	if err != nil {
		return Manifest{}, err
	}
	if len(barriers.Incomplete) > 0 {
		return Manifest{}, newError(CodeForgetUnfinished, "profile has unfinished forget cleanup (%d tombstone(s)); resume forget before backing up", len(barriers.Incomplete))
	}
	profiles, err := inspectProfiles(ctx, root)
	if err != nil {
		return Manifest{}, err
	}
	for _, profile := range profiles {
		if profile.Audio.Missing+profile.Audio.OutOfBounds+profile.Audio.HashMismatch > 0 {
			return Manifest{}, newError(CodeAudioMissing, "profile %s has %d missing and %d out-of-bounds audio reference(s)", profile.ID, profile.Audio.Missing, profile.Audio.OutOfBounds)
		}
	}
	manifest := Manifest{
		Schema:        ManifestSchema,
		CreatedAt:     time.Now().UTC().Format(time.RFC3339),
		Profiles:      profiles,
		Files:         fileEntries(before),
		BarrierDigest: barriers.Digest,
		BarrierIDs:    barriers.IDs,
	}

	tmp := out + ".tmp-" + randomSuffix()
	if err := writeArchive(tmp, root, before, manifest); err != nil {
		_ = os.Remove(tmp)
		return Manifest{}, err
	}
	after, err := snapshot(root)
	if err != nil {
		_ = os.Remove(tmp)
		return Manifest{}, err
	}
	if !sameSnapshot(before, after) {
		_ = os.Remove(tmp)
		return Manifest{}, newError(CodeWritersActive, "files changed during the copy; stop every writer and retry")
	}
	if err := os.Rename(tmp, out); err != nil {
		_ = os.Remove(tmp)
		return Manifest{}, fmt.Errorf("backup: publish archive: %w", err)
	}
	return manifest, nil
}

// RestoreOptions configures a restore into an empty destination.
type RestoreOptions struct {
	Archive       string
	Dest          string
	BarrierSource string
	// ConfirmWritersStopped asserts that the barrier source root has no writer.
	ConfirmWritersStopped bool
}

// Restore extracts an archive into Dest after validating every database, the
// managed audio references and the forget barrier against BarrierSource.
func Restore(ctx context.Context, opts RestoreOptions) (Manifest, error) {
	if !opts.ConfirmWritersStopped {
		return Manifest{}, newError(CodeWritersActive, "the barrier source must be stopped; pass --confirm-writers-stopped")
	}
	source, err := requireRealDir(opts.BarrierSource)
	if err != nil {
		return Manifest{}, err
	}
	archive, err := filepath.Abs(strings.TrimSpace(opts.Archive))
	if err != nil {
		return Manifest{}, fmt.Errorf("backup: resolve archive: %w", err)
	}
	if info, err := os.Lstat(archive); err != nil || !info.Mode().IsRegular() {
		return Manifest{}, newError(CodeInvalidArchive, "archive is not a regular file")
	}
	dest, err := filepath.Abs(strings.TrimSpace(opts.Dest))
	if err != nil {
		return Manifest{}, fmt.Errorf("backup: resolve destination: %w", err)
	}
	if inside(source, dest) || inside(dest, source) {
		return Manifest{}, newError(CodeDestinationNotEmpty, "destination and barrier source must not contain each other")
	}
	if err := requireEmptyDestination(dest); err != nil {
		return Manifest{}, err
	}

	sourceBefore, err := ReadBarriers(ctx, source)
	if err != nil {
		return Manifest{}, err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dest), ".restore-"+randomSuffix())
	if err != nil {
		return Manifest{}, fmt.Errorf("backup: create staging directory: %w", err)
	}
	defer os.RemoveAll(tmp)
	manifest, err := extractArchive(archive, tmp)
	if err != nil {
		return Manifest{}, err
	}
	if manifest.Schema != ManifestSchema {
		return Manifest{}, newError(CodeInvalidArchive, "unsupported manifest schema %q", manifest.Schema)
	}
	if err := validateRoot(ctx, tmp); err != nil {
		return Manifest{}, err
	}
	archiveBarriers, err := ReadBarriers(ctx, tmp)
	if err != nil {
		return Manifest{}, err
	}
	sourceAfter, err := ReadBarriers(ctx, source)
	if err != nil {
		return Manifest{}, err
	}
	if sourceAfter.Digest != sourceBefore.Digest {
		return Manifest{}, newError(CodeForgetBarrierUnverifiable, "the trusted root's forget barrier changed during restore; all writers must stay stopped")
	}
	if err := compareBarriers(archiveBarriers, sourceAfter); err != nil {
		return Manifest{}, err
	}
	if err := os.Remove(dest); err != nil && !errors.Is(err, os.ErrNotExist) {
		return Manifest{}, fmt.Errorf("backup: replace destination: %w", err)
	}
	if err := os.Rename(tmp, dest); err != nil {
		return Manifest{}, fmt.Errorf("backup: publish restore: %w", err)
	}
	return manifest, nil
}

// VerifyOptions configures a read-only verification pass.
type VerifyOptions struct {
	ProfilesRoot string
}

// VerifyReport is the verify command's result.
type VerifyReport struct {
	Schema   string         `json:"schema"`
	Profiles []ProfileEntry `json:"profiles"`
	Barrier  BarrierSet     `json:"barrier"`
}

// Verify checks every profile database, its managed audio references and the
// forget state without modifying the root.
func Verify(ctx context.Context, opts VerifyOptions) (VerifyReport, error) {
	root, err := requireRealDir(opts.ProfilesRoot)
	if err != nil {
		return VerifyReport{}, err
	}
	profiles, err := inspectProfiles(ctx, root)
	if err != nil {
		return VerifyReport{}, err
	}
	for _, profile := range profiles {
		if profile.Audio.Missing+profile.Audio.OutOfBounds+profile.Audio.HashMismatch > 0 {
			return VerifyReport{}, newError(CodeAudioMissing, "profile %s has %d missing and %d out-of-bounds audio reference(s)", profile.ID, profile.Audio.Missing, profile.Audio.OutOfBounds)
		}
	}
	barriers, err := ReadBarriers(ctx, root)
	if err != nil {
		return VerifyReport{}, err
	}
	if len(barriers.Incomplete) > 0 {
		return VerifyReport{}, newError(CodeForgetUnfinished, "profile has unfinished forget cleanup (%d tombstone(s))", len(barriers.Incomplete))
	}
	return VerifyReport{Schema: "backup-verify-report-v1", Profiles: profiles, Barrier: barriers}, nil
}

// requireRealDir resolves an existing real directory, rejecting symlinks.
func requireRealDir(path string) (string, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return "", newError(CodeNotADirectory, "path is required")
	}
	abs, err := filepath.Abs(trimmed)
	if err != nil {
		return "", fmt.Errorf("backup: resolve path: %w", err)
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return "", newError(CodeNotADirectory, "path does not exist")
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", newError(CodeNotADirectory, "path must be a real directory")
	}
	return abs, nil
}

// requireEmptyDestination accepts a missing path or an existing empty real
// directory.
func requireEmptyDestination(dest string) error {
	info, err := os.Lstat(dest)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return newError(CodeDestinationNotEmpty, "destination must be an empty directory")
	}
	entries, err := os.ReadDir(dest)
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return newError(CodeDestinationNotEmpty, "destination directory is not empty")
	}
	return nil
}

// inside reports whether child is root itself or below root.
func inside(root, child string) bool {
	rel, err := filepath.Rel(root, child)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && !filepath.IsAbs(rel))
}

func randomSuffix() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

// sha256File hashes one regular file.
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := copyBuffer(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func fileEntries(files []snapshotFile) []FileEntry {
	out := make([]FileEntry, 0, len(files))
	for _, f := range files {
		out = append(out, FileEntry{Path: f.Path, Size: f.Size, SHA256: f.SHA256})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}
