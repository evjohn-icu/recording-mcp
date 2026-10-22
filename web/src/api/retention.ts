import { request } from './client'

export interface ForgetRequest { scope: 'turn' | 'recording' | 'conversation'; turn_id?: string; recording_id?: string; conversation_id?: string; expected_state?: string; allow_whole_audio_removal?: boolean }
export interface ForgetPreview extends Record<string, unknown> {
  expected_state: string; turns: number; notes: number; attachments: number; candidates: number; vectors: number; jobs: number;
  audio_files?: { offsets_known: boolean; remove_file: boolean; start_ms?: number; end_ms?: number }[];
  requires_broadened_audio?: boolean; managed_backup_count?: number; index_rebuild_required?: boolean; retention_notice?: string;
}
export function previewForget(body: ForgetRequest): Promise<ForgetPreview> { return request('/api/forget/preview', { method: 'POST', body: JSON.stringify(body) }) }
export function commitForget(body: ForgetRequest): Promise<{ tombstone_id: string; status: string; cleanup_pending?: boolean; message?: string }> { return request('/api/forget', { method: 'POST', body: JSON.stringify(body) }) }
export function forgetStatus(id: string): Promise<{ id: string; status: string; redaction_done?: boolean; files?: unknown[] }> { return request(`/api/forget/${encodeURIComponent(id)}`) }
export interface PendingForgetOperation { id: string; scope: string; conversation_id?: string; recording_id?: string; requested_id: string; status: string; redaction_done: boolean }
export function pendingForgets(): Promise<{ operations: PendingForgetOperation[] }> { return request('/api/forget') }
export function retryForget(id: string): Promise<{ status: string }> { return request(`/api/forget/${encodeURIComponent(id)}/retry`, { method: 'POST', body: '{}' }) }
export function suppressionPreview(conversationId: string, rowId: string): Promise<{ managed_backup_count: number; retention_notice: string; source_policy: string; source_retained: boolean }> { return request(`/api/conversations/${encodeURIComponent(conversationId)}/notes/${encodeURIComponent(rowId)}/suppression-preview`) }
export interface SuppressionOperation { operation_id: string; status: 'complete' | 'pending_external_cleanup' | string; cleanup_pending: boolean }
export function suppressNote(conversationId: string, rowId: string, expected_version?: string): Promise<SuppressionOperation> { return request(`/api/conversations/${encodeURIComponent(conversationId)}/notes/${encodeURIComponent(rowId)}/suppress`, { method: 'POST', body: JSON.stringify(expected_version ? { expected_version } : {}) }) }
export function suppressionStatus(id: string): Promise<SuppressionOperation> { return request(`/api/suppressions/${encodeURIComponent(id)}`) }
export function retrySuppression(id: string): Promise<SuppressionOperation> { return request(`/api/suppressions/${encodeURIComponent(id)}/retry`, { method: 'POST', body: '{}' }) }
export function pendingSuppressions(): Promise<{ operations: SuppressionOperation[] }> { return request('/api/suppressions') }
