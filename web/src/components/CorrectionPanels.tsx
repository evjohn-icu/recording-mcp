import { useEffect, useState } from 'react'
import { ApiError, getTranscriptSource, getTranscriptSegment, correctTranscript, getSpeakerBindings, proposeSpeaker, reviewSpeakerBinding } from '../api/client'
import type { TranscriptSource, SpeakerBinding } from '../api/types'
import { DiffView } from './DiffView'

function message(error: unknown): string {
  return error instanceof ApiError ? error.message : '无法连接本地服务'
}

export function CorrectionPanel({ turnId, onChanged, onSuccess }: { turnId: string; onChanged: () => void; onSuccess?: () => void }) {
  const [open, setOpen] = useState(false)
  const [source, setSource] = useState<TranscriptSource | null>(null)
  const [texts, setTexts] = useState<Record<string, string>>({})
  const [revision, setRevision] = useState('')
  const [loading, setLoading] = useState(false)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState('')
  const [conflict, setConflict] = useState<TranscriptSource['result'] | null>(null)
  const [success, setSuccess] = useState(false)
  const [rebaseNeedsReview, setRebaseNeedsReview] = useState(false)
  const [unmatchedDrafts, setUnmatchedDrafts] = useState<Array<{ originalIndex: number; text: string }>>([])

  async function load() {
    setLoading(true); setError(''); setSuccess(false); setRebaseNeedsReview(false); setUnmatchedDrafts([])
    try {
      const next = await getTranscriptSource(turnId)
      setSource(next)
      setTexts(Object.fromEntries(next.result.turns.map((turn) => [turn.id, turn.text])))
      setRevision(next.result.source.transcript_revision)
    } catch (e) { setError(message(e)) } finally { setLoading(false) }
  }
  useEffect(() => { if (open && !source) void load() }, [open])

  async function submit() {
    if (!source || rebaseNeedsReview) return
    setSaving(true); setError(''); setConflict(null)
    const newRevision = `web-${crypto.randomUUID()}`
    try {
      await correctTranscript(source.conversation_id, source.recording_id, source.segment_id, source.result.source.transcript_revision, newRevision, texts)
      setSuccess(true); setSource(null); setConflict(null); setRebaseNeedsReview(false); onSuccess?.(); onChanged()
    } catch (e) {
      if (e instanceof ApiError && e.status === 409) {
        try { setConflict(await getTranscriptSegment(source.conversation_id, source.recording_id, source.segment_id)) }
        catch (refreshError) { setError(`版本冲突；草稿已保留。刷新当前版本失败：${message(refreshError)}`) }
      } else setError(message(e))
    } finally { setSaving(false) }
  }
  return <section className="review-panel">
    <button type="button" className="link-button" aria-expanded={open} onClick={() => setOpen(!open)}>纠正转写</button>
    {open && <div className="review-content">
      {loading && <p aria-busy="true">正在加载原始转写…</p>}
      {error && <p role="alert" className="note-error">{error}</p>}
      {success && <p role="status">已提交；后续处理已排队，尚未完成。</p>}
      {rebaseNeedsReview && <div className="tab-note" role="status">
        <p>草稿已按原始轮次编号合并到服务器当前版本。请检查下方完整片段后再确认提交。</p>
        {unmatchedDrafts.map((draft) => <p key={draft.originalIndex}>原轮次 {draft.originalIndex} 已不在当前版本中；未提交的草稿仍保留供你检查：<DiffView before="（当前版本已移除此轮）" after={draft.text} beforeLabel="服务器当前版本" afterLabel="保留的草稿" /></p>)}
        <button type="button" onClick={() => setRebaseNeedsReview(false)}>我已检查合并文本，继续</button>
      </div>}
      {source && <>
        <p className="tab-note">当前修订：<code>{source.result.source.transcript_revision}</code>。提交完整片段修订。</p>
        {source.result.turns.map((item) => <label className="correction-field" key={item.id}>
          <span>轮次 {item.original_index} · {item.speaker_scoped_id || '说话人未知'}</span>
          <textarea rows={3} value={texts[item.id] ?? ''} onChange={(event) => setTexts({ ...texts, [item.id]: event.target.value })} />
        </label>)}
        <button type="button" disabled={saving || loading || rebaseNeedsReview} onClick={() => void submit()}>{saving ? '提交中…' : '提交修订'}</button>
      </>}
      {conflict && source && <div className="conflict-panel" role="alert">
        <h4>修订冲突</h4><p>服务器已更新到 <code>{conflict.source.transcript_revision}</code>。你的草稿仍保留，未自动覆盖。</p>
        {conflict.turns.map((item) => {
          const old = source.result.turns.find((turn) => turn.original_index === item.original_index)?.text ?? ''
          const mine = Object.entries(texts).find(([id]) => source.result.turns.find((turn) => turn.id === id)?.original_index === item.original_index)?.[1] ?? old
          return <div key={item.id}><h5>轮次 {item.original_index}：当前版本与草稿</h5><DiffView before={item.text} after={mine} beforeLabel="服务器当前版本" afterLabel="保留的草稿" />{old === item.text && <small>基准文本未变</small>}</div>
        })}
        <button type="button" disabled={saving} onClick={() => {
          const oldByIndex = new Map(source.result.turns.map((turn) => [turn.original_index, { base: turn.text, draft: texts[turn.id] ?? turn.text }]))
          const currentIndices = new Set(conflict.turns.map((turn) => turn.original_index))
          // Preserve edits; unchanged fields adopt the current server text.
          // The full merged segment still requires explicit user review.
          const merged = Object.fromEntries(conflict.turns.map((turn) => {
            const old = oldByIndex.get(turn.original_index)
            return [turn.id, old && old.draft !== old.base ? old.draft : turn.text]
          }))
          const orphaned = source.result.turns
            .filter((turn) => !currentIndices.has(turn.original_index))
            .map((turn) => ({ originalIndex: turn.original_index, text: texts[turn.id] ?? turn.text }))
          setSource({ ...source, result: conflict }); setTexts(merged); setUnmatchedDrafts(orphaned)
          setConflict(null); setError(''); setRebaseNeedsReview(true)
        }}>以服务器版本为新基准（合并并检查草稿）</button>
      </div>}
    </div>}
  </section>
}

export function SpeakerReviewPanel({ recordingId, turnId, speakerId, onChanged }: { recordingId: string; turnId: string; speakerId: string; onChanged: () => void }) {
  const [open, setOpen] = useState(false)
  const [bindings, setBindings] = useState<SpeakerBinding[]>([])
  const [revision, setRevision] = useState('')
  const [personIds, setPersonIds] = useState<Record<string, string>>({})
  const [note, setNote] = useState('')
  const [loading, setLoading] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  async function load() {
    setLoading(true); setError('')
    try {
      const [list, source] = await Promise.all([getSpeakerBindings(recordingId, speakerId), getTranscriptSource(turnId)])
      setBindings(list.bindings); setRevision(source.result.source.transcript_revision)
      setPersonIds(Object.fromEntries(list.bindings.map((binding) => [binding.id, binding.person_id || ''])))
    } catch (e) { setError(message(e)) } finally { setLoading(false) }
  }
  useEffect(() => { if (open) void load() }, [open, recordingId, speakerId])
  async function act(id: string, action: 'confirm' | 'reject' | 'revoke') {
    setBusy(true); setError('')
    try { await reviewSpeakerBinding(id, action, personIds[id] || '', note); await load(); onChanged() }
    catch (e) { setError(message(e)) } finally { setBusy(false) }
  }
  async function propose() {
    setBusy(true); setError('')
    try { await proposeSpeaker(recordingId, speakerId, revision, [turnId]); await load(); onChanged() }
    catch (e) { setError(message(e)) } finally { setBusy(false) }
  }
  return <section className="review-panel">
    <button type="button" className="link-button" aria-expanded={open} onClick={() => setOpen(!open)}>说话人审核</button>
    {open && <div className="review-content">
      {loading && <p aria-busy="true">正在加载绑定…</p>}{error && <p role="alert" className="note-error">{error}</p>}
      {revision && <p>当前转写修订：<code>{revision}</code></p>}
      {bindings.map((binding) => <article className="speaker-binding" key={binding.id}>
        <p><strong>{binding.status}</strong> · 版本 {binding.version} · 修订 <code>{binding.transcript_revision || '未知'}</code></p>
        <p>证据轮次：{binding.evidence_turn_ids.join(', ') || '无'}</p>
        {binding.status === 'proposed' && <>
          <label>明确的 Person ID<input type="text" value={personIds[binding.id] || ''} onChange={(event) => setPersonIds({ ...personIds, [binding.id]: event.target.value })} placeholder="输入已有 person_id；不填入真实姓名" /></label>
          <button type="button" disabled={busy || !personIds[binding.id]?.trim()} onClick={() => void act(binding.id, 'confirm')}>确认身份</button>
          <button type="button" className="secondary-button" disabled={busy} onClick={() => void act(binding.id, 'reject')}>拒绝</button>
        </>}
        {binding.status === 'confirmed' && <><p>已确认 Person ID：{binding.person_id}</p><button type="button" className="secondary-button" disabled={busy} onClick={() => void act(binding.id, 'revoke')}>撤销确认</button></>}
      </article>)}
      <label>审核备注（可选）<input type="text" value={note} onChange={(event) => setNote(event.target.value)} /></label>
      <button type="button" disabled={busy || loading || !revision} onClick={() => void propose()}>{busy ? '处理中…' : '基于此轮提出说话人绑定'}</button>
    </div>}
  </section>
}

export type { TranscriptSource }
