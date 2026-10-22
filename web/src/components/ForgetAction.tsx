import { useEffect, useRef, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { commitForget, forgetStatus, pendingForgets, previewForget, retryForget, type ForgetPreview, type ForgetRequest } from '../api/retention'
import { useAudio } from './AudioBar'

export function forgetStorageKey(profile: string) { return `recording-forget:${profile || 'auth-session'}` }
const changedEvent = 'recording-forget-changed'
function clock(ms?: number) { return ms == null ? '未知' : `+${Math.floor(ms / 60000)}:${String(Math.floor(ms / 1000) % 60).padStart(2, '0')}` }
export function ForgetAction({ request, label, onDone, profile = '' }: { request: ForgetRequest; label: string; onDone?: () => void; profile?: string }) {
  const pendingKey = forgetStorageKey(profile)
  const [preview, setPreview] = useState<ForgetPreview | null>(null)
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const [allowWhole, setAllowWhole] = useState(false)
  const dialog = useRef<HTMLDivElement>(null)
  const opener = useRef<HTMLElement | null>(null)
  const { close } = useAudio()
  const navigate = useNavigate()
  useEffect(() => { if (preview) { opener.current = document.activeElement as HTMLElement; dialog.current?.focus() } else opener.current?.focus() }, [preview])
  async function start() { setBusy(true); setError(''); try { setPreview(await previewForget(request)) } catch (e) { setError(e instanceof Error ? e.message : '预览失败') } finally { setBusy(false) } }
  async function commit() {
    if (!preview) return
    setBusy(true); setError('')
    try {
      const result = await commitForget({ ...request, expected_state: preview.expected_state, allow_whole_audio_removal: allowWhole })
      sessionStorage.setItem(pendingKey, result.tombstone_id)
      window.dispatchEvent(new Event(changedEvent))
      await poll(result.tombstone_id)
      if (!sessionStorage.getItem(pendingKey)) { setPreview(null); close(); onDone?.(); if (request.scope === 'conversation') navigate('/') }
    } catch (e) { setError(e instanceof Error ? e.message : '遗忘操作失败') } finally { setBusy(false) }
  }
  async function poll(id: string) { let s = await forgetStatus(id); for (let i = 0; i < 4 && s.status !== 'complete'; i++) { await new Promise(r => setTimeout(r, 1000)); s = await forgetStatus(id) } if (s.status === 'complete') { sessionStorage.removeItem(pendingKey); window.dispatchEvent(new Event(changedEvent)) } }
  return <>
    <button type="button" className="danger-button" onClick={() => void start()} disabled={busy}>{label}</button>
    {error && !preview && <p role="alert" className="note-error">{error} <button type="button" className="secondary-button" onClick={() => void start()}>重试</button></p>}
    {preview && <div className="modal-backdrop"><section className="modal" role="dialog" aria-modal="true" aria-labelledby="forget-title" tabIndex={-1} ref={dialog} onKeyDown={e => { if (e.key === 'Escape' && !busy) setPreview(null); if(e.key==='Tab' && dialog.current){const f=Array.from(dialog.current.querySelectorAll<HTMLElement>('button:not([disabled]),input:not([disabled])'));if(f.length){if(e.shiftKey&&document.activeElement===f[0]){e.preventDefault();f[f.length-1].focus()}else if(!e.shiftKey&&document.activeElement===f[f.length-1]){e.preventDefault();f[0].focus()}}}}}>
      <h2 id="forget-title">确认永久遗忘</h2><p>操作范围：{request.scope === 'turn' ? '这句话' : request.scope === 'recording' ? '此录音' : '整个会话'}。请核对影响后再次确认。</p>
      <ul><li>转写：{preview.turns ?? 0}；归纳：{preview.notes ?? 0}</li><li>附件：{preview.attachments ?? 0}；候选：{preview.candidates ?? 0}；向量：{preview.vectors ?? 0}；任务：{preview.jobs ?? 0}</li><li>本地音频文件：{preview.audio_files?.length ?? 0}</li></ul>
      {preview.audio_files?.map((a,i) => <p key={i}>音频区间：{a.offsets_known ? `${clock(a.start_ms)}–${clock(a.end_ms)}` : '时间未知'}{a.remove_file ? '（整文件将删除）' : ''}</p>)}
      <p><strong>托管备份将删除 {preview.managed_backup_count ?? 0} 份。</strong> {preview.retention_notice || '云端/手动导出/系统备份不会删除；提供方录音的未来同步会被阻止，但其他本地原始数据仍保留。'}</p>
      {preview.requires_broadened_audio && <label><input type="checkbox" checked={allowWhole} onChange={e=>setAllowWhole(e.target.checked)}/>我明确确认：时间偏移未知，允许删除整段音频文件</label>}
      {error && <p role="alert" className="note-error">{error}</p>}
      <div className="modal-actions"><button type="button" onClick={() => void commit()} disabled={busy || (preview.requires_broadened_audio && !allowWhole)}>{busy ? '处理中…' : '确认永久遗忘'}</button><button type="button" className="secondary-button" onClick={() => setPreview(null)} disabled={busy}>取消</button></div>
    </section></div>}
  </>
}

export function PendingForget({ profile }: { profile: string }) {
  const key = forgetStorageKey(profile)
  const [id, setId] = useState(() => sessionStorage.getItem(key) || '')
  const [status, setStatus] = useState('检查中…')
  const [busy, setBusy] = useState(false)
  const refresh = async () => {
    const local = sessionStorage.getItem(key) || ''
    if (local) { setId(local); return }
    try { const result = await pendingForgets(); setId(result.operations?.[0]?.id || '') }
    catch { setId('') }
  }
  useEffect(() => { void refresh(); const changed = () => { void refresh() }; window.addEventListener(changedEvent, changed); window.addEventListener('storage', changed); return () => { window.removeEventListener(changedEvent, changed); window.removeEventListener('storage', changed) } }, [key])
  useEffect(() => { if (!id) return; let alive=true; const check=async()=>{try{const result=await forgetStatus(id);if(!alive)return;setStatus(result.status);if(result.status==='complete'){if(sessionStorage.getItem(key)===id)sessionStorage.removeItem(key);void refresh()}}catch{if(alive)setStatus('状态暂不可用')}};void check();const timer=window.setInterval(()=>void check(),3000);return()=>{alive=false;clearInterval(timer)} }, [id,key])
  async function retry(){setBusy(true);try{await retryForget(id);setStatus('重试已提交；正在检查状态')}catch(e){setStatus(e instanceof Error?e.message:'重试失败')}finally{setBusy(false)}}
  if(!id)return null
  return <aside className="state-view" role="status"><strong>遗忘操作处理中</strong>：{status} <button type="button" className="secondary-button" disabled={busy} onClick={()=>void retry()}>重试清理</button></aside>
}
export function setPendingForget(profile: string, id: string) { sessionStorage.setItem(forgetStorageKey(profile), id); window.dispatchEvent(new Event(changedEvent)) }
export function clearPendingForget(profile: string) { sessionStorage.removeItem(forgetStorageKey(profile)); window.dispatchEvent(new Event(changedEvent)) }
export function pendingForgetChangedEvent() { return changedEvent }
