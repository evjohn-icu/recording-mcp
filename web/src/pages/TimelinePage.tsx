import { useEffect, useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import { getTopics, listConversations } from '../api/client'
import type { AnalysisStatus, ConversationSummary, Topic } from '../api/types'
import { useApi } from '../hooks/useApi'
import { Filters, type FilterValues } from '../components/Filters'
import { TimelineList } from '../components/TimelineList'
import { ImportPanel } from '../components/ImportPanel'
import { StateView } from '../components/StateView'

function filtersFromParams(params: URLSearchParams): FilterValues {
  return {
    startedAfter: params.get('started_after') || undefined,
    startedBefore: params.get('started_before') || undefined,
    topicId: params.get('topic_id') || undefined,
    status: (params.get('status') as AnalysisStatus | null) || undefined,
  }
}

export function TimelinePage() {
  const [params] = useSearchParams()
  return <TimelinePageContent key={params.toString()} />
}

function TimelinePageContent() {
  const [params, setParams] = useSearchParams()
  const filters = filtersFromParams(params)
  const filterKey = JSON.stringify(filters)
  const [cursor, setCursor] = useState<string | undefined>(undefined)
  const [items, setItems] = useState<ConversationSummary[]>([])
  const [nextCursor, setNextCursor] = useState<string | undefined>(undefined)

  const { data, loading, error, reload } = useApi(
    (signal) =>
      listConversations(
        {
          limit: 50,
          cursor,
          topic_id: filters.topicId,
          status: filters.status,
          started_after: filters.startedAfter,
          started_before: filters.startedBefore,
        },
        signal,
      ).then((page) => ({ page, cursor })),
    [filterKey, cursor],
  )

  const topics = useApi((signal) => getTopics(undefined, signal), [])

  useEffect(() => {
    const result = data
    if (!result || result.cursor !== cursor) return
    setItems((previous) => {
      const combined = cursor ? [...previous, ...result.page.items] : result.page.items
      const seen = new Set<string>()
      return combined.filter((item) => {
        if (seen.has(item.conversation_id)) return false
        seen.add(item.conversation_id)
        return true
      })
    })
    setNextCursor(result.page.next_cursor)
  }, [data, cursor])

  useEffect(() => {
    setItems([])
    setNextCursor(undefined)
  }, [filterKey])

  function updateFilters(next: FilterValues) {
    const updated = new URLSearchParams()
    if (next.startedAfter) updated.set('started_after', next.startedAfter)
    if (next.startedBefore) updated.set('started_before', next.startedBefore)
    if (next.topicId) updated.set('topic_id', next.topicId)
    if (next.status) updated.set('status', next.status)
    setCursor(undefined)
    setItems([])
    setNextCursor(undefined)
    setParams(updated, { replace: true })
  }

  const topicList: Topic[] = topics.data?.topics ?? []
  const loadingMore = loading && cursor !== undefined

  return (
    <section className="page">
      <header className="page-header">
        <h1>时间线</h1>
        <p className="page-subtitle">按时间浏览本地录音与会话</p>
      </header>
      <Filters value={filters} topics={topicList} onChange={updateFilters} />
      <ImportPanel onImported={reload} />
      <StateView
        loading={loading && items.length === 0}
        error={items.length === 0 ? error : undefined}
        onRetry={reload}
        empty={false}
        emptyMessage="还没有录音"
      >
        {error && items.length > 0 && <p role="alert">加载失败：{error.message} <button type="button" onClick={reload}>重试</button></p>}
        {items.length === 0 && !loading && !error && (
          <div className="empty-onboarding" data-testid="empty-onboarding">
            <p>这里会显示你的本地录音。点击上方“导入录音”选择音频文件，处理完成后即可查看转写、搜索和证据。</p>
          </div>
        )}
        <TimelineList
          items={items}
          nextCursor={nextCursor}
          loadingMore={loadingMore}
          onLoadMore={() => setCursor(nextCursor)}
        />
      </StateView>
    </section>
  )
}
