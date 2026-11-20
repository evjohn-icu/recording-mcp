package audioanalysis

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"recordingmcp/internal/memory"
)

// The managed-PCM resolver is the single source of truth for turning a memory
// recording id into the audio file under the profile's audio root (plan 4.2).
// The local-upload path is resolved from the recording's own upload hash; the
// cloud-sync path falls back to the active ASR attachments. Every match must
// agree on one managed file and the resolved path stays inside the root; the
// caller opens it relative to the root, which os.Root then confines to the
// real file.

// Identity constants for sync_recordings rows written by the local upload
// path. They mirror ingest.DeviceID/ImportOwner without importing the ingest
// package.
const (
	importSyncOwner  = "local-import"
	importSyncDevice = "device:local-import"
)

// ResolveRecordingPCM resolves one recording's managed PCM file. The returned
// path is relative to audioRoot so the caller can open it through os.Root and
// can never be tricked into reading a file outside the managed tree.
//
// A local import is found through recordings.source_sha256: a legal 64 hex
// digest selects the sync_recordings row of `import:<first 32 hex>` written by
// the local-import owner/device, and memory.RecordingID must confirm the
// internal identity. Recordings without that provenance fall back to the
// active source attachments: every match must carry complete provenance that
// hashes back to the recording, and all matches must point at the same managed
// file. Deleted rows, empty paths, forged identities, paths outside the root
// and ambiguous sources are refused.
func ResolveRecordingPCM(ctx context.Context, db *sql.DB, audioRoot, recordingID string) (string, error) {
	if strings.TrimSpace(audioRoot) == "" {
		return "", NewError(CodeModelUnavailable, false, "no audio directory configured")
	}
	absRoot, err := filepath.Abs(audioRoot)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(recordingID) == "" {
		return "", NewError(CodeModelUnavailable, false, "recording id is required")
	}
	var sourceHash string
	err = db.QueryRowContext(ctx, `SELECT source_sha256 FROM recordings WHERE id=?`, recordingID).Scan(&sourceHash)
	if err == nil && validHash(sourceHash) {
		providerID := "import:" + strings.ToLower(sourceHash)[:32]
		if memory.RecordingID(importSyncDevice, providerID) == recordingID {
			var stitched string
			rowErr := db.QueryRowContext(ctx,
				`SELECT stitched_path FROM sync_recordings
				 WHERE recording_id=? AND owner=? AND device_id=?`,
				providerID, importSyncOwner, importSyncDevice).Scan(&stitched)
			if rowErr == nil && strings.TrimSpace(stitched) != "" {
				return managedRel(absRoot, stitched)
			}
			// No usable local-import row: fall through to the attachment
			// provenance instead of failing, so repaired or legacy rows keep
			// working.
		}
	} else if err != nil && !isNoRows(err) {
		return "", err
	}

	rows, err := db.QueryContext(ctx,
		`SELECT json_extract(a.source_json,'$.device_id'), json_extract(a.source_json,'$.recording_id'), s.stitched_path
		 FROM source_attachments a
		 JOIN sync_recordings s
		   ON s.recording_id = json_extract(a.source_json,'$.recording_id')
		  AND s.device_id   = json_extract(a.source_json,'$.device_id')
		 WHERE a.recording_id=? AND s.state!='deleted' AND s.stitched_path IS NOT NULL AND s.stitched_path!=''
		 ORDER BY a.id`, recordingID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	resolved := ""
	matches := 0
	for rows.Next() {
		var device, provider, stitched sql.NullString
		if err := rows.Scan(&device, &provider, &stitched); err != nil {
			return "", err
		}
		if !device.Valid || !provider.Valid || !stitched.Valid {
			return "", NewError(CodeModelUnavailable, false, "audio provenance is incomplete")
		}
		if memory.RecordingID(device.String, provider.String) != recordingID {
			return "", NewError(CodeModelUnavailable, false, "audio provenance does not match the recording")
		}
		rel, err := managedRel(absRoot, stitched.String)
		if err != nil {
			return "", err
		}
		if matches > 0 && rel != resolved {
			return "", NewError(CodeModelUnavailable, false, "the managed audio sources disagree on one file")
		}
		resolved = rel
		matches++
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if matches == 0 {
		return "", NewError(CodeModelUnavailable, false, "no managed audio for this recording")
	}
	return resolved, nil
}

// managedRel converts one recorded path into a root-relative path, refusing
// anything that does not name a file strictly below the root.
func managedRel(absRoot, stitched string) (string, error) {
	abs, err := filepath.Abs(stitched)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(absRoot, abs)
	if err != nil || rel == "." || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", NewError(CodeModelUnavailable, false, "audio path escapes the managed root")
	}
	return rel, nil
}

func isNoRows(err error) bool { return err == sql.ErrNoRows }

// WholePCMInput reports the exact PCM window the enqueue paths pin into one
// request: the window length in milliseconds is floor(file bytes / 32) — the
// whole decoded 16 kHz mono s16le stream, rounded down to whole samples — and
// windowSHA256 covers exactly the first endMS*32 bytes, the half-open interval
// an adapter reads. One read-only file descriptor serves both so the length
// and the hash describe the same bytes even if the file is appended to later.
//
// A file shorter than one millisecond reports audio_input_invalid: the ingest
// stage skips audio analysis for that recording and keeps ASR running, while
// the manual enqueue path surfaces the code to the caller. A missing file or
// an IO error is returned unchanged for the caller's existing error path.
func WholePCMInput(ctx context.Context, audioRoot, relativePath string) (durationMS int64, windowSHA256 string, err error) {
	root, err := os.OpenRoot(audioRoot)
	if err != nil {
		return 0, "", NewError(CodeModelUnavailable, false, "audio directory is not readable")
	}
	defer root.Close()
	file, err := root.Open(relativePath)
	if err != nil {
		return 0, "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return 0, "", err
	}
	if !info.Mode().IsRegular() {
		return 0, "", NewError(CodeModelUnavailable, false, "the managed audio is not a regular file")
	}
	endMS := info.Size() / 32
	if endMS <= 0 {
		return 0, "", NewError(CodeInputInvalid, false, "the managed audio holds less than one millisecond of PCM")
	}
	h := sha256.New()
	if _, err := io.CopyN(h, file, endMS*32); err != nil {
		if errors.Is(err, io.EOF) {
			return 0, "", NewError(CodeInputInvalid, false, "the managed audio is shorter than its recorded window")
		}
		return 0, "", err
	}
	return endMS, hex.EncodeToString(h.Sum(nil)), nil
}
