package migrations

import (
	"context"
	"database/sql"
)

// applyASRProgress adds the durable ASR window checkpoint projection and the
// GPU diagnostics the worker reports. source_attachments remains the
// checkpoint; these columns are the readable projection the UI polls.
func applyASRProgress(ctx context.Context, tx *sql.Tx) error {
	columns := []struct{ table, name, ddl string }{
		{"sync_recordings", "completed_chunks", `ALTER TABLE sync_recordings ADD COLUMN completed_chunks INTEGER NOT NULL DEFAULT 0`},
		{"sync_recordings", "failed_chunks", `ALTER TABLE sync_recordings ADD COLUMN failed_chunks INTEGER NOT NULL DEFAULT 0`},
		{"sync_recordings", "coverage_ms", `ALTER TABLE sync_recordings ADD COLUMN coverage_ms INTEGER NOT NULL DEFAULT 0`},
		{"worker_heartbeats", "gpu_name", `ALTER TABLE worker_heartbeats ADD COLUMN gpu_name TEXT NOT NULL DEFAULT ''`},
		{"worker_heartbeats", "vram_total_mb", `ALTER TABLE worker_heartbeats ADD COLUMN vram_total_mb INTEGER NOT NULL DEFAULT 0`},
		{"worker_heartbeats", "vram_free_mb", `ALTER TABLE worker_heartbeats ADD COLUMN vram_free_mb INTEGER NOT NULL DEFAULT 0`},
		{"worker_heartbeats", "asr_device", `ALTER TABLE worker_heartbeats ADD COLUMN asr_device TEXT NOT NULL DEFAULT ''`},
		{"worker_heartbeats", "asr_dtype", `ALTER TABLE worker_heartbeats ADD COLUMN asr_dtype TEXT NOT NULL DEFAULT ''`},
		{"worker_heartbeats", "asr_error", `ALTER TABLE worker_heartbeats ADD COLUMN asr_error TEXT NOT NULL DEFAULT ''`},
	}
	for _, column := range columns {
		if err := addColumnIfMissing(ctx, tx, column.table, column.name, column.ddl); err != nil {
			return err
		}
	}
	return nil
}
