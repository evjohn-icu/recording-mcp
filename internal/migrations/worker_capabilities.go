package migrations

import (
	"context"
	"database/sql"
	"fmt"
)

// applyWorkerCapabilities adds the heartbeat capability columns. ALTER TABLE
// has no IF NOT EXISTS, so a database that already has them (for example a
// partially rolled-back test fixture) must be detected instead of failing.
func applyWorkerCapabilities(ctx context.Context, tx *sql.Tx) error {
	for _, column := range []struct{ name, ddl string }{
		{"asr_provider", `ALTER TABLE worker_heartbeats ADD COLUMN asr_provider TEXT NOT NULL DEFAULT ''`},
		{"asr_available", `ALTER TABLE worker_heartbeats ADD COLUMN asr_available INTEGER NOT NULL DEFAULT 0`},
		{"embed_provider", `ALTER TABLE worker_heartbeats ADD COLUMN embed_provider TEXT NOT NULL DEFAULT ''`},
	} {
		if err := addColumnIfMissing(ctx, tx, "worker_heartbeats", column.name, column.ddl); err != nil {
			return err
		}
	}
	return nil
}

func addColumnIfMissing(ctx context.Context, tx *sql.Tx, table, column, ddl string) error {
	rows, err := tx.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var cid int
		var name, kind string
		var notNull, pk int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &pk); err != nil {
			return err
		}
		if name == column {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if found {
		return nil
	}
	if _, err := tx.ExecContext(ctx, ddl); err != nil {
		return fmt.Errorf("add column %s.%s: %w", table, column, err)
	}
	return nil
}
