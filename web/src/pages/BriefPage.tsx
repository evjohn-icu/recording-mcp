import { useEffect, useMemo, useRef, useState } from 'react'
import { getTopics, listConversations } from '../api/client'
import { getBriefPolishConfig, listBriefPeople, polishBrief, previewBrief, type BriefRequest, type BriefResult, type PolishResponse } from '../api/brief'
import { exportBriefMarkdown, type BriefMarkdownFormat } from '../api/brief-markdown'
import { useApi } from '../hooks/useApi'
import { useAudio } from '../components/AudioBar'

type ScopePickerKind = 'person' | 'topic' | 'conversation' | 'project'

const sectionLabels: Record<string, string> = {
  background: '背景', previous_discussions: '过往讨论', follow_up_candidates: '待跟进候选', changes_conflicts: '变化与冲突', unknowns: '未知事项',
}
function ids(value: string): string[] { return value.split(/[\s,，]+/).map((item) => item.trim()).filter(Boolean) }
function asRFC3339(value: string): string | undefined { return value ? new Date(value).toISOString() : undefined }

export function BriefPage() {
  const conversations = useApi((signal) => listConversations({ limit: 100 }, signal), [])
  const people = useApi(() => listBriefPeople(), [])
  const topics = useApi((signal) => getTopics(undefined, signal), [])
  const [kind, setKind] = useState<ScopePickerKind>('conversation')
  const [conversationIds, setConversationIds] = useState<string[]>([])
  const [topicIds, setTopicIds] = useState<string[]>([])
  const [personIdsText, setPersonIdsText] = useState('')
  const [after, setAfter] = useState('')
  const [before, setBefore] = useState('')
  const [result, setResult] = useState<BriefResult | null>(null)
  const [savedRequest, setSavedRequest] = useState<BriefRequest | null>(null)
  const [error, setError] = useState('')
  const [copyStatus, setCopyStatus] = useState('')
  const [busy, setBusy] = useState(false)
  const [format, setFormat] = useState<BriefMarkdownFormat>('compact')
  const [polishConfig, setPolishConfig] = useState<{ loading: boolean; enabled: boolean }>({ loading: true, enabled: false })
  const [polishConsent, setPolishConsent] = useState(false)
  const [polishBusy, setPolishBusy] = useState(false)
  const [polishError, setPolishError] = useState('')
  const [polishResult, setPolishResult] = useState<PolishResponse | null>(null)
  const polishAbort = useRef<AbortController | null>(null)
  const polishGeneration = useRef(0)
  const previewAbort = useRef<AbortController | null>(null)
  const previewGeneration = useRef(0)
  const [openEvidence, setOpenEvidence] = useState<Record<string, boolean>>({})
  const audio = useAudio()
  const evidenceById = useMemo(() => new Map((result?.evidence ?? []).map((item) => [item.id, item])), [result])
  useEffect(() => {
    const controller = new AbortController()
    getBriefPolishConfig(controller.signal).then((config) => setPolishConfig({ loading: false, enabled: config.enabled })).catch(() => setPolishConfig({ loading: false, enabled: false }))
    return () => controller.abort()
  }, [])
  useEffect(() => () => { polishAbort.current?.abort(); previewAbort.current?.abort() }, [])
  const requestBody = (): BriefRequest => ({ scope: { kind, person_ids: kind === 'person' ? ids(personIdsText) : [], topic_ids: kind === 'topic' || kind === 'project' ? topicIds : [], conversation_ids: kind === 'conversation' || kind === 'project' ? conversationIds : [] }, started_after: asRFC3339(after), started_before: asRFC3339(before), max_items: 40, max_evidence: 80, max_bytes: 100000 })
  const draftIsStale = Boolean(result && savedRequest && JSON.stringify(requestBody()) !== JSON.stringify(savedRequest))
  useEffect(() => {
    previewGeneration.current += 1
    previewAbort.current?.abort()
    setBusy(false)
    setPolishConsent(false)
    polishGeneration.current += 1
    polishAbort.current?.abort()
    setPolishResult(null)
    setPolishError('')
    setPolishBusy(false)
  }, [kind, conversationIds, topicIds, personIdsText, after, before])
  async function submit(event: React.FormEvent) {
    event.preventDefault(); setBusy(true); setError(''); setCopyStatus(''); setPolishResult(null); setPolishError(''); setPolishConsent(false); setPolishBusy(false); polishGeneration.current += 1; polishAbort.current?.abort()
    const generation = ++previewGeneration.current
    previewAbort.current?.abort()
    const controller = new AbortController(); previewAbort.current = controller
    const body = requestBody()
    try { const next = await previewBrief(body, controller.signal); if (generation === previewGeneration.current) { setResult(next); setSavedRequest(body) } } catch (e) { if (!controller.signal.aborted && generation === previewGeneration.current) setError(e instanceof Error ? e.message : '生成预览失败') } finally { if (generation === previewGeneration.current) setBusy(false) }
  }
  function toggleId(values: string[], value: string, set: (next: string[]) => void) { set(values.includes(value) ? values.filter((x) => x !== value) : [...values, value]) }
  async function copyMarkdown() {
    if (!result) return
    try { await navigator.clipboard.writeText(exportBriefMarkdown(result, format)); setCopyStatus('Markdown 已复制。') } catch { setCopyStatus('复制失败，请检查剪贴板权限。') }
  }
  function downloadMarkdown() {
    if (!result) return
    const blob = new Blob([exportBriefMarkdown(result, format)], { type: 'text/markdown;charset=utf-8' })
    const url = URL.createObjectURL(blob)
    const anchor = document.createElement('a'); anchor.href = url; anchor.download = `brief-${result.snapshot_id}.md`; anchor.click()
    URL.revokeObjectURL(url); setCopyStatus('Markdown 文件已下载。')
  }
  async function requestPolish() {
    if (!result || !savedRequest || !polishConsent || !polishConfig.enabled) return
    const generation = ++polishGeneration.current
    polishAbort.current?.abort()
    const controller = new AbortController(); polishAbort.current = controller
    const snapshot = result.snapshot_id; setPolishBusy(true); setPolishConsent(false); setPolishError(''); setPolishResult(null)
    try {
      const candidate = await polishBrief({ request: savedRequest, snapshot_id: snapshot, consent: true }, controller.signal)
      if (generation === polishGeneration.current && candidate.snapshot_id === result.snapshot_id) setPolishResult(candidate)
    } catch (e) { if (!controller.signal.aborted && generation === polishGeneration.current) setPolishError(e instanceof Error ? e.message : '润色请求失败；确定性简报仍可使用。') }
    finally { if (generation === polishGeneration.current) setPolishBusy(false) }
  }
  const convs = conversations.data?.items ?? []
  const topicItems = topics.data?.topics ?? []
  return <section className="page brief-page">
    <header className="page-header"><h1>记忆简报</h1><p className="page-subtitle">根据已保存的转写与归纳生成可核对的预览；断言不会自动升级为已确认事实。</p></header>
    <form className="temporal-form" onSubmit={(e) => void submit(e)}>
      <label>范围类型<select value={kind} onChange={(e) => setKind(e.target.value as ScopePickerKind)}>{[['person','人物'],['topic','主题'],['conversation','会话'],['project','项目（主题与会话）']].map(([v,l]) => <option value={v} key={v}>{l}</option>)}</select></label>
      {(kind === 'conversation' || kind === 'project') && <fieldset><legend>{kind === 'project' ? '项目内会话' : '会话'}</legend>{convs.map((c) => <label className="toggle-label" key={c.conversation_id}><input type="checkbox" checked={conversationIds.includes(c.conversation_id)} onChange={() => toggleId(conversationIds, c.conversation_id, setConversationIds)} />{c.title || c.conversation_id}</label>)}{conversations.loading && <p role="status">正在加载会话…</p>}{conversations.error && <p role="alert" className="note-error">会话列表加载失败。</p>}{!conversations.loading && !conversations.error && !convs.length && <p>没有可选择的会话。</p>}</fieldset>}
      {(kind === 'topic' || kind === 'project') && <fieldset><legend>主题</legend>{topicItems.map((t) => <label className="toggle-label" key={t.id}><input type="checkbox" checked={topicIds.includes(t.id)} onChange={() => toggleId(topicIds, t.id, setTopicIds)} />{t.title}</label>)}{topics.loading && <p role="status">正在加载主题…</p>}{topics.error && <p role="alert" className="note-error">主题列表加载失败。</p>}{!topics.loading && !topics.error && !topicItems.length && <p>没有可选择的主题。</p>}</fieldset>}
      {kind === 'person' && <fieldset><legend>当前已确认关联的人物</legend>{people.data?.people.map((p) => <label className="toggle-label" key={p.id}><input type="checkbox" checked={ids(personIdsText).includes(p.id)} onChange={() => setPersonIdsText((prev) => ids(prev).includes(p.id) ? ids(prev).filter((x) => x !== p.id).join(', ') : [...ids(prev), p.id].join(', '))} />{p.label}</label>)}{!people.data?.people.length && <p>没有当前有效的已确认人物关联。</p>}<label>人物 ID（逗号分隔）<input value={personIdsText} onChange={(e) => setPersonIdsText(e.target.value)} placeholder="person-…" /></label></fieldset>}
      <div className="filter-row"><label>开始时间<input type="datetime-local" value={after} onChange={(e) => setAfter(e.target.value)} /></label><label>结束时间<input type="datetime-local" value={before} onChange={(e) => setBefore(e.target.value)} /></label></div>
      <button type="submit" disabled={busy}>{busy ? '生成中…' : '生成简报预览'}</button>
    </form>
    {error && <p role="alert" className="note-error">{error}</p>}
    {result && <section className="brief-result" aria-label="简报预览"><header><h2>简报预览</h2><div className="turn-actions"><label>导出格式<select aria-label="Markdown 导出格式" value={format} onChange={(e) => setFormat(e.target.value as BriefMarkdownFormat)}><option value="compact">精简</option><option value="detailed">详细</option></select></label><button type="button" className="secondary-button" onClick={() => void copyMarkdown()}>复制 Markdown</button><button type="button" className="secondary-button" onClick={downloadMarkdown}>下载 .md</button></div></header>
      {draftIsStale && <p className="status-badge" role="status">表单已修改；下方仍是快照 {result.snapshot_id} 的已保存结果。重新生成后才会更新。</p>}
      {copyStatus && <p className="page-subtitle" role="status">{copyStatus}</p>}
      <p>快照 {result.snapshot_id} · 已选择 {result.coverage.selected_turns} / {result.coverage.eligible_turns} 个轮次 · {result.truncated ? '结果已截断' : '完整返回'} · {result.degraded ? '部分数据不可用' : '数据状态正常'}</p>
      {Object.entries(result.sections).map(([key, entries]) => <section key={key}><h3>{sectionLabels[key] || key}</h3>{entries.length ? entries.map((entry) => <article className="note-card" key={entry.id}><p>{entry.text} {entry.text_truncated && <span className="status-badge">节选已截断</span>}</p><p className="page-subtitle">来源：{entry.source_state} · 断言：{entry.assertion_state}</p>
        {entry.evidence_ids.length > 0 && <button type="button" className="link-button" onClick={() => setOpenEvidence((s) => ({ ...s, [entry.id]: !s[entry.id] }))}>{openEvidence[entry.id] ? '收起证据' : `查看证据（${entry.evidence_ids.length}）`}</button>}
        {openEvidence[entry.id] && entry.evidence_ids.map((id) => { const ev = evidenceById.get(id); return ev ? <blockquote key={id}>{ev.text}<div className="turn-actions"><button type="button" className="secondary-button" onClick={() => audio.play(ev.turn_id)}>播放证据</button><span>{ev.source_state}</span></div></blockquote> : <p key={id}>证据 {id} 不在当前返回中</p> })}
      </article>) : <p className="page-subtitle">暂无条目</p>}</section>)}
      <section><h3>证据（{result.evidence.length}）</h3>{result.evidence.map((ev) => <article className="note-card" key={ev.id}>
        <p>{ev.text} {ev.text_truncated && <span className="status-badge">节选已截断</span>}</p><p className="page-subtitle">来源状态：{ev.source_state} · 轮次：{ev.turn_id}{ev.global_start_ms != null ? ` · ${ev.global_start_ms} ms` : ''}</p>
        <p className="page-subtitle">录音时间：{ev.started_at || '未知'}</p>
        {ev.transcript_revision && <p className="page-subtitle">转写修订：{ev.transcript_revision}</p>}
        {ev.source_attachment_id && <p className="page-subtitle">来源附件：{ev.source_attachment_id}</p>}
        {ev.provenance && <details><summary>来源说明</summary><p>{ev.provenance}</p></details>}
        <div className="turn-actions"><button type="button" className="secondary-button" onClick={() => audio.play(ev.turn_id, { startMs: ev.global_start_ms ?? null, endMs: ev.global_end_ms ?? null })}>播放证据</button><a href={`/c/${encodeURIComponent(ev.conversation_id)}?item=${encodeURIComponent(ev.turn_id)}`}>打开完整来源</a></div>
      </article>)}</section>
      {result.gaps.length > 0 && <section><h3>数据缺口</h3><ul>{result.gaps.map((gap, i) => <li key={`${gap.kind}-${i}`}>{gap.kind}：{gap.detail}</li>)}</ul></section>}
      <section className="brief-polish" aria-labelledby="brief-polish-title"><h3 id="brief-polish-title">可选 AI 润色</h3>
        {polishConfig.loading ? <p role="status">正在读取润色服务状态…</p> : !polishConfig.enabled ? <p>润色服务未启用；确定性简报仍可复制或下载。</p> : <>
          <p>润色会把此快照范围内的简报请求发送到服务端配置的模型处理；该模型可能由外部服务提供。只会在你勾选同意并点击按钮后发送。已发送的数据和导出的副本无法撤回。</p>
          <label className="toggle-label"><input type="checkbox" checked={polishConsent} onChange={(e) => setPolishConsent(e.target.checked)} />我同意将此范围的简报内容发送给服务端配置的模型进行润色。</label>
          <button type="button" className="secondary-button" disabled={!polishConsent || polishBusy || busy || draftIsStale} onClick={() => void requestPolish()}>{polishBusy ? '润色中…' : '生成独立候选'}</button>
          {draftIsStale && <p>表单已修改，请先重新生成快照，再请求润色。</p>}
          {polishBusy && <p role="status">正在等待候选文本…</p>}
          {polishError && <p role="alert" className="note-error">{polishError} 确定性简报仍可使用。</p>}
          {polishResult && polishResult.snapshot_id === result.snapshot_id && <div className="brief-candidates" aria-label="AI 润色候选"><h4>独立候选（待审核）</h4>{polishResult.degraded && <p>候选服务部分降级：{polishResult.reason || '原因未提供'}</p>}{polishResult.candidates.map((candidate) => <article className="note-card" key={candidate.entry_id}><p>{candidate.text}</p><p className="page-subtitle">候选状态：proposed · 来源条目：{candidate.entry_id} · 证据 ID：{candidate.evidence_ids.join('、') || '无'}</p>{candidate.evidence_ids.length > 0 && <button type="button" className="link-button" onClick={() => setOpenEvidence((s) => ({ ...s, [`candidate:${candidate.entry_id}`]: !s[`candidate:${candidate.entry_id}`] }))}>{openEvidence[`candidate:${candidate.entry_id}`] ? '收起关联证据' : '查看关联证据'}</button>}{openEvidence[`candidate:${candidate.entry_id}`] && candidate.evidence_ids.map((id) => { const ev = evidenceById.get(id); return ev ? <blockquote key={id}>{ev.text}<div className="turn-actions"><button type="button" className="secondary-button" onClick={() => audio.play(ev.turn_id, { startMs: ev.global_start_ms ?? null, endMs: ev.global_end_ms ?? null })}>播放证据</button><span>{ev.source_state} · {ev.transcript_revision || '修订未知'}</span></div></blockquote> : <p key={id}>证据 {id} 不在当前返回中</p> })}</article>)}</div>}
        </>}
      </section>
    </section>}
  </section>
}
