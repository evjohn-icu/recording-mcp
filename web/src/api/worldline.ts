import { request } from './client'

export type WorldlineStatus = 'all' | 'unconfirmed' | 'reviewed' | 'conflict'
export type WorldlineCategory = 'facts' | 'plans' | 'unclassified'

export interface WorldlineEvidence {
  turn_id: string
  revision: string
  conversation_id: string
  recording_id: string
  text: string
  offset_ms?: number
  audio_status?: 'available' | 'offset_unknown' | 'out_of_range' | 'deleted' | 'missing' | 'unavailable' | string
}

export interface WorldlineClaim {
  id: string
  version: string
  summary: string
  kind: string
  status: 'unconfirmed' | 'reviewed' | 'conflict' | string
  valid_from?: string | null
  valid_until?: string | null
  mentioned_at?: string | null
  time_basis: string
  time_precision: 'instant' | 'interval' | 'open' | 'unknown' | string
  change_type: 'report' | 'reality_change' | 'record_correction' | string
  evidence: WorldlineEvidence[]
  conflict_with: string[]
}

export interface WorldlineLane {
  id: string
  title: string
  source_topic_ids?: string[]
  subject: string
  category: WorldlineCategory
  claims: WorldlineClaim[]
  has_more?: boolean
  source_topic_count?: number
}

export interface WorldlineTopic {
  id: string
  title: string
  source_topic_ids: string[]
  lanes: WorldlineLane[]
  has_more?: boolean
  source_topic_count?: number
}

export interface WorldlineStats {
  source_notes: number
  topics: number
  claims: number
  dated_claims: number
  unknown_time: number
  conflicts: number
}

export interface WorldlineResponse {
  version: string
  topics: WorldlineTopic[]
  total: number
  next_offset?: number
  stats: WorldlineStats
  range: { start?: string; end?: string }
}

export interface WorldlineTopicDetail {
  version: string
  topic: WorldlineTopic
  stats: WorldlineStats
  total: number
  next_offset?: number
}

export interface WorldlineParams {
  q?: string
  from?: string
  to?: string
  status?: WorldlineStatus
  offset?: number
  limit?: number
}

export type WorldlineAction = 'rename_topic' | 'merge_topics' | 'split_topic' | 'move_claim' | 'review' | 'correct' | 'change' | 'set_time'

export interface WorldlineFeedback {
  expected_version: string
  action: WorldlineAction
  target_topic_id?: string
  target_claim_id?: string
  destination_topic_id?: string
  title?: string
  summary?: string
  valid_from?: string
  valid_until?: string
  mentioned_at?: string
  claim_ids?: string[]
  evidence?: Array<{ turn_id: string; revision: string }>
}

export interface WorldlineGeneration {
  job_id: string
  status: 'pending' | 'running' | 'complete' | 'failed' | string
  version?: string
  semantic_status?: 'not_configured' | 'no_candidates' | 'evaluated' | 'failed' | string
  semantic_model?: string
  semantic_weights_version?: string
  semantic_pair_count?: number
  semantic_suggestion_count?: number
  semantic_candidate_pair_count?: number
  semantic_truncated?: boolean
}

export interface WorldlineAnalysisJob extends WorldlineGeneration { job_id: string; request_id?: string; error?: string }

export interface WorldlineSemanticSuggestion {
  status: string
  relation: string
  model: string
  weights_version?: string
  source_claim_ids: string[]
  source_evidence: Array<{ turn_id: string; revision: string }>
  earlier_summary?: string
  later_summary?: string
  earlier_evidence?: WorldlineEvidence[]
  later_evidence?: WorldlineEvidence[]
  identity_status?: string
  reason?: string
  requires_review: boolean
  suggestion_id: string
  review_status?: 'accept' | 'reject' | 'insufficient' | 'reopen' | string
}

export type WorldlineMentionRelation = 'first_mention' | 'subsequent_mention' | 'repeated_statement' | 'plan' | 'human_correction' | 'human_reality_change' | 'review_required' | string

export interface WorldlineThreadSummary {
  id: string
  manual_group?: boolean
  title: string
  source_topic_ids: string[]
  macro_topic_id: string
  macro_topic_title: string
  category?: WorldlineCategory
  conversation_count: number
  entry_count: number
  dated_entry_count: number
  unknown_entry_count: number
  semantic_review_count?: number
  first_mentioned_at?: string | null
  last_mentioned_at?: string | null
}

export interface WorldlineThreadEntry {
  id: string
  claim_id: string
  version: string
  summary: string
  relation: WorldlineMentionRelation
  relation_reason?: string
  mentioned_at?: string | null
  time_known: boolean
  status: string
  category?: WorldlineCategory
  macro_topic_id: string
  lane_id: string
  claim: WorldlineClaim
  evidence: WorldlineEvidence[]
  conversation_ids: string[]
  semantic_suggestion?: WorldlineSemanticSuggestion
}

export interface WorldlineThreadsResponse {
  version: string
  total: number
  cross_conversation_threads?: number
  next_offset?: number
  range: { start?: string; end?: string; mentioned_start?: string; mentioned_end?: string }
  threads: WorldlineThreadSummary[]
}

export interface WorldlineThreadDetail {
  version: string
  thread: WorldlineThreadSummary
  entries_total: number
  unknown_entries_total: number
  entries_next_offset?: number
  unknown_entries_next_offset?: number
  entries: WorldlineThreadEntry[]
  unknown_entries: WorldlineThreadEntry[]
}

export interface WorldlineThreadParams {
  q?: string
  status?: WorldlineStatus
  from?: string
  to?: string
  offset?: number
  limit?: number
}

export interface WorldlineThreadDetailParams {
  q?: string
  status?: WorldlineStatus
  from?: string
  to?: string
  entries_offset?: number
  unknown_offset?: number
  limit?: number
}

export function getWorldline(params: WorldlineParams = {}, signal?: AbortSignal): Promise<WorldlineResponse> {
  const query = new URLSearchParams()
  for (const [key, value] of Object.entries(params)) {
    if (value !== undefined && value !== null && value !== '') query.set(key, String(value))
  }
  const suffix = query.size ? `?${query.toString()}` : ''
  return request(`/api/worldline${suffix}`, { signal })
}

export function getWorldlineTopic(id: string, params: { offset?: number; limit?: number; q?: string; status?: WorldlineStatus; from?: string; to?: string } = {}, signal?: AbortSignal): Promise<WorldlineTopicDetail> {
  const query = new URLSearchParams()
  for (const [key, value] of Object.entries(params)) if (value !== undefined) query.set(key, String(value))
  const suffix = query.size ? `?${query.toString()}` : ''
  return request(`/api/worldline/topics/${encodeURIComponent(id)}${suffix}`, { signal })
}

export function submitWorldlineFeedback(feedback: WorldlineFeedback): Promise<{ version: string }> {
  return request('/api/worldline/feedback', { method: 'POST', body: JSON.stringify(feedback) })
}

export function renameWorldlineThread(id: string, title: string, expectedVersion: string, requestID: string): Promise<{ version: string }> {
  return request(`/api/worldline/threads/${encodeURIComponent(id)}/rename`, { method: 'POST', body: JSON.stringify({ title, expected_version: expectedVersion, request_id: requestID }) })
}

export function groupWorldlineThreads(threadIDs: string[], title: string, expectedVersion: string, requestID: string): Promise<{ version: string }> {
  return request('/api/worldline/threads/group', { method: 'POST', body: JSON.stringify({ action: 'group', thread_ids: threadIDs, title, expected_version: expectedVersion, request_id: requestID }) })
}
export function revokeWorldlineThreadGroup(groupID: string, expectedVersion: string, requestID: string): Promise<{ version: string }> {
  return request('/api/worldline/threads/group', { method: 'POST', body: JSON.stringify({ action: 'revoke', group_id: groupID, expected_version: expectedVersion, request_id: requestID }) })
}

export function reviewWorldlineSemantic(suggestionID: string, decision: 'accept' | 'reject' | 'insufficient' | 'reopen', expectedVersion: string, requestID: string): Promise<{ version: string }> {
  return request('/api/worldline/semantic-review', { method: 'POST', body: JSON.stringify({ suggestion_id: suggestionID, decision, expected_version: expectedVersion, request_id: requestID }) })
}

export function generateWorldline(requestID: string): Promise<WorldlineGeneration> {
  return request('/api/worldline/generate', { method: 'POST', body: JSON.stringify({ request_id: requestID }) })
}

export function startWorldlineAnalysis(requestID: string, signal?: AbortSignal): Promise<WorldlineAnalysisJob> {
  return request('/api/worldline/analysis', { method: 'POST', body: JSON.stringify({ request_id: requestID }), signal })
}
export function getWorldlineAnalysis(jobID: string, signal?: AbortSignal): Promise<WorldlineAnalysisJob> {
  return request(`/api/worldline/analysis/${encodeURIComponent(jobID)}`, { signal })
}

export function getWorldlineThreads(params: WorldlineThreadParams = {}, signal?: AbortSignal): Promise<WorldlineThreadsResponse> {
  const query = new URLSearchParams()
  for (const [key, value] of Object.entries(params)) if (value !== undefined && value !== null && value !== '') query.set(key, String(value))
  return request(`/api/worldline/threads${query.size ? `?${query.toString()}` : ''}`, { signal })
}

export function getWorldlineThread(id: string, params: WorldlineThreadDetailParams = {}, signal?: AbortSignal): Promise<WorldlineThreadDetail> {
  const query = new URLSearchParams()
  for (const [key, value] of Object.entries(params)) if (value !== undefined && value !== null && value !== '') query.set(key, String(value))
  return request(`/api/worldline/threads/${encodeURIComponent(id)}${query.size ? `?${query.toString()}` : ''}`, { signal })
}
