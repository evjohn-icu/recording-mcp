package migrations

import (
	"context"
	"database/sql"
)

// forgetAudioStagingDDL records, per tombstone, the sorted recordings whose
// analysis staging must be removed by the cleanup coordinator. The forget
// redaction transaction writes the list before the source rows are erased, so
// the later cleanup can target <audio>/.analysis-tmp/<sha256(recordingID)>
// directories without guessing paths or touching other recordings' staging.
// Old databases keep the empty default; tombstones created before analysis
// staging existed must never delete staging by inference.
const forgetAudioStagingDDL = `ALTER TABLE forget_tombstones ADD COLUMN affected_recordings_json TEXT NOT NULL DEFAULT '[]'`

// applyForgetAudioStaging is version 25. It reuses addColumnIfMissing so
// replaying the migration on a database that already carries the column stays
// a no-op (SQLite has no ALTER TABLE ADD COLUMN IF NOT EXISTS).
func applyForgetAudioStaging(ctx context.Context, tx *sql.Tx) error {
	return addColumnIfMissing(ctx, tx, "forget_tombstones", "affected_recordings_json", forgetAudioStagingDDL)
}
