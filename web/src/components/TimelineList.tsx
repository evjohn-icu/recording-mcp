import { Link } from 'react-router-dom'
import type { ConversationSummary } from '../api/types'
import { StatusBadge } from './StatusBadge'

function formatDate(value: string | null): string {
  if (!value) return '时间未知'
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return '时间未知'
  const parts = [date.getFullYear(), date.getMonth() + 1, date.getDate()]
  const time = [date.getHours(), date.getMinutes()].map((part) => String(part).padStart(2, '0'))
  return `${parts[0]}-${String(parts[1]).padStart(2, '0')}-${String(parts[2]).padStart(2, '0')} ${time.join(':')}`
}

export function TimelineList({ items, nextCursor, loadingMore, onLoadMore }: {
  items: ConversationSummary[]
  nextCursor?: string
  loadingMore?: boolean
  onLoadMore?: () => void
}) {
  return (
    <div className="timeline-list">
      {items.map((item) => (
        <Link className="timeline-row" to={`/c/${encodeURIComponent(item.conversation_id)}`} key={item.conversation_id}>
          <div className="timeline-main">
            <h2>{item.title || '未命名会话'}</h2>
            <time dateTime={item.recording_started_at || undefined}>{formatDate(item.recording_started_at)}</time>
          </div>
          <div className="timeline-status"><StatusBadge status={item.analysis_status} /></div>
          <div className="timeline-counts" aria-label="会话统计">
            <span>片段 {item.received_segments}</span>
            <span>轮次 {item.original_turns}</span>
            <span>已关联 {item.topic_linked_turns}</span>
          </div>
        </Link>
      ))}
      {nextCursor && onLoadMore && (
        <button type="button" className="load-more" onClick={onLoadMore} disabled={loadingMore}>
          {loadingMore ? '加载中…' : '加载更多'}
        </button>
      )}
    </div>
  )
}
