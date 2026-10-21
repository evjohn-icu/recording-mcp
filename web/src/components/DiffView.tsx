import { useMemo } from 'react'

interface DiffViewProps {
  before: string
  after: string
  beforeLabel?: string
  afterLabel?: string
}

type DiffLine = { type: 'same' | 'add' | 'remove'; text: string }

function diffLines(before: string, after: string): DiffLine[] {
  const a = before.split(/\r?\n/)
  const b = after.split(/\r?\n/)
  const table: number[][] = Array.from({ length: a.length + 1 }, () => Array<number>(b.length + 1).fill(0))
  for (let i = a.length - 1; i >= 0; i -= 1) {
    for (let j = b.length - 1; j >= 0; j -= 1) {
      table[i][j] = a[i] === b[j] ? table[i + 1][j + 1] + 1 : Math.max(table[i + 1][j], table[i][j + 1])
    }
  }
  const result: DiffLine[] = []
  let i = 0
  let j = 0
  while (i < a.length && j < b.length) {
    if (a[i] === b[j]) {
      result.push({ type: 'same', text: a[i] })
      i += 1
      j += 1
    } else if (table[i + 1][j] >= table[i][j + 1]) {
      result.push({ type: 'remove', text: a[i] })
      i += 1
    } else {
      result.push({ type: 'add', text: b[j] })
      j += 1
    }
  }
  while (i < a.length) result.push({ type: 'remove', text: a[i++] })
  while (j < b.length) result.push({ type: 'add', text: b[j++] })
  return result
}

export function DiffView({ before, after, beforeLabel = '旧版本', afterLabel = '当前版本' }: DiffViewProps) {
  const lines = useMemo(() => diffLines(before, after), [before, after])
  return (
    <div className="diff-view">
      <div className="diff-labels"><span>{beforeLabel}</span><span>{afterLabel}</span></div>
      <div className="diff-lines">
        {lines.map((line, index) => (
          <div className={`diff-line diff-${line.type}`} key={`${index}-${line.type}-${line.text}`}>
            <span className="diff-marker" aria-hidden="true">{line.type === 'add' ? '+' : line.type === 'remove' ? '−' : ' '}</span>
            <span>{line.text || ' '}</span>
          </div>
        ))}
      </div>
    </div>
  )
}
