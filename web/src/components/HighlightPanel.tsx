import { useEffect, useState } from 'react'
import { ApiError } from '../api/client'
import { createHighlight, deleteHighlight, getHighlight, listHighlights, updateHighlight, type Highlight, type HighlightCreate } from '../api/highlights'
import { getTranscriptSource } from '../api/client'
import { useAudio } from './AudioBar'

const labels = { important: '重要', follow_up: '待跟进', question: '问题' } as const
const allTags = ['all', ...Object.keys(labels)] as const

type TagFilter = typeof allTags[number]
function formatRecordingTime(ms: number | null): string | null {
  if (ms === null || !Number.isFinite(ms)) return null
  const seconds = Math.floor(Math.max(0, ms) / 1000)
  const hours = Math.floor(seconds / 3600)
  const minutes = Math.floor((seconds % 3600) / 60)
  const remainder = seconds % 60
  return hours > 0 ? `${hours}:${String(minutes).padStart(2, '0')}:${String(remainder).padStart(2, '0')}` : `${minutes}:${String(remainder).padStart(2, '0')}`
}

export function HighlightPanel({ recordingId, conversationId, turns, onEvidence }: {
  recordingId: string; conversationId: string; turns?: { id: string; global_start_ms: number | null; global_end_ms: number | null; text: string }[]
  onEvidence: (turnId: string) => void
}) {
  const [items, setItems] = useState<Highlight[]>([])
  const [error, setError] = useState('')
  const [drafts, setDrafts] = useState<Record<string, string>>({})
  const [tagDrafts, setTagDrafts] = useState<Record<string, Highlight['tag']>>({})
  const [conflicts, setConflicts] = useState<Record<string, Highlight>>({})
  const [filter, setFilter] = useState<TagFilter>('all')
  const [busy, setBusy] = useState(false)
  const [busyItem, setBusyItem] = useState<string | null>(null)
  const [notice, setNotice] = useState('')
  const audio = useAudio()
  async function reload() {
    try { const r = await listHighlights(recordingId); setItems(r.highlights); setError('') }
    catch (e) { setError(e instanceof Error ? e.message : '无法读取重点') }
  }
  useEffect(() => { void reload() }, [recordingId])
  async function add(anchor: HighlightCreate) {
    setBusy(true); setError(''); setNotice('')
    try { await createHighlight(recordingId, anchor); await reload(); setNotice('重点已保存') }
    catch (e) { setError(e instanceof Error ? e.message : '保存失败') }
    finally { setBusy(false) }
  }
  async function addTurn(turnId: string) {
    try {
      const source = await getTranscriptSource(turnId)
      const turn = turns?.find((t) => t.id === turnId)
      await add({ request_id: crypto.randomUUID(), anchor_kind: 'turn', turn_id: turnId,
        start_ms: turn?.global_start_ms, end_ms: turn?.global_end_ms,
        source_attachment_id: source.source_attachment_id, transcript_revision: source.result.source.transcript_revision,
        tag: 'important', note_text: '' })
    } catch (e) { setError(e instanceof Error ? e.message : '无法读取转写来源') }
  }
  async function save(item: Highlight, expectedVersion = item.version) {
    const note_text = drafts[item.id] ?? item.note_text
    const tag = tagDrafts[item.id] ?? item.tag
    setBusyItem(item.id); setError(''); setNotice('')
    try {
      const saved = await updateHighlight(item.id, expectedVersion, { note_text, tag })
      setItems((current) => current.map((value) => value.id === saved.id ? saved : value))
      setConflicts((current) => { const next = { ...current }; delete next[item.id]; return next })
      setDrafts((current) => { const next = { ...current }; delete next[item.id]; return next })
      setTagDrafts((current) => { const next = { ...current }; delete next[item.id]; return next })
      setNotice('更改已保存')
    } catch (e) {
      if (e instanceof ApiError && e.status === 409) {
        try { const latest = await getHighlight(item.id); setConflicts((current) => ({ ...current, [item.id]: latest })) }
        catch { setError('版本冲突，无法读取服务器最新版本；本地草稿仍保留。') }
      } else setError(e instanceof Error ? e.message : '保存失败')
    } finally { setBusyItem(null) }
  }
  async function relink(item: Highlight, turnId: string) {
    setBusyItem(item.id); setError(''); setNotice('')
    try { const src = await getTranscriptSource(turnId); const saved = await updateHighlight(item.id, item.version, { relink: { turn_id: turnId, source_attachment_id: src.source_attachment_id, transcript_revision: src.result.source.transcript_revision } }); setItems((current) => current.map((value) => value.id === saved.id ? saved : value)); setNotice('来源已重新关联') }
    catch (e) { setError(e instanceof Error ? e.message : '重新关联失败') }
    finally { setBusyItem(null) }
  }
  async function remove(item: Highlight) {
    setBusyItem(item.id); setError(''); setNotice('')
    try { await deleteHighlight(item.id, item.version); setItems((current) => current.filter((value) => value.id !== item.id)); setNotice('重点已删除') }
    catch (e) { setError(e instanceof ApiError && e.status === 409 ? '删除失败：此重点已更新，请重新加载后再试。' : e instanceof Error ? e.message : '删除失败') }
    finally { setBusyItem(null) }
  }
  const shown = filter === 'all' ? items : items.filter((item) => item.tag === filter)
  return <section className="highlight-panel" aria-label="记忆重点">
    <header><h3>记忆重点 <span aria-live="polite">({shown.length}/{items.length})</span></h3><div className="turn-actions">
      {turns?.map((turn, index) => <button type="button" className="secondary-button" key={turn.id} onClick={() => void addTurn(turn.id)} disabled={busy} title={turn.text}>{`从此轮次标记 ${index + 1}${formatRecordingTime(turn.global_start_ms) ? ` · ${formatRecordingTime(turn.global_start_ms)}` : ''}：${turn.text.slice(0, 48) || '无转写文本'}`}</button>)}
      {audio.turnId && audio.actualPositionMs !== null && <button type="button" className="secondary-button" disabled={busy} onClick={() => { const t = turns?.find((x) => x.id === audio.turnId); if (!t) return; void getTranscriptSource(t.id).then((s) => add({ request_id: crypto.randomUUID(), anchor_kind: 'point', start_ms: audio.actualPositionMs, end_ms: audio.actualPositionMs, turn_id: t.id, source_attachment_id: s.source_attachment_id, transcript_revision: s.result.source.transcript_revision, tag: 'important', note_text: '' })).catch((e) => setError(e.message)) }}>标记当前播放位置</button>}
    </div></header>
    {error && <p role="alert" className="note-error">{error}<button type="button" className="secondary-button" onClick={() => void reload()}>重新加载</button></p>}
    {notice && <p role="status">{notice}</p>}
    <label>按标签筛选
      <select aria-label="按标签筛选重点" value={filter} onChange={(e) => setFilter(e.target.value as TagFilter)}>
        <option value="all">全部标签</option>{Object.entries(labels).map(([key, label]) => <option value={key} key={key}>{label}</option>)}
      </select>
    </label>
    {items.length === 0 ? <p>还没有标记重点</p> : shown.length === 0 ? <p role="status">没有符合此标签的重点。可切换标签筛选查看其他重点。</p> : shown.map((item) => {
      const start = formatRecordingTime(item.start_ms), end = formatRecordingTime(item.end_ms)
      const time = start && end && start !== end ? `${start}–${end}` : start || end
      const conflict = conflicts[item.id]
      const itemBusy = busyItem === item.id
      return <article className="note-card" key={item.id}>
        <header><strong>{labels[item.tag]}</strong><span>{item.state === 'current' ? '来源有效' : '需要重新关联'}</span></header>
        {time && <p>录音时间：{time}{item.anchor_kind === 'point' ? '（播放位置）' : item.anchor_kind === 'range' ? '（片段范围）' : '（轮次）'}</p>}
        {item.quote_text && <blockquote>{item.quote_text}</blockquote>}
        <label>备注<textarea disabled={itemBusy} value={drafts[item.id] ?? item.note_text} onChange={(e) => setDrafts((d) => ({ ...d, [item.id]: e.target.value }))} /></label>
        <label>标签<select disabled={itemBusy} value={tagDrafts[item.id] ?? item.tag} onChange={(e) => setTagDrafts((d) => ({ ...d, [item.id]: e.target.value as Highlight['tag'] }))}>{Object.entries(labels).map(([key, label]) => <option value={key} key={key}>{label}</option>)}</select></label>
        {conflict && <div role="alert" className="note-error">
          <p>服务器版本 {conflict.version}：{labels[conflict.tag]}；备注：{conflict.note_text || '（空）'}</p>
          {conflict.quote_text && <blockquote>{conflict.quote_text}</blockquote>}
          <p>你的本地草稿仍保留在上方。选择“以服务器新版为基础重试”后，会将当前草稿显式保存到版本 {conflict.version}。</p>
          <button type="button" className="secondary-button" disabled={busyItem !== null} onClick={() => { setItems((current) => current.map((value) => value.id === item.id ? conflict : value)); setConflicts((current) => { const next = { ...current }; delete next[item.id]; return next }) }}>重新加载版本</button>
          <button type="button" className="secondary-button" disabled={busyItem !== null} onClick={() => void save(conflict, conflict.version)}>以服务器新版为基础重试</button>
        </div>}
        <div className="turn-actions">
          <button type="button" className="secondary-button" disabled={busyItem !== null} onClick={() => void save(item)}>{itemBusy ? '保存中…' : '保存'}</button>
          {item.turn_id && <button type="button" className="link-button" onClick={() => onEvidence(item.turn_id!)}>查看来源</button>}
          {item.state === 'needs_relink' && <label>重新关联到当前轮次<select disabled={busyItem !== null} defaultValue="" onChange={(e) => e.target.value && void relink(item, e.target.value)}><option value="">选择轮次</option>{turns?.map((t) => <option value={t.id} key={t.id}>{t.id}</option>)}</select></label>}
          <button type="button" className="secondary-button" disabled={busyItem !== null} onClick={() => void remove(item)}>{itemBusy ? '处理中…' : '删除'}</button>
          <audio controls preload="none" src={`/api/highlights/${encodeURIComponent(item.id)}/audio`} aria-label="播放重点音频" />
        </div>
      </article>
    })}
  </section>
}
