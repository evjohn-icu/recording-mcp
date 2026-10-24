import { useMemo, useState } from 'react'
import { getTopics, listConversations } from '../api/client'
import { listBriefPeople, previewBrief, type BriefRequest, type BriefResult } from '../api/brief'
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
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const [openEvidence, setOpenEvidence] = useState<Record<string, boolean>>({})
  const audio = useAudio()
  const evidenceById = useMemo(() => new Map((result?.evidence ?? []).map((item) => [item.id, item])), [result])
  async function submit(event: React.FormEvent) {
    event.preventDefault(); setBusy(true); setError(''); setResult(null)
    const body: BriefRequest = { scope: { kind, person_ids: kind === 'person' ? ids(personIdsText) : [], topic_ids: kind === 'topic' || kind === 'project' ? topicIds : [], conversation_ids: kind === 'conversation' || kind === 'project' ? conversationIds : [] }, started_after: asRFC3339(after), started_before: asRFC3339(before), max_items: 40, max_evidence: 80, max_bytes: 100000 }
    try { setResult(await previewBrief(body)) } catch (e) { setError(e instanceof Error ? e.message : '生成预览失败') } finally { setBusy(false) }
  }
  function toggleId(values: string[], value: string, set: (next: string[]) => void) { set(values.includes(value) ? values.filter((x) => x !== value) : [...values, value]) }
  async function copyMarkdown() {
    if (!result) return
    const rows = [`Snapshot: ${result.snapshot_id}`, `截断：${result.truncated ? '是' : '否'} · 降级：${result.degraded ? '是' : '否'}`, '',
      ...Object.entries(result.sections).flatMap(([key, entries]) => [`## ${sectionLabels[key] || key}`, ...entries.map((entry) => `- ${entry.text}${entry.text_truncated ? ' [节选已截断]' : ''}\n  - 来源状态：${entry.source_state}；断言状态：${entry.assertion_state}\n  - 证据 ID：${entry.evidence_ids.join(', ') || '无'}`), '']),
      '## 证据', ...result.evidence.map((ev) => `- ${ev.text}${ev.text_truncated ? ' [节选已截断]' : ''}\n  - 来源状态：${ev.source_state}；轮次：${ev.turn_id}；会话：${ev.conversation_id}\n  - 修订：${ev.transcript_revision || '未知'}；来源附件：${ev.source_attachment_id || '未知'}\n  - 来源说明：${ev.provenance || '无'}`), '',
      '## 数据缺口', ...result.gaps.map((gap) => `- ${gap.kind}：${gap.detail}`), '']
    try { await navigator.clipboard.writeText(`# 记忆简报\n\n${rows.join('\n')}`) } catch { setError('复制失败，请检查剪贴板权限') }
  }
  const convs = conversations.data?.items ?? []
  const topicItems = topics.data?.topics ?? []
  return <section className="page brief-page">
    <header className="page-header"><h1>记忆简报</h1><p className="page-subtitle">根据已保存的转写与归纳生成可核对的预览；断言不会自动升级为已确认事实。</p></header>
    <form className="temporal-form" onSubmit={(e) => void submit(e)}>
      <label>范围类型<select value={kind} onChange={(e) => setKind(e.target.value as ScopePickerKind)}>{[['person','人物'],['topic','主题'],['conversation','会话'],['project','项目（主题与会话）']].map(([v,l]) => <option value={v} key={v}>{l}</option>)}</select></label>
      {(kind === 'conversation' || kind === 'project') && <fieldset><legend>{kind === 'project' ? '项目内会话' : '会话'}</legend>{convs.map((c) => <label className="toggle-label" key={c.conversation_id}><input type="checkbox" checked={conversationIds.includes(c.conversation_id)} onChange={() => toggleId(conversationIds, c.conversation_id, setConversationIds)} />{c.title || c.conversation_id}</label>)}</fieldset>}
      {(kind === 'topic' || kind === 'project') && <fieldset><legend>主题</legend>{topicItems.map((t) => <label className="toggle-label" key={t.id}><input type="checkbox" checked={topicIds.includes(t.id)} onChange={() => toggleId(topicIds, t.id, setTopicIds)} />{t.title}</label>)}</fieldset>}
      {kind === 'person' && <fieldset><legend>当前已确认关联的人物</legend>{people.data?.people.map((p) => <label className="toggle-label" key={p.id}><input type="checkbox" checked={ids(personIdsText).includes(p.id)} onChange={() => setPersonIdsText((prev) => ids(prev).includes(p.id) ? ids(prev).filter((x) => x !== p.id).join(', ') : [...ids(prev), p.id].join(', '))} />{p.label}</label>)}{!people.data?.people.length && <p>没有当前有效的已确认人物关联。</p>}<label>人物 ID（逗号分隔）<input value={personIdsText} onChange={(e) => setPersonIdsText(e.target.value)} placeholder="person-…" /></label></fieldset>}
      <div className="filter-row"><label>开始时间<input type="datetime-local" value={after} onChange={(e) => setAfter(e.target.value)} /></label><label>结束时间<input type="datetime-local" value={before} onChange={(e) => setBefore(e.target.value)} /></label></div>
      <button type="submit" disabled={busy}>{busy ? '生成中…' : '生成简报预览'}</button>
    </form>
    {error && <p role="alert" className="note-error">{error}</p>}
    {result && <section className="brief-result" aria-label="简报预览"><header><h2>简报预览</h2><button type="button" className="secondary-button" onClick={() => void copyMarkdown()}>复制 Markdown</button></header>
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
    </section>}
  </section>
}
