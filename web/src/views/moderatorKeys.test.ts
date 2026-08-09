import { describe, expect, it } from 'vitest'
import type { GameView } from '../protocol'
import { keyAction } from './moderatorKeys'

const base: GameView = {
  phase: 'idle', tossupId: 't4', tossupNumber: 4, buzzes: [], lockedOut: [], teams: [], protests: [], history: [],
  rules: { individual: false, tossupPoints: 10, powerPoints: 15, negPoints: -5, bonusParts: 3, bonusPartPoints: 10, bounceback: false },
}
const key = (k: string, code = '', shiftKey = false) => ({ key: k, code, shiftKey })
const awaiting: GameView = {
  ...base,
  phase: 'awaiting_ruling',
  buzzes: [{ id: 'b9', playerId: 'p1', teamId: 'T1', offsetMs: 1, correctionMs: 0, clamped: false, status: 'awaiting', power: false, points: 0 }],
}

describe('keyAction', () => {
  it('space starts in idle and advances from bonus or a dead tossup, never mid-tossup', () => {
    expect(keyAction(key(' '), base)).toEqual({ type: 'start_tossup', payload: { tossupId: 't4' } })
    expect(keyAction(key(' '), { ...base, phase: 'tossup_dead' })).toEqual({ type: 'next', payload: { tossupId: 't4' } })
    expect(keyAction(key(' '), { ...base, phase: 'reading' })).toBeNull()
    expect(keyAction(key(' '), awaiting)).toBeNull()
  })

  it('D kills a tossup only while reading', () => {
    expect(keyAction(key('d'), { ...base, phase: 'reading' })).toEqual({ type: 'next', payload: { tossupId: 't4' } })
    expect(keyAction(key('d'), base)).toBeNull()
  })

  it.each([
    ['p', 'power'],
    ['c', 'correct'],
    ['N', 'neg'],
    ['z', 'no_penalty'],
  ])('%s rules the awaiting buzz %s', (k, result) => {
    expect(keyAction(key(k), awaiting)).toEqual({ type: 'rule', payload: { buzzId: 'b9', result } })
  })

  it('ruling keys do nothing without an awaiting buzz, and P/N do nothing when the rules lack them', () => {
    expect(keyAction(key('c'), base)).toBeNull()
    const plain = { ...awaiting, rules: { ...awaiting.rules, powerPoints: 0, negPoints: 0 } }
    expect(keyAction(key('p'), plain)).toBeNull()
    expect(keyAction(key('n'), plain)).toBeNull()
  })

  it('digits rule bonus parts; shift marks them missed', () => {
    const bonus: GameView = { ...base, phase: 'bonus', bonus: { id: 'B4', teamId: 'T1', parts: [{ ruled: false, correct: false }, { ruled: false, correct: false }, { ruled: false, correct: false }] } }
    expect(keyAction(key('2', 'Digit2'), bonus)).toEqual({ type: 'rule_bonus', payload: { bonusId: 'B4', part: 1, correct: true } })
    expect(keyAction(key('@', 'Digit2', true), bonus)).toEqual({ type: 'rule_bonus', payload: { bonusId: 'B4', part: 1, correct: false } })
    expect(keyAction(key('4', 'Digit4'), bonus)).toBeNull()
  })

  it('U undoes the latest action', () => {
    expect(keyAction(key('u'), base)).toBeNull()
    expect(keyAction(key('u'), { ...base, lastActionId: 'a17' })).toEqual({ type: 'undo', payload: { actionId: 'a17' } })
  })
})
