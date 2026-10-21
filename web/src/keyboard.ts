export interface KeyboardHandlers {
  close?: () => void
}

export const CLOSE_EVENT = 'local-memory-close'

function isTypingTarget(target: EventTarget | null): boolean {
  if (!(target instanceof HTMLElement)) return false
  if (target.isContentEditable) return true
  const tag = target.tagName
  return tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT'
}

export function resultRows(): HTMLElement[] {
  return Array.from(document.querySelectorAll<HTMLElement>('[data-result-row="true"]'))
}

export function moveActiveResult(delta: number): void {
  const rows = resultRows()
  if (rows.length === 0) return
  const active = document.activeElement
  const current = active instanceof HTMLElement ? rows.indexOf(active) : -1
  let next = current < 0 ? (delta > 0 ? 0 : rows.length - 1) : current + delta
  if (next < 0) next = 0
  if (next >= rows.length) next = rows.length - 1
  rows[next].focus()
  rows[next].scrollIntoView({ block: 'nearest' })
}

export function openActiveResult(): void {
  const active = document.activeElement
  if (active instanceof HTMLElement && active.matches('[data-result-row="true"]')) active.click()
}

export function installKeyboard(handlers: KeyboardHandlers = {}): () => void {
  function onKeyDown(event: KeyboardEvent) {
    if (event.defaultPrevented || event.metaKey || event.ctrlKey || event.altKey) return
    if (event.key === 'Escape') {
      handlers.close?.()
      window.dispatchEvent(new Event(CLOSE_EVENT))
      return
    }
    if (isTypingTarget(event.target)) return
    if (event.key === '/') {
      const input = document.querySelector<HTMLInputElement>('[data-search-input="true"]')
      if (input) {
        event.preventDefault()
        input.focus()
      }
      return
    }
    if (event.key === 'j' || event.key === 'k') {
      event.preventDefault()
      moveActiveResult(event.key === 'j' ? 1 : -1)
      return
    }
    if (event.key === 'Enter') openActiveResult()
  }
  window.addEventListener('keydown', onKeyDown)
  return () => window.removeEventListener('keydown', onKeyDown)
}
