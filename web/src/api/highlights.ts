import { request } from './client'

export interface Highlight {
  id: string; conversation_id: string; recording_id: string
  anchor_kind: 'point' | 'range' | 'turn'; start_ms: number | null; end_ms: number | null
  turn_id: string | null; source_attachment_id: string | null; transcript_revision: string
  tag: 'important' | 'follow_up' | 'question'; note_text: string; version: number
  created_at_ms: number; updated_at_ms: number; state: 'current' | 'needs_relink'; quote_text: string | null
}
export interface HighlightCreate {
  request_id: string; anchor_kind: Highlight['anchor_kind']; start_ms?: number | null; end_ms?: number | null
  turn_id?: string | null; source_attachment_id?: string | null; transcript_revision: string
  tag: Highlight['tag']; note_text: string
}
export function listHighlights(recordingId: string): Promise<{ contract_version: string; highlights: Highlight[] }> {
  return request(`/api/recordings/${encodeURIComponent(recordingId)}/highlights`)
}
export function createHighlight(recordingId: string, body: HighlightCreate): Promise<Highlight> {
  return request(`/api/recordings/${encodeURIComponent(recordingId)}/highlights`, { method: 'POST', body: JSON.stringify(body) })
}
export function getHighlight(id: string): Promise<Highlight> { return request(`/api/highlights/${encodeURIComponent(id)}`) }
export function updateHighlight(id: string, expectedVersion: number, patch: { tag?: Highlight['tag']; note_text?: string; relink?: { turn_id: string; source_attachment_id: string; transcript_revision: string } }): Promise<Highlight> {
  return request(`/api/highlights/${encodeURIComponent(id)}`, { method: 'PATCH', body: JSON.stringify({ expected_version: expectedVersion, ...patch }) })
}
export function deleteHighlight(id: string, expectedVersion: number): Promise<void> {
  return request(`/api/highlights/${encodeURIComponent(id)}?expected_version=${expectedVersion}`, { method: 'DELETE' })
}
export function highlightAudioURL(id: string): string { return `/api/highlights/${encodeURIComponent(id)}/audio` }
