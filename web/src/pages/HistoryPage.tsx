import { useMemo, useState } from 'react'
import { ApiError, correctFact, getTimeHistory, getTimeState } from '../api/client'
import type { TemporalEvent } from '../api/types'
import { useApi } from '../hooks/useApi'
import { StateView } from '../components/StateView'

const dateInput = (value: Date) => new Date(value.getTime() - value.getTimezoneOffset() * 60000).toISOString().slice(0, 16)
const asISO = (value: string) => value ? new Date(value).toISOString() : undefined
const prettyTime = (value?: string) => value ? new Date(value).toLocaleString() : '记录时间未知'
function refs(event: TemporalEvent): { turn_id: string; revision: string }[] {
  const values = [...(event.structured_data?.evidence as string[] || []), ...(event.structured_data?.references as string[] || [])]
  return values.flatMap((value) => {
    const i = value.lastIndexOf('@')
    return i > 0 && i < value.length - 1 ? [{ turn_id: value.slice(0, i), revision: value.slice(i + 1) }] : []
  })
}
function CorrectionForm({ event, head, evidence, onDone }: { event: TemporalEvent; head: string; evidence: { turn_id: string; revision: string }[]; onDone: () => void }) {
  const [content, setContent] = useState(event.content)
  const [validAt, setValidAt] = useState('')
  const [unknown, setUnknown] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  async function submit(e: React.FormEvent) {
    e.preventDefault()
    if (!unknown && !validAt) { setError('请填写变化适用时间，或明确选择“适用时间未知”。'); return }
    setBusy(true); setError('')
    try {
      const target = String(event.structured_data?.target_event_id || event.id)
      await correctFact({ id: crypto.randomUUID(), target_id: target, expected_head: head, known_at: new Date().toISOString(), source: 'local-user', content, ...(validAt ? { valid_at: asISO(validAt) } : {}), evidence })
      onDone()
    } catch (e) {
      setError(e instanceof ApiError && e.status === 409 ? `记录冲突（409）：${e.message}。您的输入已保留；请刷新后核对最新版本。` : e instanceof Error ? e.message : '提交失败')
    } finally { setBusy(false) }
  }
  return <form className="temporal-form" onSubmit={submit}>
    <label>报告的新陈述<textarea value={content} onChange={(e) => setContent(e.target.value)} required /></label>
    <label>变化适用时间（本地时间）<input type="datetime-local" value={validAt} disabled={unknown} onChange={(e) => setValidAt(e.target.value)} /></label>
    <label className="toggle-label"><input type="checkbox" checked={unknown} onChange={(e) => { setUnknown(e.target.checked); if (e.target.checked) setValidAt('') }} /> 适用时间未知</label>
    {error && <p role="alert" className="note-error">{error}</p>}
    <div className="turn-actions"><button disabled={busy}>{busy ? '提交中…' : '追加报告（不覆盖原记录）'}</button><button type="button" className="secondary-button" onClick={onDone}>取消</button></div>
  </form>
}

export function HistoryPage() {
  const history = useApi((signal) => getTimeHistory(signal), [])
  const [validAt, setValidAt] = useState(() => dateInput(new Date()))
  const [knownAt, setKnownAt] = useState(() => dateInput(new Date()))
  const [refresh, setRefresh] = useState(0)
  const [editing, setEditing] = useState<string | null>(null)
  const refreshHistory = () => { setRefresh((v) => v + 1); history.reload() }
  const stateQuery = useApi((signal) => getTimeState(asISO(validAt)!, asISO(knownAt)!, signal), [validAt, knownAt, refresh])
  const events = history.data?.events ?? []
  const heads = useMemo(() => {
    const result: Record<string, string> = {}
    for (const event of events) {
      const target = String(event.structured_data?.target_event_id || '')
      if (!target) continue
      const previous = result[target]
      const old = events.find((e) => e.id === previous)
      if (!old || new Date(event.recorded_at || event.timestamp) >= new Date(old.recorded_at || old.timestamp)) result[target] = event.id
    }
    return result
  }, [events])
  return <section className="page">
    <header className="page-header"><h1>事实变化记录</h1><p className="page-subtitle">记录的是来源中的报告陈述，不代表已确认的事实。适用时间与系统获知时间分别显示；缺失的记录时间不会推测。</p></header>
    <section className="filters temporal-cutoffs" aria-label="双时间状态查询">
      <label>适用时间 valid_at<input type="datetime-local" value={validAt} onChange={(e) => setValidAt(e.target.value)} /></label>
      <label>获知截止 known_at<input type="datetime-local" value={knownAt} onChange={(e) => setKnownAt(e.target.value)} /></label>
    </section>
    <h2>该时间点的状态</h2>
    <StateView loading={stateQuery.loading} error={stateQuery.error} onRetry={stateQuery.reload} empty={!stateQuery.loading && !stateQuery.error && !(stateQuery.data?.states.length)} emptyMessage="该时间范围内没有可投影的记录">
      <div className="temporal-cards">{stateQuery.data?.states.map((state) => <article className="note-card" key={state.id}><strong>{state.dimension}</strong><p className="note-summary">{state.summary}</p><small>状态：{state.status} · 更新：{prettyTime(state.updated_at)}</small></article>)}</div>
    </StateView>
    <h2>历史报告</h2>
    <StateView loading={history.loading} error={history.error} onRetry={history.reload} empty={!history.loading && !history.error && events.length === 0} emptyMessage="还没有事实变化报告">
      <div className="temporal-cards">{events.map((event) => {
        const stale = event.source_status === 'source_stale'
        const eventRefs = refs(event)
        const isCorrection = event.type === 'fact.corrected' || event.type === 'fact.retracted'
        const target = String(event.structured_data?.target_event_id || event.id)
        const head = heads[target] || target
        return <article className="note-card temporal-card" key={event.id}>
          <div className="note-card-header"><span className="note-kind">{String(event.structured_data?.dimension || '事实报告')}</span><span className={stale ? 'history-removed' : 'note-origin'}>{stale ? '来源已更新' : event.source_status === 'unknown' ? '来源状态未知' : '来源当前有效'}</span><span className="note-origin">{isCorrection ? '后续报告' : '报告陈述 · 未确认'}</span></div>
          <p className="note-summary">{event.content}</p>
          <dl className="temporal-dates"><div><dt>适用时间</dt><dd>{prettyTime(event.valid_at || event.valid_from || event.timestamp)}</dd></div><div><dt>系统记录 / 获知时间</dt><dd>{prettyTime(event.recorded_at)}</dd></div></dl>
          {!stale && !isCorrection && eventRefs.length > 0 && <button type="button" className="secondary-button" onClick={() => setEditing(editing === event.id ? null : event.id)}>更正／补充报告</button>}
          {editing === event.id && <CorrectionForm event={event} head={head} evidence={eventRefs} onDone={() => { setEditing(null); refreshHistory() }} />}
        </article>
      })}</div>
    </StateView>
  </section>
}
