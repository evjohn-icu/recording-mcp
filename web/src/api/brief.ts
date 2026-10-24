import { request } from './client'

export interface BriefRequest {
  scope: { kind: 'person' | 'topic' | 'conversation' | 'project'; person_ids: string[]; topic_ids: string[]; conversation_ids: string[] }
  started_after?: string; started_before?: string; max_items?: number; max_evidence?: number; max_bytes?: number
}
export interface BriefEntry { id: string; text: string; text_truncated?: boolean; evidence_ids: string[]; source_state: 'current' | 'superseded' | 'unavailable'; assertion_state: 'proposed' | 'unverified' | 'explicitly_confirmed' }
export interface BriefEvidence { id: string; turn_id: string; conversation_id: string; recording_id: string; source_attachment_id?: string; transcript_revision?: string; global_start_ms?: number; global_end_ms?: number; text_truncated?: boolean; topic_ids: string[]; person_ids: string[]; text: string; started_at?: string | null; source_state: string; provenance: string }
export interface BriefResult {
  snapshot_id: string; scope: BriefRequest['scope']; sections: Record<'background' | 'previous_discussions' | 'follow_up_candidates' | 'changes_conflicts' | 'unknowns', BriefEntry[]>
  evidence: BriefEvidence[]; coverage: { eligible_turns: number; selected_turns: number; selected_notes: number; omitted_turns: number; omitted_notes: number }
  gaps: { kind: string; detail: string }[]; truncated: boolean; degraded: boolean
}
export function previewBrief(body: BriefRequest): Promise<BriefResult> { return request('/api/briefs/preview', { method: 'POST', body: JSON.stringify(body) }) }

export function listBriefPeople(): Promise<{people: {id: string; label: string}[]}> { return request('/api/briefs/people') }
