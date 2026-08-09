import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { eventTime } from '../input'
import { BuzzButton } from './BuzzButton'

afterEach(cleanup)

describe('BuzzButton', () => {
  it('converts both timestamp conventions to the epoch clock', () => {
    expect(eventTime(1500)).toBe(performance.timeOrigin + 1500)
    expect(eventTime(1.76e12)).toBe(1.76e12)
  })

  it('buzzes on pointerdown with the event time on the epoch clock', () => {
    const onBuzz = vi.fn()
    render(<BuzzButton enabled label="BUZZ" onBuzz={onBuzz} />)
    fireEvent.pointerDown(screen.getByRole('button'), { button: 0 })
    expect(onBuzz).toHaveBeenCalledOnce()
    const t = onBuzz.mock.calls[0]?.[0] as number
    expect(t).toBeGreaterThan(performance.timeOrigin)
    expect(t).toBeLessThanOrEqual(performance.timeOrigin + performance.now())
  })

  it('does not buzz on click alone, on other mouse buttons, or when disabled', () => {
    const onBuzz = vi.fn()
    const { rerender } = render(<BuzzButton enabled label="BUZZ" onBuzz={onBuzz} />)
    fireEvent.click(screen.getByRole('button'))
    fireEvent.pointerDown(screen.getByRole('button'), { button: 2 })
    rerender(<BuzzButton enabled={false} label="Locked" onBuzz={onBuzz} />)
    fireEvent.pointerDown(screen.getByRole('button'), { button: 0 })
    fireEvent.keyDown(window, { code: 'Space' })
    expect(onBuzz).not.toHaveBeenCalled()
  })

  it('buzzes on Space, ignoring key repeat and typing in inputs', () => {
    const onBuzz = vi.fn()
    render(
      <>
        <input aria-label="name" />
        <BuzzButton enabled label="BUZZ" onBuzz={onBuzz} />
      </>,
    )
    fireEvent.keyDown(window, { code: 'Space' })
    fireEvent.keyDown(window, { code: 'Space', repeat: true })
    fireEvent.keyDown(screen.getByLabelText('name'), { code: 'Space' })
    expect(onBuzz).toHaveBeenCalledOnce()
  })
})
