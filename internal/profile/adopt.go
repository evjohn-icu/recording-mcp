package profile

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// AdoptLegacy creates a fresh profile from a consistent read-only SQLite
// snapshot plus explicitly supplied local audio/event directories. It never
// modifies or migrates the source database and never merges into an existing
// profile.
func (m *Manager) AdoptLegacy(ctx context.Context, name, password, legacyDB, audioDir, eventsDB string) (Paths, error) {
	if strings.TrimSpace(legacyDB) == "" {
		return Paths{}, errors.New("profile: legacy database path required")
	}
	source, err := regularFilePath(legacyDB)
	if err != nil {
		return Paths{}, fmt.Errorf("profile: legacy database: %w", err)
	}
	var audioSource string
	if strings.TrimSpace(audioDir) != "" {
		audioSource, err = realDirectoryPath(audioDir)
		if err != nil {
			return Paths{}, fmt.Errorf("profile: audio directory: %w", err)
		}
	}
	var eventSource string
	if strings.TrimSpace(eventsDB) != "" {
		eventSource, err = regularFilePath(eventsDB)
		if err != nil {
			return Paths{}, fmt.Errorf("profile: events database: %w", err)
		}
	}
	paths, err := m.Create(name, password)
	if err != nil {
		return Paths{}, err
	}
	success := false
	defer func() {
		if !success {
			_ = os.RemoveAll(paths.Root)
		}
	}()
	if err = vacuumReadOnlyInto(ctx, source, paths.Database); err != nil {
		return Paths{}, fmt.Errorf("profile: snapshot legacy database: %w", err)
	}
	if err = os.Chmod(paths.Database, 0600); err != nil {
		return Paths{}, err
	}
	if audioSource != "" {
		if err = copyAudioTree(audioSource, paths.Audio); err != nil {
			return Paths{}, err
		}
	}
	if err = rewriteAudioPaths(ctx, paths.Database, audioSource, paths.Audio); err != nil {
		return Paths{}, err
	}
	if eventSource != "" {
		if err = vacuumReadOnlyInto(ctx, eventSource, paths.Plane); err != nil {
			return Paths{}, fmt.Errorf("profile: snapshot events database: %w", err)
		}
		if err = os.Chmod(paths.Plane, 0600); err != nil {
			return Paths{}, err
		}
	}
	success = true
	return paths, nil
}

func regularFilePath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("must be a regular non-symlink file")
	}
	return abs, nil
}
func realDirectoryPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("must be a real non-symlink directory")
	}
	return abs, nil
}

func vacuumReadOnlyInto(ctx context.Context, source, destination string) error {
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(source)}
	// WAL mode keeps recent commits in the -wal file. VACUUM INTO from a
	// read-only handle cannot always read that state (and may need the -shm
	// index), so checkpoint first through a read-write handle. If the source is
	// genuinely read-only, fall back to the previous read-only behavior.
	if db, err := sql.Open("sqlite", u.String()+"?_pragma=busy_timeout(5000)"); err == nil {
		db.SetMaxOpenConns(1)
		if _, err := db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err == nil {
			_, err = db.ExecContext(ctx, `VACUUM INTO ?`, destination)
			db.Close()
			if err == nil {
				return nil
			}
		} else {
			db.Close()
		}
	}
	db, err := sql.Open("sqlite", u.String()+"?mode=ro")
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err = db.ExecContext(ctx, `VACUUM INTO ?`, destination); err != nil {
		return err
	}
	return nil
}

func copyAudioTree(source, destination string) error {
	root, err := os.OpenRoot(source)
	if err != nil {
		return err
	}
	defer root.Close()
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == source {
			return nil
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("profile: refusing symlink in legacy audio tree: %s", rel)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		target := filepath.Join(destination, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0700)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		in, err := root.Open(filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		defer in.Close()
		if err = os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(out, in)
		syncErr := out.Sync()
		closeErr := out.Close()
		if copyErr != nil {
			return copyErr
		}
		if syncErr != nil {
			return syncErr
		}
		return closeErr
	})
}

func rewriteAudioPaths(ctx context.Context, database, oldRoot, newRoot string) error {
	db, err := sql.Open("sqlite", database)
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	exists := func(table string) (bool, error) {
		var n int
		err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n)
		return n > 0, err
	}
	recs, err := exists("sync_recordings")
	if err != nil {
		return err
	}
	chunks, err := exists("sync_chunks")
	if err != nil {
		return err
	}
	if !recs && !chunks {
		return nil
	}
	if oldRoot == "" {
		if recs {
			var n int
			if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sync_recordings WHERE stitched_path IS NOT NULL AND stitched_path<>''`).Scan(&n); err != nil {
				return err
			}
			if n > 0 {
				return errors.New("profile: --audio-dir is required to adopt stored sync audio paths")
			}
		}
		if chunks {
			var n int
			if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sync_chunks WHERE file_path<>''`).Scan(&n); err != nil {
				return err
			}
			if n > 0 {
				return errors.New("profile: --audio-dir is required to adopt stored sync chunk paths")
			}
		}
		return nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rewrite := func(query, update string) error {
		rows, err := tx.QueryContext(ctx, query)
		if err != nil {
			return err
		}
		type pair struct{ id, path string }
		var values []pair
		for rows.Next() {
			var x pair
			if err = rows.Scan(&x.id, &x.path); err != nil {
				rows.Close()
				return err
			}
			values = append(values, x)
		}
		if err = rows.Close(); err != nil {
			return err
		}
		for _, x := range values {
			if x.path == "" {
				continue
			}
			stored := x.path
			if !filepath.IsAbs(stored) {
				stored = filepath.Join(oldRoot, stored)
			}
			abs, e := filepath.Abs(stored)
			if e != nil {
				return e
			}
			rel, e := filepath.Rel(oldRoot, abs)
			if e != nil || rel == "." || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
				return fmt.Errorf("profile: stored audio path is outside --audio-dir: %s", x.path)
			}
			dest := filepath.Join(newRoot, rel)
			info, e := os.Lstat(dest)
			if e != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("profile: referenced audio file was not copied: %s", rel)
			}
			if _, e = tx.ExecContext(ctx, update, dest, x.id); e != nil {
				return e
			}
		}
		return nil
	}
	if recs {
		if err = rewrite(`SELECT recording_id,stitched_path FROM sync_recordings WHERE stitched_path IS NOT NULL AND stitched_path<>''`, `UPDATE sync_recordings SET stitched_path=? WHERE recording_id=?`); err != nil {
			return err
		}
	}
	if chunks {
		if err = rewrite(`SELECT recording_id||':'||seq,file_path FROM sync_chunks WHERE file_path<>''`, `UPDATE sync_chunks SET file_path=? WHERE recording_id||':'||seq=?`); err != nil {
			return err
		}
	}
	return tx.Commit()
}
