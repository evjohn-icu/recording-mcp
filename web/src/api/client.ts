import type {
  AnnotationHistoryResponse,
  AuthInfo,
  ConversationListParams,
  ConversationPage,
  ConversationView,
  EditResult,
  EvidenceContext,
  HybridParams,
  HybridView,
  ItemDetail,
  SearchPageView,
  SearchParams,
  SessionResponse,
  TranscriptSource,
  SpeakerBinding,
  Topic,
  TurnPage,
  TurnParams,
  TemporalEvent,
  TemporalState,
} from './types'

let csrfToken: string | null = null

export function setCsrfToken(token: string | null): void {
  csrfToken = token
}

export function getCsrfToken(): string | null {
  return csrfToken
}

export class ApiError extends Error {
  readonly code: string
  readonly status: number
  readonly details: unknown

  constructor(code: string, status: number, message: string, details?: unknown) {
    super(message)
    this.name = 'ApiError'
    this.code = code
    this.status = status
    this.details = details
  }
}

function queryString(params: object): string {
  const query = new URLSearchParams()
  for (const [key, value] of Object.entries(params)) {
    if (value !== undefined && value !== null && value !== '') {
      query.set(key, String(value))
    }
  }
  const encoded = query.toString()
  return encoded ? `?${encoded}` : ''
}

async function parseError(response: Response): Promise<ApiError> {
  let payload: unknown
  try {
    payload = await response.json()
  } catch {
    return new ApiError('internal', response.status || 500, '本地服务返回了无法识别的错误')
  }
  const envelope = payload as { error?: { code?: string; message?: string; details?: unknown } } | null
  return new ApiError(
    envelope?.error?.code || 'internal',
    response.status || 500,
    envelope?.error?.message || '本地服务发生错误',
    envelope?.error?.details,
  )
}

export async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const method = (init.method || 'GET').toUpperCase()
  const headers = new Headers(init.headers)
  if (init.body !== undefined && init.body !== null && !headers.has('Content-Type')) {
    headers.set('Content-Type', 'application/json')
  }
  if (method !== 'GET' && method !== 'HEAD' && csrfToken && !headers.has('X-CSRF-Token')) {
    headers.set('X-CSRF-Token', csrfToken)
  }
  const response = await fetch(path, {
    ...init,
    method,
    headers,
    credentials: 'same-origin',
  })
  if (!response.ok) {
    throw await parseError(response)
  }
  if (response.status === 204) return undefined as T
  return response.json() as Promise<T>
}

export function getSession(signal?: AbortSignal): Promise<SessionResponse> {
  return request<SessionResponse>('/api/session', { signal })
}

export function createSession(token: string, signal?: AbortSignal): Promise<SessionResponse> {
  return request<SessionResponse>('/api/session', {
    method: 'POST',
    body: JSON.stringify({ token }),
    signal,
  })
}

export function login(password: string): Promise<{ csrf_token: string; profile: string }> {
  return request('/api/login', { method: 'POST', body: JSON.stringify({ password }) })
}
export function loginProfile(profile: string, password: string): Promise<{ csrf_token: string; profile: string }> {
  return request('/api/profiles/login', { method: 'POST', body: JSON.stringify({ profile, password }) })
}
export function authInfo(): Promise<AuthInfo> { return request('/api/auth-info') }
export function logout(): Promise<void> { return request('/api/logout', { method: 'POST', body: '{}' }) }
export function getTranscriptSource(turnId: string): Promise<TranscriptSource> { return request(`/api/turns/${encodeURIComponent(turnId)}/transcript-source`) }
export function getTranscriptSegment(conversationId: string, recordingId: string, segmentId: string): Promise<TranscriptSource['result']> {
  return request(`/api/conversations/${encodeURIComponent(conversationId)}/recordings/${encodeURIComponent(recordingId)}/segments/${encodeURIComponent(segmentId)}`)
}
export function correctTranscript(conversationId: string, recordingId: string, segmentId: string, expected_revision: string, new_revision: string, texts: Record<string, string>): Promise<unknown> {
  return request(`/api/conversations/${encodeURIComponent(conversationId)}/recordings/${encodeURIComponent(recordingId)}/segments/${encodeURIComponent(segmentId)}/correction`, { method: 'POST', body: JSON.stringify({ expected_revision, new_revision, texts }) })
}
export function getSpeakerBindings(recordingId: string, speakerId: string): Promise<{ bindings: SpeakerBinding[] }> {
  return request(`/api/recordings/${encodeURIComponent(recordingId)}/speakers?${new URLSearchParams({ speaker_scoped_id: speakerId })}`)
}
export function proposeSpeaker(recordingId: string, speaker_scoped_id: string, transcript_revision: string, evidence_turn_ids: string[]): Promise<SpeakerBinding> {
  return request(`/api/recordings/${encodeURIComponent(recordingId)}/speakers/proposals`, { method: 'POST', body: JSON.stringify({ speaker_scoped_id, transcript_revision, evidence_turn_ids }) })
}
export function reviewSpeakerBinding(id: string, action: 'confirm' | 'reject' | 'revoke', person_id: string, note: string): Promise<SpeakerBinding> {
  return request(`/api/speaker-bindings/${encodeURIComponent(id)}`, { method: 'PUT', body: JSON.stringify({ action, ...(action === 'confirm' ? { person_id } : {}), note }) })
}

export function getTimeHistory(signal?: AbortSignal): Promise<{ events: TemporalEvent[] }> { return request('/api/time/history', { signal }) }
export function getTimeState(validAt: string, knownAt: string, signal?: AbortSignal): Promise<{ states: TemporalState[]; scope: string }> {
  return request(`/api/time/state${queryString({ valid_at: validAt, known_at: knownAt })}`, { signal })
}
export function observeFact(event: Record<string, unknown>, evidence: { turn_id: string; revision: string }[]): Promise<{ inserted: number }> {
  return request('/api/time/facts', { method: 'POST', body: JSON.stringify({ event, evidence }) })
}
export function correctFact(body: { id: string; target_id: string; expected_head: string; known_at: string; source: string; content?: string; valid_at?: string; retract?: boolean; evidence: { turn_id: string; revision: string }[] }): Promise<{ inserted: number }> {
  return request('/api/time/corrections', { method: 'POST', body: JSON.stringify(body) })
}

export function listConversations(params: ConversationListParams = {}, signal?: AbortSignal): Promise<ConversationPage> {
  return request<ConversationPage>(`/api/conversations${queryString(params)}`, { signal })
}

export function getConversation(conversationId: string, signal?: AbortSignal): Promise<ConversationView> {
  return request<ConversationView>(`/api/conversations/${encodeURIComponent(conversationId)}`, { signal })
}

export function getTurns(conversationId: string, params: TurnParams, signal?: AbortSignal): Promise<TurnPage> {
  return request<TurnPage>(
    `/api/conversations/${encodeURIComponent(conversationId)}/turns${queryString({
      recording_id: params.recording_id,
      include_ambient: params.include_ambient ? 1 : 0,
      offset: params.offset,
      limit: params.limit,
    })}`,
    { signal },
  )
}

export function getAnnotationHistory(conversationId: string, signal?: AbortSignal): Promise<AnnotationHistoryResponse> {
  return request<AnnotationHistoryResponse>(
    `/api/conversations/${encodeURIComponent(conversationId)}/annotations/history`,
    { signal },
  )
}

export function editNote(
  conversationId: string,
  rowId: string,
  expectedVersion: string,
  newVersion: string,
  summary: string,
  signal?: AbortSignal,
): Promise<EditResult> {
  return request<EditResult>(
    `/api/conversations/${encodeURIComponent(conversationId)}/notes/${encodeURIComponent(rowId)}`,
    {
      method: 'PUT',
      body: JSON.stringify({ expected_version: expectedVersion, new_version: newVersion, summary }),
      signal,
    },
  )
}

export function getItem(itemId: string, signal?: AbortSignal): Promise<ItemDetail> {
  return request<ItemDetail>(`/api/items/${encodeURIComponent(itemId)}`, { signal })
}

export function getEvidence(turnId: string, radius?: number, maxBytes?: number, signal?: AbortSignal): Promise<EvidenceContext> {
  return request<EvidenceContext>(
    `/api/turns/${encodeURIComponent(turnId)}/evidence${queryString({ radius, max_bytes: maxBytes })}`,
    { signal },
  )
}

export function getAudioURL(turnId: string): string {
  return `/api/turns/${encodeURIComponent(turnId)}/audio`
}

export function search(params: SearchParams, signal?: AbortSignal): Promise<SearchPageView> {
  return request<SearchPageView>(`/api/search${queryString({
    q: params.q,
    scope: params.scope,
    topic_id: params.topic_id,
    started_after: params.started_after,
    started_before: params.started_before,
    cursor: params.cursor,
    limit: params.limit,
    include_ambient: params.include_ambient ? 1 : 0,
  })}`, { signal })
}

export function searchHybrid(params: HybridParams, signal?: AbortSignal): Promise<HybridView> {
  return request<HybridView>(`/api/search/hybrid${queryString(params)}`, { signal })
}

export function getTopics(conversationId?: string, signal?: AbortSignal): Promise<{ experimental: boolean; topics: Topic[] }> {
  return request<{ experimental: boolean; topics: Topic[] }>(`/api/topics${queryString({ conversation_id: conversationId })}`, { signal })
}

export function health(signal?: AbortSignal): Promise<{ status: 'ok' }> {
  return request<{ status: 'ok' }>('/healthz', { signal })
}
