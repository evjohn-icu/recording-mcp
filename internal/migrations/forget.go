package migrations

// Migration 14 is intentionally a list of complete statements because SQLite
// trigger bodies contain semicolons and cannot be split with statements().
var forgetStatements = []string{
	`CREATE TABLE IF NOT EXISTS forget_tombstones (
		id TEXT PRIMARY KEY,
		scope TEXT NOT NULL CHECK(scope IN ('turn','recording','conversation')),
		conversation_id TEXT NOT NULL DEFAULT '',
		recording_id TEXT NOT NULL DEFAULT '',
		source_segment_id TEXT NOT NULL DEFAULT '',
		segment_index INTEGER NOT NULL DEFAULT 0,
		requested_id TEXT NOT NULL,
		start_ms INTEGER NULL,
		end_ms INTEGER NULL,
		locator_key TEXT NOT NULL UNIQUE,
		expected_state_hash TEXT NOT NULL,
		redaction_done INTEGER NOT NULL DEFAULT 0 CHECK(redaction_done IN (0,1)),
		status TEXT NOT NULL CHECK(status IN ('pending_external_cleanup','complete')),
		created_at INTEGER NOT NULL
	);`,
	`CREATE INDEX IF NOT EXISTS forget_tombstones_recording ON forget_tombstones(recording_id,source_segment_id,segment_index);`,
	`CREATE TABLE IF NOT EXISTS forget_files (
		tombstone_id TEXT NOT NULL REFERENCES forget_tombstones(id),
		path TEXT NOT NULL,
		start_byte INTEGER NULL,
		end_byte INTEGER NULL,
		remove_file INTEGER NOT NULL CHECK(remove_file IN (0,1)),
		done INTEGER NOT NULL DEFAULT 0 CHECK(done IN (0,1)),
		PRIMARY KEY(tombstone_id,path)
	);`,
	`CREATE TABLE IF NOT EXISTS forget_provider_fences (
		tombstone_id TEXT NOT NULL REFERENCES forget_tombstones(id),
		provider_recording_id TEXT NOT NULL,
		device_id TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL,
		PRIMARY KEY(tombstone_id,provider_recording_id,device_id)
	);`,
	`CREATE INDEX IF NOT EXISTS forget_provider_fences_lookup ON forget_provider_fences(provider_recording_id,device_id);`,
	`CREATE TABLE IF NOT EXISTS ai_note_suppressions (
		recording_id TEXT NOT NULL,
		source_segment_id TEXT NOT NULL,
		kind TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		PRIMARY KEY(recording_id,source_segment_id,kind)
	);`,
	`CREATE TRIGGER IF NOT EXISTS forget_block_turn_insert BEFORE INSERT ON turns
		WHEN EXISTS (
			SELECT 1 FROM forget_tombstones f WHERE (f.recording_id=NEW.recording_id OR (f.scope='conversation' AND f.conversation_id=NEW.conversation_id)) AND (
				f.scope IN ('recording','conversation') OR
				(f.scope='turn' AND (NEW.id=f.requested_id OR f.start_ms IS NULL OR f.end_ms IS NULL OR NEW.global_start_ms IS NULL OR NEW.global_end_ms IS NULL OR
					(NEW.global_start_ms < f.end_ms AND NEW.global_end_ms > f.start_ms)))
			)
		)
		BEGIN SELECT RAISE(ABORT,'source is permanently forgotten'); END;`,
	`CREATE TRIGGER IF NOT EXISTS forget_block_attachment_insert BEFORE INSERT ON source_attachments
		WHEN EXISTS (SELECT 1 FROM forget_tombstones f WHERE (f.recording_id=NEW.recording_id OR (f.scope='conversation' AND f.conversation_id=NEW.conversation_id)) AND
			(f.scope IN ('recording','conversation') OR (f.scope='turn' AND f.source_segment_id=NEW.segment_id)))
		BEGIN SELECT RAISE(ABORT,'source is permanently forgotten'); END;`,
	`CREATE TRIGGER IF NOT EXISTS forget_block_suppressed_evidence BEFORE INSERT ON note_evidence
		WHEN EXISTS (
			SELECT 1 FROM ai_note_suppressions s
			JOIN turns t ON t.recording_id=s.recording_id
			JOIN attachment_turns at ON at.turn_id=t.id
			JOIN source_attachments a ON a.id=at.attachment_id AND a.segment_id=s.source_segment_id
			JOIN notes n ON n.id=NEW.note_id AND n.kind=s.kind
			WHERE t.id=NEW.turn_id
		)
		BEGIN SELECT RAISE(ABORT,'AI note kind is suppressed for this source'); END;`,
	`CREATE TRIGGER IF NOT EXISTS forget_block_suppressed_candidate_insert BEFORE INSERT ON memory_candidates
		WHEN EXISTS (
			SELECT 1 FROM json_each(NEW.evidence_turn_ids_json) e
			JOIN turns t ON t.id=e.value
			JOIN attachment_turns at ON at.turn_id=t.id
			JOIN source_attachments a ON a.id=at.attachment_id
			JOIN ai_note_suppressions s ON s.recording_id=t.recording_id AND s.source_segment_id=a.segment_id AND s.kind=NEW.kind
		)
		BEGIN SELECT RAISE(ABORT,'AI note kind is suppressed for this source'); END;`,
	`CREATE TRIGGER IF NOT EXISTS forget_block_candidate_insert BEFORE INSERT ON memory_candidates
		WHEN EXISTS (
			SELECT 1 FROM forget_tombstones f WHERE (
				(f.scope='conversation' AND f.conversation_id=NEW.conversation_id) OR
				(f.scope='recording' AND f.recording_id IN (SELECT id FROM recordings WHERE conversation_id=NEW.conversation_id)) OR
				(f.scope='turn' AND EXISTS (SELECT 1 FROM json_each(NEW.evidence_turn_ids_json) e WHERE e.value=f.requested_id))
			)
		)
		BEGIN SELECT RAISE(ABORT,'evidence is permanently forgotten'); END;`,
}
