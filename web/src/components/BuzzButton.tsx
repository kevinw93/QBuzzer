import { useEffect } from 'react'
import { eventTime, isTyping } from '../input'

/**
 * The buzz fires on press (pointerdown / keydown), never on click: a click
 * fires on release, often 50–150 ms later. The timestamp is the event's own,
 * converted to the epoch-based clock that clock sync uses.
 */
export function BuzzButton({ enabled, label, onBuzz }: { enabled: boolean; label: string; onBuzz: (clientTimeMs: number) => void }) {
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.code !== 'Space' || e.repeat || isTyping(e.target)) return
      e.preventDefault() // no page scroll
      if (enabled) onBuzz(eventTime(e.timeStamp))
    }
    window.addEventListener('keydown', onKey)
    return () => {
      window.removeEventListener('keydown', onKey)
    }
  }, [enabled, onBuzz])

  return (
    <button
      type="button"
      className="buzz"
      disabled={!enabled}
      onPointerDown={(e) => {
        if (e.button !== 0 || !enabled) return
        e.preventDefault()
        onBuzz(eventTime(e.timeStamp))
      }}
    >
      {label}
    </button>
  )
}
