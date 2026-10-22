import { useCallback, useEffect, useState } from 'react'
import { pendingSuppressions, retrySuppression, type SuppressionOperation } from '../api/retention'

export function PendingSuppressions() {
  const [operations, setOperations] = useState<SuppressionOperation[]>([])
  const [busyId, setBusyId] = useState('')
  const [error, setError] = useState('')
  const refresh = useCallback(async () => {
    try {
      const result = await pendingSuppressions()
      setOperations(result.operations || [])
      setError('')
    } catch (e) {
      setError(e instanceof Error ? e.message : '无法读取待清理状态')
    }
  }, [])

  useEffect(() => {
    void refresh()
    const timer = window.setInterval(() => void refresh(), 3000)
    return () => window.clearInterval(timer)
  }, [refresh])

  async function retry(operationId: string) {
    setBusyId(operationId); setError('')
    try {
      const result = await retrySuppression(operationId)
      if (result.status === 'complete' && !result.cleanup_pending) {
        setOperations((current) => current.filter((item) => item.operation_id !== operationId))
      } else {
        setOperations((current) => current.map((item) => item.operation_id === operationId ? result : item))
      }
      await refresh()
    } catch (e) {
      setError(e instanceof Error ? e.message : '托管清理重试失败')
    } finally {
      setBusyId('')
    }
  }

  if (operations.length === 0 && !error) return null
  return <aside className="state-view" aria-label="待完成的归纳清理" role="status">
    <strong>{operations.length ? '归纳已移除；托管清理仍在处理中。' : '托管清理状态暂不可用。'}</strong>
    {error && <p role="alert" className="note-error">{error}</p>}
    {operations.map((operation) => <p key={operation.operation_id}>
      清理操作 {operation.operation_id.slice(0, 8)}：待完成
      <button type="button" className="secondary-button" disabled={busyId === operation.operation_id} onClick={() => void retry(operation.operation_id)}>
        {busyId === operation.operation_id ? '重试中…' : '重试托管清理'}
      </button>
    </p>)}
  </aside>
}
