package app

import (
	"context"
	"errors"
	"os"
	"recordingmcp/internal/plane"
	"recordingmcp/internal/retention"
	"strings"
)

// CleanupForget removes managed external copies only after the durable DB
// redaction. Failures leave the tombstone pending for an explicit retry.
func (s *Service) CleanupForget(ctx context.Context, id string) error {
	release, err := retention.Lock(ctx, s.audioRoot, true)
	if err != nil {
		return err
	}
	defer release()
	var conversationID string
	if err := s.Store.DB().QueryRowContext(ctx, `SELECT conversation_id FROM forget_tombstones WHERE id=?`, id).Scan(&conversationID); err != nil {
		return err
	}
	rows, err := s.Store.DB().QueryContext(ctx, `SELECT path,COALESCE(start_byte,0),COALESCE(end_byte,0),remove_file FROM forget_files WHERE tombstone_id=? AND done=0 ORDER BY path`, id)
	if err != nil {
		return err
	}
	type file struct {
		path       string
		start, end int64
		remove     bool
	}
	var files []file
	for rows.Next() {
		var f file
		if err = rows.Scan(&f.path, &f.start, &f.end, &f.remove); err != nil {
			rows.Close()
			return err
		}
		files = append(files, f)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, f := range files {
		if err := eraseAudioFile(s.audioRoot, f.path, f.start, f.end, f.remove); err != nil {
			return err
		}
		if _, err := s.Store.DB().ExecContext(ctx, `UPDATE forget_files SET done=1 WHERE tombstone_id=? AND path=?`, id, f.path); err != nil {
			return err
		}
	}
	if s.ForgetPlane != nil {
		events, err := s.ForgetPlane.Search(ctx, plane.Query{})
		if err != nil {
			return err
		}
		chosen := map[string]bool{}
		for _, e := range events {
			if e.ConversationID != conversationID {
				continue
			}
			refs, malformed := sourceReferences(e)
			// Legacy copies without precise provenance cannot safely retain
			// content from the affected conversation. Explicit independent
			// evidence is preserved, even when its revision is stale.
			if len(refs) == 0 || malformed {
				chosen[e.ID] = true
				continue
			}
			for _, ref := range refs {
				at := strings.LastIndex(ref, "@")
				if at <= 0 {
					chosen[e.ID] = true
					break
				}
				var exists bool
				if err := s.Store.DB().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM turns WHERE id=?)`, ref[:at]).Scan(&exists); err != nil {
					return err
				}
				if !exists {
					chosen[e.ID] = true
					break
				}
			}
		}
		for changed := true; changed; {
			changed = false
			for _, e := range events {
				target, _ := e.StructuredData["target_event_id"].(string)
				if !chosen[e.ID] && target != "" && chosen[target] {
					chosen[e.ID] = true
					changed = true
				}
			}
		}
		ids := make([]string, 0, len(chosen))
		for id := range chosen {
			ids = append(ids, id)
		}
		if len(ids) > 0 {
			if err := s.ForgetPlane.PurgeContent(ctx, ids); err != nil {
				return err
			}
		}
	}
	if index, ok := s.Semantic.(interface{ Invalidate() }); ok {
		index.Invalidate()
	}
	for _, dir := range []string{s.ManagedIndexDir, s.ManagedBackupDir} {
		if dir != "" {
			if err := clearManagedDirectory(dir); err != nil {
				return err
			}
		}
	}
	// Do not claim completion while WAL copies are still held by another
	// reader/process. The caller can retry after those readers release them.
	var busy, pages, checkpointed int
	if err := s.Store.DB().QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &pages, &checkpointed); err != nil {
		return err
	}
	if busy != 0 {
		return errors.New("database readers still hold old WAL pages; retry cleanup")
	}
	if _, err := s.Store.DB().ExecContext(ctx, `VACUUM`); err != nil {
		return err
	}
	return nil
}

func clearManagedDirectory(path string) error {
	root, err := os.OpenRoot(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer root.Close()
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	entries, err := dir.ReadDir(-1)
	dir.Close()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := root.RemoveAll(entry.Name()); err != nil {
			return err
		}
	}
	return nil
}
