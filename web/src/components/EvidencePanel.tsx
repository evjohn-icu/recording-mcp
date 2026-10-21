import { useApi } from '../hooks/useApi'
import { useEffect, useState } from 'react'
import { getEvidence, getItem } from '../api/client'
import type { AudioStatus, EvidenceTurn } from '../api/types'
import { StateView } from './StateView'
import { useAudio } from './AudioBar'

function turnTime(turn: EvidenceTurn): string {
  if (turn.global_start_ms === null || !Number.isFinite(turn.global_start_ms)) return '时间未知'
  const total = Math.max(0, turn.global_start_ms) / 1000
  const minutes = Math.floor(total / 60)
  const seconds = total - minutes * 60
  return `+${String(minutes).padStart(2, '0')}:${seconds.toFixed(1).padStart(4, '0')}`
}

export function EvidencePanel({ turnId, onClose }: { turnId: string; onClose: () => void }) {
  const { play } = useAudio()
  const { data, loading, error, reload } = useApi((signal) => getEvidence(turnId, undefined, undefined, signal), [turnId])
  const [metadata, setMetadata] = useState<Record<string, { audio_status?: AudioStatus; speaker_person_id?: string; speaker_confirmed?: boolean }>>({})

  useEffect(() => {
    let active = true
    const controller = new AbortController()
    setMetadata({})
    if (data) {
      const needsMetadata = data.turns.filter((turn) => turn.audio_status === undefined)
      if (needsMetadata.length === 0) {
        setMetadata({})
      } else {
        void Promise.all(needsMetadata.map(async (turn) => {
          try {
            const { item } = await getItem(turn.id, controller.signal)
            return [turn.id, {
              audio_status: item.audio_status,
              speaker_person_id: item.speaker_person_id,
              speaker_confirmed: item.speaker_confirmed,
            }] as const
          } catch {
            return null
          }
        })).then((entries) => {
          if (active) setMetadata(Object.fromEntries(entries.filter((entry): entry is NonNullable<typeof entry> => entry !== null)))
        })
      }
    }
    return () => { active = false; controller.abort() }
  }, [data])

  return (
    <aside className="evidence-panel" role="dialog" aria-modal="false" aria-label="证据窗口">
      <header className="panel-header">
        <h2>证据窗口</h2>
        <button type="button" className="secondary-button" onClick={onClose}>关闭</button>
      </header>
      <StateView loading={loading} error={error} onRetry={reload} empty={!!data && data.turns.length === 0} emptyMessage="没有可展示的上下文">
        {data && (
          <div className="evidence-body">
            {data.truncated && <p className="panel-warning" role="status">证据窗口已截断</p>}
              {data.turns.map((turn) => {
                const extra = metadata[turn.id]
                const status = turn.audio_status ?? extra?.audio_status
                const speaker = turn.speaker_person_id ?? extra?.speaker_person_id
                const confirmed = turn.speaker_confirmed === true || extra?.speaker_confirmed === true
                const audioMessage = status === 'deleted' ? '录音已删除（转写与归纳保留）'
                  : status === 'missing' ? '本机找不到音频文件'
                    : status === 'offset_unknown' ? '该片段缺少时间信息，无法定位音频'
                      : status === 'out_of_range' ? '音频时间超出录音范围'
                        : status === 'unavailable' ? '此录音没有本地音频'
                          : '音频状态未知，暂不可播放'
                return (
                  <article className={`evidence-turn ${turn.is_target ? 'evidence-target' : ''}`.trim()} key={turn.id}>
                    <header>
                      <span>{turnTime(turn)}</span>
                      <span>{speaker && confirmed ? `${turn.speaker_scoped_id || ''}${turn.speaker_scoped_id ? '（' : ''}${speaker}${turn.speaker_scoped_id ? '）' : ''}` : turn.speaker_scoped_id ? `${turn.speaker_scoped_id} · 未确认身份` : '未确认身份'}</span>
                      {turn.is_target && <span className="target-tag">目标轮次</span>}
                    </header>
                    <p>{turn.text}</p>
                    {status === 'available' ? (
                      <button type="button" className="secondary-button" onClick={() => play(turn.id, { startMs: turn.global_start_ms, endMs: turn.global_end_ms })}>播放</button>
                    ) : (
                      <button type="button" className="secondary-button" disabled title={audioMessage}>{audioMessage}</button>
                    )}
                  </article>
                )
              })}
          </div>
        )}
      </StateView>
    </aside>
  )
}
