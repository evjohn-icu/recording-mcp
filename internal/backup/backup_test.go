package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"recordingmcp/internal/memory"
)

// seedProfile creates a profile root with one conversation, recording, turn,
// managed audio file and (optionally) a completed forget tombstone.
func seedProfile(t *testing.T, root, name string, withForget bool) string {
	t.Helper()
	profileRoot := filepath.Join(root, name)
	dbPath := filepath.Join(profileRoot, "db", "context-memory.db")
	store, err := memory.Open(dbPath)
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	defer store.Close()
	db := store.DB()
	execSQL(t, db, `INSERT INTO conversations(id,title,summary,annotation_version,annotation_basis,recording_id,source_sha256,transcript_version,duration_ms,transcript_json,annotations_json)
 VALUES('conv:a','Alpha','','v1','','rec:a','sha','tv',1000,'{}','{}')`)
	execSQL(t, db, `INSERT INTO recordings(id,conversation_id,source_file,source_sha256,duration_ms,recording_started_at,absolute_time_known)
 VALUES('rec:a','conv:a','file.pcm','sha',1000,NULL,0)`)
	execSQL(t, db, `INSERT INTO turns(id,conversation_id,recording_id,segment_index,global_start_ms,global_end_ms,speaker_scoped_id,speaker_entity_id,content,source_sha256,transcript_version,original_turn_id,turn_json)
 VALUES('t:1','conv:a','rec:a',0,0,1000,NULL,NULL,'restored turn text','sha','rev-1','t:1','{}')`)

	audioDir := filepath.Join(profileRoot, "audio")
	if err := os.MkdirAll(audioDir, 0700); err != nil {
		t.Fatalf("mkdir audio: %v", err)
	}
	pcmPath := filepath.Join(audioDir, "rec-a.pcm")
	if err := os.WriteFile(pcmPath, bytes.Repeat([]byte{0x01, 0x02}, 16*1000), 0600); err != nil {
		t.Fatalf("write pcm: %v", err)
	}
	execSQL(t, db, `INSERT INTO sync_recordings(recording_id,owner,device_id,state,stitched_path,updated_at)
 VALUES('provider-rec','owner','device','ingested',?,1)`, pcmPath)
	if withForget {
		execSQL(t, db, `INSERT INTO forget_tombstones(id,scope,requested_id,locator_key,expected_state_hash,redaction_done,status,created_at)
 VALUES('forget:test','turn','t:1','locator:forget:test','hash',1,'complete',1)`)
		execSQL(t, db, `INSERT INTO forget_provider_fences(tombstone_id,provider_recording_id,device_id,created_at)
 VALUES('forget:test','provider-rec','device',1)`)
	}
	return profileRoot
}

// openSQL opens the profile database directly for seeding.
func openSQL(path string) (*sql.DB, error) {
	return sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
}

// execSQL fails the test when a seed statement fails.
func execSQL(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

func completedForget(t *testing.T, root, name, tombstoneID string) {
	t.Helper()
	db, err := openSQL(filepath.Join(root, name, "db", "context-memory.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	execSQL(t, db, `INSERT INTO forget_tombstones(id,scope,requested_id,locator_key,expected_state_hash,redaction_done,status,created_at)
 VALUES(?, 'turn','t:1',?,'hash',1,'complete',1)`, tombstoneID, "locator:"+tombstoneID)
	execSQL(t, db, `INSERT INTO forget_provider_fences(tombstone_id,provider_recording_id,device_id,created_at)
 VALUES(?,'provider-rec','device',1)`, tombstoneID)
}

// fileHash returns the sha256 of a file.
func fileHash(t *testing.T, path string) string {
	t.Helper()
	hash, err := sha256File(path)
	if err != nil {
		t.Fatalf("hash %s: %v", path, err)
	}
	return hash
}

func TestBackupRestoreIntegrityAndAudio(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "profiles")
	remoteRoot := filepath.Join(t.TempDir(), "remote")
	if err := os.MkdirAll(remoteRoot, 0700); err != nil {
		t.Fatal(err)
	}
	seedProfile(t, root, "home", true)
	archive := filepath.Join(remoteRoot, "backup.tgz")

	manifest, err := Create(ctx, CreateOptions{ProfilesRoot: root, Out: archive, ConfirmWritersStopped: true})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if manifest.Schema != ManifestSchema {
		t.Fatalf("manifest schema = %q", manifest.Schema)
	}
	if len(manifest.Files) == 0 || len(manifest.Profiles) != 1 || manifest.Profiles[0].ID != "home" {
		t.Fatalf("manifest = %+v", manifest)
	}
	if manifest.Profiles[0].Audio.Available != 1 {
		t.Fatalf("audio status = %+v", manifest.Profiles[0].Audio)
	}
	if manifest.BarrierDigest == "" {
		t.Fatal("manifest has no barrier digest")
	}
	// The archive itself records the exact bytes that were copied.
	for _, entry := range manifest.Files {
		if entry.Size <= 0 || entry.SHA256 == "" {
			t.Fatalf("file entry = %+v", entry)
		}
	}

	dest := filepath.Join(t.TempDir(), "restored")
	restored, err := Restore(ctx, RestoreOptions{
		Archive:               archive,
		Dest:                  dest,
		BarrierSource:         root,
		ConfirmWritersStopped: true,
	})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if restored.BarrierDigest != manifest.BarrierDigest {
		t.Fatalf("restored barrier digest = %q, want %q", restored.BarrierDigest, manifest.BarrierDigest)
	}

	// Data survives: open the restored database and read the turn.
	store, err := memory.Open(filepath.Join(dest, "home", "db", "context-memory.db"))
	if err != nil {
		t.Fatalf("open restored db: %v", err)
	}
	var content string
	if err := store.DB().QueryRow(`SELECT content FROM turns WHERE id='t:1'`).Scan(&content); err != nil {
		store.Close()
		t.Fatalf("read restored turn: %v", err)
	}
	store.Close()
	if content != "restored turn text" {
		t.Fatalf("restored content = %q", content)
	}
	// The managed audio file is restored byte-identically.
	restoredPCM := filepath.Join(dest, "home", "audio", "rec-a.pcm")
	if _, err := os.Stat(restoredPCM); err != nil {
		t.Fatalf("restored audio missing: %v", err)
	}
	if fileHash(t, restoredPCM) != fileHash(t, filepath.Join(root, "home", "audio", "rec-a.pcm")) {
		t.Fatal("restored audio differs from the source")
	}

	// Verify accepts the restored root.
	report, err := Verify(ctx, VerifyOptions{ProfilesRoot: dest})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(report.Profiles) != 1 || report.Profiles[0].Schema == 0 {
		t.Fatalf("verify report = %+v", report)
	}
}

func TestBackupRejectsPreForgetArchive(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "profiles")
	seedProfile(t, root, "home", false)
	archive := filepath.Join(t.TempDir(), "pre-forget.tgz")
	if _, err := Create(ctx, CreateOptions{ProfilesRoot: root, Out: archive, ConfirmWritersStopped: true}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// A permanent forget lands after the backup.
	completedForget(t, root, "home", "forget:later")
	_, err := Restore(ctx, RestoreOptions{
		Archive:               archive,
		Dest:                  filepath.Join(t.TempDir(), "restored"),
		BarrierSource:         root,
		ConfirmWritersStopped: true,
	})
	if code := ErrorCode(err); code != CodeBackupPredatesForget {
		t.Fatalf("Restore code = %q (err=%v), want %q", code, err, CodeBackupPredatesForget)
	}
}

func TestBackupRejectsUnfinishedForget(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "profiles")
	seedProfile(t, root, "home", false)
	db, err := openSQL(filepath.Join(root, "home", "db", "context-memory.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	execSQL(t, db, `INSERT INTO forget_tombstones(id,scope,requested_id,locator_key,expected_state_hash,redaction_done,status,created_at)
 VALUES('forget:pending','turn','t:1','locator:pending','hash',0,'pending_external_cleanup',1)`)
	db.Close()

	_, err = Create(ctx, CreateOptions{ProfilesRoot: root, Out: filepath.Join(t.TempDir(), "x.tgz"), ConfirmWritersStopped: true})
	if code := ErrorCode(err); code != CodeForgetUnfinished {
		t.Fatalf("Create code = %q (err=%v), want %q", code, err, CodeForgetUnfinished)
	}
}

func TestBackupRequiresStoppedWriterAssertion(t *testing.T) {
	root := filepath.Join(t.TempDir(), "profiles")
	seedProfile(t, root, "home", false)
	_, err := Create(context.Background(), CreateOptions{ProfilesRoot: root, Out: filepath.Join(t.TempDir(), "x.tgz")})
	if code := ErrorCode(err); code != CodeWritersActive {
		t.Fatalf("Create code = %q, want %q", code, CodeWritersActive)
	}
}

func TestBackupRefusesSymlinkInRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "profiles")
	seedProfile(t, root, "home", false)
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "home", "link.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	_, err := Create(context.Background(), CreateOptions{ProfilesRoot: root, Out: filepath.Join(t.TempDir(), "x.tgz"), ConfirmWritersStopped: true})
	if code := ErrorCode(err); code != CodeInvalidArchive {
		t.Fatalf("Create code = %q (err=%v), want %q", code, err, CodeInvalidArchive)
	}
}

func TestBackupMissingAudioFails(t *testing.T) {
	root := filepath.Join(t.TempDir(), "profiles")
	seedProfile(t, root, "home", false)
	if err := os.Remove(filepath.Join(root, "home", "audio", "rec-a.pcm")); err != nil {
		t.Fatal(err)
	}
	_, err := Create(context.Background(), CreateOptions{ProfilesRoot: root, Out: filepath.Join(t.TempDir(), "x.tgz"), ConfirmWritersStopped: true})
	if code := ErrorCode(err); code != CodeAudioMissing {
		t.Fatalf("Create code = %q (err=%v), want %q", code, err, CodeAudioMissing)
	}
}

func TestRestoreRejectsPathTraversal(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "evil.tgz")
	if err := writeRawArchive(archive, []rawEntry{
		{name: "../escape.txt", body: []byte("x")},
	}); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "profiles")
	seedProfile(t, root, "home", false)
	_, err := Restore(context.Background(), RestoreOptions{
		Archive:               archive,
		Dest:                  filepath.Join(t.TempDir(), "dest"),
		BarrierSource:         root,
		ConfirmWritersStopped: true,
	})
	if code := ErrorCode(err); code != CodeInvalidArchive {
		t.Fatalf("Restore code = %q (err=%v), want %q", code, err, CodeInvalidArchive)
	}
}

func TestRestoreRejectsNonEmptyDestination(t *testing.T) {
	root := filepath.Join(t.TempDir(), "profiles")
	seedProfile(t, root, "home", false)
	archive := filepath.Join(t.TempDir(), "ok.tgz")
	if _, err := Create(context.Background(), CreateOptions{ProfilesRoot: root, Out: archive, ConfirmWritersStopped: true}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "dest")
	if err := os.MkdirAll(dest, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, "keep.txt"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := Restore(context.Background(), RestoreOptions{Archive: archive, Dest: dest, BarrierSource: root, ConfirmWritersStopped: true})
	if code := ErrorCode(err); code != CodeDestinationNotEmpty {
		t.Fatalf("Restore code = %q (err=%v), want %q", code, err, CodeDestinationNotEmpty)
	}
}

// TestWriteTarFileDetectsWriterChange pins the post-copy guard: when the bytes
// no longer match the pre-copy snapshot, the tar writer must refuse.
func TestWriteTarFileDetectsWriterChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(path, []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	stale := snapshotFile{Path: "file.txt", Size: 6, SHA256: "0000"}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	err := writeTarFile(tw, dir, stale)
	if code := ErrorCode(err); code != CodeWritersActive {
		t.Fatalf("writeTarFile code = %q (err=%v), want %q", code, err, CodeWritersActive)
	}
}

// rawEntry is one hand-built tar entry for traversal tests.
type rawEntry struct {
	name string
	body []byte
}

func writeRawArchive(path string, entries []rawEntry) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	gz := gzip.NewWriter(file)
	tw := tar.NewWriter(gz)
	for _, entry := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: entry.name, Mode: 0600, Size: int64(len(entry.body)), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		if _, err := tw.Write(entry.body); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// TestArchiveManifestJSON is a format guard: the manifest must round-trip the
// documented schema and stable field names.
func TestArchiveManifestJSON(t *testing.T) {
	encoded, err := json.Marshal(Manifest{Schema: ManifestSchema, CreatedAt: "now", Profiles: []ProfileEntry{{ID: "home", Schema: 22}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"schema"`, `"created_at"`, `"profiles"`, `"files"`, `"barrier_digest"`} {
		if !bytes.Contains(encoded, []byte(field)) {
			t.Fatalf("manifest JSON lacks %s: %s", field, encoded)
		}
	}
}

// TestRestoreDetectsCorruptDatabase rebuilds an archive whose database bytes
// were damaged after the manifest hash was recomputed, so the failure must come
// from the SQLite integrity check rather than the hash guard.
func TestRestoreDetectsCorruptDatabase(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "profiles")
	seedProfile(t, root, "home", false)
	archive := filepath.Join(t.TempDir(), "ok.tgz")
	if _, err := Create(ctx, CreateOptions{ProfilesRoot: root, Out: archive, ConfirmWritersStopped: true}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	work := t.TempDir()
	manifest, err := extractArchive(archive, work)
	if err != nil {
		t.Fatalf("extractArchive: %v", err)
	}
	dbPath := filepath.Join(work, "home", "db", "context-memory.db")
	data, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 128 && i < len(data); i++ {
		data[i] = 0xff
	}
	if err := os.WriteFile(dbPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	files, err := snapshot(work)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Files = fileEntries(files)
	corrupt := filepath.Join(t.TempDir(), "corrupt.tgz")
	if err := writeArchive(corrupt, work, files, manifest); err != nil {
		t.Fatalf("writeArchive: %v", err)
	}

	_, err = Restore(ctx, RestoreOptions{
		Archive:               corrupt,
		Dest:                  filepath.Join(t.TempDir(), "restored"),
		BarrierSource:         root,
		ConfirmWritersStopped: true,
	})
	if code := ErrorCode(err); code != CodeIntegrityFailed {
		t.Fatalf("Restore code = %q (err=%v), want %q", code, err, CodeIntegrityFailed)
	}
}

// TestRestoreRejectsUnfinishedSourceForget covers the trusted-root side of the
// barrier: an unfinished forget makes the source unusable, even when the
// archive itself is clean.
func TestRestoreRejectsUnfinishedSourceForget(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "profiles")
	seedProfile(t, root, "home", false)
	archive := filepath.Join(t.TempDir(), "ok.tgz")
	if _, err := Create(ctx, CreateOptions{ProfilesRoot: root, Out: archive, ConfirmWritersStopped: true}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	db, err := openSQL(filepath.Join(root, "home", "db", "context-memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	execSQL(t, db, `INSERT INTO forget_tombstones(id,scope,requested_id,locator_key,expected_state_hash,redaction_done,status,created_at)
 VALUES('forget:pending','turn','t:1','locator:pending','hash',1,'pending_external_cleanup',1)`)
	db.Close()

	_, err = Restore(ctx, RestoreOptions{
		Archive:               archive,
		Dest:                  filepath.Join(t.TempDir(), "restored"),
		BarrierSource:         root,
		ConfirmWritersStopped: true,
	})
	if code := ErrorCode(err); code != CodeForgetBarrierUnverifiable {
		t.Fatalf("Restore code = %q (err=%v), want %q", code, err, CodeForgetBarrierUnverifiable)
	}
}
