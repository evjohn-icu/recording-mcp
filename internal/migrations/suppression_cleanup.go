package migrations

var suppressionCleanupStatements = []string{
	`CREATE TABLE IF NOT EXISTS suppression_cleanup_operations (operation_id TEXT PRIMARY KEY, conversation_id TEXT NOT NULL, note_id TEXT NOT NULL, status TEXT NOT NULL CHECK(status IN ('pending','complete')), created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, UNIQUE(conversation_id,note_id));`,
	`CREATE TRIGGER IF NOT EXISTS profile_retired_suppression_cleanup_operations_INSERT BEFORE INSERT ON suppression_cleanup_operations WHEN EXISTS(SELECT 1 FROM profile_control WHERE id=1 AND retired=1) BEGIN SELECT RAISE(ABORT,'profile is permanently retired'); END;`,
	`CREATE TRIGGER IF NOT EXISTS profile_retired_suppression_cleanup_operations_UPDATE BEFORE UPDATE ON suppression_cleanup_operations WHEN EXISTS(SELECT 1 FROM profile_control WHERE id=1 AND retired=1) BEGIN SELECT RAISE(ABORT,'profile is permanently retired'); END;`,
}
