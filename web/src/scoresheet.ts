import type { BuzzView, RoomState } from './protocol'

// DRAFT FORMAT: a placeholder until tournament directors' required format is
// known. One row per finished tossup: the buzzes, then per team the tossup
// points, bonus points, and running total.

function cell(v: string | number): string {
  const s = String(v)
  return /[",\n;]/.test(s) ? `"${s.replaceAll('"', '""')}"` : s
}

const rulingLabel: Record<string, string> = {
  correct: '',
  incorrect: 'neg',
  no_penalty: 'wrong',
  lost: 'lost',
}

export function scoresheetCSV(room: RoomState): string {
  const { game, players } = room
  const playerName = new Map(players.map((pl) => [pl.id, pl.name]))
  const teams = game.teams.map((t) => ({
    id: t.id,
    name: t.name || playerName.get(t.id) || t.id,
  }))
  const teamName = new Map(teams.map((t) => [t.id, t.name]))
  const totals = new Map(teams.map((t) => [t.id, 0]))

  const header = ['Tossup', 'Buzzes']
  for (const t of teams) header.push(`${t.name} TU`, `${t.name} Bonus`, `${t.name} Total`)
  const rows = [header.map(cell).join(',')]

  for (const rec of game.history) {
    const tu = new Map<string, number>()
    const bonus = new Map<string, number>()
    for (const b of rec.buzzes) tu.set(b.teamId, (tu.get(b.teamId) ?? 0) + b.points)
    for (const part of rec.bonus?.parts ?? []) {
      const to = part.correct ? rec.bonus?.teamId : part.bouncebackTeamId
      if (to) bonus.set(to, (bonus.get(to) ?? 0) + game.rules.bonusPartPoints)
    }
    const row: (string | number)[] = [rec.number, rec.buzzes.map((b) => describe(b, playerName, teamName)).join('; ')]
    for (const t of teams) {
      const total = (totals.get(t.id) ?? 0) + (tu.get(t.id) ?? 0) + (bonus.get(t.id) ?? 0)
      totals.set(t.id, total)
      row.push(tu.get(t.id) ?? 0, bonus.get(t.id) ?? 0, total)
    }
    rows.push(row.map(cell).join(','))
  }
  return rows.join('\n') + '\n'
}

function describe(b: BuzzView, playerName: Map<string, string>, teamName: Map<string, string>): string {
  const who = playerName.get(b.playerId) ?? b.playerId
  const team = teamName.get(b.teamId)
  const label = rulingLabel[b.status] ?? b.status
  const parts = [team && team !== who ? `${who} (${team})` : who]
  if (b.power) parts.push('power')
  if (label) parts.push(label)
  parts.push(b.points > 0 ? `+${String(b.points)}` : String(b.points))
  return parts.join(' ')
}

export function downloadCSV(filename: string, csv: string): void {
  const url = URL.createObjectURL(new Blob([csv], { type: 'text/csv' }))
  const a = document.createElement('a')
  a.href = url
  a.download = filename
  a.click()
  URL.revokeObjectURL(url)
}
