package app

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// eraseAudioFile accepts only a persisted manifest path beneath the configured
// audio root. It never interprets a path from an HTTP request. Silence preserves
// sample coordinates, so later utterances keep their original offsets.
func eraseAudioFile(rootPath, path string, from, to int64, remove bool) error {
	if rootPath == "" {
		return errors.New("audio cleanup requires a configured audio root")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(rootPath, abs)
	if err != nil || rel == "." || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return errors.New("audio cleanup path is outside the managed root")
	}
	root, err := os.OpenRoot(rootPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer root.Close()
	info, err := root.Lstat(rel)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("audio cleanup requires a regular file")
	}
	if remove {
		return root.Remove(rel)
	}
	if from < 0 || to <= from || from%2 != 0 || to%2 != 0 || info.Size()%2 != 0 || to > info.Size() {
		return errors.New("audio cleanup interval does not match PCM samples")
	}
	if !strings.EqualFold(filepath.Ext(rel), ".pcm") {
		return errors.New("interval cleanup requires PCM")
	}
	in, err := root.Open(rel)
	if err != nil {
		return err
	}
	defer in.Close()
	// A deterministic pending name is safe to discard on retry. It remains in
	// the same controlled directory and is never an audio-serving target.
	tmp := rel + ".forget-pending"
	if err := root.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	out, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		out.Close()
		if !ok {
			_ = root.Remove(tmp)
		}
	}()
	if _, err = io.CopyN(out, in, from); err != nil {
		return err
	}
	zeros := make([]byte, 64*1024)
	for remaining := to - from; remaining > 0; {
		n := min(remaining, int64(len(zeros)))
		if _, err = out.Write(zeros[:n]); err != nil {
			return err
		}
		remaining -= n
	}
	if _, err = in.Seek(to, io.SeekStart); err != nil {
		return err
	}
	if _, err = io.Copy(out, in); err != nil {
		return err
	}
	if err = out.Sync(); err != nil {
		return err
	}
	if err = out.Close(); err != nil {
		return err
	}
	if err = root.Rename(tmp, rel); err != nil {
		return err
	}
	dir, err := root.Open(filepath.Dir(rel))
	if err != nil {
		return err
	}
	err = dir.Sync()
	_ = dir.Close()
	if err != nil {
		return fmt.Errorf("persist audio replacement: %w", err)
	}
	ok = true
	return nil
}
