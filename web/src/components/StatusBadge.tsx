const labels: Record<string, string> = {
  pending: '待分析',
  partial: '部分已分析',
  ready: '已索引',
  failed: '索引失败',
  disabled: '语义索引已关闭',
  available: '可播放',
  offset_unknown: '该片段缺少时间信息，无法定位音频',
  out_of_range: '音频时间超出录音范围',
  deleted: '录音已删除（转写与归纳保留）',
  missing: '本机找不到音频文件',
  unavailable: '此录音没有本地音频',
}

export function statusLabel(status: string): string {
  return labels[status] || status
}

export function StatusBadge({ status, className = '' }: { status: string; className?: string }) {
  return <span className={`status-badge status-${status} ${className}`.trim()}>{statusLabel(status)}</span>
}
