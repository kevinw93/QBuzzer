import type { ClientMessage, PayloadOf } from '../messages'
import type { GameView, Ruling } from '../protocol'
import type { SeqType } from '../net/connection'

export type Action = { [T in SeqType]: { type: T; payload: PayloadOf<ClientMessage, T> } }[SeqType]

export const keyHelp: [string, string][] = [
  ['Space', 'Start tossup / next'],
  ['D', 'Dead tossup (no buzzes)'],
  ['P', 'Power'],
  ['C', '10 (correct)'],
  ['N', 'Neg'],
  ['Z', '0 (wrong, no penalty)'],
  ['1–9', 'Bonus part correct'],
  ['Shift + 1–9', 'Bonus part missed'],
  ['U', 'Undo'],
]

export function awaitingBuzz(g: GameView) {
  return g.buzzes.find((b) => b.status === 'awaiting')
}

export function rule(g: GameView, result: Ruling): Action | null {
  const b = awaitingBuzz(g)
  return b ? { type: 'rule', payload: { buzzId: b.id, result } } : null
}

/** Maps a key press to the moderator action it stands for in this game state. */
export function keyAction(e: { key: string; code: string; shiftKey: boolean }, g: GameView): Action | null {
  const tossupId = g.tossupId
  const digit = /^Digit([1-9])$/.exec(e.code)
  if (digit?.[1] && g.phase === 'bonus' && g.bonus) {
    const part = Number(digit[1]) - 1
    if (part >= g.bonus.parts.length) return null
    return { type: 'rule_bonus', payload: { bonusId: g.bonus.id, part, correct: !e.shiftKey } }
  }
  switch (e.key.toLowerCase()) {
    case ' ':
      if (g.phase === 'idle') return { type: 'start_tossup', payload: { tossupId } }
      if (g.phase === 'bonus' || g.phase === 'tossup_dead') return { type: 'next', payload: { tossupId } }
      return null
    case 'd':
      return g.phase === 'reading' ? { type: 'next', payload: { tossupId } } : null
    case 'p':
      return g.rules.powerPoints > 0 ? rule(g, 'power') : null
    case 'c':
      return rule(g, 'correct')
    case 'n':
      return g.rules.negPoints !== 0 ? rule(g, 'neg') : null
    case 'z':
      return rule(g, 'no_penalty')
    case 'u':
      return g.lastActionId ? { type: 'undo', payload: { actionId: g.lastActionId } } : null
  }
  return null
}
