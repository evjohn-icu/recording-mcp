import { useCallback, useEffect, useState } from 'react'
import { ApiError } from '../api/client'
import { getDecisionTrail, reviewDecision } from '../api/decisions'
import type { DecisionRecord, DecisionTrail } from '../api/decisions'
import { useAudio } from './AudioBar'

const STATE_LABELS: Record<string, string> = {
  observation: '观察',
  proposal: '提议',
  decision_candidate: '候选决定',
  confirmed: '已确认',
  superseded: '已取代',
  unresolved: '待解决',
}

const REASON_LABELS: Record<string, string> = {
  multiple_candidates: '存在多个候选，尚未裁决',
  source_invalidated: '来源已失效',
  source_unknown: '证据不足',
  reopened: '已重新打开',
  rejected: '已拒绝',
  latest_confirmation_source_invalidated: '最新确认的来源已失效',
}

function reasonText(reason: string): string {
  return REASON_LABELS[reason] || reason
}

function formatMoment(value: string | null | undefined): string {
  if (!value) return ''
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return value
  return date.toLocaleString('zh-CN', { year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' })
}

// timeLabel never promotes recorded_at into valid_from: an unknown effective
// time stays unknown.
function timeLabel(record: DecisionRecord): string {
  if (record.valid_from) return `生效 ${formatMoment(record.valid_from)}`
  if (record.mentioned_at) return `提及 ${formatMoment(record.mentioned_at)}`
  return '生效时间未知'
}

export function DecisionTrailPanel({ topicId, topicTitle }: { topicId: string; topicTitle: string }) {
  const [trail, setTrail] = useState<DecisionTrail | null>(null)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [busy, setBusy] = useState(false)
  const [expanded, setExpanded] = useState<Record<string, boolean>>({})
  const { play } = useAudio()

  const load = useCallback(async () => {
    try {
      const value = await getDecisionTrail({ topic_id: topicId, include_history: true })
      setTrail(value)
      setError('')
    } catch (cause) {
      if (cause instanceof ApiError && cause.code === 'decision_not_found') {
        setTrail(null)
        setError('')
        return
      }
      setError(cause instanceof Error ? cause.message : '无法读取决策轨迹')
    }
  }, [topicId])

  useEffect(() => {
    setTrail(null)
    setNotice('')
    void load()
  }, [load])

  async function submit(record: DecisionRecord, action: 'confirm' | 'reject' | 'reopen') {
    if (!trail) return
    setBusy(true)
    setNotice('')
    try {
      const updated = await reviewDecision({
        expected_version: trail.version,
        candidate_id: record.candidate_id || record.id,
        action,
        request_id: crypto.randomUUID(),
        reviewer: sessionStorage.getItem('recording-profile') || 'local-user',
      })
      setTrail(updated)
      setError('')
    } catch (cause) {
      if (cause instanceof ApiError && cause.code === 'decision_version_conflict') {
        setNotice('版本已更新，已刷新')
        await load()
      } else {
        setError(cause instanceof Error ? cause.message : '复核失败')
      }
    } finally {
      setBusy(false)
    }
  }

  const topic = trail?.topics?.[0]

  return (
    <section className="decision-panel" data-testid="decision-panel" aria-label="决策轨迹">
      <header className="decision-header">
        <h2>决策轨迹</h2>
        <span className="decision-topic">{topic?.title || topicTitle}</span>
      </header>
      {error && <p role="alert" className="note-error">{error}</p>}
      {notice && <p role="status" className="decision-notice">{notice}</p>}
      {!trail && !error && <p className="state-view">加载中…</p>}
      {trail && !topic && <p className="state-view empty-state">该主题还没有可追踪的决策。</p>}
      {topic && (
        <>
          <div className="decision-current" data-testid="decision-current" data-state={topic.current_state}>
            <strong>当前：{STATE_LABELS[topic.current_state] || topic.current_state}</strong>
            {topic.unresolved_reasons && topic.unresolved_reasons.length > 0 && (
              <p className="decision-reasons">{topic.unresolved_reasons.map(reasonText).join('；')}</p>
            )}
            {topic.current && (
              <p className="decision-current-text">
                {topic.current.text}
                <span className="decision-time">{timeLabel(topic.current)}</span>
              </p>
            )}
          </div>
          <ol className="decision-history" data-testid="decision-history">
            {topic.history.map((record) => (
              <li key={record.id} className={`decision-record decision-${record.state}`} data-decision-state={record.state}>
                <div className="decision-record-head">
                  <span className="decision-state">{STATE_LABELS[record.state] || record.state}</span>
                  <span className="decision-time">{timeLabel(record)}</span>
                  {record.reviewer && <span className="decision-reviewer">复核：{record.reviewer}</span>}
                </div>
                <p className="decision-text">{record.text}</p>
                {record.unresolved_reasons && record.unresolved_reasons.length > 0 && (
                  <p className="decision-reasons">{record.unresolved_reasons.map(reasonText).join('；')}</p>
                )}
                {record.evidence.length > 0 && (
                  <button
                    type="button"
                    className="secondary-button"
                    aria-expanded={Boolean(expanded[record.id])}
                    onClick={() => setExpanded((current) => ({ ...current, [record.id]: !current[record.id] }))}
                  >
                    查看证据
                  </button>
                )}
                {expanded[record.id] && (
                  <ul className="decision-evidence" data-testid="decision-evidence">
                    {record.evidence.map((item, index) => (
                      <li key={`${item.turn_id}-${index}`}>
                        <p>{item.text || '来源片段'}</p>
                        <small>修订 {item.revision || '未知'} · {item.source_status === 'current' ? '来源有效' : '来源已失效'}</small>
                        {item.source_status === 'current' && item.turn_id ? (
                          <button
                            type="button"
                            className="secondary-button"
                            onClick={() => play(item.turn_id, { startMs: item.offset_ms ?? null, endMs: null })}
                          >
                            播放
                          </button>
                        ) : (
                          <span className="decision-stale">来源已失效，不能确认</span>
                        )}
                      </li>
                    ))}
                  </ul>
                )}
                <div className="decision-actions">
                  {(record.state === 'decision_candidate') && (record.candidate_id || record.id) && (
                    <>
                      <button type="button" className="primary-button" data-testid="decision-review-confirm" disabled={busy}
                        onClick={() => void submit(record, 'confirm')}>确认</button>
                      <button type="button" className="secondary-button" data-testid="decision-review-reject" disabled={busy}
                        onClick={() => void submit(record, 'reject')}>拒绝</button>
                    </>
                  )}
                  {(record.state === 'confirmed' || record.state === 'superseded' || record.review_status === 'rejected' || record.review_status === 'reopened') && record.candidate_id && (
                    <button type="button" className="secondary-button" data-testid="decision-review-reopen" disabled={busy}
                      onClick={() => void submit(record, 'reopen')}>重新打开</button>
                  )}
                </div>
              </li>
            ))}
          </ol>
        </>
      )}
      {trail?.truncated && <p className="decision-notice">轨迹已按大小预算截断。</p>}
    </section>
  )
}
