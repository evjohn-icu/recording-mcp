import { useEffect, useMemo, useRef, useState } from 'react'
import { Link } from 'react-router-dom'
import { ApiError, getItem } from '../api/client'
import {
  getWorldline,
  getWorldlineTopic,
  startWorldlineAnalysis,
  getWorldlineAnalysis,
  getWorldlineThreads,
  getWorldlineThread,
  submitWorldlineFeedback,
  renameWorldlineThread,
  groupWorldlineThreads,
  revokeWorldlineThreadGroup,
  reviewWorldlineSemantic,
  type WorldlineAction,
  type WorldlineClaim,
  type WorldlineFeedback,
  type WorldlineLane,
  type WorldlineResponse,
  type WorldlineStatus,
  type WorldlineThreadDetail,
  type WorldlineThreadEntry,
  type WorldlineThreadSummary,
  type WorldlineThreadsResponse,
  type WorldlineTopic,
} from '../api/worldline'
import { useAudio } from '../components/AudioBar'
import { DecisionTrailPanel } from '../components/DecisionTrailPanel'
import './worldline.css'

type Zoom = 'all' | 'day' | 'week' | 'month'
type WorldlineView = 'threads' | 'validity'
const ZOOM_MS: Record<Exclude<Zoom, 'all'>, number> = { day: 86400000, week: 7 * 86400000, month: 30 * 86400000 }
const CATEGORY_LABELS = { facts: '事实', plans: '计划', unclassified: '待分类' }
const STATUS_LABELS: Record<string, string> = { unconfirmed: '未确认', reviewed: '已复核', conflict: '存在冲突' }

function dateValue(value?: string | null): number | undefined {
  if (!value) return undefined
  const parsed = Date.parse(value)
  return Number.isFinite(parsed) ? parsed : undefined
}

function dayLabel(ms: number, includeYear = false): string {
  const d = new Date(ms)
  return new Intl.DateTimeFormat('zh-CN', includeYear ? { year: 'numeric', month: 'short', day: 'numeric' } : { month: 'short', day: 'numeric' }).format(d)
}

function dateTimeLocal(value?: string | null): string {
  const ms = dateValue(value)
  if (ms === undefined) return ''
  const d = new Date(ms - new Date(ms).getTimezoneOffset() * 60000)
  return d.toISOString().slice(0, 16)
}

function toRFC3339(value: string): string | undefined {
  return value ? new Date(value).toISOString() : undefined
}

function waitForSignal(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    const cleanup = () => signal.removeEventListener('abort', onAbort)
    const timer = window.setTimeout(() => { cleanup(); resolve() }, ms)
    const onAbort = () => { window.clearTimeout(timer); cleanup(); reject(new DOMException('Aborted', 'AbortError')) }
    if (signal.aborted) onAbort()
    else signal.addEventListener('abort', onAbort, { once: true })
  })
}

function allClaims(data: WorldlineResponse | null): WorldlineClaim[] {
  return (data?.topics || []).flatMap((topic) => topic.lanes.flatMap((lane) => lane.claims))
}

function claimPosition(claim: WorldlineClaim, start: number, end: number): { left: number; width: number } | null {
  const from = dateValue(claim.valid_from)
  const until = dateValue(claim.valid_until)
  if (from === undefined && until === undefined) return null
  const duration = Math.max(end - start, 1)
  const leftMs = Math.max(start, Math.min(end, from ?? start))
  const rightMs = Math.max(start, Math.min(end, until ?? from ?? end))
  const left = ((leftMs - start) / duration) * 100
  const width = Math.max(from === undefined || until === undefined ? 2.5 : 1.2, ((rightMs - leftMs) / duration) * 100)
  return { left, width: Math.min(width, 100 - left) }
}

function WorldlineFeedbackForm({
  action,
  topic,
  lane,
  claim,
  destinations,
  version,
  onClose,
  onSaved,
}: {
  action: WorldlineAction
  topic: WorldlineTopic
  lane: WorldlineLane
  claim?: WorldlineClaim
  destinations: Array<{ macro: string; lane: WorldlineLane }>
  version: string
  onClose: () => void
  onSaved: () => void
}) {
  const [title, setTitle] = useState(action === 'rename_topic' ? lane.title : '')
  const [summary, setSummary] = useState(claim?.summary || '')
  const [destination, setDestination] = useState('')
  const [validFrom, setValidFrom] = useState(dateTimeLocal(claim?.valid_from))
  const [validUntil, setValidUntil] = useState(dateTimeLocal(claim?.valid_until))
  const [mentionedAt, setMentionedAt] = useState(dateTimeLocal(claim?.mentioned_at))
  const [claimIds, setClaimIds] = useState(claim?.id || '')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  const labels: Record<WorldlineAction, string> = {
    rename_topic: '重命名主题', merge_topics: '合并主题', split_topic: '拆分主题', move_claim: '移动归属', review: '复核主张',
    correct: '修正记录', change: '记录现实变化', set_time: '设置适用时间',
  }
  const compatibleDestinations = destinations.filter((item) => item.lane.id !== lane.id
    && lane.subject.trim() !== ''
    && lane.subject.trim().toLowerCase() !== 'unknown'
    && item.lane.subject === lane.subject
    && item.lane.subject.trim().toLowerCase() !== 'unknown'
    && item.lane.category === lane.category)

  async function submit(event: React.FormEvent) {
    event.preventDefault()
    setBusy(true)
    setError('')
    const payload: WorldlineFeedback = { expected_version: version, action }
    if (action === 'rename_topic' || action === 'merge_topics' || action === 'split_topic') payload.target_topic_id = lane.id
    if (claim) payload.target_claim_id = claim.id
    if (title.trim()) payload.title = title.trim()
    if (summary.trim()) payload.summary = summary.trim()
    if (destination) payload.destination_topic_id = destination
    if (validFrom) payload.valid_from = toRFC3339(validFrom)
    if (validUntil) payload.valid_until = toRFC3339(validUntil)
    if (mentionedAt) payload.mentioned_at = toRFC3339(mentionedAt)
    if (claimIds.trim()) payload.claim_ids = claimIds.split(',').map((id) => id.trim()).filter(Boolean)
    if (action === 'change') payload.evidence = claim?.evidence.map(({ turn_id, revision }) => ({ turn_id, revision })) || []
    if (action === 'set_time' && !validFrom && !validUntil) { setError('请至少填写一个适用时间边界。'); setBusy(false); return }
    try {
      await submitWorldlineFeedback(payload)
      onSaved()
    } catch (cause) {
      setError(cause instanceof ApiError && cause.status === 409
        ? '资料已在别处更新。草稿仍保留；刷新后核对当前内容，再重新提交。'
        : cause instanceof Error ? cause.message : '无法保存更改')
    } finally { setBusy(false) }
  }

  return <div className="wl-modal-backdrop" role="presentation" onMouseDown={(e) => { if (e.target === e.currentTarget) onClose() }}>
    <form className="wl-feedback" role="dialog" aria-modal="true" aria-labelledby="wl-feedback-title" onSubmit={submit}>
      <header><div><span className="wl-eyebrow">主题整理</span><h2 id="wl-feedback-title">{labels[action]}</h2></div><button type="button" className="wl-icon-button" aria-label="关闭" onClick={onClose}>×</button></header>
      <p className="wl-form-context">{topic.title} / {lane.title}{claim ? ` · ${claim.summary}` : ''}</p>
      {action === 'rename_topic' && <label>新主题名称<input autoFocus value={title} onChange={(e) => setTitle(e.target.value)} required /></label>}
      {action === 'merge_topics' && <label>合并到主题<select value={destination} onChange={(e) => setDestination(e.target.value)} required><option value="">选择目标主题</option>{compatibleDestinations.map((item) => <option key={item.lane.id} value={item.lane.id}>{item.macro} / {item.lane.title}</option>)}</select></label>}
      {action === 'split_topic' && <><label>新主题名称<input autoFocus value={title} onChange={(e) => setTitle(e.target.value)} required /></label><label>移动的主张 ID（逗号分隔）<textarea value={claimIds} onChange={(e) => setClaimIds(e.target.value)} placeholder="输入要移动到新主题的主张 ID" required /></label></>}
      {action === 'move_claim' && <label>移动到主题<select value={destination} onChange={(e) => setDestination(e.target.value)} required><option value="">选择目标主题</option>{compatibleDestinations.map((item) => <option key={item.lane.id} value={item.lane.id}>{item.macro} / {item.lane.title}</option>)}</select></label>}
      {action === 'correct' && <label>修正后的陈述<textarea value={summary} onChange={(e) => setSummary(e.target.value)} required /></label>}
      {action === 'change' && <><label>当前现实情况<textarea value={summary} onChange={(e) => setSummary(e.target.value)} required /></label><p className="wl-form-hint">这会记录一项现实变化；系统不会据提及时间推断适用时间。</p></>}
      {action === 'set_time' && <><label>适用时间开始<input type="datetime-local" value={validFrom} onChange={(e) => setValidFrom(e.target.value)} /></label><label>适用时间结束（可留空表示开放区间）<input type="datetime-local" value={validUntil} onChange={(e) => setValidUntil(e.target.value)} /></label><label>原文提及时间（单独记录）<input type="datetime-local" value={mentionedAt} onChange={(e) => setMentionedAt(e.target.value)} /></label></>}
      {action === 'review' && <p className="wl-form-hint">确认你已核对这条主张及其来源。复核不会把提及时间转成适用时间。</p>}
      {error && <p className="wl-error" role="alert">{error}</p>}
      <footer><button type="button" className="secondary-button" onClick={onClose}>取消</button><button type="submit" disabled={busy}>{busy ? '保存中…' : '保存'}</button></footer>
    </form>
  </div>
}

export function WorldlinePage() {
  const [search, setSearch] = useState('')
  const [query, setQuery] = useState('')
  const [status, setStatus] = useState<WorldlineStatus>('all')
  const [viewMode, setViewMode] = useState<WorldlineView>('threads')
  const [offset, setOffset] = useState(0)
  const [zoom, setZoom] = useState<Zoom>('all')
  const [windowCenter, setWindowCenter] = useState<number | null>(null)
  const [selected, setSelected] = useState<WorldlineClaim | null>(null)
  const [expanded, setExpanded] = useState<Record<string, boolean>>({})
  const [feedback, setFeedback] = useState<{ action: WorldlineAction; topic: WorldlineTopic; lane: WorldlineLane; claim?: WorldlineClaim; version?: string } | null>(null)
  const [refreshKey, setRefreshKey] = useState(0)
  const [generationState, setGenerationState] = useState<'idle' | 'running' | 'complete' | 'failed'>('idle')
  const [generationError, setGenerationError] = useState('')
  const [generationResult, setGenerationResult] = useState<Awaited<ReturnType<typeof startWorldlineAnalysis>> | null>(null)
  const generationRequestID = useRef('')
  const generationAbort = useRef<AbortController | null>(null)
  const [extraTopics, setExtraTopics] = useState<Record<string, WorldlineTopic>>({})
  const [loadingTopic, setLoadingTopic] = useState('')
  const topicLoadGeneration = useRef(0)
  const topicLoadControllers = useRef(new Set<AbortController>())
  const [topicLoadError, setTopicLoadError] = useState('')
  const [data, setData] = useState<WorldlineResponse | null>(null)
  const [threadStats, setThreadStats] = useState({ total: 0, crossConversation: 0 })
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const { play } = useAudio()

  useEffect(() => () => { generationAbort.current?.abort(); generationAbort.current = null }, [])

  useEffect(() => {
    function handleKey(event: KeyboardEvent) {
      if (event.key === 'Escape') {
        if (feedback) setFeedback(null)
        else if (selected) setSelected(null)
        return
      }
      const target = event.target
      if (!(target instanceof HTMLElement) || !target.matches('.wl-claim-bar, .wl-mention-track-dot, .wl-claim-unknown')) return
      if (!['ArrowLeft', 'ArrowRight', 'ArrowUp', 'ArrowDown', 'Home', 'End'].includes(event.key)) return
      const controls = Array.from(document.querySelectorAll<HTMLElement>('.wl-claim-bar, .wl-mention-track-dot, .wl-claim-unknown'))
      const index = controls.indexOf(target)
      let next = index
      if (event.key === 'Home') next = 0
      else if (event.key === 'End') next = controls.length - 1
      else if (event.key === 'ArrowLeft' || event.key === 'ArrowUp') next = Math.max(0, index - 1)
      else next = Math.min(controls.length - 1, index + 1)
      if (next !== index && controls[next]) { event.preventDefault(); controls[next].focus() }
    }
    document.addEventListener('keydown', handleKey)
    return () => document.removeEventListener('keydown', handleKey)
  }, [feedback, selected])

  useEffect(() => {
    if (viewMode !== 'validity') return
    const generation = ++topicLoadGeneration.current
    for (const pending of topicLoadControllers.current) pending.abort()
    topicLoadControllers.current.clear()
    const controller = new AbortController()
    setLoading(true)
    setError('')
    setExtraTopics({})
    setTopicLoadError('')
    getWorldline({ q: query, status, offset, limit: 100, from: zoom === 'all' ? undefined : new Date(viewStart).toISOString(), to: zoom === 'all' ? undefined : new Date(viewEnd).toISOString() }, controller.signal)
      .then((result) => { setData(result); setSelected((current) => current ? allClaims(result).find((claim) => claim.id === current.id) || null : null) })
      .catch((cause) => { if (!controller.signal.aborted) setError(cause instanceof Error ? cause.message : '无法载入主题时间线') })
      .finally(() => { if (!controller.signal.aborted) setLoading(false) })
    return () => {
      controller.abort()
      for (const pending of topicLoadControllers.current) pending.abort()
      topicLoadControllers.current.clear()
      if (topicLoadGeneration.current === generation) topicLoadGeneration.current++
    }
  }, [query, status, offset, refreshKey, zoom, windowCenter, viewMode])

  const topics = useMemo(() => data?.topics.map((topic) => extraTopics[topic.id] || topic) || [], [data, extraTopics])
  const claims = useMemo(() => topics.flatMap((topic) => topic.lanes.flatMap((lane) => lane.claims)), [topics])
  const dated = useMemo(() => claims.flatMap((claim) => [dateValue(claim.valid_from), dateValue(claim.valid_until), dateValue(claim.mentioned_at)]).filter((v): v is number => v !== undefined), [claims])
  const serverStart = dateValue(data?.range.start) ?? (dated.length ? Math.min(...dated) : Date.now() - 86400000 * 30)
  const serverEnd = dateValue(data?.range.end) ?? (dated.length ? Math.max(...dated) : Date.now())
  const allStart = Math.min(serverStart, serverEnd - 1)
  const allEnd = Math.max(serverEnd, serverStart + 1)
  const focus = windowCenter ?? allStart + (allEnd - allStart) / 2
  const viewStart = zoom === 'all' ? allStart : focus - ZOOM_MS[zoom] / 2
  const viewEnd = zoom === 'all' ? allEnd : focus + ZOOM_MS[zoom] / 2
  const span = Math.max(viewEnd - viewStart, 1)
  const ticks = 6
  const tickValues = Array.from({ length: ticks }, (_, index) => viewStart + span * index / (ticks - 1))
  const hasMore = data?.next_offset !== undefined && data.next_offset !== null

  function doSearch(event: React.FormEvent) { event.preventDefault(); setOffset(0); setQuery(search.trim()) }
  async function runGeneration() {
    generationAbort.current?.abort()
    const controller = new AbortController()
    generationAbort.current = controller
    if (!generationRequestID.current) {
      generationRequestID.current = typeof crypto !== 'undefined' && 'randomUUID' in crypto
        ? `worldline-${crypto.randomUUID()}`
        : `worldline-${Date.now()}-${Math.random().toString(36).slice(2)}`
    }
    setGenerationError('')
    setGenerationState('running')
    try {
      let result = await startWorldlineAnalysis(generationRequestID.current, controller.signal)
      const deadline = Date.now() + 10 * 60 * 1000
      while (result.status === 'queued' || result.status === 'running' || result.status === 'retry_wait') {
        if (Date.now() >= deadline) throw new Error('任务仍在队列中；可以重试查看状态。')
        await waitForSignal(1000, controller.signal)
        result = await getWorldlineAnalysis(result.job_id, controller.signal)
      }
      if (result.status !== 'complete') { if (result.status === 'failed' || result.status === 'cancelled') generationRequestID.current = ''; throw new Error(result.status === 'cancelled' ? '整理任务已取消。' : (result.error || '整理失败，请重试。')) }
      generationRequestID.current = ''
      setGenerationResult(result)
      setGenerationState('complete')
      setRefreshKey((value) => value + 1)
    } catch (cause) {
      if (controller.signal.aborted) { if (generationAbort.current === controller) { setGenerationState('idle'); setGenerationError('已停止等待；后台任务会继续运行，继续整理将恢复查看它。') }; return }
      setGenerationState('failed')
      setGenerationError(cause instanceof Error ? cause.message : '整理失败，请重试。')
    } finally { if (generationAbort.current === controller) generationAbort.current = null }
  }
  function pan(direction: -1 | 1) {
    if (zoom === 'all') return
    setWindowCenter((current) => (current ?? focus) + direction * ZOOM_MS[zoom])
  }
  function toggleTopic(id: string) { setExpanded((current) => ({ ...current, [id]: !(current[id] ?? true) })) }
  function openFeedback(action: WorldlineAction, topic: WorldlineTopic, lane: WorldlineLane, claim?: WorldlineClaim) { setFeedback({ action, topic, lane, claim, version: data?.version }) }
  async function loadMoreClaims(topic: WorldlineTopic) {
    const generation = topicLoadGeneration.current
    const controller = new AbortController()
    topicLoadControllers.current.add(controller)
    setLoadingTopic(topic.id); setTopicLoadError('')
    try {
      const offset = topic.lanes.reduce((sum, lane) => sum + lane.claims.length, 0)
      const page = await getWorldlineTopic(topic.id, { offset, limit: 100, q: query, status, from: zoom === 'all' ? undefined : new Date(viewStart).toISOString(), to: zoom === 'all' ? undefined : new Date(viewEnd).toISOString() }, controller.signal)
      if (generation !== topicLoadGeneration.current) return
      setExtraTopics((current) => {
        const existing = current[topic.id] || topic
        const incoming = new Map(page.topic.lanes.map((lane) => [lane.id, lane]))
        const merged = existing.lanes.map((lane) => {
          const next = incoming.get(lane.id)
          if (!next) return lane
          const seen = new Set(lane.claims.map((claim) => claim.id))
          return { ...lane, ...next, claims: [...lane.claims, ...next.claims.filter((claim) => !seen.has(claim.id))] }
        })
        const existingIDs = new Set(existing.lanes.map((lane) => lane.id))
        for (const lane of page.topic.lanes) {
          if (!existingIDs.has(lane.id)) merged.push(lane)
        }
        const hasMore = page.next_offset !== undefined && page.next_offset !== null
        return { ...current, [topic.id]: { ...existing, ...page.topic, lanes: merged, has_more: hasMore } }
      })
    } catch (cause) { if (generation === topicLoadGeneration.current && !controller.signal.aborted) setTopicLoadError(cause instanceof Error ? cause.message : '无法载入更多主张') }
    finally {
      topicLoadControllers.current.delete(controller)
      if (generation === topicLoadGeneration.current) setLoadingTopic('')
    }
  }

  return <section className="wl-page">
    <header className="wl-heading">
      <div><span className="wl-eyebrow">LOCAL MEMORY · WORLDLINE</span><h1>主题世界线</h1><p>按主题查看主张的适用时间、来源提及和变化记录。</p></div>
      <div className="wl-heading-tools">
        <div className="wl-metrics" aria-label="时间线统计">
          {viewMode === 'threads' ? <><span><b>{threadStats.total || '—'}</b>具体主题</span><span><b>{threadStats.total ? threadStats.crossConversation : '—'}</b>跨会话主题</span></> : <><span><b>{data?.stats.topics ?? '—'}</b>主题</span><span><b>{data?.stats.claims ?? '—'}</b>主张</span><span><b>{data?.stats.unknown_time ?? '—'}</b>时间未知</span><span><b>{data?.stats.conflicts ?? '—'}</b>冲突</span></>}
        </div>
        <button type="button" className="wl-generate-button" disabled={generationState === 'running'} onClick={() => void runGeneration()}>{generationState === 'running' ? '整理中…' : generationState === 'failed' ? '重试整理' : '整理世界线'}</button>{generationState==='running'&&<button type="button" className="secondary-button" onClick={()=>generationAbort.current?.abort()}>停止等待</button>}
        <span className="wl-generation-status" role={generationError ? 'alert' : 'status'}>
          {generationError || (generationState === 'running' ? '正在整理并核对来源…' : generationState === 'complete' ? generationSemanticLabel(generationResult) : '整理会读取已有来源；语义建议需人工核对，不会自动改写事实。')}
        </span>
      </div>
    </header>
    <div className="wl-toolbar">
      <form className="wl-search" onSubmit={doSearch}><span aria-hidden="true">⌕</span><input aria-label="搜索主题与主张" placeholder="搜索主题、人物或陈述…" value={search} onChange={(e) => setSearch(e.target.value)} /><button type="submit">搜索</button></form>
      <div className="wl-view-switch" aria-label="世界线视图">
        <button type="button" className={viewMode === 'threads' ? 'selected' : ''} onClick={() => { setStatus('all'); setViewMode('threads') }}>跨会话提及脉络</button>
        <button type="button" className={viewMode === 'validity' ? 'selected' : ''} onClick={() => setViewMode('validity')}>事实适用时间</button>
      </div>
      <label className="wl-status-filter">状态<select aria-label="状态筛选" value={status} onChange={(e) => { setStatus(e.target.value as WorldlineStatus); setOffset(0) }}><option value="all">全部状态</option><option value="unconfirmed">未确认</option><option value="reviewed">已复核</option><option value="conflict">存在冲突</option></select></label>
      {viewMode === 'validity' && <div className="wl-zoom" aria-label="时间缩放">
        <button type="button" className={zoom === 'all' ? 'selected' : ''} onClick={() => setZoom('all')}>全范围</button>
        <button type="button" className={zoom === 'day' ? 'selected' : ''} onClick={() => { setZoom('day'); setWindowCenter(focus) }}>日</button>
        <button type="button" className={zoom === 'week' ? 'selected' : ''} onClick={() => { setZoom('week'); setWindowCenter(focus) }}>周</button>
        <button type="button" className={zoom === 'month' ? 'selected' : ''} onClick={() => { setZoom('month'); setWindowCenter(focus) }}>月</button>
      </div>}
      {viewMode === 'validity' && <div className="wl-pan"><button aria-label="向前浏览" type="button" disabled={zoom === 'all'} onClick={() => pan(-1)}>←</button><button aria-label="向后浏览" type="button" disabled={zoom === 'all'} onClick={() => pan(1)}>→</button></div>}
    </div>
    <div className="wl-legend"><span><i className="wl-legend-claim" />适用时间</span><span><i className="wl-legend-mention" />提及时间（不是适用时间）</span><span><i className="wl-legend-unknown" />时间未知</span><span><i className="wl-legend-conflict" />冲突</span></div>

    {viewMode === 'validity' && loading && !data ? <div className="wl-state" role="status"><i className="wl-spinner" />正在整理主题世界线…</div> : viewMode === 'validity' && error && !data ? <div className="wl-state wl-state-error" role="alert"><strong>无法载入世界线</strong><p>{error}</p><button onClick={() => setRefreshKey((v) => v + 1)}>重试</button></div> : null}
    {viewMode === 'validity' && error && data && <div className="wl-inline-error" role="alert">更新失败：{error}<button className="link-button" onClick={() => setRefreshKey((v) => v + 1)}>重试</button></div>}

    {viewMode === 'threads' && <WorldlineThreadsView query={query} refreshKey={refreshKey} zoom={zoom} setZoom={setZoom} windowCenter={windowCenter} setWindowCenter={setWindowCenter} status={status} onStats={setThreadStats} onRetry={() => setRefreshKey((value) => value + 1)} onClearSearch={() => { setSearch(''); setQuery(''); setOffset(0) }} onFeedback={(action, topic, lane, claim, version) => setFeedback({ action, topic, lane, claim, version })} />}

    {viewMode === 'validity' && data && <div className={`wl-content ${selected ? 'has-selection' : ''}`}>
      <div className="wl-board-wrap">
        <div className="wl-board" aria-label="主题世界线图">
          <div className="wl-axis-row"><div className="wl-axis-label">主题 / 主张</div><div className="wl-axis-track">{tickValues.map((tick, index) => <span key={index} style={{ left: `${index / (ticks - 1) * 100}%` }}>{dayLabel(tick, span > 365 * 86400000)}</span>)}</div></div>
          {topics.map((topic) => {
            const isOpen = expanded[topic.id] ?? true
            const topicClaims = topic.lanes.flatMap((lane) => lane.claims)
            return <section className="wl-topic" key={topic.id}>
              <header className="wl-topic-head">
                <button className="wl-topic-toggle" aria-expanded={isOpen} onClick={() => toggleTopic(topic.id)}><span className="wl-chevron">{isOpen ? '⌄' : '›'}</span><span className="wl-topic-name">{topic.title}</span><span className="wl-topic-count">{topicClaims.length} 条</span></button>
                <div className="wl-topic-actions"><span className="wl-topic-count">{topic.source_topic_count ?? topicClaims.length} 个来源主题</span></div>
              </header>
              {isOpen && topic.lanes.map((lane) => <LaneView key={lane.id} topic={topic} lane={lane} start={viewStart} end={viewEnd} onSelect={setSelected} onFeedback={openFeedback} />)}
              {isOpen && topicClaims.some((claim) => dateValue(claim.mentioned_at) !== undefined) && <MentionTrack claims={topicClaims} start={viewStart} end={viewEnd} onSelect={setSelected} />}
              {isOpen && topic.has_more && <div className="wl-more-claims"><button className="secondary-button" disabled={loadingTopic === topic.id} onClick={() => void loadMoreClaims(topic)}>{loadingTopic === topic.id ? '载入中…' : '加载更多主张'}</button></div>}
            </section>
          })}
        </div>
        {!loading && data.topics.length === 0 && <div className="wl-state wl-empty"><div className="wl-empty-icon">◷</div><strong>{query ? '没有匹配的主题或主张' : '还没有可展示的主题主张'}</strong><p>主题、来源和时间信息出现后，会在这里组成可核对的世界线。</p>{query && <button className="secondary-button" onClick={() => { setSearch(''); setQuery('') }}>清除搜索</button>}</div>}
        {data.topics.length > 0 && <div className="wl-pagination"><span>显示 {offset + 1}–{Math.min(offset + 100, data.total)} / {data.total} 个主题</span><button className="secondary-button" disabled={offset === 0 || loading} onClick={() => setOffset(Math.max(0, offset - 100))}>上一页</button><button className="secondary-button" disabled={!hasMore || loading} onClick={() => setOffset(data.next_offset ?? offset + 100)}>下一页</button></div>}
      </div>
      {topicLoadError && <div className="wl-inline-error" role="alert">{topicLoadError}</div>}
      {selected && <ClaimInspector claim={selected} topic={topics.find((topic) => topic.lanes.some((lane) => lane.claims.some((item) => item.id === selected.id)))} lane={topics.flatMap((topic) => topic.lanes).find((lane) => lane.claims.some((item) => item.id === selected.id))} play={play} onClose={() => setSelected(null)} onFeedback={openFeedback} />}
      <UnknownPanel claims={claims.filter((claim) => dateValue(claim.valid_from) === undefined && dateValue(claim.valid_until) === undefined)} onSelect={setSelected} />
    </div>}
    {feedback && <WorldlineFeedbackForm action={feedback.action} topic={feedback.topic} lane={feedback.lane} claim={feedback.claim} destinations={topics.flatMap((macro) => macro.lanes.map((lane) => ({ macro: macro.title, lane })))} version={feedback.version || data?.version || ''} onClose={() => setFeedback(null)} onSaved={() => { setFeedback(null); setRefreshKey((v) => v + 1) }} />}
  </section>
}

function WorldlineThreadsView({ query, refreshKey, zoom, setZoom, windowCenter, setWindowCenter, status, onStats, onRetry, onClearSearch, onFeedback }: {
  query: string
  refreshKey: number
  zoom: Zoom
  setZoom: (zoom: Zoom) => void
  windowCenter: number | null
  setWindowCenter: React.Dispatch<React.SetStateAction<number | null>>
  status: WorldlineStatus
  onStats: (stats: { total: number; crossConversation: number }) => void
  onRetry: () => void
  onClearSearch: () => void
  onFeedback: (action: WorldlineAction, topic: WorldlineTopic, lane: WorldlineLane, claim: WorldlineClaim, version: string) => void
}) {
  const [data, setData] = useState<WorldlineThreadsResponse | null>(null)
  const [details, setDetails] = useState<Record<string, WorldlineThreadDetail>>({})
  const [expanded, setExpanded] = useState<Record<string, boolean>>({})
  const [groupSelection, setGroupSelection] = useState<string[]>([])
  const [selected, setSelected] = useState<{ thread: WorldlineThreadSummary; entry: WorldlineThreadEntry } | null>(null)
  const [offset, setOffset] = useState(0)
  const [loading, setLoading] = useState(true)
  const [loadingMore, setLoadingMore] = useState('')
  const [error, setError] = useState('')
  const [detailErrors, setDetailErrors] = useState<Record<string, string>>({})
  const [visibleEntries, setVisibleEntries] = useState<Record<string, number>>({})
  const [visibleUnknownEntries, setVisibleUnknownEntries] = useState<Record<string, number>>({})
  const requestGeneration = useRef(0)
  const pageControllers = useRef(new Set<AbortController>())
  const { play } = useAudio()

  useEffect(() => {
    const generation = ++requestGeneration.current
    const controller = new AbortController()
    setLoading(true)
    setError('')
    setData(null)
    onStats({ total: 0, crossConversation: 0 })
    setDetails({})
    setDetailErrors({})
    setSelected(null)
    setExpanded({})
    void getWorldlineThreads({ q: query, status, offset, limit: 8 }, controller.signal)
      .then(async (result) => {
        if (controller.signal.aborted || requestGeneration.current !== generation) return
        setData(result)
        onStats({ total: result.total, crossConversation: result.cross_conversation_threads || 0 })
        if (result.threads.length) setExpanded({ [result.threads[0].id]: true })
        const pages = await Promise.all(result.threads.map(async (thread) => {
          try { return [thread.id, await getWorldlineThread(thread.id, { limit: 25, q: query, status }, controller.signal)] as const }
          catch (cause) { return [thread.id, cause instanceof Error ? cause.message : '无法载入提及历史'] as const }
        }))
        if (controller.signal.aborted || requestGeneration.current !== generation) return
        const nextDetails: Record<string, WorldlineThreadDetail> = {}
        const nextErrors: Record<string, string> = {}
        for (const [id, value] of pages) {
          if (typeof value === 'string') nextErrors[id] = value
          else nextDetails[id] = value
        }
        setDetails(nextDetails)
        setDetailErrors(nextErrors)
        setVisibleEntries(Object.fromEntries(result.threads.map((thread) => [thread.id, 5])))
        setVisibleUnknownEntries(Object.fromEntries(result.threads.map((thread) => [thread.id, 5])))
      })
      .catch((cause) => { if (!controller.signal.aborted && requestGeneration.current === generation) setError(cause instanceof Error ? cause.message : '无法载入主题提及历史') })
      .finally(() => { if (!controller.signal.aborted && requestGeneration.current === generation) setLoading(false) })
    return () => {
      controller.abort()
      for (const pending of pageControllers.current) pending.abort()
      pageControllers.current.clear()
      if (requestGeneration.current === generation) requestGeneration.current++
    }
  }, [query, offset, refreshKey, onStats, status])

  useEffect(() => { setOffset(0) }, [query, status])

  const datedTimes = Object.values(details).flatMap((detail) => detail.entries.map((entry) => dateValue(entry.mentioned_at)).filter((value): value is number => value !== undefined))
  const allStart = dateValue(data?.range.mentioned_start) ?? (datedTimes.length ? Math.min(...datedTimes) : Date.now() - 86400000 * 30)
  const allEnd = dateValue(data?.range.mentioned_end) ?? (datedTimes.length ? Math.max(...datedTimes) : Date.now())
  const focus = windowCenter ?? allStart + Math.max(1, allEnd - allStart) / 2
  const start = zoom === 'all' ? allStart : focus - ZOOM_MS[zoom] / 2
  const end = zoom === 'all' ? allEnd : focus + ZOOM_MS[zoom] / 2
  const panStep = zoom === 'all' ? 0 : ZOOM_MS[zoom]
  const span = Math.max(end - start, 1)
  const ticks = Array.from({ length: 6 }, (_, index) => start + span * index / 5)
  const hasMore = data?.next_offset !== undefined && data.next_offset !== null
  const categories = useMemo(() => {
    const groups = new Map<string, WorldlineThreadSummary[]>()
    for (const thread of data?.threads || []) {
      const key = thread.macro_topic_title || '待归类'
      groups.set(key, [...(groups.get(key) || []), thread])
    }
    return [...groups.entries()]
  }, [data])

  function position(at?: string | null): number | null {
    const value = dateValue(at)
    if (value === undefined || value < start || value > end) return null
    return ((value - start) / span) * 100
  }
  function selectEntry(thread: WorldlineThreadSummary, entry: WorldlineThreadEntry) {
    setSelected({ thread, entry })
    setExpanded((current) => ({ ...current, [thread.id]: true }))
  }
  async function loadMore(thread: WorldlineThreadSummary): Promise<boolean> {
    const current = details[thread.id]
    if (!current) return false
    const generation = requestGeneration.current
    const controller = new AbortController()
    pageControllers.current.add(controller)
    setLoadingMore(thread.id)
    setDetailErrors((previous) => ({ ...previous, [thread.id]: '' }))
    try {
      const page = await getWorldlineThread(thread.id, {
        q: query,
        status,
        entries_offset: current.entries.length,
        unknown_offset: current.unknown_entries.length,
        limit: 25,
      }, controller.signal)
      if (generation !== requestGeneration.current) return false
      setDetails((previous) => ({
        ...previous,
        [thread.id]: {
          ...page,
          entries: [...current.entries, ...page.entries.filter((entry) => !current.entries.some((old) => old.id === entry.id))],
          unknown_entries: [...current.unknown_entries, ...page.unknown_entries.filter((entry) => !current.unknown_entries.some((old) => old.id === entry.id))],
        },
      }))
      return true
    } catch (cause) { if (generation === requestGeneration.current && !controller.signal.aborted) setDetailErrors((previous) => ({ ...previous, [thread.id]: cause instanceof Error ? cause.message : '无法载入更多提及历史' })); return false }
    finally {
      pageControllers.current.delete(controller)
      if (generation === requestGeneration.current) setLoadingMore('')
    }
  }

  async function fetchNextPage(thread: WorldlineThreadSummary) {
    const visible = visibleEntries[thread.id] ?? 5
    if (await loadMore(thread)) setVisibleEntries((previous) => ({ ...previous, [thread.id]: visible + 5 }))
  }

  async function renameThread(thread: WorldlineThreadSummary) {
    const next = window.prompt('主题别名（留空可恢复来源名称）', thread.title)
    if (next === null || !data) return
    const requestID = `worldline-rename-${crypto.randomUUID()}`
    try { await renameWorldlineThread(thread.id, next, data.version, requestID); onRetry() }
    catch (cause) { setError(cause instanceof Error ? cause.message : '重命名失败') }
  }

  async function groupSelectedThreads() {
    if(!data||groupSelection.length<2)return
    const title=window.prompt('新分组名称',groupSelection.map(id=>data.threads.find(t=>t.id===id)?.title).filter(Boolean).join(' + '));if(title===null)return
    try{await groupWorldlineThreads(groupSelection,title,data.version,`worldline-group-${crypto.randomUUID()}`);setGroupSelection([]);onRetry()}catch(cause){setError(cause instanceof Error?cause.message:'分组失败')}
  }
  async function revokeGroup(thread:WorldlineThreadSummary){if(!data)return;try{await revokeWorldlineThreadGroup(thread.id,data.version,`worldline-ungroup-${crypto.randomUUID()}`);onRetry()}catch(cause){setError(cause instanceof Error?cause.message:'撤销分组失败')}}

  function feedbackTarget(thread: WorldlineThreadSummary, entry: WorldlineThreadEntry, action: WorldlineAction, version: string) {
    const claim = entry.claim
    const lane: WorldlineLane = { id: entry.lane_id, title: thread.title, source_topic_ids: thread.source_topic_ids, subject: 'unknown', category: entry.category || 'unclassified', claims: [claim] }
    const topic: WorldlineTopic = { id: thread.macro_topic_id, title: thread.macro_topic_title, source_topic_ids: thread.source_topic_ids, lanes: [lane] }
    onFeedback(action, topic, lane, claim, version)
  }

  const selectedClaim = selected?.entry.claim
  const selectedLane: WorldlineLane | undefined = selected && selectedClaim ? { id: selected.entry.lane_id, title: selected.thread.title, source_topic_ids: selected.thread.source_topic_ids, subject: 'unknown', category: selected.entry.category || 'unclassified', claims: [selectedClaim] } : undefined
  const selectedTopic: WorldlineTopic | undefined = selected && selectedLane ? { id: selected.thread.macro_topic_id, title: selected.thread.macro_topic_title, source_topic_ids: selected.thread.source_topic_ids, lanes: [selectedLane] } : undefined

  if (loading && !data) return <div className="wl-state" role="status"><i className="wl-spinner" />正在整理跨会话提及脉络…</div>
  if (error && !data) return <div className="wl-state wl-state-error" role="alert"><strong>无法载入提及脉络</strong><p>{error}</p><button onClick={onRetry}>重试</button></div>
  if (!data || !data.threads.length) return <div className="wl-state wl-empty"><div className="wl-empty-icon">◷</div><strong>{query ? '没有匹配的具体主题' : '还没有跨会话主题'}</strong><p>同一具体主题在多段录音中的提及会按原始标题串联；提及日期不会被当作事实生效日期。</p>{query && <button className="secondary-button" onClick={onClearSearch}>清除搜索</button>}</div>

  return <div className={`wl-thread-content ${selected ? 'has-selection' : ''}`}>
    <div className={`wl-thread-main ${selected ? 'has-selection' : ''}`}>
      <div className="wl-thread-explainer"><strong>当时这样说过，不代表事实生效</strong><span>按具体主题串联多次对话；主题分类仅帮助浏览。同主题不代表同一说话人。</span></div>
      <div className="wl-thread-group-controls"><span>{groupSelection.length} 个主题已选</span><button type="button" disabled={groupSelection.length<2} onClick={() => void groupSelectedThreads()}>手动分组</button><button type="button" className="secondary-button" onClick={() => setGroupSelection([])}>清除选择</button></div>
      <div className="wl-thread-legend"><span><i className="wl-thread-marker" />录音提及</span><span><i className="wl-thread-unknown-marker" />提及时间未知</span><span>事实适用时间请切换到“事实适用时间”视图确认</span></div>
      <div className="wl-thread-time-controls"><div className="wl-zoom" aria-label="提及时间缩放"><button type="button" className={zoom === 'all' ? 'selected' : ''} onClick={() => { setZoom('all'); setWindowCenter(null) }}>全范围</button><button type="button" className={zoom === 'day' ? 'selected' : ''} onClick={() => { setZoom('day'); setWindowCenter(allStart + (allEnd - allStart) / 2) }}>日</button><button type="button" className={zoom === 'week' ? 'selected' : ''} onClick={() => { setZoom('week'); setWindowCenter(allStart + (allEnd - allStart) / 2) }}>周</button><button type="button" className={zoom === 'month' ? 'selected' : ''} onClick={() => { setZoom('month'); setWindowCenter(allStart + (allEnd - allStart) / 2) }}>月</button></div><div className="wl-pan"><button aria-label="向前浏览提及" type="button" disabled={zoom === 'all'} onClick={() => setWindowCenter((windowCenter ?? allStart + (allEnd - allStart) / 2) - panStep)}>←</button><button aria-label="向后浏览提及" type="button" disabled={zoom === 'all'} onClick={() => setWindowCenter((windowCenter ?? allStart + (allEnd - allStart) / 2) + panStep)}>→</button></div></div>
      <div className="wl-thread-board-wrap">
        <div className="wl-thread-board">
          <div className="wl-axis-row"><div className="wl-axis-label">具体主题 / 跨会话提及</div><div className="wl-axis-track">{ticks.map((tick, index) => <span key={index} style={{ left: `${index / 5 * 100}%` }}>{dayLabel(tick, span > 365 * 86400000)}</span>)}</div></div>
          {categories.map(([category, threads]) => <section className="wl-thread-category" key={category}>
            <header className="wl-thread-category-head"><span>{category}</span><small>{threads.length} 个主题</small></header>
            {threads.map((thread) => {
              const detail = details[thread.id]
              const entries = detail?.entries || []
              const unknownEntries = detail?.unknown_entries || []
              const allThreadEntries = [...entries, ...unknownEntries]
              const unknownValidity = [...entries, ...unknownEntries].filter((entry) => !dateValue(entry.claim.valid_from) && !dateValue(entry.claim.valid_until))
              const isExpanded = expanded[thread.id] ?? false
              return <article className={`wl-thread-row ${isExpanded ? 'is-expanded' : ''}`} key={thread.id}>
                <div className="wl-thread-label"><label><input type="checkbox" aria-label={`选择主题 ${thread.title} 用于分组`} checked={groupSelection.includes(thread.id)} onChange={(event) => setGroupSelection((current) => event.target.checked ? [...current,thread.id] : current.filter(id=>id!==thread.id))}/></label><button type="button" className="wl-thread-toggle" aria-expanded={isExpanded} onClick={() => setExpanded((current) => ({ ...current, [thread.id]: !isExpanded }))}><span className="wl-chevron">{isExpanded ? '⌄' : '›'}</span><span>{thread.title}</span></button>{thread.manual_group&&<button type="button" className="secondary-button" onClick={() => void revokeGroup(thread)}>撤销分组</button>}<button type="button" className="secondary-button" onClick={() => void renameThread(thread)}>重命名</button>{(thread.semantic_review_count || 0) > 0 && <small className="wl-thread-review-count">{thread.semantic_review_count} 处关系待核对</small>}<small>{thread.conversation_count} 段会话 · {thread.entry_count} 条提及</small><small>{unknownValidity.length ? `适用时间未知 ${unknownValidity.length} 条` : '适用时间见主张详情'}</small></div>
                <div className="wl-thread-track">{Array.from({ length: 5 }, (_, index) => <i key={index} className="wl-grid-line" style={{ left: `${index * 25}%` }} />)}{entries.map((entry, index) => { const left = position(entry.mentioned_at); return left === null ? null : <button key={entry.id} type="button" className={`wl-thread-marker relation-${entry.relation}`} style={{ left: `${left}%`, top: `${12 + (index % 3) * 11}px` }} title={`${dayLabel(Date.parse(entry.mentioned_at!), true)} · ${entry.summary} · 当时这样说过，不代表事实生效`} aria-label={`${thread.title}，${dayLabel(Date.parse(entry.mentioned_at!), true)}提及：${entry.summary}；不代表事实生效`} onClick={() => selectEntry(thread, entry)} /> })}</div>
                {isExpanded && <div className="wl-thread-expanded">
                  {detailErrors[thread.id] && <p className="wl-inline-error" role="alert">{detailErrors[thread.id]} <button className="link-button" onClick={() => void loadMore(thread)}>重试</button></p>}
                  {!detail && !detailErrors[thread.id] && <p className="wl-muted">正在载入来源片段…</p>}
                  {entries.length > 0 ? <ol className="wl-thread-entries">{entries.slice(0, visibleEntries[thread.id] ?? 5).map((entry) => <li key={entry.id} className={selected?.entry.id === entry.id ? 'selected' : ''}><button type="button" onClick={() => selectEntry(thread, entry)}><time>{entry.mentioned_at ? dayLabel(Date.parse(entry.mentioned_at), true) : '提及日期未知'}</time><span className="wl-relation-label">{relationLabel(entry.relation)}</span><strong>{entry.summary}</strong><small>同主题不代表同一说话人 · {entry.conversation_ids.length || 1} 段来源会话</small><blockquote>{entry.evidence[0]?.text || entry.summary}</blockquote></button>{entry.semantic_suggestion && <SemanticSuggestionCard entry={entry} threadID={thread.id} threadEntries={allThreadEntries} version={data.version} onSelect={(source) => selectEntry(thread, source)} onReviewed={onRetry} />}</li>)}</ol> : <p className="wl-muted">目前没有带明确提及日期的来源。</p>}
                  {visibleEntries[thread.id] < entries.length && <button className="secondary-button wl-thread-more" type="button" onClick={() => setVisibleEntries((previous) => ({ ...previous, [thread.id]: (previous[thread.id] ?? 5) + 5 }))}>再显示 5 条提及</button>}
                  {unknownEntries.length > 0 && <section className="wl-thread-unknown"><h3>提及时间未知 <span>{detail?.unknown_entries_total ?? unknownEntries.length}</span></h3>{unknownEntries.slice(0, visibleUnknownEntries[thread.id] ?? 5).map((entry) => <button key={entry.id} type="button" onClick={() => selectEntry(thread, entry)}><strong>{entry.summary}</strong><small>提及时间未知；适用时间{dateValue(entry.claim.valid_from) || dateValue(entry.claim.valid_until) ? '另见详情' : '也未知'}</small></button>)}</section>}
                  {visibleUnknownEntries[thread.id] < unknownEntries.length && <button className="secondary-button wl-thread-more" type="button" onClick={() => setVisibleUnknownEntries((previous) => ({ ...previous, [thread.id]: (previous[thread.id] ?? 5) + 5 }))}>再显示 5 条未定位提及</button>}
                  {detail && ((detail.entries_next_offset !== undefined) || (detail.unknown_entries_next_offset !== undefined)) && <button className="secondary-button wl-thread-more" type="button" disabled={loadingMore === thread.id} onClick={() => void fetchNextPage(thread)}>{loadingMore === thread.id ? '载入中…' : '加载更多提及'}</button>}
                </div>}
              </article>
            })}
          </section>)}
        </div>
        <div className="wl-pagination"><span>显示 {offset + 1}–{Math.min(offset + 8, data.total)} / {data.total} 个具体主题</span><button className="secondary-button" disabled={offset === 0 || loading} onClick={() => setOffset(Math.max(0, offset - 8))}>上一页</button><button className="secondary-button" disabled={!hasMore || loading} onClick={() => setOffset(data.next_offset ?? offset + 8)}>下一页</button></div>
      </div>
      {error && <div className="wl-inline-error" role="alert">{error}</div>}
      {selected && selectedTopic && selectedLane && selectedClaim && <ClaimInspector claim={selectedClaim} topic={selectedTopic} lane={selectedLane} play={play} onClose={() => setSelected(null)} onFeedback={(action) => feedbackTarget(selected.thread, selected.entry, action, data.version)} mentionContext={{ relation: relationLabel(selected.entry.relation), date: selected.entry.mentioned_at || undefined }} />}
      {selected && <DecisionTrailPanel topicId={selected.thread.macro_topic_id} topicTitle={selected.thread.macro_topic_title} />}
    </div>
    <UnknownValidityThreadPanel threads={data.threads} details={details} onSelect={selectEntry} />
  </div>
}

function relationLabel(relation: string): string {
  const labels: Record<string, string> = {
    first_mention: '首次提及', subsequent_mention: '后续提及', repeated_statement: '重复陈述', plan: '计划或意向',
    human_correction: '人工修正', human_reality_change: '人工记录现实变化', review_required: '需要人工核对', insufficient_evidence: '无法判断，来源信息不足',
  }
  return labels[relation] || '来源提及'
}

function reviewStatusLabel(status?: string): string { return ({ accept:'已接受',reject:'已拒绝',insufficient:'证据不足',reopen:'重新核对' } as Record<string,string>)[status || ''] || '已处理' }

function SemanticSuggestionCard({ entry, threadID, threadEntries, version, onSelect, onReviewed }: {
  entry: WorldlineThreadEntry
  threadID: string
  threadEntries: WorldlineThreadEntry[]
  version: string
  onSelect: (source: WorldlineThreadEntry) => void
  onReviewed: () => void
}) {
  const suggestion = entry.semantic_suggestion
  const [pendingDecision, setPendingDecision] = useState<'accept'|'reject'|'insufficient'|'reopen'|null>(null)
  const [reviewError, setReviewError] = useState('')
  const [draftDecision, setDraftDecision] = useState<'accept'|'reject'|'insufficient'|'reopen'|null>(null)
  const reviewRequest = useRef<{ decision: string; id: string } | null>(null)
  const [reviewVersion, setReviewVersion] = useState(version)
  const [sourceDiffs, setSourceDiffs] = useState<Array<{label:string;before:string;after:string;evidence?:string}>>([])
  useEffect(() => setReviewVersion(version), [version])
  if (!suggestion) return null
  const reviewableSuggestion = suggestion
  const sources = (suggestion.source_claim_ids || []).slice(0, 2).map((claimID) => threadEntries.find((candidate) => candidate.claim_id === claimID))
  const sourceDetails = [
    { label: '较早来源', summary: suggestion.earlier_summary, evidence: suggestion.earlier_evidence || [], entry: sources[0] },
    { label: '较新来源', summary: suggestion.later_summary, evidence: suggestion.later_evidence || [], entry: sources[1] },
  ]
  async function decide(decision: 'accept'|'reject'|'insufficient'|'reopen') { if (!reviewableSuggestion.suggestion_id) return; if (!reviewRequest.current || reviewRequest.current.decision!==decision) reviewRequest.current={decision,id:`worldline-review-${crypto.randomUUID()}`};setDraftDecision(decision);setPendingDecision(decision);setReviewError('');try { await reviewWorldlineSemantic(reviewableSuggestion.suggestion_id,decision,reviewVersion,reviewRequest.current.id);reviewRequest.current=null;setDraftDecision(null);setSourceDiffs([]);onReviewed() } catch (cause) { setReviewError(cause instanceof Error ? cause.message : '复核提交失败；当前选择仍保留，可重试。');if(cause instanceof ApiError&&cause.status===409){try{const [latestList,latestDetail]=await Promise.all([getWorldlineThreads({limit:1}),getWorldlineThread(threadID,{limit:500})]);setReviewVersion(latestList.version);const current=[...latestDetail.entries,...latestDetail.unknown_entries];setSourceDiffs([{label:'较早来源',before:reviewableSuggestion.earlier_summary||'',after:current.find(x=>x.claim_id===reviewableSuggestion.source_claim_ids[0])?.summary||'当前来源已不存在',evidence:current.find(x=>x.claim_id===reviewableSuggestion.source_claim_ids[0])?.evidence[0]?.text},{label:'较新来源',before:reviewableSuggestion.later_summary||'',after:current.find(x=>x.claim_id===reviewableSuggestion.source_claim_ids[1])?.summary||'当前来源已不存在',evidence:current.find(x=>x.claim_id===reviewableSuggestion.source_claim_ids[1])?.evidence[0]?.text}])}catch{setReviewError('版本已变化，当前决定仍保留；刷新页面后可查看最新来源。')}}} finally { setPendingDecision(null) } }
  return <section className="wl-semantic-suggestion" aria-label="语义建议，待人工核对">
    <header><strong>语义建议 · {suggestion.requires_review ? '待核对' : reviewStatusLabel(suggestion.review_status)}</strong><span>{relationLabel(suggestion.relation)}</span></header>
    <p>这只是对来源关系的建议，不表示事实成立，也不会自动修改主题或记录。</p>
    <small>模型：{suggestion.model || '未返回模型名称'}</small>
    {reviewError && <p className="wl-inline-error" role="alert">{reviewError}</p>}
    {sourceDiffs.length>0&&<div className="wl-semantic-sources" aria-label="最新来源对比">{sourceDiffs.map(source=><div key={source.label}><span>{source.label} · 最新来源</span><small>建议时：{source.before||'未提供'}</small><strong>当前：{source.after}</strong>{source.evidence&&<blockquote>{source.evidence}</blockquote>}</div>)}</div>}
    {draftDecision && reviewError && <button type="button" disabled={pendingDecision!==null} onClick={() => void decide(draftDecision)}>重试{reviewStatusLabel(draftDecision)}决定</button>}
    {suggestion.requires_review ? <div className="wl-review-actions"><button type="button" disabled={pendingDecision!==null} onClick={() => void decide('accept')}>接受关系建议</button><button type="button" disabled={pendingDecision!==null} onClick={() => void decide('reject')}>拒绝</button><button type="button" disabled={pendingDecision!==null} onClick={() => void decide('insufficient')}>证据不足</button></div> : <button type="button" disabled={pendingDecision!==null} onClick={() => void decide('reopen')}>重新打开核对</button>}
    <div className="wl-semantic-sources">
      {sourceDetails.map((source, index) => <div key={source.label}>
        <span>{source.label}</span>
        <strong>{source.summary || source.entry?.summary || '来源摘要未返回'}</strong>
        {source.evidence[0]?.text && <blockquote>{source.evidence[0].text}</blockquote>}
        {source.entry && <button type="button" onClick={() => onSelect(source.entry!)}>查看这条整理记录</button>}
        {source.evidence[0]?.conversation_id && <Link to={`/c/${encodeURIComponent(source.evidence[0].conversation_id)}`}>打开来源对话</Link>}
        {!source.entry && !source.evidence[0]?.conversation_id && <p>来源详情暂不可用。</p>}
      </div>)}
    </div>
  </section>
}

function generationSemanticLabel(result: Awaited<ReturnType<typeof startWorldlineAnalysis>> | null): string {
  if (!result?.semantic_status) return '整理完成；语义引擎状态未返回，不能确认是否生成了建议。'
  if (result.semantic_status === 'not_configured') return '整理完成；语义引擎未配置，未生成语义建议。'
  if (result.semantic_status === 'failed') return '整理完成；语义评估失败，未确认建议结果。'
  if (result.semantic_status === 'no_candidates') return '整理完成；没有符合条件的跨会话来源对。'
  if (result.semantic_status === 'evaluated') {
    const model = result.semantic_model?.trim() || '模型名称未返回'
    const capped = result.semantic_truncated ? '（候选较多，本次仅处理部分）' : ''
    return `整理完成 · ${model} · ${result.semantic_suggestion_count ?? 0} 条建议待核对${capped}；不会自动改写事实。`
  }
  return `整理完成；语义状态：${result.semantic_status}。`
}

function UnknownValidityThreadPanel({ threads, details, onSelect }: { threads: WorldlineThreadSummary[]; details: Record<string, WorldlineThreadDetail>; onSelect: (thread: WorldlineThreadSummary, entry: WorldlineThreadEntry) => void }) {
  const unknown = threads.flatMap((thread) => [...(details[thread.id]?.entries || []), ...(details[thread.id]?.unknown_entries || [])].filter((entry) => !dateValue(entry.claim.valid_from) && !dateValue(entry.claim.valid_until)).map((entry) => ({ thread, entry })))
  return <section className="wl-unknown-panel wl-thread-unknown-validity" aria-label="适用时间未知的提及"><header><span className="wl-unknown-icon">?</span><div><h2>适用时间未知</h2><p>提及节点仍保留在脉络中；这些来源没有可确认的事实生效日期。</p></div><b>{unknown.length}</b></header>{unknown.length === 0 ? <p className="wl-unknown-empty">加载的主题中暂未发现此类主张。</p> : <div className="wl-unknown-list">{unknown.map(({ thread, entry }) => <button key={`${thread.id}:${entry.id}`} type="button" onClick={() => onSelect(thread, entry)}><span className={`wl-status-dot status-${entry.status}`} /><span>{thread.title} · {entry.summary}</span><small>{entry.mentioned_at ? `当时提及 ${dayLabel(Date.parse(entry.mentioned_at), true)}，不代表适用时间` : '提及时间也未知'}</small></button>)}</div>}</section>
}

function LaneView({ topic, lane, start, end, onSelect, onFeedback }: { topic: WorldlineTopic; lane: WorldlineLane; start: number; end: number; onSelect: (claim: WorldlineClaim) => void; onFeedback: (action: WorldlineAction, topic: WorldlineTopic, lane: WorldlineLane, claim?: WorldlineClaim) => void }) {
  const { play } = useAudio()
  return <div className={`wl-lane wl-lane-${lane.category}`}>
    <div className="wl-lane-label"><span>{CATEGORY_LABELS[lane.category] || lane.category}</span><small>{lane.subject || '主题'} · {lane.title}</small><div className="wl-lane-actions"><button className="wl-text-button" type="button" onClick={() => onFeedback('rename_topic', topic, lane)}>重命名</button><button className="wl-text-button" type="button" onClick={() => onFeedback('merge_topics', topic, lane)}>合并</button><button className="wl-text-button" type="button" onClick={() => onFeedback('split_topic', topic, lane)}>拆分</button></div></div>
    <div className="wl-lane-track">
      {Array.from({ length: 5 }, (_, i) => <i key={i} className="wl-grid-line" style={{ left: `${i * 25}%` }} />)}
      {lane.claims.filter((claim) => claimPosition(claim, start, end) !== null).map((claim) => {
        const position = claimPosition(claim, start, end)
        if (!position) return null
        return <div className={`wl-claim-group ${claim.status === 'conflict' ? 'is-conflict' : ''}`} key={claim.id}>
          <button type="button" className={`wl-claim-bar ${claim.status === 'conflict' ? 'is-conflict' : ''} ${claim.change_type === 'reality_change' ? 'is-change' : ''}`} style={{ left: `${position.left}%`, width: `${position.width}%` }} title={`${claim.summary} · 适用时间 ${claim.valid_from ? dayLabel(Date.parse(claim.valid_from), true) : '开放'}${claim.valid_until ? ` 至 ${dayLabel(Date.parse(claim.valid_until), true)}` : ''}`} onClick={() => onSelect(claim)}>{position.width > 10 ? claim.summary : ''}</button>
        </div>
      })}
    </div>
  </div>
}

function MentionTrack({ claims, start, end, onSelect }: { claims: WorldlineClaim[]; start: number; end: number; onSelect: (claim: WorldlineClaim) => void }) {
  const mentioned = claims.filter((claim) => { const at = dateValue(claim.mentioned_at); return at !== undefined && at >= start && at <= end })
  if (!mentioned.length) return null
  return <div className="wl-mention-track-row"><div className="wl-lane-label"><span>原文提及</span><small>不是适用时间</small></div><div className="wl-mention-track">{Array.from({ length: 5 }, (_, index) => <i key={index} className="wl-grid-line" style={{ left: `${index * 25}%` }} />)}{mentioned.map((claim, index) => <button key={claim.id} type="button" className="wl-mention-track-dot" style={{ left: `${((Date.parse(claim.mentioned_at!) - start) / Math.max(end - start, 1)) * 100}%`, top: `${8 + (index % 3) * 10}px` }} title={`${dayLabel(Date.parse(claim.mentioned_at!), true)} · ${claim.summary} · 提及时间，不代表适用时间`} aria-label={`${claim.summary}，原文提及时间 ${dayLabel(Date.parse(claim.mentioned_at!), true)}`} onClick={() => onSelect(claim)} />)}</div></div>
}

function UnknownPanel({ claims, onSelect }: { claims: WorldlineClaim[]; onSelect: (claim: WorldlineClaim) => void }) {
  return <section className="wl-unknown-panel" aria-label="适用时间未知的主张"><header><span className="wl-unknown-icon">?</span><div><h2>适用时间未知</h2><p>这些主张没有明确的适用时间；提及时间不会替代它。</p></div><b>{claims.length}</b></header>
    {claims.length === 0 ? <p className="wl-unknown-empty">当前结果中的主张都有适用时间。</p> : <div className="wl-unknown-list">{claims.map((claim) => <button type="button" key={claim.id} onClick={() => onSelect(claim)}><span className={`wl-status-dot status-${claim.status}`} /><span>{claim.summary}</span><small>{claim.mentioned_at ? `原文提及 ${dayLabel(Date.parse(claim.mentioned_at), true)}` : '未记录提及时间'}</small></button>)}</div>}
  </section>
}

function ClaimInspector({ claim, topic, lane, play, onClose, onFeedback, mentionContext }: { claim: WorldlineClaim; topic?: WorldlineTopic; lane?: WorldlineLane; play: (id: string, timing?: { startMs: number | null; endMs: number | null }) => void; onClose: () => void; onFeedback: (action: WorldlineAction, topic: WorldlineTopic, lane: WorldlineLane, claim?: WorldlineClaim) => void; mentionContext?: { relation: string; date?: string } }) {
  const [showAllEvidence, setShowAllEvidence] = useState(false)
  const [audioStates, setAudioStates] = useState<Record<string, string>>({})
  useEffect(() => { setShowAllEvidence(false) }, [claim.id])
  const evidence = showAllEvidence ? claim.evidence : claim.evidence.slice(0, 3)
  useEffect(() => {
    let alive = true
    const requested = claim.evidence.slice(0, 3)
    void Promise.all(requested.map(async (item) => {
      if (item.audio_status) return [item.turn_id, item.audio_status] as const
      try {
        const detail = await getItem(item.turn_id)
        return [item.turn_id, detail.item.audio_status || 'unavailable'] as const
      } catch { return [item.turn_id, 'unverified'] as const }
    })).then((entries) => { if (alive) setAudioStates(Object.fromEntries(entries)) })
    return () => { alive = false }
  }, [claim.id, claim.evidence])
  if (!topic || !lane) return null
  return <aside className="wl-inspector" aria-label="主张详情">
    <header><div><span className="wl-eyebrow">主张详情</span><h2>{topic.title}</h2></div><button type="button" className="wl-icon-button" aria-label="关闭详情" onClick={onClose}>×</button></header>
    <div className="wl-inspector-body">
      {mentionContext && <div className="wl-thread-inspector-callout"><strong>当时这样说过，不代表事实生效</strong><span>{mentionContext.date ? dayLabel(Date.parse(mentionContext.date), true) : '提及日期未知'} · {mentionContext.relation} · 同主题不代表同一说话人</span><small>这里的复核、更正和时间设置作用于这条记录，不会重命名或移动主题。</small></div>}
      <div className="wl-claim-title"><span className={`wl-status-pill status-${claim.status}`}>{STATUS_LABELS[claim.status] || claim.status}</span><p>{claim.summary}</p></div>
      <dl className="wl-time-facts"><div><dt>适用时间</dt><dd>{claim.valid_from ? dayLabel(Date.parse(claim.valid_from), true) : '未知 / 开放'}{claim.valid_until ? ` — ${dayLabel(Date.parse(claim.valid_until), true)}` : ''}</dd></div><div><dt>原文提及时间</dt><dd>{claim.mentioned_at ? dayLabel(Date.parse(claim.mentioned_at), true) : '未记录'}</dd></div><div><dt>时间依据</dt><dd>{claim.time_basis || '未提供'} · {claim.time_precision === 'unknown' ? '精度未知' : claim.time_precision}</dd></div><div><dt>记录类型</dt><dd>{claim.change_type === 'reality_change' ? '现实变化' : claim.change_type === 'record_correction' ? '记录修正' : '来源报告'}</dd></div></dl>
      {claim.conflict_with.length > 0 && <div className="wl-conflict-callout"><strong>关联冲突</strong><span>{claim.conflict_with.join('、')}</span></div>}
      <section className="wl-evidence"><h3>来源原文 <span>{claim.evidence.length}</span></h3>{evidence.length === 0 ? <p className="wl-muted">没有可用的来源片段。</p> : evidence.map((item) => {
        const audio = item.audio_status || audioStates[item.turn_id]
        const disabledReason = audio === 'available' ? '' : audio === 'offset_unknown' ? '缺少音频定位时间' : audio === 'out_of_range' ? '来源时间超出录音范围' : audio === 'deleted' ? '录音音频已删除' : audio === 'missing' ? '本机找不到音频文件' : audio === 'unverified' || !audio ? '正在核验本地音频状态' : '此来源没有可用音频'
        return <article key={`${item.turn_id}:${item.revision}`}><blockquote>“{item.text}”</blockquote><footer><span className="wl-source-actions"><button type="button" className="wl-text-button" disabled={audio !== 'available'} title={disabledReason || '播放来源音频'} onClick={() => play(item.turn_id, item.offset_ms === undefined ? undefined : { startMs: item.offset_ms, endMs: null })}>▶ 播放原文</button>{audio !== 'available' && <small>{disabledReason}</small>}</span><Link to={`/c/${encodeURIComponent(item.conversation_id)}`}>打开录音详情 ↗</Link></footer></article>
      })}{claim.evidence.length > 3 && <button type="button" className="wl-text-button" onClick={() => setShowAllEvidence((v) => !v)}>{showAllEvidence ? '收起来源' : `查看全部 ${claim.evidence.length} 个来源`}</button>}</section>
      <div className="wl-inspector-actions"><button type="button" onClick={() => onFeedback('review', topic, lane, claim)}>复核</button><button type="button" className="secondary-button" onClick={() => onFeedback('correct', topic, lane, claim)}>修正记录</button>{lane.category === 'facts' && claim.evidence.length > 0 && <button type="button" className="secondary-button" onClick={() => onFeedback('change', topic, lane, claim)}>记录现实变化</button>}<button type="button" className="secondary-button" onClick={() => onFeedback('set_time', topic, lane, claim)}>设置适用时间</button>{!mentionContext && <button type="button" className="secondary-button" onClick={() => onFeedback('move_claim', topic, lane, claim)}>移动到主题</button>}</div>
    </div>
  </aside>
}
