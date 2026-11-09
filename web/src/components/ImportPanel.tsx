import { useEffect, useRef, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { ApiError, getCapabilities, importRecording } from '../api/client'
import type { Capabilities } from '../api/types'

const ACCEPTED = '.wav,.m4a,.mp3,.aac,.flac,.ogg,.opus'

function formatBytes(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`
}

function capabilityMessage(caps: Capabilities | null): string {
  if (!caps) return '正在读取本地能力…'
  if (caps.asr.available) return `语音转写已就绪（${caps.asr.provider}）`
  switch (caps.asr.reason) {
    case 'asr_not_configured':
      return '未配置 ASR：设置 ASR_PROVIDER=doubao 并提供 VOLC_API_KEY，或使用明确的 fixture 演示模式'
    case 'worker_offline':
      return '后台工作进程未运行：启动 worker 后才能转写（可先排队）'
    case 'worker_asr_mismatch':
      return '工作进程没有启用该 ASR provider：请让 web 与 worker 使用同一个 ASR_PROVIDER'
    default:
      return '当前无法转写音频'
  }
}

function errorText(error: unknown): { message: string; action: string } {
  if (error instanceof ApiError) {
    const details = error.details as { action?: string } | undefined
    return { message: error.message, action: details?.action || '' }
  }
  return { message: error instanceof Error ? error.message : '上传失败', action: '' }
}

export function ImportPanel({ onImported }: { onImported?: () => void }) {
  const navigate = useNavigate()
  const [caps, setCaps] = useState<Capabilities | null>(null)
  const [file, setFile] = useState<File | null>(null)
  const [busy, setBusy] = useState(false)
  const [message, setMessage] = useState('')
  const [action, setAction] = useState('')
  const inputRef = useRef<HTMLInputElement | null>(null)

  useEffect(() => {
    let active = true
    getCapabilities()
      .then((value) => { if (active) setCaps(value) })
      .catch(() => { if (active) setCaps(null) })
    return () => { active = false }
  }, [])

  const configured = caps?.asr.configured ?? false
  const canUpload = Boolean(file) && configured && !busy
  const extension = file ? (file.name.match(/\.[^.]+$/) || [''])[0].toLowerCase() : ''

  async function submit() {
    if (!file || !configured) return
    setBusy(true)
    setMessage('')
    setAction('')
    try {
      const result = await importRecording(file)
      onImported?.()
      navigate(`/c/${encodeURIComponent(result.conversation_id)}`)
    } catch (error) {
      const info = errorText(error)
      setMessage(info.message)
      setAction(info.action)
    } finally {
      setBusy(false)
    }
  }

  return (
    <section className="import-panel" data-testid="import-panel">
      <header className="import-panel-header">
        <h2>导入录音</h2>
        <p className="import-hint" data-testid="import-capability">{capabilityMessage(caps)}</p>
      </header>
      <div className="import-controls">
        <input
          ref={inputRef}
          type="file"
          accept={ACCEPTED}
          data-testid="import-file-input"
          aria-label="选择音频文件"
          onChange={(event) => {
            setFile(event.target.files?.[0] ?? null)
            setMessage('')
            setAction('')
          }}
        />
        {file && (
          <dl className="import-file-meta">
            <dt>文件</dt>
            <dd data-testid="import-file-name">{file.name}</dd>
            <dt>大小</dt>
            <dd data-testid="import-file-size">{formatBytes(file.size)}</dd>
            <dt>格式</dt>
            <dd data-testid="import-file-format">{extension.replace('.', '')}</dd>
          </dl>
        )}
        <button
          type="button"
          className="primary-button"
          data-testid="import-submit"
          disabled={!canUpload}
          aria-busy={busy}
          onClick={() => void submit()}
        >
          {busy ? '上传中…' : '导入并转写'}
        </button>
      </div>
      {!configured && caps && (
        <p className="import-warning">上传已禁用：{capabilityMessage(caps)}</p>
      )}
      {configured && caps && !caps.worker.alive && (
        <p className="import-warning">任务会先排队，worker 启动后继续处理。</p>
      )}
      {(message || action) && (
        <p className="note-error" role="alert" data-testid="import-error">
          {message}
          {action && <span className="import-action">下一步：{action}</span>}
        </p>
      )}
    </section>
  )
}
