import { useEffect, useState } from 'react'
import { ApiError } from '../api/client'
import { createHighlight, deleteHighlight, listHighlights, updateHighlight, type Highlight, type HighlightCreate } from '../api/highlights'
import { getTranscriptSource } from '../api/client'
import { useAudio } from './AudioBar'

const labels = { important: '重要', follow_up: '待跟进', question: '问题' } as const

export function HighlightPanel({ recordingId, conversationId, turns, onEvidence }: {
  recordingId: string; conversationId: string; turns?: { id: string; global_start_ms: number | null; global_end_ms: number | null; text: string }[]
  onEvidence: (turnId: string) => void
}) {
  const [items, setItems] = useState<Highlight[]>([])
  const [error, setError] = useState('')
  const [drafts, setDrafts] = useState<Record<string, string>>({})
  const [tagDrafts, setTagDrafts] = useState<Record<string, Highlight['tag']>>({})
  const [busy, setBusy] = useState(false)
  const audio = useAudio()
  async function reload() { try { const r = await listHighlights(recordingId); setItems(r.highlights); setError('') } catch (e) { setError(e instanceof Error ? e.message : '无法读取重点') } }
  useEffect(() => { void reload() }, [recordingId])
  async function add(anchor: HighlightCreate) {
    setBusy(true); setError('')
    try { await createHighlight(recordingId, anchor); await reload() } catch (e) { setError(e instanceof Error ? e.message : '保存失败') } finally { setBusy(false) }
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
  async function save(item: Highlight) {
    const note_text = drafts[item.id] ?? item.note_text
    const tag = tagDrafts[item.id] ?? item.tag
    try { await updateHighlight(item.id, item.version, { note_text, tag }); await reload() }
    catch (e) { setError(e instanceof ApiError && e.status === 409 ? '此重点已被其他修改更新。草稿仍保留；请重新加载后再保存。' : e instanceof Error ? e.message : '保存失败') }
  }
  async function relink(item: Highlight, turnId: string) {
    try { const src = await getTranscriptSource(turnId); await updateHighlight(item.id, item.version, { relink: { turn_id: turnId, source_attachment_id: src.source_attachment_id, transcript_revision: src.result.source.transcript_revision } }); await reload() }
    catch (e) { setError(e instanceof Error ? e.message : '重新关联失败') }
  }
  return <section className="highlight-panel" aria-label="记忆重点">
    <header><h3>记忆重点</h3><div className="turn-actions">
      {turns?.map((turn, index) => <button type="button" className="secondary-button" key={turn.id} onClick={() => void addTurn(turn.id)} disabled={busy} title={turn.text}>{`从此轮次标记 ${index + 1}${turn.global_start_ms !== null ? ` · ${Math.floor(turn.global_start_ms / 1000)}秒` : ''}：${turn.text.slice(0, 48) || '无转写文本'}`}</button>)}
      {audio.turnId && audio.actualPositionMs !== null && <button type="button" className="secondary-button" onClick={() => { const t = turns?.find((x) => x.id === audio.turnId); if (!t) return; void getTranscriptSource(t.id).then((s) => add({ request_id: crypto.randomUUID(), anchor_kind: 'point', start_ms: audio.actualPositionMs, end_ms: audio.actualPositionMs, turn_id: t.id, source_attachment_id: s.source_attachment_id, transcript_revision: s.result.source.transcript_revision, tag: 'important', note_text: '' })).catch((e) => setError(e.message)) }}>标记当前播放位置</button>}
    </div></header>
    {error && <p role="alert" className="note-error">{error} {error.includes('草稿仍保留') && <button type="button" className="secondary-button" onClick={() => void reload()}>重新加载版本</button>}</p>}
    {items.length === 0 ? <p>还没有标记重点</p> : items.map((item) => <article className="note-card" key={item.id}>
      <header><strong>{labels[item.tag]}</strong><span>{item.state === 'current' ? '来源有效' : '需要重新关联'}</span></header>
      {item.quote_text && <blockquote>{item.quote_text}</blockquote>}
      <label>备注<textarea value={drafts[item.id] ?? item.note_text} onChange={(e) => setDrafts((d) => ({ ...d, [item.id]: e.target.value }))} /></label>
      <label>标签<select value={tagDrafts[item.id] ?? item.tag} onChange={(e) => setTagDrafts((d) => ({ ...d, [item.id]: e.target.value as Highlight['tag'] }))}>{Object.entries(labels).map(([key, label]) => <option value={key} key={key}>{label}</option>)}</select></label>
      <div className="turn-actions">
        <button type="button" className="secondary-button" onClick={() => void save(item)}>保存</button>
        {item.turn_id && <button type="button" className="link-button" onClick={() => onEvidence(item.turn_id!)}>查看来源</button>}
        {item.state === 'needs_relink' && <label>重新关联到当前轮次<select defaultValue="" onChange={(e) => e.target.value && void relink(item, e.target.value)}><option value="">选择轮次</option>{turns?.map((t) => <option value={t.id} key={t.id}>{t.id}</option>)}</select></label>}
        <button type="button" className="secondary-button" onClick={() => void deleteHighlight(item.id, item.version).then(reload).catch((e) => setError(e.message))}>删除</button>
        <audio controls preload="none" src={`/api/highlights/${encodeURIComponent(item.id)}/audio`} aria-label="播放重点音频" />
      </div>
    </article>)}
  </section>
}
