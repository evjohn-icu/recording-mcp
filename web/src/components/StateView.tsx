import type { ReactNode } from 'react'
import { ApiError } from '../api/client'

interface StateViewProps {
  loading?: boolean
  error?: unknown
  empty?: boolean
  emptyMessage?: string
  onRetry?: () => void
  children?: ReactNode
}

export function StateView({ loading = false, error, empty = false, emptyMessage = '', onRetry, children }: StateViewProps) {
  if (loading) return <div className="state-view" aria-busy="true"><span className="spinner" aria-hidden="true" />加载中…</div>
  if (error) {
    const message = error instanceof ApiError && error.status >= 400 && error.status < 500
      ? error.message
      : '无法连接本地服务'
    return (
      <div className="state-view error-state" role="alert">
        <p>{message}</p>
        {onRetry && <button type="button" onClick={onRetry}>重试</button>}
      </div>
    )
  }
  if (empty) return <div className="state-view empty-state">{emptyMessage}</div>
  return <>{children}</>
}
