package migrations

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
)

// TestApplyConcurrentStartup mirrors web and worker opening a fresh profile at
// the same time. Both must succeed and record each migration exactly once.
func TestApplyConcurrentStartup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "concurrent.db")
	dsn := path + "?_txlock=immediate&_pragma=busy_timeout(5000)"
	open := func() *sql.DB {
		db, err := sql.Open("sqlite", dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		return db
	}
	first, second := open(), open()
	ctx := context.Background()
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, db := range []*sql.DB{first, second} {
		wg.Add(1)
		go func(db *sql.DB) {
			defer wg.Done()
			<-start
			errs <- Apply(ctx, db)
		}(db)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Apply: %v", err)
		}
	}
	latest := migrations[len(migrations)-1].version
	if got := mustVersion(t, ctx, first); got != latest {
		t.Fatalf("Version() = %d, want %d", got, latest)
	}
	var rows int
	if err := first.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != len(migrations) {
		t.Fatalf("schema_migrations rows = %d, want %d", rows, len(migrations))
	}
}
