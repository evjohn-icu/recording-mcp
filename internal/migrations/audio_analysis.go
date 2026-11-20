package migrations

// audioAnalysisStatements adds the versioned non-ASR audio-analysis results.
// ASR keeps writing source_attachments only; these tables store speech
// activity, acoustic tags, diarization segments and speaker embeddings with
// their own run identity and lifecycle.
//
// The identity index is unique so a repeated generation cannot create a
// second run, and the partial current index keeps at most one active run per
// (recording, capability). Speaker embeddings live in their own table and are
// never serialized into result_json.
var audioAnalysisStatements = []string{
	`CREATE TABLE IF NOT EXISTS audio_analysis_runs (
		id TEXT PRIMARY KEY,
		job_id TEXT NOT NULL UNIQUE REFERENCES processing_jobs(id),
		recording_id TEXT NOT NULL REFERENCES recordings(id),
		source_audio_sha256 TEXT NOT NULL,
		input_sha256 TEXT NOT NULL,
		transcript_revision TEXT NULL,
		source_attachment_ids_json TEXT NOT NULL DEFAULT '[]',
		source_snapshot_sha256 TEXT NOT NULL DEFAULT '',
		capability TEXT NOT NULL,
		start_ms INTEGER NOT NULL DEFAULT 0,
		end_ms INTEGER NOT NULL DEFAULT 0,
		model_id TEXT NOT NULL,
		model_revision TEXT NOT NULL,
		config_sha256 TEXT NOT NULL,
		generation TEXT NOT NULL,
		status TEXT NOT NULL CHECK(status IN ('running','succeeded','failed','invalidated')),
		active INTEGER NOT NULL DEFAULT 0 CHECK(active IN (0,1)),
		result_json TEXT NOT NULL DEFAULT '{}',
		error_code TEXT NULL,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	);`,
	`CREATE UNIQUE INDEX IF NOT EXISTS audio_analysis_runs_identity ON audio_analysis_runs(recording_id,source_audio_sha256,input_sha256,capability,model_id,model_revision,config_sha256,source_snapshot_sha256,generation);`,
	`CREATE UNIQUE INDEX IF NOT EXISTS audio_analysis_runs_current ON audio_analysis_runs(recording_id,capability) WHERE active=1;`,
	`CREATE INDEX IF NOT EXISTS audio_analysis_runs_recording ON audio_analysis_runs(recording_id,capability,status);`,
	`CREATE TABLE IF NOT EXISTS audio_analysis_events (
		id TEXT PRIMARY KEY,
		run_id TEXT NOT NULL REFERENCES audio_analysis_runs(id) ON DELETE CASCADE,
		event_type TEXT NOT NULL CHECK(event_type IN ('speech_activity','acoustic_event','speaker_segment','speaker_identity_proposal')),
		start_ms INTEGER NOT NULL,
		end_ms INTEGER NOT NULL,
		confidence REAL NULL CHECK(confidence IS NULL OR (confidence>=0 AND confidence<=1)),
		payload_json TEXT NOT NULL,
		CHECK(start_ms>=0 AND end_ms>start_ms)
	);`,
	`CREATE INDEX IF NOT EXISTS audio_analysis_events_run ON audio_analysis_events(run_id,start_ms,end_ms,event_type,id);`,
	`CREATE TABLE IF NOT EXISTS audio_speaker_embeddings (
		event_id TEXT PRIMARY KEY REFERENCES audio_analysis_events(id) ON DELETE CASCADE,
		dims INTEGER NOT NULL CHECK(dims>0),
		vector BLOB NOT NULL
	);`,
	`CREATE TABLE IF NOT EXISTS processing_job_dependencies (
		job_id TEXT NOT NULL REFERENCES processing_jobs(id) ON DELETE CASCADE,
		dependency_id TEXT NOT NULL REFERENCES processing_jobs(id) ON DELETE CASCADE,
		PRIMARY KEY(job_id,dependency_id),
		CHECK(job_id<>dependency_id)
	);`,
	`CREATE INDEX IF NOT EXISTS processing_job_dependencies_parent ON processing_job_dependencies(dependency_id);`,
	`CREATE TABLE IF NOT EXISTS audio_analysis_heads (
		recording_id TEXT NOT NULL REFERENCES recordings(id),
		capability TEXT NOT NULL,
		desired_job_id TEXT NOT NULL REFERENCES processing_jobs(id),
		current_run_id TEXT NULL REFERENCES audio_analysis_runs(id),
		PRIMARY KEY(recording_id,capability)
	);`,
}

// audioAnalysisMigration is version 23. The retirement fences are added here
// because the tables did not exist when migration 15 ran.
var audioAnalysisMigration = migration{
	version: 23,
	name:    "audio-analysis",
	statements: append(append([]string{}, audioAnalysisStatements...),
		retirementFenceStatements("audio_analysis_runs", "audio_analysis_events",
			"audio_speaker_embeddings", "audio_analysis_heads", "processing_job_dependencies")...),
}
