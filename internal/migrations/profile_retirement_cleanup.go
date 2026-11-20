package migrations

import (
	"context"
	"database/sql"
)

// profileRetirementCleanupDDL adds the durable cleanup proof to the retirement
// fence. A retired profile must not be archived until its external cleanup
// (event store, main database WAL/VACUUM, managed audio/tasks/index/backup
// files) finished; the proof column records that completion so the backup gate
// can trust it. Old databases without the column cannot prove cleanup and
// stay gated until the password-authorized cleanup retry records it.
const profileRetirementCleanupDDL = `ALTER TABLE profile_control ADD COLUMN cleanup_done INTEGER NOT NULL DEFAULT 0 CHECK(cleanup_done IN (0,1))`

// applyProfileRetirementCleanup is version 24. It reuses addColumnIfMissing so
// replaying the migration on a database that already carries the column stays
// a no-op (SQLite has no ALTER TABLE ADD COLUMN IF NOT EXISTS).
func applyProfileRetirementCleanup(ctx context.Context, tx *sql.Tx) error {
	return addColumnIfMissing(ctx, tx, "profile_control", "cleanup_done", profileRetirementCleanupDDL)
}
