import { useEffect, useRef, useState, type ReactNode } from 'react'
import { getAudioURL } from '../api/client'
import { createContext, useContext } from 'react'

function formatMs(value: number): string {
  const total = Math.max(0, value) / 1000
  const minutes = Math.floor(total / 60)
  return `+${String(minutes).padStart(2, '0')}:${(total - minutes * 60).toFixed(1).padStart(4, '0')}`
}

function formatSeconds(value: number): string {
  const minutes = Math.floor(value / 60)
  return `${String(minutes).padStart(2, '0')}:${(value - minutes * 60).toFixed(1).padStart(4, '0')}`
}

interface AudioTiming {
  startMs: number | null
  endMs: number | null
}

interface AudioContextValue {
  turnId: string | null
  play: (turnId: string, timing?: AudioTiming) => void
  close: () => void
}

const AudioContext = createContext<AudioContextValue>({
  turnId: null,
  play: () => undefined,
  close: () => undefined,
})

export function useAudio(): AudioContextValue {
  return useContext(AudioContext)
}

export function AudioBar({ children }: { children: ReactNode }) {
  const [turnId, setTurnId] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [timing, setTiming] = useState<AudioTiming | undefined>()
  const [currentTime, setCurrentTime] = useState(0)
  const audioRef = useRef<HTMLAudioElement>(null)

  useEffect(() => {
    if (!turnId || !audioRef.current) return
    setError(null)
    const audio = audioRef.current
    void audio.play().catch((reason: unknown) => {
      setError(reason instanceof DOMException && reason.name === 'NotAllowedError'
        ? '浏览器阻止了自动播放，请点击播放器的播放按钮'
        : '无法开始播放音频，请检查播放器后重试')
    })
  }, [turnId])

  const close = () => {
    const audio = audioRef.current
    if (audio) {
      audio.pause()
      audio.removeAttribute('src')
      audio.load()
    }
    setTurnId(null)
    setError(null)
    setTiming(undefined)
    setCurrentTime(0)
  }

  const play = (id: string, segmentTiming?: AudioTiming) => {
    if (id === turnId && audioRef.current) {
      setError(null)
      void audioRef.current.play().catch((reason: unknown) => {
        setError(reason instanceof DOMException && reason.name === 'NotAllowedError'
          ? '浏览器阻止了播放，请点击播放器的播放按钮'
          : '无法开始播放音频，请检查播放器后重试')
      })
      return
    }
    setError(null)
    setTiming(segmentTiming)
    setCurrentTime(0)
    setTurnId(id)
  }

  return (
    <AudioContext.Provider value={{ turnId, play, close }}>
      {children}
      {turnId && (
        <aside className="audio-bar" aria-label="音频播放条">
          <span className="audio-bar-label">当前片段{timing?.startMs != null ? ` · ${formatMs(timing.startMs)}${timing.endMs != null ? `–${formatMs(timing.endMs)}` : ''}` : ''}{audioRef.current && Number.isFinite(currentTime) && currentTime > 0 ? ` · 播放 ${formatSeconds(currentTime)}` : ''}</span>
          <audio
            ref={audioRef}
            controls
            autoPlay
            src={getAudioURL(turnId)}
            onError={() => setError('音频加载或解码失败，无法播放（不一定表示文件缺失）')}
            onTimeUpdate={(event) => setCurrentTime(event.currentTarget.currentTime)}
            onEnded={() => setTurnId(null)}
          />
          {error && <span className="audio-error" role="alert">{error}</span>}
          <button type="button" className="secondary-button" onClick={close}>关闭</button>
        </aside>
      )}
    </AudioContext.Provider>
  )
}
