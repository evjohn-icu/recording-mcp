package app

import (
	"context"
	"errors"
	"os"
	"recordingmcp/internal/retention"
)

const profileRetirementTombstone = "profile-retirement-v1"

// BeginProfileRetirement fences every SQLite ingress before touching any
// retained file or managed derivative.
func (s *Service) BeginProfileRetirement(ctx context.Context) error {
	return s.Store.BeginProfileRetirement(ctx, profileRetirementTombstone)
}

// CleanupRetiredProfile resumes a password-authorized local profile purge.
// The profile directory and credentials stay as a retired marker; the
// canonical database retains only profile_control after completion.
func (s *Service) CleanupRetiredProfile(ctx context.Context, tasksDir string) error {
	_, retired, err := s.Store.ProfileRetirement(ctx)
	if err != nil {
		return err
	}
	if !retired {
		return errors.New("profile retirement is not fenced")
	}
	// Purge canonical/event databases immediately after the committed fence;
	// waiting for active audio readers must not leave readable transcript rows.
	if s.ForgetPlane != nil {
		if err := s.ForgetPlane.RetireAndPurge(ctx); err != nil {
			return err
		}
	}
	if err = s.Store.PurgeRetiredProfile(ctx); err != nil {
		return err
	}
	var busy, pages, checkpointed int
	if err = s.Store.DB().QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &pages, &checkpointed); err != nil {
		return err
	}
	if busy != 0 {
		return errors.New("database readers still hold old pages; retry profile cleanup")
	}
	if _, err = s.Store.DB().ExecContext(ctx, `VACUUM`); err != nil {
		return err
	}
	release, err := retention.Lock(ctx, s.audioRoot, true)
	if err != nil {
		return err
	}
	defer release()
	if s.audioRoot != "" {
		if err := clearAudioDirectory(s.audioRoot); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	for _, dir := range []string{tasksDir, s.ManagedIndexDir, s.ManagedBackupDir} {
		if dir != "" {
			if err := clearManagedDirectory(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}

	if index, ok := s.Semantic.(interface{ Invalidate() }); ok {
		index.Invalidate()
	}
	return nil
}

// RetireProfile is idempotent and intended for the authenticated local web
// profile-management flow.
func (s *Service) RetireProfile(ctx context.Context, tasksDir string) error {
	if err := s.BeginProfileRetirement(ctx); err != nil {
		return err
	}
	return s.CleanupRetiredProfile(ctx, tasksDir)
}

func clearAudioDirectory(path string) error {
	root, err := os.OpenRoot(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer root.Close()
	d, err := root.Open(".")
	if err != nil {
		return err
	}
	entries, err := d.ReadDir(-1)
	_ = d.Close()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() == ".retention.lock" {
			continue
		}
		if err := root.RemoveAll(entry.Name()); err != nil {
			return err
		}
	}
	return nil
}
