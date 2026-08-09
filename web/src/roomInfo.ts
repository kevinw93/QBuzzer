import type { PlayerView, RoomState } from './protocol'

export function names(room: RoomState): Map<string, PlayerView> {
  return new Map(room.players.map((pl) => [pl.id, pl]))
}

export function teamLabel(room: RoomState, teamId: string): string {
  const t = room.game.teams.find((x) => x.id === teamId)
  return t?.name || names(room).get(teamId)?.name || teamId
}
