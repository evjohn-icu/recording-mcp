package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"recordingmcp/internal/process"
)

const worldlineJobStage = "worldline"

type WorldlineAnalysisJob struct {
	JobID                      string `json:"job_id"`
	Status                     string `json:"status"`
	RequestID                  string `json:"request_id,omitempty"`
	Version                    string `json:"version,omitempty"`
	SemanticStatus             string `json:"semantic_status,omitempty"`
	SemanticModel              string `json:"semantic_model,omitempty"`
	SemanticWeightsVersion     string `json:"semantic_weights_version,omitempty"`
	SemanticCandidatePairCount int    `json:"semantic_candidate_pair_count,omitempty"`
	SemanticPairCount          int    `json:"semantic_pair_count,omitempty"`
	CompletedPairCount         int    `json:"completed_pair_count,omitempty"`
	RemainingPairCount         int    `json:"remaining_pair_count,omitempty"`
	FailedPairCount            int    `json:"failed_pair_count,omitempty"`
	SemanticSuggestionCount    int    `json:"semantic_suggestion_count,omitempty"`
	SemanticTruncated          bool   `json:"semantic_truncated,omitempty"`
	Error                      string `json:"error,omitempty"`
}

// EnqueueAnalysis writes a durable, idempotent analysis request to the shared
// processing queue. The payload contains source references and versions only.
func (s *WorldlineService) EnqueueAnalysis(ctx context.Context, scope, requestID string) (WorldlineAnalysisJob, error) {
	if s == nil || s.Time == nil || s.Time.Store == nil || strings.TrimSpace(scope) == "" || strings.TrimSpace(requestID) == "" {
		return WorldlineAnalysisJob{}, errors.New("worldline service, profile scope, and request_id required")
	}
	// Resolve the durable idempotency record before reading the live projection.
	// A retry after source edits must return the original frozen snapshot, not
	// silently replace it with a newer corpus.
	key := "worldline|" + scope + "|" + requestID
	var existingID, existingPayload, existingStatus string
	err := s.Time.Store.DB().QueryRowContext(ctx, `SELECT id,payload_json,status FROM processing_jobs WHERE idempotency_key=?`, key).Scan(&existingID, &existingPayload, &existingStatus)
	if err == nil {
		var frozen struct {
			Scope     string `json:"scope"`
			RequestID string `json:"request_id"`
		}
		if json.Unmarshal([]byte(existingPayload), &frozen) != nil || frozen.Scope != scope || frozen.RequestID != requestID {
			return WorldlineAnalysisJob{}, errors.New("stored worldline job payload is invalid")
		}
		return WorldlineAnalysisJob{JobID: existingID, Status: existingStatus, RequestID: requestID}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return WorldlineAnalysisJob{}, err
	}
	topics, _, version, err := s.project(ctx, scope)
	if err != nil {
		return WorldlineAnalysisJob{}, err
	}
	pairs, totalPairs := collectWorldlineSemanticPairs(topics, 10000)
	if pairs == nil {
		pairs = []semanticPair{}
	}
	if totalPairs > len(pairs) {
		return WorldlineAnalysisJob{}, errors.New("worldline snapshot exceeds 10000 semantic pairs")
	}
	refs := []EvidenceReference{}
	notes := map[string]string{}
	conversations := []string{}
	for _, topic := range topics {
		for _, lane := range topic.Lanes {
			for _, claim := range lane.Claims {
				refs = append(refs, evidenceRefs(claim.Evidence)...)
				for _, evidence := range claim.Evidence {
					conversations = append(conversations, evidence.ConversationID)
				}
				if id, ok := claim.Metadata["source_note_id"].(string); ok {
					v, _ := claim.Metadata["source_note_version"].(string)
					notes[id] = v
				}
			}
		}
	}
	payload, err := json.Marshal(map[string]any{"scope": scope, "request_id": requestID, "source_snapshot_version": version, "pairs": pairs, "references": uniqueEvidenceRefs(refs), "source_note_versions": notes, "source_conversation_ids": uniqueSorted(conversations)})
	if err != nil {
		return WorldlineAnalysisJob{}, err
	}
	id, _, err := process.Enqueue(ctx, s.Time.Store.DB(), process.Spec{Stage: worldlineJobStage, InputSnapshotID: version, IdempotencyKey: key, PayloadJSON: string(payload), MaxAttempts: 5}, time.Now().UTC())
	if err != nil {
		return WorldlineAnalysisJob{}, err
	}
	job, err := process.Get(ctx, s.Time.Store.DB(), id)
	if err != nil {
		return WorldlineAnalysisJob{}, err
	}
	return WorldlineAnalysisJob{JobID: id, Status: job.Status, RequestID: requestID}, nil
}

func evidenceRefs(evidence []WorldlineEvidence) []EvidenceReference {
	out := make([]EvidenceReference, 0, len(evidence))
	for _, e := range evidence {
		out = append(out, EvidenceReference{TurnID: e.TurnID, Revision: e.Revision})
	}
	return out
}

// RunAnalysisJob resumes the finite pair snapshot stored with the queue row.
func (s *WorldlineService) RunAnalysisJob(ctx context.Context, scope, requestID string) (map[string]any, error) {
	key := "worldline|" + scope + "|" + requestID
	var jobID, payload string
	if err := s.Time.Store.DB().QueryRowContext(ctx, `SELECT id,payload_json FROM processing_jobs WHERE idempotency_key=? AND stage=?`, key, worldlineJobStage).Scan(&jobID, &payload); err != nil {
		return nil, err
	}
	job, err := process.Get(ctx, s.Time.Store.DB(), jobID)
	if err != nil {
		return nil, err
	}
	var frozen struct {
		Scope     string         `json:"scope"`
		RequestID string         `json:"request_id"`
		Version   string         `json:"source_snapshot_version"`
		Pairs     []semanticPair `json:"pairs"`
	}
	if err := json.Unmarshal([]byte(payload), &frozen); err != nil || frozen.Scope != scope || frozen.RequestID != requestID || frozen.Version == "" || frozen.Pairs == nil {
		return nil, errors.New("worldline job has an invalid frozen pair snapshot")
	}
	if job.Status == "succeeded" {
		var result map[string]any
		var raw sql.NullString
		_ = s.Time.Store.DB().QueryRowContext(ctx, `SELECT result_ref FROM processing_attempts WHERE job_id=? ORDER BY attempt DESC LIMIT 1`, jobID).Scan(&raw)
		if raw.Valid && json.Unmarshal([]byte(raw.String), &result) == nil {
			return result, nil
		}
	}
	if job.Status != "running" || job.LeaseToken == "" {
		return nil, process.ErrLeaseLost
	}
	guard := func(c context.Context) error {
		return process.Renew(c, s.Time.Store.DB(), jobID, job.LeaseToken, 30*time.Second, time.Now())
	}
	summary, err := s.runDurableWorldlinePairsGuarded(ctx, scope, jobID, job.AttemptCount, frozen.Pairs, guard)
	if err != nil {
		return nil, err
	}
	return map[string]any{"job_id": jobID, "status": "complete", "version": frozen.Version, "semantic_status": summary.Status, "semantic_model": summary.Model, "semantic_weights_version": summary.WeightsVersion, "semantic_candidate_pair_count": summary.CandidatePairCount, "semantic_pair_count": summary.PairCount, "semantic_suggestion_count": summary.SuggestionCount, "semantic_truncated": false, "completed_pair_count": summary.CompletedPairCount, "remaining_pair_count": summary.RemainingPairCount, "failed_pair_count": summary.FailedPairCount}, nil
}

func (s *WorldlineService) AnalysisStatus(ctx context.Context, scope, jobID string) (WorldlineAnalysisJob, error) {
	if s == nil || s.Time == nil {
		return WorldlineAnalysisJob{}, errors.New("worldline service unavailable")
	}
	job, err := WorldlineJobStatus(ctx, s.Time.Store.DB(), scope, jobID)
	if err != nil {
		return job, err
	}
	rawJob, err := process.Get(ctx, s.Time.Store.DB(), jobID)
	if err != nil {
		return job, err
	}
	if job.Status == "succeeded" {
		job.Status = "complete"
	}
	var payload struct {
		Version string         `json:"source_snapshot_version"`
		Pairs   []semanticPair `json:"pairs"`
	}
	_ = json.Unmarshal([]byte(rawJob.PayloadJSON), &payload)
	job.Version = payload.Version
	job.SemanticCandidatePairCount = len(payload.Pairs)
	model, weights, known := "", "", false
	if identity, ok := s.DecisionEngine.(worldlineModelIdentity); ok {
		if m, w, e := identity.ModelIdentity(ctx); e == nil {
			model, weights, known = m, w, true
		}
	}
	job.SemanticModel, job.SemanticWeightsVersion = model, weights
	if job.Status == "queued" || job.Status == "running" || job.Status == "retry_wait" {
		job.SemanticStatus = "processing"
	}
	events, e := s.Time.History(ctx, scope)
	if e != nil {
		return job, e
	}
	completed := map[string]bool{}
	failed := map[string]bool{}
	for _, ev := range events {
		if ev.Type == "worldline.semantic_cache" && ev.SourceStatus == "current" {
			key, _ := ev.StructuredData["cache_key"].(string)
			if key != "" {
				completed[key] = true
			}
		}
		if ev.Type == "worldline.semantic_failure" && ev.SourceStatus == "current" {
			jid, _ := ev.StructuredData["job_id"].(string)
			pid, _ := ev.StructuredData["pair_id"].(string)
			if jid == job.JobID && pid != "" {
				failed[pid] = true
			}
		}
	}
	for _, pair := range payload.Pairs {
		if known && completed[worldlinePairCacheKey(pair, model, weights)] {
			job.CompletedPairCount++
		} else if failed[pair.ID] {
			job.FailedPairCount++
		}
	}
	job.SemanticPairCount = job.CompletedPairCount
	job.RemainingPairCount = max(0, len(payload.Pairs)-job.CompletedPairCount-job.FailedPairCount)
	job.SemanticSuggestionCount = job.CompletedPairCount
	if job.Status == "complete" {
		var raw sql.NullString
		_ = s.Time.Store.DB().QueryRowContext(ctx, `SELECT result_ref FROM processing_attempts WHERE job_id=? ORDER BY attempt DESC LIMIT 1`, job.JobID).Scan(&raw)
		var result map[string]any
		if raw.Valid && json.Unmarshal([]byte(raw.String), &result) == nil {
			job.SemanticStatus, _ = result["semantic_status"].(string)
			job.SemanticSuggestionCount, _ = intValue(result["semantic_suggestion_count"])
			job.SemanticTruncated, _ = result["semantic_truncated"].(bool)
			job.CompletedPairCount, _ = intValue(result["completed_pair_count"])
			job.RemainingPairCount, _ = intValue(result["remaining_pair_count"])
			job.FailedPairCount, _ = intValue(result["failed_pair_count"])
		}
	}
	if job.Status == "failed" {
		job.SemanticStatus = "failed"
		job.Error = "analysis failed; retry with a new request_id"
		job.FailedPairCount = max(job.FailedPairCount, 1)
		job.RemainingPairCount = max(0, job.SemanticCandidatePairCount-job.CompletedPairCount-job.FailedPairCount)
	} else if job.Status == "retry_wait" {
		job.Error = "a pair attempt failed; automatic retry is scheduled"
	}
	return job, nil
}

func intValue(v any) (int, bool) {
	switch x := v.(type) {
	case int:
		return x, true
	case int64:
		return int(x), true
	case float64:
		return int(x), true
	}
	return 0, false
}

func WorldlineJobStatus(ctx context.Context, db *sql.DB, scope, jobID string) (WorldlineAnalysisJob, error) {
	if db == nil || strings.TrimSpace(scope) == "" || strings.TrimSpace(jobID) == "" {
		return WorldlineAnalysisJob{}, errors.New("database, profile scope and job_id required")
	}
	job, err := process.Get(ctx, db, jobID)
	if err != nil {
		return WorldlineAnalysisJob{}, err
	}
	if job.Stage != worldlineJobStage {
		return WorldlineAnalysisJob{}, errors.New("job is not a worldline analysis")
	}
	var payload struct {
		Scope     string `json:"scope"`
		RequestID string `json:"request_id"`
	}
	_ = json.Unmarshal([]byte(job.PayloadJSON), &payload)
	if payload.Scope != scope {
		return WorldlineAnalysisJob{}, errors.New("worldline job not found")
	}
	return WorldlineAnalysisJob{JobID: job.ID, Status: job.Status, RequestID: payload.RequestID}, nil
}
