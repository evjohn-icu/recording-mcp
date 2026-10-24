import type { BriefResult } from './brief'

export type BriefMarkdownFormat = 'compact' | 'detailed'

const labels: Record<string, string> = {
  background: '背景', previous_discussions: '过往讨论', follow_up_candidates: '待跟进候选',
  changes_conflicts: '变化与冲突', unknowns: '未知事项',
}

// Every data value in the export is treated as plain text, including text that
// may contain Markdown syntax or HTML copied from a transcript.
export function markdownText(value: unknown): string {
  return String(value ?? '未知')
    .replace(/[&<>]/g, (char) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;' })[char]!)
    .replace(/[\\`*_{}[\]()#+.!|~\-]/g, '\\$&')
    // Keep arbitrary line breaks from starting Markdown blocks or indented code.
    .replace(/\r\n|[\r\n]/g, ' ⏎ ')
}

function stateLine(source: string, assertion: string): string {
  return `来源状态：${markdownText(source)}；断言状态：${markdownText(assertion)}`
}

export function exportBriefMarkdown(result: BriefResult, format: BriefMarkdownFormat = 'compact'): string {
  const rows = [
    '# 记忆简报', '',
    `- Snapshot: ${markdownText(result.snapshot_id)}`,
    `- 范围：${markdownText(result.scope.kind)}；人物 ${(result.scope.person_ids ?? []).map(markdownText).join('、') || '无'}；主题 ${(result.scope.topic_ids ?? []).map(markdownText).join('、') || '无'}；会话 ${(result.scope.conversation_ids ?? []).map(markdownText).join('、') || '无'}`,
    `- 覆盖：选择 ${result.coverage.selected_turns}/${result.coverage.eligible_turns} 个轮次；省略 ${result.coverage.omitted_turns} 个轮次、${result.coverage.omitted_notes} 条归纳。${result.truncated ? '结果已截断。' : '未报告截断。'}${result.degraded ? '部分数据不可用。' : '数据状态正常。'}`,
    '- 此简报是指定快照的证据导出；数据状态可能变化，已导出的副本无法撤回。', '',
  ]
  for (const [key, entries] of Object.entries(result.sections)) {
    rows.push(`## ${labels[key] ?? markdownText(key)}`)
    if (!entries.length) rows.push('_暂无条目。_')
    for (const entry of entries) {
      rows.push(`- ${markdownText(entry.text)}${entry.text_truncated ? '（正文已截断）' : ''}`)
      rows.push(`  - ${stateLine(entry.source_state, entry.assertion_state)}`)
      rows.push(`  - 证据 ID：${entry.evidence_ids.map(markdownText).join('、') || '无'}`)
    }
    rows.push('')
  }
  rows.push(`## 证据（${result.evidence.length}）`)
  if (!result.evidence.length) rows.push('_没有返回证据。_')
  for (const ev of result.evidence) {
    rows.push(`- ${markdownText(ev.id)}：${markdownText(ev.text)}${ev.text_truncated ? '（摘录已截断）' : ''}`)
    rows.push(`  - 来源状态：${markdownText(ev.source_state)}；轮次：${markdownText(ev.turn_id)}；会话：${markdownText(ev.conversation_id)}；录音：${markdownText(ev.recording_id)}`)
    rows.push(`  - 录音时间：${ev.started_at ? markdownText(ev.started_at) : '未知时间'}${ev.global_start_ms != null ? `；位置：${ev.global_start_ms} ms${ev.global_end_ms != null ? `–${ev.global_end_ms} ms` : ''}` : ''}`)
    // Source revision is part of evidence identity, so retain it in compact exports too.
    rows.push(`  - 修订：${markdownText(ev.transcript_revision)}；来源附件：${markdownText(ev.source_attachment_id)}`)
    if (format === 'detailed') {
      rows.push(`  - 来源说明：${markdownText(ev.provenance)}`)
      rows.push(`  - 主题 ID：${(ev.topic_ids ?? []).map(markdownText).join('、') || '无'}；人物 ID：${(ev.person_ids ?? []).map(markdownText).join('、') || '无'}`)
    }
  }
  rows.push('', '## 数据缺口')
  if (result.gaps.length) for (const gap of result.gaps) rows.push(`- ${markdownText(gap.kind)}：${markdownText(gap.detail)}`)
  else rows.push('_未报告数据缺口。_')
  if (result.evidence.some((ev) => !ev.started_at)) rows.push('- 部分证据的录音时间未知；不要据此推断时间顺序。')
  return rows.join('\n').trimEnd() + '\n'
}
