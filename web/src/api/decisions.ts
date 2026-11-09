import { request } from './client'

export type DecisionState = 'observation' | 'proposal' | 'decision_candidate' | 'confirmed' | 'superseded' | 'unresolved' | string

export interface DecisionEvidence {
  turn_id: string
  revision: string
  conversation_id?: string
  recording_id?: string
  text?: string
  offset_ms?: number | null
  source_status: string
}

export interface DecisionRecord {
  id: string
  candidate_id?: string
  topic_id: string
  topic_title: string
  state: DecisionState
  review_status?: string
  text: string
  summary?: string
  source_kind: string
  source_id?: string
  source_version?: string
  valid_from: string | null
  valid_until: string | null
  time_basis?: string
  mentioned_at: string | null
  recorded_at: string | null
  confirmed_at: string | null
  reviewer?: string
  note?: string
  evidence: DecisionEvidence[]
  supersedes?: string
  superseded_by?: string
  conflicts_with?: string[]
  unresolved_reasons?: string[]
  candidate_source?: string
}

export interface DecisionCounts {
  observations: number
  proposals: number
  candidates: number
  confirmed: number
  superseded: number
  unresolved: number
}

export interface DecisionTopic {
  id: string
  title: string
  current_state: DecisionState
  current: DecisionRecord | null
  history: DecisionRecord[]
  unresolved: DecisionRecord[]
  unresolved_reasons?: string[]
  counts: DecisionCounts
}

export interface DecisionTrail {
  schema_version: string
  version: string
  topics: DecisionTopic[]
  truncated: boolean
}

export interface DecisionReviewBody {
  expected_version: string
  candidate_id: string
  action: 'confirm' | 'reject' | 'reopen'
  request_id: string
  reviewer?: string
  note?: string
  text?: string
  valid_from?: string
  valid_until?: string
}


function decisionQuery(params: { topic_id?: string; known_at?: string; include_history?: boolean }): string {
  const query = new URLSearchParams()
  if (params.topic_id) query.set('topic_id', params.topic_id)
  if (params.known_at) query.set('known_at', params.known_at)
  if (params.include_history !== undefined) query.set('include_history', String(params.include_history))
  const encoded = query.toString()
  return encoded ? `?${encoded}` : ''
}

export function getDecisionTrail(params: { topic_id?: string; known_at?: string; include_history?: boolean } = {}): Promise<DecisionTrail> {
  return request<DecisionTrail>(`/api/decisions${decisionQuery(params)}`)
}

export function reviewDecision(body: DecisionReviewBody): Promise<DecisionTrail> {
  return request<DecisionTrail>('/api/decisions/review', { method: 'POST', body: JSON.stringify(body) })
}
