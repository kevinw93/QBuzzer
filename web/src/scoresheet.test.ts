import { describe, expect, it } from 'vitest'
import type { RoomState } from './protocol'
import { scoresheetCSV } from './scoresheet'

const room: RoomState = {
  epoch: 'e', version: 9, code: 'ABC234', moderatorConnected: true,
  players: [
    { id: 'p1', name: 'Alice', connected: true },
    { id: 'p2', name: 'Bob, Jr.', connected: true },
  ],
  game: {
    phase: 'idle', tossupId: 't3', tossupNumber: 3, buzzes: [], lockedOut: [], protests: [], lastActionId: 'a9',
    teams: [
      { id: 'T1', name: 'Alpha', score: 35, playerIds: ['p1'] },
      { id: 'T2', name: 'Beta', score: 5, playerIds: ['p2'] },
    ],
    rules: { individual: false, tossupPoints: 10, powerPoints: 15, negPoints: -5, bonusParts: 3, bonusPartPoints: 10, bounceback: true },
    history: [
      {
        tossupId: 't1', number: 1,
        buzzes: [
          { id: 'b1', playerId: 'p2', teamId: 'T2', offsetMs: 900, correctionMs: 20, clamped: false, status: 'incorrect', power: false, points: -5 },
          { id: 'b2', playerId: 'p1', teamId: 'T1', offsetMs: 2100, correctionMs: 15, clamped: false, status: 'correct', power: true, points: 15 },
        ],
        bonus: { id: 'B1', teamId: 'T1', parts: [{ ruled: true, correct: true }, { ruled: true, correct: false, bouncebackTeamId: 'T2' }, { ruled: true, correct: true }] },
      },
      { tossupId: 't2', number: 2, buzzes: [] },
    ],
  },
}

describe('scoresheetCSV', () => {
  it('writes one row per tossup with running totals', () => {
    expect(scoresheetCSV(room)).toBe(
      [
        'Tossup,Buzzes,Alpha TU,Alpha Bonus,Alpha Total,Beta TU,Beta Bonus,Beta Total',
        '1,"Bob, Jr. (Beta) neg -5; Alice (Alpha) power +15",15,20,35,-5,10,5',
        '2,,0,0,35,0,0,5',
        '',
      ].join('\n'),
    )
  })

  it('uses player names as team names in individual play', () => {
    const solo: RoomState = {
      ...room,
      game: { ...room.game, rules: { ...room.game.rules, individual: true }, teams: [{ id: 'p1', name: '', score: 0, playerIds: ['p1'] }], history: [] },
    }
    expect(scoresheetCSV(solo).split('\n')[0]).toBe('Tossup,Buzzes,Alice TU,Alice Bonus,Alice Total')
  })
})
