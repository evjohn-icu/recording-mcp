import { createElement as h, useCallback, useEffect, useState, type FormEvent, type ReactNode } from 'react'
import { ApiError, createSession, getSession, setCsrfToken } from './api/client'

function tokenFromInput(value: string): string {
  const text = value.trim()
  if (!text) return ''
  try {
    const parsed = new URL(text)
    const hashToken = parsed.hash.match(/^#token=(.*)$/)?.[1]
    return hashToken ? decodeURIComponent(hashToken) : text
  } catch {
    const bare = text.match(/(?:^|#)token=([^#]*)/)?.[1]
    return bare ? decodeURIComponent(bare) : text
  }
}

export async function bootstrapFromHash(): Promise<boolean> {
  const hash = window.location.hash
  if (!hash.startsWith('#token=')) return false
  const token = tokenFromInput(hash)
  if (!token) return false
  try {
    const session = await createSession(token)
    setCsrfToken(session.csrf_token)
    window.history.replaceState(null, '', `${window.location.pathname}${window.location.search}`)
    return true
  } catch {
    return false
  }
}

export interface AuthGateProps {
  children: ReactNode
}

export function AuthGate({ children }: AuthGateProps) {
  const [authenticated, setAuthenticated] = useState<boolean | null>(null)
  const [message, setMessage] = useState('')
  const [token, setToken] = useState('')
  const [submitting, setSubmitting] = useState(false)

  const checkSession = useCallback(async () => {
    try {
      const session = await getSession()
      setCsrfToken(session.csrf_token)
      setAuthenticated(true)
      setMessage('')
    } catch (error) {
      setCsrfToken(null)
      setAuthenticated(false)
      setMessage(error instanceof ApiError && error.status === 401 ? '' : '无法连接本地服务')
    }
  }, [])

  useEffect(() => {
    let alive = true
    void (async () => {
      await bootstrapFromHash()
      if (alive) await checkSession()
    })()
    return () => {
      alive = false
    }
  }, [checkSession])

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const value = tokenFromInput(token)
    if (!value) {
      setMessage('请输入启动令牌')
      return
    }
    setSubmitting(true)
    setMessage('')
    try {
      const session = await createSession(value)
      setCsrfToken(session.csrf_token)
      setAuthenticated(true)
      setToken('')
    } catch (error) {
      if (error instanceof ApiError && error.status === 401) setMessage('令牌无效或已过期')
      else if (error instanceof ApiError) setMessage(error.message)
      else setMessage('无法连接本地服务')
    } finally {
      setSubmitting(false)
    }
  }

  if (authenticated === null) {
    return h('div', { className: 'auth-state', 'aria-busy': true }, '加载中…')
  }
  if (authenticated) return h('div', { className: 'auth-root' }, children)

  return h(
    'main',
    { className: 'auth-page' },
    h(
      'section',
      { className: 'auth-card', 'aria-labelledby': 'auth-title' },
      h('h1', { id: 'auth-title' }, '本地记忆'),
      h('p', null, '请输入启动令牌以打开本地记忆。'),
      h(
        'form',
        { onSubmit: submit },
        h('label', { htmlFor: 'bootstrap-token' }, '启动令牌或完整链接'),
        h('input', {
          id: 'bootstrap-token',
          value: token,
          onChange: (event: { target: HTMLInputElement }) => setToken(event.target.value),
          autoComplete: 'off',
          spellCheck: false,
          placeholder: '粘贴启动令牌或带 #token= 的完整链接',
        }),
        h('button', { type: 'submit', disabled: submitting }, submitting ? '验证中…' : '进入'),
      ),
      message ? h('p', { className: 'auth-error', role: 'alert' }, message) : null,
    ),
  )
}
