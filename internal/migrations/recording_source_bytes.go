package migrations

import (
	"context"
	"database/sql"
)

// applyRecordingSourceBytes records the uploaded file size. It tolerates a
// database that already carries the column after a partial rollback.
func applyRecordingSourceBytes(ctx context.Context, tx *sql.Tx) error {
	return addColumnIfMissing(ctx, tx, "recordings", "source_bytes",
		`ALTER TABLE recordings ADD COLUMN source_bytes INTEGER NOT NULL DEFAULT 0`)
}
