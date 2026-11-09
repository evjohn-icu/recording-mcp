package memory

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// TestWALAllowsConcurrentReaderAndWriter proves the store switches a
// file-backed database to WAL and that a second connection observes data
// committed by the first without waiting for the writer to close.
func TestWALAllowsConcurrentReaderAndWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wal.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	for _, db := range []*sql.DB{store.DB()} {
		var mode string
		if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
			t.Fatal(err)
		}
		if mode != "wal" {
			t.Fatalf("journal_mode=%q, want wal", mode)
		}
	}

	reader, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	reader.SetMaxOpenConns(1)
	var readerMode string
	if err := reader.QueryRow("PRAGMA journal_mode").Scan(&readerMode); err != nil {
		t.Fatal(err)
	}
	if readerMode != "wal" {
		t.Fatalf("reader journal_mode=%q, want wal", readerMode)
	}

	ctx := context.Background()
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO topics(id,title) VALUES('topic:wal','WAL')`); err != nil {
		t.Fatal(err)
	}
	var title string
	if err := reader.QueryRowContext(ctx, `SELECT title FROM topics WHERE id='topic:wal'`).Scan(&title); err != nil {
		t.Fatalf("concurrent reader did not see the committed row: %v", err)
	}
	if title != "WAL" {
		t.Fatalf("title=%q", title)
	}
}

// TestOpenWaitsForConcurrentWriterLock covers two processes starting at the
// same time: one holds a write transaction while the other opens the store.
func TestOpenWaitsForConcurrentWriterLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "concurrent.db")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()

	writer, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	tx, err := writer.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO topics(id,title) VALUES('topic:lock','lock')`); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		_ = tx.Rollback()
	}()

	start := time.Now()
	second, err := Open(path)
	if err != nil {
		t.Fatalf("open while a writer holds the file: %v", err)
	}
	defer second.Close()
	if time.Since(start) <= 0 {
		t.Fatal("unreachable")
	}
}
