package pipeline

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"recordingmcp/internal/decision"
)

// ErrRunNotFound reports that no decision run with the requested id exists.
var ErrRunNotFound = errors.New("pipeline: decision run not found")

// RunRecord is one recorded decision-engine evaluation together with the
// answers it produced. JobID may be empty when the evaluation was not tied to
// a processing job; every other field mirrors the decision_runs row.
type RunRecord struct {
	JobID           string
	InputSnapshotID string
	InputHash       string
	Model           string
	WeightsVersion  string
	Endpoint        string
	QuestionsJSON   string
	UsageJSON       string
	Attempt         int
	Answers         map[string]decision.Answer
}

// decisionRunID derives the deterministic id of a decision run from its
// identity triple. Identical (job, attempt, input hash) always yields the same
// id, which is what makes RecordDecisionRun idempotent.
func decisionRunID(jobID string, attempt int, inputHash string) string {
	sum := sha256.Sum256([]byte(jobID + "|" + strconv.Itoa(attempt) + "|" + inputHash))
	return "run:" + hex.EncodeToString(sum[:])[:32]
}

// RecordDecisionRun persists one evaluation and its answers, returning the
// deterministic run id. The run row is inserted with INSERT OR IGNORE so a
// repeat of the same (job_id, attempt, input_hash) keeps the first record; the
// answers are upserted so the latest payload wins. Run and answers are written
// in a single transaction.
func RecordDecisionRun(ctx context.Context, db *sql.DB, rec RunRecord, now time.Time) (string, error) {
	runID := decisionRunID(rec.JobID, rec.Attempt, rec.InputHash)

	usage := rec.UsageJSON
	if usage == "" {
		usage = "{}"
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("pipeline: record decision run: begin: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO decision_runs(id, job_id, attempt, input_snapshot_id, input_hash, model, weights_version, endpoint, questions_json, usage_json, created_at)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		runID, nullableString(rec.JobID), rec.Attempt, rec.InputSnapshotID, rec.InputHash,
		rec.Model, rec.WeightsVersion, rec.Endpoint, rec.QuestionsJSON, usage, now.UnixMilli(),
	); err != nil {
		return "", fmt.Errorf("pipeline: record decision run %s: insert run: %w", runID, err)
	}

	for questionID, answer := range rec.Answers {
		answerJSON, err := json.Marshal(answer)
		if err != nil {
			return "", fmt.Errorf("pipeline: record decision run %s: marshal answer %q: %w", runID, questionID, err)
		}
		distributionJSON, err := json.Marshal(distributionOrEmpty(answer.Distribution))
		if err != nil {
			return "", fmt.Errorf("pipeline: record decision run %s: marshal distribution %q: %w", runID, questionID, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT OR REPLACE INTO decision_answers(run_id, question_id, question_type, answer_json, distribution_json)
			 VALUES(?, ?, ?, ?, ?)`,
			runID, questionID, answer.Type, string(answerJSON), string(distributionJSON),
		); err != nil {
			return "", fmt.Errorf("pipeline: record decision run %s: upsert answer %q: %w", runID, questionID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("pipeline: record decision run %s: commit: %w", runID, err)
	}
	return runID, nil
}

// GetDecisionRun loads one recorded run and its answers by id. A missing run
// yields ErrRunNotFound; a run with no answers yields an empty (non-nil) map.
func GetDecisionRun(ctx context.Context, db *sql.DB, runID string) (RunRecord, error) {
	var (
		rec   RunRecord
		jobID sql.NullString
		usage sql.NullString
	)
	err := db.QueryRowContext(ctx,
		`SELECT job_id, attempt, input_snapshot_id, input_hash, model, weights_version, endpoint, questions_json, usage_json
		 FROM decision_runs WHERE id = ?`, runID,
	).Scan(&jobID, &rec.Attempt, &rec.InputSnapshotID, &rec.InputHash,
		&rec.Model, &rec.WeightsVersion, &rec.Endpoint, &rec.QuestionsJSON, &usage)
	if errors.Is(err, sql.ErrNoRows) {
		return RunRecord{}, ErrRunNotFound
	}
	if err != nil {
		return RunRecord{}, fmt.Errorf("pipeline: get decision run %s: %w", runID, err)
	}
	rec.JobID = jobID.String
	rec.UsageJSON = usage.String

	rows, err := db.QueryContext(ctx,
		`SELECT question_id, answer_json FROM decision_answers WHERE run_id = ? ORDER BY question_id`, runID)
	if err != nil {
		return RunRecord{}, fmt.Errorf("pipeline: get decision run %s: query answers: %w", runID, err)
	}
	defer rows.Close()

	rec.Answers = make(map[string]decision.Answer)
	for rows.Next() {
		var questionID, answerJSON string
		if err := rows.Scan(&questionID, &answerJSON); err != nil {
			return RunRecord{}, fmt.Errorf("pipeline: get decision run %s: scan answer: %w", runID, err)
		}
		var answer decision.Answer
		if err := json.Unmarshal([]byte(answerJSON), &answer); err != nil {
			return RunRecord{}, fmt.Errorf("pipeline: get decision run %s: unmarshal answer %q: %w", runID, questionID, err)
		}
		rec.Answers[questionID] = answer
	}
	if err := rows.Err(); err != nil {
		return RunRecord{}, fmt.Errorf("pipeline: get decision run %s: read answers: %w", runID, err)
	}
	return rec, nil
}

// nullableString maps the empty string to SQL NULL so an absent job id is not
// confused with a job literally named "".
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// distributionOrEmpty normalizes a nil distribution to an empty object so the
// stored distribution_json is always valid JSON of the expected shape.
func distributionOrEmpty(d map[string]float64) map[string]float64 {
	if d == nil {
		return map[string]float64{}
	}
	return d
}
