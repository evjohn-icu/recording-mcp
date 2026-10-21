import { Link } from 'react-router-dom'
import type { SearchHit } from '../api/types'

function offsetLabel(milliseconds: number | null): string {
  if (milliseconds === null || !Number.isFinite(milliseconds)) return '时间未知'
  return `+${(milliseconds / 1000).toFixed(1)}s`
}

function reasonLabel(why: string): string {
  if (why === 'note') return 'AI 归纳检索命中'
  if (why === 'turn') return '原始转写检索命中'
  if (/hybrid|semantic|lexical|rerank/i.test(why)) return '混合检索相关结果（不代表包含精确词语）'
  return `检索相关结果：${why}`
}

export function SearchResults({ hits }: { hits: SearchHit[] }) {
  if (hits.length === 0) return <div className="state-view empty-state">没有匹配的结果</div>
  return (
    <div className="search-results" aria-label="搜索结果">
      {hits.map((hit, index) => (
        <Link
          className="search-result-row"
          data-result-row="true"
          data-result-index={index}
          tabIndex={0}
          to={hit.conversation_id ? `/c/${encodeURIComponent(hit.conversation_id)}?item=${encodeURIComponent(hit.id)}` : '#'}
          key={`${hit.kind}-${hit.id}-${index}`}
        >
          <div className="search-result-meta">
            <span className={`result-kind result-kind-${hit.kind}`}>
              {hit.kind === 'turn' ? '原始转录' : 'AI 归纳'}
            </span>
            <span>{hit.conversation_title || '未知会话'}</span>
            <span>{hit.kind === 'turn' ? offsetLabel(hit.global_start_ms) : '归纳'}</span>
          </div>
          <p className="search-result-text">{hit.text || '（无文本）'}</p>
          <span className="search-result-why">{reasonLabel(hit.why)}</span>
        </Link>
      ))}
    </div>
  )
}
