import { useCallback, useEffect, useRef, useState } from 'react'
import { ApiError } from '../api/client'

export interface UseApiResult<T> {
  data: T | undefined
  loading: boolean
  error: ApiError | Error | undefined
  reload: () => void
}

export function useApi<T>(fn: (signal: AbortSignal) => Promise<T>, deps: readonly unknown[]): UseApiResult<T> {
  const fnRef = useRef(fn)
  fnRef.current = fn
  const [data, setData] = useState<T>()
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<ApiError | Error>()
  const [reloadTick, setReloadTick] = useState(0)

  const reload = useCallback(() => setReloadTick((value) => value + 1), [])

  useEffect(() => {
    const controller = new AbortController()
    let active = true
    setLoading(true)
    setError(undefined)
    void fnRef.current(controller.signal)
      .then((value) => {
        if (!active) return
        setData(value)
        setLoading(false)
      })
      .catch((reason: unknown) => {
        if (!active || (reason instanceof DOMException && reason.name === 'AbortError')) return
        if (reason instanceof Error && reason.name === 'AbortError') return
        setError(reason instanceof ApiError || reason instanceof Error ? reason : new Error('请求失败'))
        setLoading(false)
      })
    return () => {
      active = false
      controller.abort()
    }
    // The caller supplies semantic dependencies; fn is held in a ref to avoid
    // rerunning a request merely because a render-created callback changed.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [...deps, reloadTick])

  return { data, loading, error, reload }
}
