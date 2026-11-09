import type { HybridView } from '../api/types'

const STATUS_LABELS: Record<string, string> = {
  sufficient: '证据充分',
  insufficient: '证据不足',
  unknown: '无法判定',
}

const STATUS_EXPLANATIONS: Record<string, string> = {
  sufficient: '当前证据可以完整回答该问题。',
  insufficient: '找到相关证据，但还不足以完整回答该问题。',
  unknown: '证据是否充分无法判定，请核对来源或稍后重试。',
}

const ASSESSMENT_GAP_KINDS: Record<string, true> = {
  evidence_assessment_unavailable: true,
  evidence_assessment_failed: true,
  budget_exhausted: true,
}

function formatTime(ms: number | null | undefined): string {
  if (ms === null || ms === undefined) return '时间未知'
  const total = Math.max(0, Math.floor(ms / 1000))
  const minutes = Math.floor(total / 60)
  const seconds = total % 60
  return `${minutes}:${seconds.toString().padStart(2, '0')}`
}

function conversationForTurn(pkg: HybridView, turnId: string): string {
  for (const evidence of pkg.evidence) {
    if (evidence.turn_id !== turnId) continue
    for (const itemId of evidence.used_by_item_ids) {
      const item = pkg.items.find((candidate) => candidate.id === itemId)
      if (item?.conversation_id) return item.conversation_id
    }
  }
  const turnItem = pkg.items.find((item) => item.kind === 'turn' && (item.id === turnId || item.turn_id === turnId))
  return turnItem?.conversation_id || pkg.items[0]?.conversation_id || ''
}

export function SufficiencyPanel({ pkg, retrievalVersion }: { pkg: HybridView; retrievalVersion: string }) {
  if (retrievalVersion !== 'v2') return null
  const assessment = pkg.sufficiency
  if (!assessment) return null
  const status = STATUS_LABELS[assessment.status] ? assessment.status : 'unknown'
  const gaps = pkg.gaps.filter((gap) => ASSESSMENT_GAP_KINDS[gap.kind])
  const noAssessor = gaps.some((gap) => gap.kind === 'evidence_assessment_unavailable')
  const byTurn = new Map(pkg.evidence.map((evidence) => [evidence.turn_id, evidence]))
  return (
    <section className={`sufficiency-panel sufficiency-${status}`} data-testid="sufficiency-panel" aria-live="polite">
      <header className="sufficiency-header">
        <span className="sufficiency-badge" data-testid="sufficiency-status">{STATUS_LABELS[status]}</span>
        <span className="sufficiency-explanation">
          {noAssessor ? '无法判定：未配置充分性评估器' : STATUS_EXPLANATIONS[status]}
        </span>
      </header>
      {assessment.evidence_refs.length > 0 && (
        <ul className="sufficiency-evidence" data-testid="sufficiency-evidence">
          {assessment.evidence_refs.map((turnId) => {
            const evidence = byTurn.get(turnId)
            const item = pkg.items.find((candidate) => candidate.kind === 'turn' && (candidate.id === turnId || candidate.turn_id === turnId))
            const conversationId = conversationForTurn(pkg, turnId)
            const text = evidence?.text || item?.content || '（证据文本不可用）'
            const startedAt = evidence?.global_start_ms ?? item?.global_start_ms ?? null
            return (
              <li key={turnId}>
                <a href={conversationId ? `/c/${encodeURIComponent(conversationId)}?item=${encodeURIComponent(turnId)}` : undefined}>
                  <span className="sufficiency-time">{formatTime(startedAt)}</span>
                  <span className="sufficiency-text">{text}</span>
                </a>
              </li>
            )
          })}
        </ul>
      )}
      {gaps.length > 0 && (
        <ul className="sufficiency-gaps" data-testid="sufficiency-gaps">
          {gaps.map((gap) => <li key={`${gap.kind}-${gap.detail}`}>{gap.kind}：{gap.detail}</li>)}
        </ul>
      )}
      {pkg.degraded && pkg.degrade_reason && (
        <p className="sufficiency-degraded">降级原因：{pkg.degrade_reason}</p>
      )}
    </section>
  )
}
