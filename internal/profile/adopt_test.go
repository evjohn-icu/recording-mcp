package profile

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAdoptLegacySnapshotsReadOnlyAndRewritesLocalAudioPaths(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	legacyDB := filepath.Join(tmp, "legacy.db")
	audio := filepath.Join(tmp, "old-audio")
	eventsDB := filepath.Join(tmp, "legacy-events.db")
	if err := os.Mkdir(audio, 0700); err != nil {
		t.Fatal(err)
	}
	pcm := filepath.Join(audio, "stitched.pcm")
	if err := os.WriteFile(pcm, []byte{1, 2, 3, 4}, 0600); err != nil {
		t.Fatal(err)
	}
	source, err := sql.Open("sqlite", legacyDB)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`CREATE TABLE conversations(id TEXT PRIMARY KEY,title TEXT)`, `INSERT INTO conversations VALUES('c1','legacy')`, `CREATE TABLE sync_recordings(recording_id TEXT PRIMARY KEY,stitched_path TEXT)`, `INSERT INTO sync_recordings VALUES('r1',?)`, `CREATE TABLE sync_chunks(recording_id TEXT,seq INTEGER,file_path TEXT,PRIMARY KEY(recording_id,seq))`, `INSERT INTO sync_chunks VALUES('r1',0,?)`} {
		var args []any
		if strings.Contains(q, "INSERT INTO sync_recordings") {
			args = []any{pcm}
		} else if strings.Contains(q, "INSERT INTO sync_chunks") {
			args = []any{pcm}
		}
		if _, err = source.ExecContext(ctx, q, args...); err != nil {
			source.Close()
			t.Fatal(err)
		}
	}
	source.Close()
	events, err := sql.Open("sqlite", eventsDB)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = events.Exec(`CREATE TABLE events(id TEXT,payload TEXT); INSERT INTO events VALUES('e1','event')`); err != nil {
		t.Fatal(err)
	}
	events.Close()
	manager, err := Open(filepath.Join(tmp, "profiles"))
	if err != nil {
		t.Fatal(err)
	}
	paths, err := manager.AdoptLegacy(ctx, "adopted", "adopted profile password", legacyDB, audio, eventsDB)
	if err != nil {
		t.Fatal(err)
	}
	copied, err := os.ReadFile(filepath.Join(paths.Audio, "stitched.pcm"))
	if err != nil || string(copied) != string([]byte{1, 2, 3, 4}) {
		t.Fatalf("audio copy=%v err=%v", copied, err)
	}
	target, err := sql.Open("sqlite", paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	var title, stitched, chunk string
	if err = target.QueryRow(`SELECT title FROM conversations WHERE id='c1'`).Scan(&title); err != nil || title != "legacy" {
		t.Fatalf("snapshot title=%q err=%v", title, err)
	}
	if err = target.QueryRow(`SELECT stitched_path FROM sync_recordings WHERE recording_id='r1'`).Scan(&stitched); err != nil {
		t.Fatal(err)
	}
	if stitched != filepath.Join(paths.Audio, "stitched.pcm") {
		t.Fatalf("stitched path=%q", stitched)
	}
	if err = target.QueryRow(`SELECT file_path FROM sync_chunks WHERE recording_id='r1' AND seq=0`).Scan(&chunk); err != nil {
		t.Fatal(err)
	}
	if chunk != stitched {
		t.Fatalf("chunk path=%q", chunk)
	}
	var controls int
	if err = target.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='profile_control'`).Scan(&controls); err != nil {
		t.Fatal(err)
	}
	if controls != 0 {
		t.Fatal("source snapshot was unexpectedly migrated")
	}
	original, err := sql.Open("sqlite", legacyDB)
	if err != nil {
		t.Fatal(err)
	}
	defer original.Close()
	if err = original.QueryRow(`SELECT stitched_path FROM sync_recordings WHERE recording_id='r1'`).Scan(&stitched); err != nil || stitched != pcm {
		t.Fatalf("original database was modified: %q %v", stitched, err)
	}
	plane, err := sql.Open("sqlite", paths.Plane)
	if err != nil {
		t.Fatal(err)
	}
	defer plane.Close()
	var payload string
	if err = plane.QueryRow(`SELECT payload FROM events WHERE id='e1'`).Scan(&payload); err != nil || payload != "event" {
		t.Fatalf("event snapshot=%q err=%v", payload, err)
	}
	if _, err = os.Stat(filepath.Join(paths.Root, "profile.json")); err != nil {
		t.Fatal(err)
	}
	if _, err = manager.AdoptLegacy(ctx, "adopted", "another profile password", legacyDB, audio, eventsDB); err == nil {
		t.Fatal("existing target profile was merged")
	}
}

func TestAdoptLegacyRejectsAudioSymlinkWithoutFollowing(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "legacy.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE only_data(id TEXT)`)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(tmp, "outside")
	if err = os.Mkdir(outside, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(outside, "secret.wav"), []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	audio := filepath.Join(tmp, "audio")
	if err = os.Mkdir(audio, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(outside, filepath.Join(audio, "link")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	manager, err := Open(filepath.Join(tmp, "profiles"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = manager.AdoptLegacy(context.Background(), "bad", "adoption profile password", dbPath, audio, ""); err == nil {
		t.Fatal("audio symlink was traversed")
	}
	if _, err = os.Stat(filepath.Join(manager.Root, "bad")); !os.IsNotExist(err) {
		t.Fatalf("failed fresh adoption was not rolled back: %v", err)
	}
	if _, err = os.Stat(filepath.Join(outside, "secret.wav")); err != nil {
		t.Fatal("source audio unexpectedly removed")
	}
}
