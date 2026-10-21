import type { NoteView } from '../api/types'

const NOTE_KIND_LABELS: Record<string, string> = {
  observation: '观察',
  request: '请求',
  intention: '意图',
  constraint: '约束',
  suggestion: '建议',
}

export function noteKindLabel(kind: string): string {
  return NOTE_KIND_LABELS[kind] || kind
}

export interface NoteCardProps {
  note: NoteView
  highlighted?: boolean
  editing: boolean
  draft: string
  saving: boolean
  onDraftChange: (value: string) => void
  onStartEdit: () => void
  onCancelEdit: () => void
  onSubmitEdit: () => void
  onOpenEvidence: (turnId: string) => void
}

export function NoteCard({
  note,
  highlighted = false,
  editing,
  draft,
  saving,
  onDraftChange,
  onStartEdit,
  onCancelEdit,
  onSubmitEdit,
  onOpenEvidence,
}: NoteCardProps) {
  const canEdit = note.annotation_version !== '' && note.evidence_turn_ids.length > 0
  return (
    <article className={`note-card ${highlighted ? 'note-highlight' : ''}`.trim()}>
      <header className="note-card-header">
        <span className="note-kind">{noteKindLabel(note.kind)}</span>
        <span className={`note-origin ${note.edited ? 'note-edited' : 'note-ai'}`}>
          {note.edited ? '人工修订 · 尚未确认' : 'AI 提议 · 尚未确认'}
        </span>
      </header>
      {editing ? (
        <div className="note-editor">
          <label htmlFor={`note-summary-${note.row_id}`}>摘要（仅摘要可修改）</label>
          <textarea
            id={`note-summary-${note.row_id}`}
            value={draft}
            rows={4}
            aria-describedby={`note-limit-${note.row_id}`}
            onChange={(event) => onDraftChange(Array.from(event.target.value).slice(0, 4000).join(''))}
          />
          <p id={`note-limit-${note.row_id}`} className="note-limit">{Array.from(draft).length}/4000 字符</p>
          <div className="note-editor-actions">
            <button type="button" onClick={onSubmitEdit} disabled={saving}>{saving ? '保存中…' : '保存'}</button>
            <button type="button" className="secondary-button" onClick={onCancelEdit} disabled={saving}>取消</button>
          </div>
        </div>
      ) : (
        <p className="note-summary">{note.summary}</p>
      )}
      {note.topic_ids.length > 0 && (
        <ul className="note-topics" aria-label="主题">
          {note.topic_ids.map((topicId) => <li key={topicId}>{topicId}</li>)}
        </ul>
      )}
      {note.evidence_turn_ids.length > 0 ? (
        <div className="note-evidence">
          {note.evidence_turn_ids.map((turnId, index) => (
            <button type="button" className="chip" key={turnId} onClick={() => onOpenEvidence(turnId)}>
              证据 {index + 1}
            </button>
          ))}
        </div>
      ) : (
        <p className="note-no-evidence">该版本缺少证据引用，暂不可编辑</p>
      )}
      {!editing && canEdit && (
        <div className="note-actions">
          <button type="button" className="secondary-button" onClick={onStartEdit}>编辑</button>
        </div>
      )}
    </article>
  )
}
