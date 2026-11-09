import { useEffect, useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import { getTopics, search, searchHybrid } from '../api/client'
import type { HybridView, Item, SearchHit, SearchPageView, Topic } from '../api/types'
import { useApi } from '../hooks/useApi'
import { Filters, type FilterValues } from '../components/Filters'
import { SearchResults } from '../components/SearchResults'
import { StateView } from '../components/StateView'
import { SufficiencyPanel } from '../components/SufficiencyPanel'

type Scope = 'raw' | 'derived' | 'both'

function itemToHit(item: Item, titles: Record<string, string>, isHybrid: boolean): SearchHit {
  const isTurn = item.kind === 'turn'
  return {
    kind: item.kind,
    id: item.id,
    text: item.content || item.summary || item.title || '',
    conversation_id: item.conversation_id,
    why: isHybrid ? 'hybrid' : isTurn ? 'turn' : 'note',
    score: 0,
    evidence_turn_ids: item.evidence_turn_ids,
    global_start_ms: item.global_start_ms,
    speaker_person_id: item.speaker_person_id,
    stale: item.stale,
    ambient: item.ambient,
    derived_from: item.derived_from,
    conversation_title: titles[item.conversation_id] || '',
  }
}

function Diagnostics({ hybrid }: { hybrid: HybridView }) {
  const rerankMissing = hybrid.degrade_reason.toLowerCase().includes('rerank')
  return (
    <details className="diagnostics">
      <summary>诊断（实验）</summary>
      <div className="diagnostics-body">
        {hybrid.degraded && (
          <p className="diagnostics-degraded" role="status">
            降级：{rerankMissing ? '未配置重排' : '语义不可用或索引为空'} — {hybrid.degrade_reason}
          </p>
        )}
        <p>语义索引条目：{hybrid.trace.semantic.indexed}</p>
        <p>字面命中：{hybrid.trace.lexical.hit_ids.length} 条</p>
        <p>语义命中：{hybrid.trace.semantic.hit_ids.length} 条</p>
        {hybrid.trace.lexical.hit_ids.length > 0 && (
          <p className="diagnostics-ids">字面 hit_ids：{hybrid.trace.lexical.hit_ids.join(', ')}</p>
        )}
        {hybrid.trace.semantic.hit_ids.length > 0 && (
          <p className="diagnostics-ids">语义 hit_ids：{hybrid.trace.semantic.hit_ids.join(', ')}</p>
        )}
        {hybrid.gaps.length > 0 && (
          <ul className="diagnostics-gaps">
            {hybrid.gaps.map((gap) => <li key={`${gap.kind}-${gap.detail}`}>{gap.kind}：{gap.detail}</li>)}
          </ul>
        )}
        <table className="diagnostics-table">
          <thead>
            <tr><th>条目</th><th>来源</th><th>分数</th></tr>
          </thead>
          <tbody>
            {hybrid.ranking.map((entry) => (
              <tr key={entry.item_id}>
                <td>{entry.kind} {entry.item_id}</td>
                <td>{entry.retrieval_sources.join(' + ') || '—'}</td>
                <td>{entry.retrieval_score.toFixed(4)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </details>
  )
}

export function SearchPage() {
  const [params] = useSearchParams()
  return <SearchPageContent key={params.toString()} />
}

function SearchPageContent() {
  const [params, setParams] = useSearchParams()
  const q = params.get('q') || ''
  const scope = (params.get('scope') as Scope | null) || 'both'
  const hybridEnabled = params.get('hybrid') === '1'
  const mode = hybridEnabled ? 'hybrid' : 'lexical'
  const retrievalVersion = params.get('retrieval_version') === 'v2' ? 'v2' : 'v1'
  const filters: FilterValues = {
    startedAfter: params.get('started_after') || undefined,
    startedBefore: params.get('started_before') || undefined,
    topicId: params.get('topic_id') || undefined,
  }
  const filterKey = JSON.stringify(filters)
  const [input, setInput] = useState(q)
  const [cursor, setCursor] = useState<string | undefined>(undefined)
  const [extraHits, setExtraHits] = useState<SearchHit[]>([])
  const [nextCursor, setNextCursor] = useState<string | undefined>(undefined)

  useEffect(() => {
    setInput(q)
  }, [q])

  useEffect(() => {
    if (input === q) return
    const timer = window.setTimeout(() => {
      const updated = new URLSearchParams(params)
      if (input) updated.set('q', input)
      else updated.delete('q')
      setCursor(undefined)
      setExtraHits([])
      setParams(updated, { replace: true })
    }, 300)
    return () => window.clearTimeout(timer)
  }, [input, q, params, setParams])

  const lexical = useApi(
    (signal) =>
      !hybridEnabled && q.trim()
        ? search(
            {
              q,
              scope,
              topic_id: filters.topicId,
              started_after: filters.startedAfter,
              started_before: filters.startedBefore,
              cursor,
              limit: 50,
            },
            signal,
          ).then((page) => ({ page, cursor }))
        : Promise.resolve({ page: { hits: [], degraded: false } as SearchPageView, cursor }),
    [q, scope, filterKey, cursor, hybridEnabled],
  )

  const hybrid = useApi(
    (signal) =>
      hybridEnabled && q.trim()
        ? searchHybrid(
            {
              q,
              mode,
              retrieval_version: retrievalVersion,
              topic_id: filters.topicId,
              started_after: filters.startedAfter,
              started_before: filters.startedBefore,
              max_items: 20,
            },
            signal,
          )
        : Promise.resolve<HybridView | undefined>(undefined),
    [q, mode, retrievalVersion, filterKey, hybridEnabled],
  )

  const topics = useApi((signal) => getTopics(undefined, signal), [])

  useEffect(() => {
    const result = lexical.data
    if (!result || result.cursor !== cursor) return
    const page = result.page
    setExtraHits((previous) => {
      const combined = cursor ? [...previous, ...page.hits] : page.hits
      const seen = new Set<string>()
      return combined.filter((hit) => {
        const identity = `${hit.kind}:${hit.id}`
        if (seen.has(identity)) return false
        seen.add(identity)
        return true
      })
    })
    setNextCursor(page.next_cursor)
  }, [lexical.data, cursor])

  useEffect(() => {
    setExtraHits([])
    setNextCursor(undefined)
  }, [q, scope, retrievalVersion, filterKey, hybridEnabled])

  function updateParams(next: Partial<Record<string, string | undefined>>) {
    const updated = new URLSearchParams(params)
    for (const [key, value] of Object.entries(next)) {
      if (value === undefined || value === '') updated.delete(key)
      else updated.set(key, value)
    }
    setCursor(undefined)
    setExtraHits([])
    setParams(updated, { replace: true })
  }

  function updateFilters(next: FilterValues) {
    updateParams({
      started_after: next.startedAfter,
      started_before: next.startedBefore,
      topic_id: next.topicId,
    })
  }

  const hybridHits = hybrid.data ? hybrid.data.items.map((item) => itemToHit(item, hybrid.data!.conversation_titles, true)) : []
  const hits = hybridEnabled ? hybridHits : extraHits
  const loading = hybridEnabled ? hybrid.loading : lexical.loading
  const error = hybridEnabled ? hybrid.error : lexical.error
  const topicList: Topic[] = topics.data?.topics ?? []
  const searched = q.trim().length > 0

  return (
    <section className="page">
      <header className="page-header">
        <h1>搜索</h1>
        <p className="page-subtitle">在原始转录与 AI 归纳中查找记忆</p>
      </header>
      <div className="search-controls">
        <label className="search-input-label">
          关键词
          <input
            type="search"
            data-search-input="true"
            value={input}
            onChange={(event) => setInput(event.target.value)}
            placeholder="输入关键词，按 / 聚焦"
          />
        </label>
        <label>
          范围
          <select value={hybridEnabled ? 'both' : scope} disabled={hybridEnabled} onChange={(event) => updateParams({ scope: event.target.value })}>
            <option value="both">{hybridEnabled ? '全部类型（混合检索）' : '全部'}</option>
          {!hybridEnabled && <><option value="raw">仅原始转录</option>
            <option value="derived">仅AI归纳</option></>}
          </select>
        </label>
        <label className="toggle-label">
          <input
            type="checkbox"
            checked={hybridEnabled}
            onChange={(event) => updateParams({ hybrid: event.target.checked ? '1' : undefined })}
          />
          混合检索（实验）
        </label>
        {hybridEnabled && (
          <label>
            检索版本
            <select aria-label="检索版本" value={retrievalVersion} onChange={(event) => updateParams({ retrieval_version: event.target.value })}>
              <option value="v1">v1（默认）</option>
              <option value="v2">v2（实验；质量因查询而异）</option>
            </select>
          </label>
        )}
      </div>
      <Filters value={filters} topics={topicList} onChange={updateFilters} />
      {hybridEnabled && hybrid.data && <Diagnostics hybrid={hybrid.data} />}
      {hybridEnabled && hybrid.data && <SufficiencyPanel pkg={hybrid.data} retrievalVersion={retrievalVersion} />}
      {!searched ? (
        <div className="state-view empty-state">输入关键词开始搜索</div>
      ) : (
        <StateView
          loading={loading && hits.length === 0}
          error={hits.length === 0 ? error : undefined}
          onRetry={hybridEnabled ? hybrid.reload : lexical.reload}
          empty={!loading && !error && hits.length === 0}
          emptyMessage="没有匹配的结果"
        >
          <SearchResults hits={hits} />
          {error && hits.length > 0 && <p role="alert">加载失败：{error.message} <button type="button" onClick={hybridEnabled ? hybrid.reload : lexical.reload}>重试</button></p>}
          {hybridEnabled && hybrid.data && hybrid.data.items.length >= 20 && <p role="status">结果最多显示 20 条，可能已截断。</p>}
          {!hybridEnabled && nextCursor && (
            <button type="button" className="load-more" onClick={() => setCursor(nextCursor)} disabled={loading}>
              {loading ? '加载中…' : '加载更多'}
            </button>
          )}
        </StateView>
      )}
    </section>
  )
}
