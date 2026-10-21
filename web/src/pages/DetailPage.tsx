import { useEffect, useRef, useState } from 'react'
import { useParams, useSearchParams } from 'react-router-dom'
import { ApiError, editNote, getAnnotationHistory, getConversation, getItem, getTurns } from '../api/client'
import type {
  AnnotationSnapshot,
  ConversationView,
  NoteView,
  RecordingView,
  TurnView,
} from '../api/types'
import { useApi } from '../hooks/useApi'
import { EvidencePanel } from '../components/EvidencePanel'
import { NoteCard, noteKindLabel } from '../components/NoteCard'
import { DiffView } from '../components/DiffView'
import { StateView } from '../components/StateView'
import { StatusBadge } from '../components/StatusBadge'
import { useAudio } from '../components/AudioBar'
import { CLOSE_EVENT } from '../keyboard'

const INDEXING_LABELS: Record<string, string> = {
  pending: '语义索引待更新',
  ready: '已索引',
  failed: '索引失败',
  disabled: '语义索引已关闭',
}

function formatDate(value: string | null): string {
  if (!value) return '时间未知'
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return '时间未知'
  const pad = (part: number) => String(part).padStart(2, '0')
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())} ${pad(date.getHours())}:${pad(date.getMinutes())}`
}

function offsetTime(milliseconds: number | null): string {
  if (milliseconds === null || !Number.isFinite(milliseconds)) return '时间未知'
  const total = Math.max(0, milliseconds) / 1000
  const minutes = Math.floor(total / 60)
  const seconds = total - minutes * 60
  return `+${String(minutes).padStart(2, '0')}:${seconds.toFixed(1).padStart(4, '0')}`
}

function playDisabledReason(status: string): string | null {
  if (status === 'available') return null
  if (status === 'offset_unknown') return '该片段缺少时间信息，无法定位音频'
  if (status === 'out_of_range') return '音频时间超出录音范围'
  if (status === 'deleted') return '录音已删除（转写与归纳保留）'
  if (status === 'missing') return '本机找不到音频文件'
  return '此录音没有本地音频'
}

function TurnRow({ turn, onOpenEvidence }: { turn: TurnView; onOpenEvidence: (turnId: string) => void }) {
  const { play } = useAudio()
  const reason = playDisabledReason(turn.audio_status)
  return (
    <article className={`turn-row ${turn.ambient ? 'turn-ambient' : ''}`.trim()}>
      <header className="turn-header">
        <span className="turn-time">{offsetTime(turn.global_start_ms)}</span>
        <span className="turn-speaker">
          {turn.speaker_scoped_id ? `${turn.speaker_scoped_id} ` : ''}
          {turn.speaker_confirmed === true && turn.speaker_person_id ? `（${turn.speaker_person_id}）` : '未确认身份'}
        </span>
        {turn.ambient && <span className="ambient-tag">环境音</span>}
      </header>
      <p className="turn-text">{turn.content || '（无文本）'}</p>
      <div className="turn-actions">
        {reason === null ? (
          <button type="button" className="secondary-button" onClick={() => play(turn.id)}>播放</button>
        ) : (
          <button type="button" className="secondary-button" disabled title={reason}>{reason}</button>
        )}
        <button type="button" className="link-button" onClick={() => onOpenEvidence(turn.id)}>查看证据</button>
      </div>
    </article>
  )
}

function RecordingTurns({
  conversationId,
  recording,
  includeAmbient,
  onOpenEvidence,
}: {
  conversationId: string
  recording: RecordingView
  includeAmbient: boolean
  onOpenEvidence: (turnId: string) => void
}) {
  const [offset, setOffset] = useState(0)
  const [items, setItems] = useState<TurnView[]>([])
  const [nextOffset, setNextOffset] = useState<number | null>(null)
  const [autoPages, setAutoPages] = useState(0)

  const requestKey = `${conversationId}:${recording.recording_id}:${includeAmbient}:${offset}`
  const { data: result, loading, error, reload } = useApi(
    (signal) =>
      getTurns(
        conversationId,
        { recording_id: recording.recording_id, include_ambient: includeAmbient, offset, limit: 200 },
        signal,
      ).then((page) => ({ key: requestKey, page })),
    [requestKey],
  )
  const data = result?.key === requestKey ? result.page : undefined

  useEffect(() => {
    setOffset(0)
    setItems([])
    setNextOffset(null)
    setAutoPages(0)
  }, [conversationId, includeAmbient, recording.recording_id])

  useEffect(() => {
    if (!data) return
    setItems((previous) => {
      const combined = offset === 0 ? data.items : [...previous, ...data.items]
      const seen = new Set<string>()
      return combined.filter((item) => {
        const key = item.kind === 'turn' ? `turn:${item.id}` : `other:${item.id}`
        if (seen.has(key)) return false
        seen.add(key)
        return true
      })
    })
    setNextOffset(data.next_offset)
  }, [data, offset])

  useEffect(() => {
    if (!data || loading) return
    const hasTurns = data.items.some((item) => item.kind === 'turn')
    if (!hasTurns && data.next_offset !== null && autoPages < 5) {
      setAutoPages((value) => value + 1)
      setOffset(data.next_offset)
    }
  }, [data, loading, autoPages])

  const turns = items.filter((item) => item.kind === 'turn')

  return (
    <section className="recording-group" aria-label={`录音 ${recording.recording_id}`}>
      <header className="recording-header">
        <h3>{formatDate(recording.started_at)}</h3>
        <StatusBadge status={recording.local_audio} />
      </header>
      <StateView loading={loading && turns.length === 0} error={turns.length === 0 ? error : undefined} onRetry={reload} empty={!loading && !error && turns.length === 0} emptyMessage="该录音没有转写片段">
        {turns.map((turn) => <TurnRow turn={turn} onOpenEvidence={onOpenEvidence} key={turn.id} />)}
      </StateView>
      {nextOffset !== null && (
        <>
          {error && turns.length > 0 && <p className="note-error" role="alert">后续片段加载失败：{error.message}</p>}
          <button type="button" className="load-more" onClick={() => error ? reload() : setOffset(nextOffset)} disabled={loading}>
            {loading ? '加载中…' : error ? '重试加载' : '加载更多'}
          </button>
        </>
      )}
    </section>
  )
}

function TranscriptTab({ conversation, onOpenEvidence }: { conversation: ConversationView; onOpenEvidence: (turnId: string) => void }) {
  const [includeAmbient, setIncludeAmbient] = useState(false)
  return (
    <div className="tab-panel">
      <p className="tab-note">转写为 ASR 原文，只读；纠错需要新的转录修订（本轮不提供）</p>
      <label className="toggle-label">
        <input type="checkbox" checked={includeAmbient} onChange={(event) => setIncludeAmbient(event.target.checked)} />
        环境音（默认隔离）
      </label>
      {conversation.recordings.length === 0
        ? <div className="state-view empty-state">此会话没有录音</div>
        : conversation.recordings.map((recording) => (
          <RecordingTurns
            conversationId={conversation.conversation_id}
            recording={recording}
            includeAmbient={includeAmbient}
            onOpenEvidence={onOpenEvidence}
            key={recording.recording_id}
          />
        ))}
    </div>
  )
}

interface ConflictState {
  submitted: string
  baseSummary: string
  currentSummary: string | null
  baseVersion: string
  currentVersion: string
}

interface ConversationDraft {
  rowId: string
  draft: string
  baseSummary: string
  expectedVersion: string
  baseVersion: string
  note: NoteView
}

const conversationDrafts = new Map<string, ConversationDraft>()

function NotesTab({
  conversation,
  onChanged,
  onOpenEvidence,
  highlightedItem,
}: {
  conversation: ConversationView
  onChanged: () => void
  onOpenEvidence: (turnId: string) => void
  highlightedItem: string
}) {
  const [editingRow, setEditingRow] = useState<string | null>(null)
  const [draft, setDraft] = useState('')
  const [baseSummary, setBaseSummary] = useState('')
  const [expectedVersion, setExpectedVersion] = useState(conversation.annotation_version)
  const [baseVersion, setBaseVersion] = useState(conversation.annotation_version)
  const [newVersion, setNewVersion] = useState('')
  const [editingNote, setEditingNote] = useState<NoteView | null>(null)
  const [saving, setSaving] = useState(false)
  const [editError, setEditError] = useState('')
  const [conflict, setConflict] = useState<ConflictState | null>(null)
  const [copied, setCopied] = useState(false)
  const conflictDialogRef = useRef<HTMLDivElement>(null)
  const conflictCloseRef = useRef<HTMLButtonElement>(null)
  const restoreFocusRef = useRef<HTMLElement | null>(null)

  useEffect(() => {
    const saved = conversationDrafts.get(conversation.conversation_id)
    const note = saved && (conversation.notes.find((entry) => entry.row_id === saved.rowId) || saved.note)
    if (!saved || !note) return
    setEditingRow(note.row_id)
    setEditingNote(note)
    setDraft(saved.draft)
    setBaseSummary(saved.baseSummary)
    setExpectedVersion(saved.expectedVersion)
    setBaseVersion(saved.baseVersion)
    setNewVersion(`web-${crypto.randomUUID()}`)
  }, [conversation.conversation_id])

  useEffect(() => {
    if (!conflict) return
    restoreFocusRef.current = document.activeElement instanceof HTMLElement ? document.activeElement : null
    conflictCloseRef.current?.focus()
    const dialog = conflictDialogRef.current
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape') {
        event.preventDefault()
        setConflict(null)
        return
      }
      if (event.key !== 'Tab' || !dialog) return
      const focusable = Array.from(dialog.querySelectorAll<HTMLElement>('button:not([disabled]), [href], input:not([disabled]), textarea:not([disabled]), select:not([disabled]), [tabindex]:not([tabindex="-1"])'))
      if (focusable.length === 0) return
      const first = focusable[0]
      const last = focusable[focusable.length - 1]
      if (event.shiftKey && (document.activeElement === first || !dialog.contains(document.activeElement))) {
        event.preventDefault()
        last.focus()
      } else if (!event.shiftKey && (document.activeElement === last || !dialog.contains(document.activeElement))) {
        event.preventDefault()
        first.focus()
      }
    }
    document.addEventListener('keydown', onKeyDown)
    return () => {
      document.removeEventListener('keydown', onKeyDown)
      restoreFocusRef.current?.focus()
    }
  }, [conflict])

  useEffect(() => {
    const onClose = () => setConflict(null)
    window.addEventListener(CLOSE_EVENT, onClose)
    return () => window.removeEventListener(CLOSE_EVENT, onClose)
  }, [])

  function startEdit(note: NoteView) {
    setEditingRow(note.row_id)
    setEditingNote(note)
    setDraft(note.summary)
    conversationDrafts.set(conversation.conversation_id, {
      rowId: note.row_id,
      draft: note.summary,
      baseSummary: note.summary,
      expectedVersion: conversation.annotation_version,
      baseVersion: conversation.annotation_version,
      note,
    })
    setBaseSummary(note.summary)
    setExpectedVersion(conversation.annotation_version)
    setBaseVersion(conversation.annotation_version)
    setNewVersion(`web-${crypto.randomUUID()}`)
    setEditError('')
    setCopied(false)
    setConflict(null)
  }

  async function submitEdit(rowId: string, version = expectedVersion, versionId = newVersion) {
    setSaving(true)
    setEditError('')
    try {
      if (Array.from(draft).length > 4000) {
        setEditError('摘要最多 4000 个字符')
        return
      }
      await editNote(conversation.conversation_id, rowId, version, versionId, draft)
      conversationDrafts.delete(conversation.conversation_id)
      setEditingRow(null)
      setEditingNote(null)
      setConflict(null)
      onChanged()
    } catch (error) {
      if (error instanceof ApiError && error.code === 'annotation_version_conflict') {
        const details = (error.details || {}) as {
          current_version?: string
          current_note?: { summary?: string } | null
        }
        setConflict({
          submitted: draft,
          baseSummary,
          currentSummary: details.current_note ? details.current_note.summary ?? '' : null,
          baseVersion,
          currentVersion: details.current_version || '',
        })
      } else if (error instanceof ApiError) {
        setEditError(error.message)
      } else {
        setEditError('无法连接本地服务')
      }
    } finally {
      setSaving(false)
    }
  }

  async function copyMine() {
    try {
      await navigator.clipboard.writeText(draft)
      setCopied(true)
    } catch {
      setCopied(false)
    }
  }

  function retryOnCurrent() {
    if (!conflict?.currentVersion || conflict.currentSummary === null) return
    const saved = conversationDrafts.get(conversation.conversation_id)
    if (saved) {
      conversationDrafts.set(conversation.conversation_id, {
        ...saved,
        expectedVersion: conflict.currentVersion,
        baseVersion: conflict.currentVersion,
        baseSummary: conflict.currentSummary,
      })
    }
    setExpectedVersion(conflict.currentVersion)
    setBaseVersion(conflict.currentVersion)
    setBaseSummary(conflict.currentSummary)
    const retryVersion = `web-${crypto.randomUUID()}`
    setNewVersion(retryVersion)
    setConflict(null)
    if (editingNote) void submitEdit(editingNote.row_id, conflict.currentVersion, retryVersion)
  }

  if (conversation.annotation_version === '' && !editingNote) {
    return <div className="tab-panel"><p className="tab-note">尚未生成 AI 归纳</p></div>
  }

  return (
    <div className="tab-panel">
      {conversation.notes.length === 0
        ? <div className="state-view empty-state">该版本没有归纳条目</div>
        : conversation.notes.map((note) => (
          <NoteCard
            highlighted={highlightedItem === note.note_id || highlightedItem === note.row_id}
            note={note}
            editing={editingRow === note.row_id}
            draft={draft}
            saving={saving}
            onDraftChange={(value) => {
              setDraft(value)
              if (editingNote) {
                const saved = conversationDrafts.get(conversation.conversation_id)
                if (saved) conversationDrafts.set(conversation.conversation_id, { ...saved, draft: value })
              }
            }}
            onStartEdit={() => startEdit(note)}
            onCancelEdit={() => {
              conversationDrafts.delete(conversation.conversation_id)
              setEditingRow(null)
              setEditingNote(null)
              setConflict(null)
              setEditError('')
            }}
            onSubmitEdit={() => void submitEdit(note.row_id)}
            onOpenEvidence={onOpenEvidence}
            key={note.row_id}
          />
        ))}
      {editingNote && !conversation.notes.some((note) => note.row_id === editingNote.row_id) && (
        <section className="note-card" aria-label="已移除条目的未保存修改">
          <h3>该条目已从当前版本移除</h3>
          <p>草稿仍保留，可复制；无法重试已删除的条目。</p>
          <textarea aria-label="保留的未保存草稿" value={draft} readOnly rows={4} />
          <button type="button" className="secondary-button" onClick={() => void copyMine()}>复制草稿</button>
        </section>
      )}
      {editError && <p className="note-error" role="alert">{editError}</p>}
      {conflict && (
        <div className="modal-backdrop">
          <div className="modal" ref={conflictDialogRef} role="dialog" aria-modal="true" aria-labelledby="conflict-title" tabIndex={-1}>
            <h2 id="conflict-title">版本冲突</h2>
            <div className="conflict-columns">
              <section>
                <h3>基准版本（{conflict.baseVersion}）</h3>
                <DiffView before={conflict.baseSummary} after={conflict.submitted} beforeLabel="基准摘要" afterLabel="已提交草稿" />
              </section>
              <section>
                <h3>当前版本</h3>
                {conflict.currentSummary === null ? <p>该条目已被其他版本移除</p> : <DiffView before={conflict.baseSummary} after={conflict.currentSummary} beforeLabel="基准摘要" afterLabel="当前摘要" />}
              </section>
            </div>
            {conflict.currentVersion && <p className="conflict-version">当前版本：{conflict.currentVersion}</p>}
            {copied && <p className="conflict-copied" role="status">已复制</p>}
            <div className="modal-actions">
              <button type="button" onClick={() => void copyMine()}>保留我的修改并复制</button>
              <button type="button" className="secondary-button" onClick={retryOnCurrent} disabled={!conflict.currentVersion || conflict.currentSummary === null}>基于当前版本重试（保留草稿）</button>
              <button ref={conflictCloseRef} type="button" className="secondary-button" onClick={() => setConflict(null)}>关闭</button>
            </div>
          </div>
        </div>
      )}
    </div>
  )
}

function HistoryTab({ conversationId, current }: { conversationId: string; current: NoteView[] }) {
  const { data, loading, error, reload } = useApi((signal) => getAnnotationHistory(conversationId, signal), [conversationId])
  const revisions: AnnotationSnapshot[] = data?.revisions ?? []
  const [selected, setSelected] = useState<number | null>(null)
  const activeIndex = selected ?? revisions.length - 1
  const snapshot = revisions[activeIndex]
  const currentById = new Map(current.map((note) => [note.note_id, note]))

  return (
    <div className="tab-panel">
      <StateView loading={loading} error={error} onRetry={reload} empty={revisions.length === 0} emptyMessage="没有历史版本">
        <div className="history-layout">
          <ol className="history-list">
            {revisions.map((revision, index) => (
              <li key={`${revision.version}-${index}`}>
                <button
                  type="button"
                  className={`history-item ${index === activeIndex ? 'history-active' : ''}`.trim()}
                  aria-current={index === activeIndex}
                  onClick={() => setSelected(index)}
                >
                  <span>第 {index + 1} 版</span>
                  <span className="history-version">{revision.version}</span>
                  <span className={revision.current ? 'history-badge-current' : 'history-badge-old'}>
                    {revision.current ? '当前' : '已被更新'}
                  </span>
                </button>
              </li>
            ))}
          </ol>
          {snapshot && (
            <section className="history-detail" aria-label={`第 ${activeIndex + 1} 版内容`}>
              <h3>第 {activeIndex + 1} 版（{snapshot.version}）</h3>
              {snapshot.basis && <p className="history-basis">{snapshot.basis}</p>}
              {(snapshot.annotations.notes ?? []).length === 0 && <p>该版本没有归纳条目</p>}
              {(snapshot.annotations.notes ?? []).map((note) => {
                const live = currentById.get(note.id)
                return (
                  <article className="history-note" key={note.id}>
                    <header>
                      <span className="note-kind">{noteKindLabel(note.kind)}</span>
                      {!live && <span className="history-badge-old">当前版本已移除</span>}
                    </header>
                    {live && live.summary !== note.summary ? (
                      <DiffView before={note.summary} after={live.summary} beforeLabel={`第 ${activeIndex + 1} 版`} afterLabel="当前版本" />
                    ) : (
                      <p>{note.summary}</p>
                    )}
                  </article>
                )
              })}
              {(snapshot.annotations.notes ?? [])
                .filter((note) => !currentById.has(note.id))
                .map((note) => (
                  <p className="history-removed" key={`removed-${note.id}`}>移除：{note.summary}</p>
                ))}
              {current
                .filter((note) => !(snapshot.annotations.notes ?? []).some((entry) => entry.id === note.note_id))
                .map((note) => (
                  <p className="history-added" key={`added-${note.note_id}`}>新增：{note.summary}</p>
                ))}
            </section>
          )}
        </div>
      </StateView>
    </div>
  )
}

export function DetailPage() {
  const { conversationId = '' } = useParams()
  const [searchParams] = useSearchParams()
  const itemId = searchParams.get('item') || ''
  const { data: linkedItem } = useApi((signal) => itemId ? getItem(itemId, signal) : Promise.resolve(undefined as never), [itemId])
  const { data, loading, error, reload } = useApi((signal) => getConversation(conversationId, signal), [conversationId])
  const [tab, setTab] = useState<'transcript' | 'notes' | 'history'>('transcript')
  const [evidenceTurn, setEvidenceTurn] = useState<string | null>(null)
  const [highlightedItem, setHighlightedItem] = useState('')
  const tabRefs = useRef<Record<string, HTMLButtonElement | null>>({})

  useEffect(() => {
    function onClose() {
      setEvidenceTurn(null)
    }
    window.addEventListener(CLOSE_EVENT, onClose)
    return () => window.removeEventListener(CLOSE_EVENT, onClose)
  }, [])

  useEffect(() => {
    setTab('transcript')
    setEvidenceTurn(null)
    setHighlightedItem('')
  }, [conversationId])

  useEffect(() => {
    if (!linkedItem || linkedItem.item.conversation_id !== conversationId) return
    const item = linkedItem.item
    if (item.kind === 'note' || item.annotation_version !== undefined) {
      setTab('notes')
      setHighlightedItem(item.id)
    } else {
      setTab('transcript')
      setEvidenceTurn(item.id)
    }
  }, [linkedItem, conversationId])

  const tabs: { id: 'transcript' | 'notes' | 'history'; label: string }[] = [
    { id: 'transcript', label: '转写' },
    { id: 'notes', label: 'AI 归纳' },
    { id: 'history', label: '历史' },
  ]

  return (
    <section className="page detail-page">
      {data ? (
        <>
          {loading && <div className="state-view" aria-busy="true">刷新中…</div>}
          {error && <div className="state-view error-state" role="alert">刷新失败；仍显示上次加载的内容。<button type="button" onClick={reload}>重试</button></div>}
          <>
            <header className="detail-header">
              <h1>{data.title || '未命名会话'}</h1>
              <p className="detail-meta">
                <time dateTime={data.recording_started_at || undefined}>{formatDate(data.recording_started_at)}</time>
                <StatusBadge status={data.processing.analysis_status} />
                <span className="annotation-version">
                  {data.annotation_version === '' ? '尚未生成 AI 归纳' : `归纳版本 ${data.annotation_version}`}
                </span>
              </p>
              <details className="diagnostics">
                <summary>诊断（实验）</summary>
                <div className="diagnostics-body">
                  <p>语义索引：{INDEXING_LABELS[data.processing.indexing_status] || data.processing.indexing_status}</p>
                  <p>未分类轮次：{data.processing.unclassified_turns}</p>
                  <p>环境音轮次：{data.processing.ambient_turns}</p>
                  <p>身份未确认轮次：{data.processing.identity_unconfirmed_turns}</p>
                </div>
              </details>
            </header>
            <div className="tabs" role="tablist" aria-label="详情视图">
              {tabs.map((entry, index) => (
                <button
                  type="button"
                  role="tab"
                  id={`tab-${entry.id}`}
                  aria-selected={tab === entry.id}
                  aria-controls={`panel-${entry.id}`}
                  tabIndex={tab === entry.id ? 0 : -1}
                  ref={(element) => { tabRefs.current[entry.id] = element }}
                  className={`tab ${tab === entry.id ? 'tab-active' : ''}`.trim()}
                  onClick={() => setTab(entry.id)}
                  onKeyDown={(event) => {
                    const direction = event.key === 'ArrowRight' ? 1 : event.key === 'ArrowLeft' ? -1 : 0
                    if (!direction && event.key !== 'Home' && event.key !== 'End') return
                    event.preventDefault()
                    const next = event.key === 'Home' ? 0 : event.key === 'End' ? tabs.length - 1 : (index + direction + tabs.length) % tabs.length
                    setTab(tabs[next].id)
                    tabRefs.current[tabs[next].id]?.focus()
                  }}
                  key={entry.id}
                >
                  {entry.label}
                </button>
              ))}
            </div>
            <div role="tabpanel" id="panel-transcript" aria-labelledby="tab-transcript" hidden={tab !== 'transcript'}>
              <TranscriptTab conversation={data} onOpenEvidence={setEvidenceTurn} />
            </div>
            <div role="tabpanel" id="panel-notes" aria-labelledby="tab-notes" hidden={tab !== 'notes'}>
              <NotesTab key={data.conversation_id} conversation={data} onChanged={reload} onOpenEvidence={setEvidenceTurn} highlightedItem={highlightedItem} />
            </div>
            <div role="tabpanel" id="panel-history" aria-labelledby="tab-history" hidden={tab !== 'history'}>
              <HistoryTab key={`${data.conversation_id}:${data.annotation_version}`} conversationId={data.conversation_id} current={data.notes} />
            </div>
            {evidenceTurn && <EvidencePanel turnId={evidenceTurn} onClose={() => setEvidenceTurn(null)} />}
          </>
        </>
      ) : (
        <StateView loading={loading} error={error} onRetry={reload} />
      )}
    </section>
  )
}
