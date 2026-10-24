package migrations

// HighlightsStatements adds durable user authored bookmarks and a content-free
// request ledger. Deleted requests retain only their rejection marker, never
// the payload hash or note text.
var HighlightsStatements = append([]string{
	`CREATE TABLE IF NOT EXISTS memory_highlights (
		id TEXT PRIMARY KEY,
		conversation_id TEXT NOT NULL REFERENCES conversations(id),
		recording_id TEXT NOT NULL REFERENCES recordings(id),
		anchor_kind TEXT NOT NULL CHECK(anchor_kind IN ('point','range','turn')),
		start_ms INTEGER NULL,
		end_ms INTEGER NULL,
		turn_id TEXT NULL,
		source_attachment_id TEXT NULL,
		transcript_revision TEXT NULL,
		tag TEXT NOT NULL CHECK(tag IN ('important','follow_up','question')),
		note_text TEXT NOT NULL DEFAULT '',
		version INTEGER NOT NULL DEFAULT 1,
		created_at_ms INTEGER NOT NULL,
		updated_at_ms INTEGER NOT NULL
	);`,
	`CREATE INDEX IF NOT EXISTS memory_highlights_recording ON memory_highlights(recording_id,created_at_ms,id);`,
	`CREATE TABLE IF NOT EXISTS memory_highlight_requests (
		request_id TEXT PRIMARY KEY,
		conversation_id TEXT NOT NULL,
		recording_id TEXT NOT NULL,
		anchor_kind TEXT NOT NULL,
		start_ms INTEGER NULL,
		end_ms INTEGER NULL,
		turn_id TEXT NULL,
		payload_sha256 TEXT NOT NULL,
		highlight_id TEXT NOT NULL,
		status TEXT NOT NULL CHECK(status IN ('active','deleted','forgotten')),
		created_at_ms INTEGER NOT NULL
	);`,
	`CREATE TRIGGER IF NOT EXISTS forget_block_highlight_insert BEFORE INSERT ON memory_highlights WHEN EXISTS(SELECT 1 FROM forget_tombstones f WHERE (f.recording_id=NEW.recording_id OR (f.scope='conversation' AND f.conversation_id=NEW.conversation_id)) AND (f.scope IN ('recording','conversation') OR (f.scope='turn' AND (f.start_ms IS NULL OR f.end_ms IS NULL OR NEW.start_ms IS NULL OR (NEW.anchor_kind='point' AND NEW.start_ms>=f.start_ms AND NEW.start_ms<f.end_ms) OR (NEW.anchor_kind!='point' AND (NEW.end_ms IS NULL OR (NEW.start_ms<f.end_ms AND NEW.end_ms>f.start_ms))))))) BEGIN SELECT RAISE(ABORT,'source is permanently forgotten'); END;`,
}, retirementFenceStatements("memory_highlights", "memory_highlight_requests")...)
