import { useEffect, useState } from 'react'
import { retrySuppression, suppressionPreview, suppressionStatus, suppressNote, type SuppressionOperation } from '../api/retention'
export function SuppressAction({ conversationId, rowId, version, onDone }: { conversationId: string; rowId: string; version: string; onDone: () => void }) {
  const [data, setData] = useState<Awaited<ReturnType<typeof suppressionPreview>> | null>(null)
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const [operation, setOperation] = useState<SuppressionOperation | null>(null)
  const [operationMessage, setOperationMessage] = useState('')
  async function preview() { setError(''); try { setData(await suppressionPreview(conversationId, rowId)) } catch (e) { setError(e instanceof Error ? e.message : '预览失败') } }
  async function updateOperation(next: SuppressionOperation) {
    setOperation(next)
    if (next.status === 'complete' && !next.cleanup_pending) {
      setOperationMessage('归纳已移除；托管清理已完成。')
      setOperation(null)
      onDone()
    } else {
      setOperationMessage('归纳已移除；托管清理仍在处理中。')
    }
  }
  async function confirm() { setBusy(true); setError(''); try { const result = await suppressNote(conversationId, rowId, version); setData(null); await updateOperation(result) } catch (e) { setError(e instanceof Error ? e.message : '无法确认抑制操作；可安全重试或重新读取预览') } finally { setBusy(false) } }
  async function retrySubmit() {
    setBusy(true); setError('')
    try { const result = await suppressNote(conversationId, rowId); setData(null); await updateOperation(result) }
    catch (e) { setError(e instanceof Error ? e.message : '无法确认抑制操作；请重新读取预览') }
    finally { setBusy(false) }
  }
  useEffect(() => {
    if (!operation?.operation_id) return
    let active = true
    const poll = async () => {
      try {
        const next = await suppressionStatus(operation.operation_id)
        if (!active) return
        setOperation(next)
        if (next.status === 'complete' && !next.cleanup_pending) {
          setOperationMessage('归纳已移除；托管清理已完成。')
          setOperation(null)
          onDone()
        } else setOperationMessage('归纳已移除；托管清理仍在处理中。')
      } catch (e) {
        if (active) setOperationMessage(`归纳已移除；清理状态暂不可用：${e instanceof Error ? e.message : '请重试检查'}`)
      }
    }
    void poll()
    const timer = window.setInterval(() => void poll(), 1500)
    return () => { active = false; window.clearInterval(timer) }
  }, [operation?.operation_id])
  async function retry() {
    if (!operation?.operation_id) return
    setBusy(true); setError('')
    try { await updateOperation(await retrySuppression(operation.operation_id)) }
    catch (e) { setError(e instanceof Error ? e.message : '清理重试失败'); setOperationMessage('归纳已移除；托管清理仍未完成。') }
    finally { setBusy(false) }
  }
  return <>
    <button type="button" className="secondary-button" onClick={() => void preview()}>移除并抑制同源归纳</button>
    {error && !data && <p role="alert" className="note-error">{error} <button type="button" onClick={() => void preview()}>重试</button></p>}
    {operationMessage && <p role="status">{operationMessage}{operation?.cleanup_pending && <button type="button" className="secondary-button" disabled={busy} onClick={() => void retry()}>{busy ? '重试中…' : '重试托管清理'}</button>}</p>}
    {error && data && <p role="alert" className="note-error">{error} <button type="button" disabled={busy} onClick={() => void retrySubmit()}>检查并重试</button></p>}
    {data && <div className="modal-backdrop"><section className="modal" role="dialog" aria-modal="true" aria-labelledby={`suppress-${rowId}`}><h2 id={`suppress-${rowId}`}>移除并抑制同源归纳</h2><p>这是归纳移除，不是永久遗忘事实。原始来源会保留；同一稳定来源和类型的后续修订仍会被抑制，独立的新会话不受影响。</p><p>托管备份：{data.managed_backup_count} 份。{data.retention_notice}</p><p>云端、手动导出和系统备份不会因此删除。</p><div className="modal-actions"><button type="button" disabled={busy} onClick={() => void confirm()}>{busy ? '处理中…' : '确认移除并抑制'}</button><button type="button" className="secondary-button" onClick={() => setData(null)}>取消</button></div></section></div>}
  </>
}
