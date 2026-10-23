package migrations

// ProcessingRuntimeDDL stores worker liveness and idempotency keys for
// explicit retry requests. It contains operational metadata only.
const ProcessingRuntimeDDL = `CREATE TABLE IF NOT EXISTS worker_heartbeats(worker_id TEXT PRIMARY KEY, pid INTEGER NOT NULL, stages_json TEXT NOT NULL, generator_enabled INTEGER NOT NULL, last_seen_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS processing_retry_requests(request_id TEXT PRIMARY KEY, job_id TEXT NOT NULL REFERENCES processing_jobs(id), expected_attempt INTEGER NOT NULL, created_at INTEGER NOT NULL);`
