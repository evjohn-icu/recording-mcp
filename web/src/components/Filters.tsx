import type { AnalysisStatus, Topic } from '../api/types'

export interface FilterValues {
  startedAfter?: string
  startedBefore?: string
  topicId?: string
  status?: AnalysisStatus
}

function pad(value: number): string {
  return String(value).padStart(2, '0')
}

function localBoundary(dateString: string, nextDay: boolean): string {
  const [year, month, day] = dateString.split('-').map(Number)
  const date = new Date(year, month - 1, day + (nextDay ? 1 : 0), 0, 0, 0, 0)
  const offset = -date.getTimezoneOffset()
  const sign = offset >= 0 ? '+' : '-'
  const absolute = Math.abs(offset)
  const zone = `${sign}${pad(Math.floor(absolute / 60))}:${pad(absolute % 60)}`
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T00:00:00${zone}`
}

function dateInputValue(value: string | undefined, end: boolean): string {
  if (!value) return ''
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return ''
  if (end) date.setDate(date.getDate() - 1)
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}`
}

export function Filters({ value, topics, onChange }: {
  value: FilterValues
  topics: Topic[]
  onChange: (next: FilterValues) => void
}) {
  const start = dateInputValue(value.startedAfter, false)
  const end = dateInputValue(value.startedBefore, true)
  const updateStart = (date: string) => onChange({ ...value, startedAfter: date ? localBoundary(date, false) : undefined })
  const updateEnd = (date: string) => onChange({ ...value, startedBefore: date ? localBoundary(date, true) : undefined })
  return (
    <section className="filters" aria-label="筛选条件">
      <label>
        开始日期
        <input type="date" value={start} onChange={(event) => updateStart(event.target.value)} />
      </label>
      <label>
        结束日期
        <input type="date" value={end} onChange={(event) => updateEnd(event.target.value)} />
      </label>
      <label>
        主题
        <select value={value.topicId || ''} onChange={(event) => onChange({ ...value, topicId: event.target.value || undefined })}>
          <option value="">全部主题</option>
          {topics.map((topic) => <option value={topic.id} key={topic.id}>{topic.title}</option>)}
        </select>
      </label>
      <label>
        状态
        <select value={value.status || ''} onChange={(event) => onChange({ ...value, status: (event.target.value || undefined) as AnalysisStatus | undefined })}>
          <option value="">全部状态</option>
          <option value="pending">待分析</option>
          <option value="partial">部分已分析</option>
        </select>
      </label>
      {(value.startedAfter || value.startedBefore) && <p className="filter-hint">时间未知的录音不参与日期筛选</p>}
      {(value.startedAfter || value.startedBefore || value.topicId || value.status) && (
        <button type="button" className="link-button" onClick={() => onChange({})}>清除筛选</button>
      )}
    </section>
  )
}

export { localBoundary }
