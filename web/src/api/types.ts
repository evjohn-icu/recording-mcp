export interface TemporalEvent {
  id: string
  type: string
  timestamp: string
  source: string
  content: string
  confidence: number
  structured_data?: Record<string, unknown>
  recorded_at?: string
  valid_at?: string
  valid_from?: string
  known_at?: string
  source_status: 'current' | 'source_stale' | 'unknown' | string
}
export interface TemporalState {
  id: string
  dimension: string
  status: string
  summary: string
  provenance: string[]
  updated_at: string
  structured_data?: Record<string, unknown>
}

export interface AuthInfo {
  mode: 'profile' | 'token' | 'profiles'
  profile?: string
  profiles?: string[]
}

export interface TranscriptTurn { id: string; original_index: number; text: string; speaker_scoped_id?: string | null }
export interface TranscriptSource {
  conversation_id: string
  recording_id: string
  segment_id: string
  source_attachment_id: string
  result: { source: { transcript_revision: string }; turns: TranscriptTurn[] }
}
export interface SpeakerBinding {
  id: string; recording_id: string; transcript_revision: string; speaker_scoped_id: string
  person_id?: string; status: string; evidence_turn_ids: string[]; version: number
}

export type AnalysisStatus = 'pending' | 'partial'
export type IndexingStatus = 'pending' | 'ready' | 'failed' | 'disabled'

export interface Topic {
  id: string
  title: string
  conversation_id: string
}

export interface ConversationSummary {
  conversation_id: string
  title: string
  recording_id: string
  recording_started_at: string | null
  received_segments: number
  original_turns: number
  topic_linked_turns: number
  analysis_status: AnalysisStatus
  annotation_version: string
}

export interface ConversationPage {
  items: ConversationSummary[]
  next_cursor?: string
}

export interface RecordingView {
  recording_id: string
  started_at: string | null
  absolute_time_known: boolean
  local_audio: 'available' | 'deleted' | 'missing' | 'unavailable' | string
  source_file?: string
  source_bytes?: number
  duration_ms?: number
}

export interface NoteView {
  note_id: string
  row_id: string
  kind: string
  summary: string
  topic_ids: string[]
  evidence_turn_ids: string[]
  annotation_version: string
  edited: boolean
}

export interface ProcessingStatus {
  conversation_id: string
  received_segments: number
  original_turns: number
  topic_linked_turns: number
  unclassified_turns: number
  ambient_turns: number
  identity_unconfirmed_turns: number
  analysis_status: AnalysisStatus
  indexing_status: IndexingStatus
}

export interface ProcessingJob {
  id: string; stage: string; input_snapshot_id: string; status: string
  attempt_count: number; max_attempts: number; worker_id: string
}
export interface WorkerHeartbeat {
  worker_id: string; pid: number; stages: string[]; generator_enabled: boolean
  last_seen_at: string; alive: boolean
}
export type PhaseState = 'pending' | 'running' | 'done' | 'failed' | 'skipped'
export type PhaseName = 'ingest' | 'asr' | 'extract' | 'publish' | 'index'
export interface ProcessingPhase {
  name: PhaseName | string
  state: PhaseState | string
  job_id?: string
  attempt?: number
  updated_at?: string
  error?: string
  error_code?: string
  error_action?: string
}
export interface ProcessingSnapshot {
  status: ProcessingStatus; jobs: ProcessingJob[]; workers: WorkerHeartbeat[]
  generator_enabled: boolean
  phases: ProcessingPhase[]
  current_stage: string
  asr_provider: string
  asr_available: boolean
  worker_alive: boolean
}

export interface ASRCapability {
  provider: string
  configured: boolean
  worker_available: boolean
  available: boolean
  reason?: string
}
export interface Capabilities {
  version: string
  asr: ASRCapability
  generator: { enabled: boolean }
  retrieval: { default_version: string; v2_available: boolean }
  worker: { alive: boolean; count: number }
  limits: { upload_bytes: number }
}

export interface ImportResult {
  conversation_id: string
  recording_id: string
  job_id: string
  duplicate: boolean
  audio: {
    filename: string
    bytes: number
    duration_ms: number
    format: string
    sample_rate: number
    channels: number
  }
}

export interface ConversationView {
  conversation_id: string
  title: string
  summary: string
  annotation_version: string
  annotation_basis: string
  recording_started_at: string | null
  recordings: RecordingView[]
  topics: Topic[]
  notes: NoteView[]
  processing: ProcessingStatus
}

export interface Item {
  id: string
  kind: string
  conversation_id: string
  recording_id?: string
  title?: string
  summary?: string
  topic_ids?: string[]
  evidence_turn_ids?: string[]
  content?: string
  speaker_scoped_id?: string
  speaker_entity_id: string | null
  speaker_person_id?: string
  speaker_confirmed?: boolean
  global_start_ms: number | null
  global_end_ms: number | null
  source_sha256?: string
  turn_id?: string
  transcript_version?: string
  claim_mode?: string
  interpretation_note?: string
  annotation_version?: string
  stale?: boolean
  ambient?: boolean
  derived_from?: string[]
  audio_status?: AudioStatus
}

export type AudioStatus = 'available' | 'offset_unknown' | 'out_of_range' | 'deleted' | 'missing' | 'unavailable' | string

export interface TurnView extends Item {
  audio_status: AudioStatus
}

export interface TurnPage {
  conversation_id: string
  recording_id: string
  items: TurnView[]
  next_offset: number | null
}

export interface SearchHit {
  kind: string
  id: string
  text: string
  conversation_id: string
  why: 'note' | 'turn' | string
  score: number
  evidence_turn_ids?: string[]
  global_start_ms: number | null
  speaker_person_id?: string
  stale?: boolean
  ambient?: boolean
  derived_from?: string[]
  conversation_title: string
}

export interface SearchPageView {
  hits: SearchHit[]
  next_cursor?: string
  degraded: boolean
  degrade_reason?: string
}

export interface ItemDetail {
  experimental: boolean
  item: Item
  evidence?: Item[]
}

export interface EvidenceTurn {
  id: string
  text: string
  speaker_scoped_id?: string | null
  global_start_ms: number | null
  global_end_ms: number | null
  segment_index: number
  is_target: boolean
  stale: boolean
  audio_status?: AudioStatus
  speaker_person_id?: string | null
  speaker_entity_id?: string | null
  speaker_confirmed?: boolean
}

export interface EvidenceContext {
  recording_id: string
  turns: EvidenceTurn[]
  truncated: boolean
  stale: boolean
}

export interface AnnotationNoteSnapshot {
  id: string
  kind: string
  topic_ids?: string[]
  summary: string
  evidence_turn_ids?: string[]
  edited?: boolean
}

export interface AnnotationSnapshot {
  version: string
  basis: string
  summary: string
  annotations: {
    conversation_id: string
    title: string
    recording_id?: string
    summary?: string
    annotation_basis?: string
    annotation_version?: string
    topics?: { id: string; title: string }[]
    notes?: AnnotationNoteSnapshot[]
    turn_topics?: { turn_id: string; topic_ids: string[] }[]
  }
  current: boolean
}

export interface AnnotationHistoryResponse {
  experimental: boolean
  conversation_id: string
  revisions: AnnotationSnapshot[]
}

export interface EditResult {
  annotation_version: string
  note_id: string
  row_id: string
  index_queued: boolean
}

export interface Gap {
  kind: string
  detail: string
}

export interface PackageItemRanking {
  item_id: string
  kind: string
  retrieval_sources: string[]
  retrieval_score: number
  retrieval_branches?: Record<string, number>
  retrieval_link_ids?: string[]
}

export interface ContextPackage {
  items: Item[]
  evidence: {
    turn_id: string
    text: string
    speaker_scoped_id?: string | null
    global_start_ms: number | null
    global_end_ms: number | null
    used_by_item_ids: string[]
  }[]
  coverage: {
    conversations: string[]
    topic_ids: string[]
    time_range: {
      started_after: string | null
      started_before: string | null
      unknown_time_included: boolean
    }
    branches: {
      lexical: boolean
      links: boolean
      evidence: boolean
      semantic: boolean
    }
    scanned: { notes: number; turns: number }
  }
  gaps: Gap[]
  truncated: boolean
  degraded: boolean
  degrade_reason: string
  trace: {
    lexical: { query: string; hit_ids: string[] }
    semantic: {
      query?: string
      backend?: string
      indexed: number
      hit_ids: string[]
      rejected_stale: number
      rejected_scope: number
    }
    links_expanded: { link_id: string; relation: string; from_id: string; to_id: string }[]
    stale_filtered: string[]
    decision_rerank: boolean
    budget: { bytes_used: number; items_used: number; evidence_used: number }
  }
  ranking: PackageItemRanking[]
}

export interface HybridView extends ContextPackage {
  conversation_titles: Record<string, string>
}

export interface SessionResponse {
  authenticated?: boolean
  csrf_token: string
  profile?: string
}

export interface SearchParams {
  q: string
  scope: 'raw' | 'derived' | 'both'
  topic_id?: string
  started_after?: string
  started_before?: string
  cursor?: string
  limit?: number
  include_ambient?: boolean
}

export interface HybridParams {
  q: string
  mode: 'hybrid' | 'lexical'
  retrieval_version?: 'v1' | 'v2'
  topic_id?: string
  conversation_id?: string
  started_after?: string
  started_before?: string
  max_items?: number
}

export interface ConversationListParams {
  limit?: number
  cursor?: string
  topic_id?: string
  status?: AnalysisStatus
  started_after?: string
  started_before?: string
}

export interface TurnParams {
  recording_id: string
  include_ambient?: boolean
  offset?: number
  limit?: number
}

export interface ApiErrorEnvelope {
  error?: {
    code?: string
    message?: string
    details?: unknown
  }
}
