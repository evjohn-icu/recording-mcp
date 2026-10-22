import { createElement as h, useCallback, useEffect, useState, type FormEvent, type ReactNode } from 'react'
import { ApiError, authInfo, createSession, getCsrfToken, getSession, login, loginProfile, logout, request, setCsrfToken } from './api/client'
import type { AuthInfo } from './api/types'
import { AudioBar } from './components/AudioBar'

function tokenFromInput(value: string): string {
  const text = value.trim()
  if (!text) return ''
  try { const parsed = new URL(text); const hashToken = parsed.hash.match(/^#token=(.*)$/)?.[1]; return hashToken ? decodeURIComponent(hashToken) : text }
  catch { const bare = text.match(/(?:^|#)token=([^#]*)/)?.[1]; return bare ? decodeURIComponent(bare) : text }
}

export async function bootstrapFromHash(): Promise<boolean> {
  const hash = window.location.hash
  if (!hash.startsWith('#token=')) return false
  const token = tokenFromInput(hash)
  if (!token) return false
  try { const session = await createSession(token); setCsrfToken(session.csrf_token); window.history.replaceState(null, '', `${window.location.pathname}${window.location.search}`); return true }
  catch { return false }
}

export interface AuthGateProps { children: ReactNode }
export function AuthGate({ children }: AuthGateProps) {
  const [authenticated, setAuthenticated] = useState<boolean | null>(null)
  const [profile, setProfile] = useState('')
  const [mode, setMode] = useState<AuthInfo['mode']>('token')
  const [profilesManager, setProfilesManager] = useState(false)
  const [retireOpen, setRetireOpen] = useState(false)
  const [retireConfirm, setRetireConfirm] = useState('')
  const [retirePassword, setRetirePassword] = useState('')
  const [retireMessage, setRetireMessage] = useState('')
  const [retireBusy, setRetireBusy] = useState(false)
  const [profiles, setProfiles] = useState<string[]>([])
  const [selectedProfile, setSelectedProfile] = useState('')
  const [message, setMessage] = useState('')
  const [credential, setCredential] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const [changingPassword, setChangingPassword] = useState(false)
  const [currentPassword, setCurrentPassword] = useState('')
  const [newPassword, setNewPassword] = useState('')
  const [passwordError, setPasswordError] = useState('')

  const readAuthInfo = useCallback(async () => {
    const info = await authInfo()
    setMode(info.mode)
    setProfilesManager(info.mode === 'profiles')
    setProfile(info.profile || '')
    setProfiles(info.profiles || [])
    setSelectedProfile(info.mode === 'profile' ? info.profile || '' : (info.profiles || [])[0] || '')
  }, [])
  const checkSession = useCallback(async () => {
    try {
      const session = await getSession()
      setCsrfToken(session.csrf_token); setAuthenticated(true); setProfile(session.profile || ''); if(session.profile) sessionStorage.setItem('recording-profile', session.profile); else sessionStorage.removeItem('recording-profile'); setMessage('')
      if (session.profile) setMode('profile')
    } catch (error) {
      setCsrfToken(null); setAuthenticated(false)
      setMessage(error instanceof ApiError && error.status === 401 ? '' : '无法连接本地服务')
      try { await readAuthInfo() } catch { setMode('token') }
    }
  }, [readAuthInfo])
  useEffect(() => {
    let alive = true
    void (async () => { await bootstrapFromHash(); if (alive) await checkSession() })()
    return () => { alive = false }
  }, [checkSession])

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const value = mode === 'token' ? tokenFromInput(credential) : credential
    if (!value) { setMessage(mode === 'token' ? '请输入启动令牌' : '请输入密码'); return }
    if (mode === 'profiles' && !selectedProfile) { setMessage('请选择配置'); return }
    setSubmitting(true); setMessage('')
    try {
      const session = mode === 'token' ? await createSession(value) : mode === 'profiles' ? await loginProfile(selectedProfile, value) : await login(value)
      setCsrfToken(session.csrf_token); setAuthenticated(true); setProfile(session.profile || ''); if(session.profile) sessionStorage.setItem('recording-profile', session.profile); else sessionStorage.removeItem('recording-profile'); setCredential('')
    } catch (error) {
      setMessage(error instanceof ApiError && error.status === 401 ? (mode === 'token' ? '令牌无效或已过期' : '配置或密码不正确') : error instanceof ApiError ? error.message : '无法连接本地服务')
    } finally { setSubmitting(false) }
  }
  async function retireProfile(event: FormEvent<HTMLFormElement>) {
    event.preventDefault(); setRetireBusy(true); setRetireMessage('')
    try {
      const response = await fetch('/api/profiles/retire', { method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json', ...(getCsrfToken() ? { 'X-CSRF-Token': getCsrfToken()! } : {}) }, body: JSON.stringify({ profile, confirm_profile: retireConfirm, password: retirePassword }) })
      const result = await response.json() as { status?: string }
      if (!response.ok) throw new Error('资料库遗忘失败；请核对资料名和密码')
      if (result.status === 'pending') { sessionStorage.setItem(`recording-retirement:${profile}`, profile); setRetireMessage('遗忘仍在处理中。请重新登录后使用保留的资料名和密码重试。') }
      else sessionStorage.removeItem(`recording-retirement:${profile}`)
      setCsrfToken(null); setAuthenticated(false); setProfile(''); sessionStorage.removeItem('recording-profile'); setRetireOpen(false); setCredential(''); await readAuthInfo()
      if (result.status === 'pending') setMessage(`「${profile}」遗忘处理中；请用资料名和密码重试。`)
    } catch (error) { setRetireMessage(error instanceof Error ? error.message : '遗忘失败') } finally { setRetireBusy(false) }
  }
  async function changePassword(event: FormEvent<HTMLFormElement>) {
    event.preventDefault(); setPasswordError('')
    try {
      await request<void>('/api/profile/password', { method: 'POST', body: JSON.stringify({ current_password: currentPassword, new_password: newPassword }) })
      setChangingPassword(false); setCsrfToken(null); setAuthenticated(false); setProfile(''); setCurrentPassword(''); setNewPassword(''); await readAuthInfo()
    } catch (error) { setPasswordError(error instanceof Error ? error.message : '密码修改失败') }
  }
  async function signOut() {
    try { await logout() } catch { /* always remove local state */ }
    // The authenticated subtree (including audio and all page-local data) is
    // removed before another profile can be selected.
    setCsrfToken(null); setAuthenticated(false); setProfile(''); sessionStorage.removeItem('recording-profile'); setCredential(''); setMessage('')
    try { await readAuthInfo() } catch { setMode('token') }
  }
  if (authenticated === null) return h('div', { className: 'auth-state', 'aria-busy': true }, '加载中…')
  if (authenticated) return h('div', { className: 'auth-root' },
    h('div', { className: 'profile-toolbar' }, h('span', null, profile ? `配置：${profile}` : '启动令牌会话'), mode === 'profile' ? h('button', { type: 'button', className: 'secondary-button', onClick: () => setChangingPassword(true) }, '修改密码') : null, profilesManager && profile ? h('button', { type: 'button', className: 'danger-button', onClick: () => setRetireOpen(true) }, '永久遗忘整个资料库') : null, h('button', { type: 'button', className: 'secondary-button', onClick: () => void signOut() }, '退出登录')),
    retireOpen ? h('div', { className: 'modal-backdrop' }, h('section', { className: 'modal', role: 'dialog', 'aria-modal': true, 'aria-labelledby': 'retire-title' }, h('h2', { id: 'retire-title' }, '永久遗忘整个资料库'), h('p', null, '将永久删除此资料库的所有本地记录、音频、托管备份及索引。云端数据与手动导出不受影响。此操作不是退出登录。'), h('form', { onSubmit: retireProfile }, h('label', null, `重新输入资料名：${profile}`, h('input', { value: retireConfirm, onChange: (e: { target: HTMLInputElement }) => setRetireConfirm(e.target.value), required: true })), h('label', null, '当前密码', h('input', { type: 'password', autoComplete: 'current-password', value: retirePassword, onChange: (e: { target: HTMLInputElement }) => setRetirePassword(e.target.value), required: true })), retireMessage ? h('p', { role: 'alert', className: 'auth-error' }, retireMessage) : null, h('button', { type: 'submit', disabled: retireBusy || retireConfirm !== profile }, retireBusy ? '处理中…' : '永久遗忘资料库'), h('button', { type: 'button', className: 'secondary-button', onClick: () => setRetireOpen(false), disabled: retireBusy }, '取消')))) : null,
    changingPassword ? h('div', { className: 'modal-backdrop' }, h('section', { className: 'modal', role: 'dialog', 'aria-modal': true, 'aria-labelledby': 'password-title' }, h('h2', { id: 'password-title' }, '修改配置密码'), h('form', { onSubmit: changePassword }, h('label', null, '当前密码', h('input', { type: 'password', autoComplete: 'current-password', value: currentPassword, onChange: (e: { target: HTMLInputElement }) => setCurrentPassword(e.target.value), required: true })), h('label', null, '新密码（至少 12 个字符）', h('input', { type: 'password', autoComplete: 'new-password', minLength: 12, value: newPassword, onChange: (e: { target: HTMLInputElement }) => setNewPassword(e.target.value), required: true })), passwordError ? h('p', { role: 'alert', className: 'auth-error' }, passwordError) : null, h('button', { type: 'submit' }, '修改并退出登录'), h('button', { type: 'button', className: 'secondary-button', onClick: () => setChangingPassword(false) }, '取消')))) : null,
    h(AudioBar, { children: h('div', { key: `${profile}:${authenticated}` }, children) }))
  const isPassword = mode === 'profile' || mode === 'profiles'
  return h('main', { className: 'auth-page' }, h('section', { className: 'auth-card', 'aria-labelledby': 'auth-title' },
    h('h1', { id: 'auth-title' }, '本地记忆'),
    h('p', null, mode === 'profiles' ? '选择一个配置并输入密码。' : mode === 'profile' ? `登录配置${profile ? `「${profile}」` : ''}。` : '请输入启动令牌以打开本地记忆。'),
    h('form', { onSubmit: submit },
      mode === 'profiles' ? h('label', { htmlFor: 'auth-profile' }, '配置', h('select', { id: 'auth-profile', value: selectedProfile, onChange: (event: { target: HTMLSelectElement }) => setSelectedProfile(event.target.value) }, profiles.length ? profiles.map((name) => h('option', { key: name, value: name }, name)) : h('option', { value: '' }, '没有可用配置'))) : null,
      h('label', { htmlFor: 'auth-credential' }, mode === 'token' ? '启动令牌或完整链接' : '密码'),
      h('input', { id: 'auth-credential', type: isPassword ? 'password' : 'text', value: credential, onChange: (event: { target: HTMLInputElement }) => setCredential(event.target.value), autoComplete: isPassword ? 'current-password' : 'off', spellCheck: false, placeholder: mode === 'token' ? '粘贴启动令牌或带 #token= 的完整链接' : '输入密码' }),
      h('button', { type: 'submit', disabled: submitting || (mode === 'profiles' && profiles.length === 0) }, submitting ? '验证中…' : '进入')),
    message ? h('p', { className: 'auth-error', role: 'alert' }, message) : null))
}
