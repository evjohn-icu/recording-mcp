package backup

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// snapshotFile is one regular file captured by the pre/post copy snapshot.
type snapshotFile struct {
	Path   string
	Size   int64
	SHA256 string
}

// snapshot hashes every regular file below root. Symlinks and special files
// are refused: a profile root is trusted local state, never a link farm, and
// following a link could copy data from outside the root.
func snapshot(root string) ([]snapshotFile, error) {
	files := []snapshotFile{}
	err := filepath.WalkDir(root, func(current string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, current)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if entry.IsDir() {
			if isExcludedDir(rel) {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return newError(CodeInvalidArchive, "profiles root contains a non-regular entry: %s", rel)
		}
		hash, err := sha256File(current)
		if err != nil {
			return err
		}
		files = append(files, snapshotFile{Path: rel, Size: info.Size(), SHA256: hash})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

// isExcludedDir reports whether rel is a directory whose contents must not be
// archived: a profile's own backup output and external caches are not part of
// the restorable state.
func isExcludedDir(rel string) bool {
	parts := strings.Split(rel, "/")
	for i, part := range parts {
		if part == "backups" && i == 1 {
			return true
		}
		if part == "cache" || part == ".cache" {
			return true
		}
	}
	return false
}

// sameSnapshot reports whether two snapshots describe identical files.
func sameSnapshot(a, b []snapshotFile) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// writeArchive writes manifest.json first, then every snapshot file. The
// archive is written to a temporary file and renamed by the caller only after
// the post-copy snapshot agrees.
func writeArchive(archive, root string, files []snapshotFile, manifest Manifest) error {
	out, err := os.OpenFile(archive, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("backup: create archive: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			out.Close()
			_ = os.Remove(archive)
		}
	}()
	gz := gzip.NewWriter(out)
	tw := tar.NewWriter(gz)

	manifestBytes, err := json.MarshalIndent(manifest, "", " ")
	if err != nil {
		return fmt.Errorf("backup: encode manifest: %w", err)
	}
	if err := writeTarEntry(tw, "manifest.json", manifestBytes); err != nil {
		return err
	}
	for _, file := range files {
		if err := writeTarFile(tw, root, file); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("backup: finish archive: %w", err)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("backup: finish compression: %w", err)
	}
	if err := out.Sync(); err != nil {
		return fmt.Errorf("backup: sync archive: %w", err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("backup: close archive: %w", err)
	}
	ok = true
	return nil
}

func writeTarEntry(tw *tar.Writer, name string, data []byte) error {
	header := &tar.Header{Name: name, Mode: 0600, Size: int64(len(data)), Typeflag: tar.TypeReg}
	if err := tw.WriteHeader(header); err != nil {
		return fmt.Errorf("backup: write header: %w", err)
	}
	if _, err := tw.Write(data); err != nil {
		return fmt.Errorf("backup: write entry: %w", err)
	}
	return nil
}

// writeTarFile streams one file into the archive and fails with writers_active
// when the bytes on disk no longer match the pre-copy snapshot.
func writeTarFile(tw *tar.Writer, root string, file snapshotFile) error {
	full := filepath.Join(root, filepath.FromSlash(file.Path))
	f, err := os.Open(full)
	if err != nil {
		return fmt.Errorf("backup: open %s: %w", file.Path, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("backup: stat %s: %w", file.Path, err)
	}
	if !info.Mode().IsRegular() || info.Size() != file.Size {
		return newError(CodeWritersActive, "file changed during the copy: %s", file.Path)
	}
	header := &tar.Header{Name: file.Path, Mode: 0600, Size: file.Size, Typeflag: tar.TypeReg}
	if err := tw.WriteHeader(header); err != nil {
		return fmt.Errorf("backup: write header: %w", err)
	}
	h := sha256.New()
	if _, err := copyBuffer(io.MultiWriter(tw, h), f); err != nil {
		return fmt.Errorf("backup: copy %s: %w", file.Path, err)
	}
	if hex.EncodeToString(h.Sum(nil)) != file.SHA256 {
		return newError(CodeWritersActive, "file changed during the copy: %s", file.Path)
	}
	return nil
}

// extractArchive unpacks an archive into dest, refusing absolute paths,
// traversal, links and non-regular entries. Every extracted byte is hashed and
// must match the manifest.
func extractArchive(archive, dest string) (Manifest, error) {
	in, err := os.Open(archive)
	if err != nil {
		return Manifest{}, fmt.Errorf("backup: open archive: %w", err)
	}
	defer in.Close()
	gz, err := gzip.NewReader(in)
	if err != nil {
		return Manifest{}, newError(CodeInvalidArchive, "archive is not a gzip stream")
	}
	defer gz.Close()
	tr := tar.NewReader(gz)

	root, err := os.OpenRoot(dest)
	if err != nil {
		return Manifest{}, fmt.Errorf("backup: open staging root: %w", err)
	}
	defer root.Close()

	var manifest Manifest
	haveManifest := false
	seen := map[string]bool{}
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Manifest{}, newError(CodeInvalidArchive, "archive is truncated or corrupt")
		}
		name := header.Name
		if err := validateEntryName(name); err != nil {
			return Manifest{}, err
		}
		if header.Typeflag != tar.TypeReg {
			return Manifest{}, newError(CodeInvalidArchive, "archive entry is not a regular file: %s", name)
		}
		if header.Size < 0 || header.Size > 1<<40 {
			return Manifest{}, newError(CodeInvalidArchive, "archive entry has an invalid size: %s", name)
		}
		if !haveManifest {
			if name != "manifest.json" {
				return Manifest{}, newError(CodeInvalidArchive, "archive does not start with manifest.json")
			}
			limited := io.LimitReader(tr, 16<<20)
			data, err := io.ReadAll(limited)
			if err != nil {
				return Manifest{}, newError(CodeInvalidArchive, "manifest is unreadable")
			}
			if err := json.Unmarshal(data, &manifest); err != nil {
				return Manifest{}, newError(CodeInvalidArchive, "manifest is not valid JSON")
			}
			haveManifest = true
			continue
		}
		if name == "manifest.json" {
			return Manifest{}, newError(CodeInvalidArchive, "archive contains a duplicate manifest")
		}
		expected, ok := manifestFile(manifest, name)
		if !ok {
			return Manifest{}, newError(CodeInvalidArchive, "archive entry is not listed in the manifest: %s", name)
		}
		if seen[name] {
			return Manifest{}, newError(CodeInvalidArchive, "archive contains a duplicate entry: %s", name)
		}
		seen[name] = true
		if err := writeExtracted(root, name, tr, expected); err != nil {
			return Manifest{}, err
		}
	}
	if !haveManifest {
		return Manifest{}, newError(CodeInvalidArchive, "archive has no manifest")
	}
	if len(seen) != len(manifest.Files) {
		return Manifest{}, newError(CodeInvalidArchive, "archive is missing %d file(s) listed in the manifest", len(manifest.Files)-len(seen))
	}
	return manifest, nil
}

// validateEntryName rejects any path that could escape the destination.
func validateEntryName(name string) error {
	if name == "" || strings.ContainsRune(name, 0) {
		return newError(CodeInvalidArchive, "archive contains an empty entry name")
	}
	if strings.Contains(name, `\`) {
		return newError(CodeInvalidArchive, "archive contains a backslash entry name")
	}
	cleaned := path.Clean(name)
	if cleaned != name || strings.HasPrefix(cleaned, "../") || cleaned == ".." || path.IsAbs(cleaned) {
		return newError(CodeInvalidArchive, "archive entry escapes the destination: %s", name)
	}
	if cleaned == "." {
		return newError(CodeInvalidArchive, "archive contains a directory entry name")
	}
	return nil
}

func manifestFile(manifest Manifest, name string) (FileEntry, bool) {
	for _, file := range manifest.Files {
		if file.Path == name {
			return file, true
		}
	}
	return FileEntry{}, false
}

// writeExtracted writes one entry below dest and verifies size and hash.
func writeExtracted(root *os.Root, name string, reader io.Reader, expected FileEntry) error {
	dir := path.Dir(name)
	if dir != "." {
		if err := os.MkdirAll(filepath.Join(root.Name(), filepath.FromSlash(dir)), 0700); err != nil {
			return fmt.Errorf("backup: create directory for %s: %w", name, err)
		}
	}
	f, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("backup: create %s: %w", name, err)
	}
	h := sha256.New()
	written, copyErr := copyBuffer(io.MultiWriter(f, h), io.LimitReader(reader, expected.Size+1))
	syncErr := f.Sync()
	closeErr := f.Close()
	if copyErr != nil {
		return fmt.Errorf("backup: extract %s: %w", name, copyErr)
	}
	if syncErr != nil {
		return fmt.Errorf("backup: sync %s: %w", name, syncErr)
	}
	if closeErr != nil {
		return fmt.Errorf("backup: close %s: %w", name, closeErr)
	}
	if written != expected.Size {
		return newError(CodeInvalidArchive, "entry size differs from the manifest: %s", name)
	}
	if hex.EncodeToString(h.Sum(nil)) != expected.SHA256 {
		return newError(CodeInvalidArchive, "entry hash differs from the manifest: %s", name)
	}
	return nil
}

// copyBuffer is io.Copy with a reusable buffer.
func copyBuffer(dst io.Writer, src io.Reader) (int64, error) {
	buf := make([]byte, 64<<10)
	return io.CopyBuffer(dst, src, buf)
}
