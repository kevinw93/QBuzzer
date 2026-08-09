import type { Snapshot } from '../net/connection'
import type { RoomState } from '../protocol'
import { names, teamLabel } from '../roomInfo'

const problemText: Record<NonNullable<Snapshot['problem']>, string> = {
  reload: 'QBuzzer was updated. Reload the page.',
  'bad-password': 'Wrong moderator password.',
  'rate-limited': 'Too many wrong passwords. Wait a minute and try again.',
  replaced: 'This session was opened in another tab or by another moderator.',
  'room-full': 'This room is full.',
  'room-gone': 'This room has closed.',
}

export function ConnectionBanner({ snap }: { snap: Snapshot }) {
  if (snap.problem) return <p className="banner error">{problemText[snap.problem]}</p>
  if (snap.status === 'connecting') return <p className="banner">Connecting…</p>
  if (snap.status === 'reconnecting') return <p className="banner warn">Connection lost. Reconnecting…</p>
  return null
}

export function ErrorToast({ snap }: { snap: Snapshot }) {
  return snap.lastError ? <p className="toast">{snap.lastError.message}</p> : null
}

export function Scoreboard({ room, highlight }: { room: RoomState; highlight?: string }) {
  const players = names(room)
  return (
    <table className="scores">
      <tbody>
        {room.game.teams.map((t) => (
          <tr key={t.id} className={t.id === highlight ? 'mine' : undefined}>
            <th>
              {teamLabel(room, t.id)}
              {room.game.lockedOut.includes(t.id) && <span className="tag">locked</span>}
            </th>
            {!room.game.rules.individual && (
              <td className="members">
                {t.playerIds.map((id) => {
                  const pl = players.get(id)
                  return (
                    <span key={id} className={pl?.connected ? undefined : 'away'}>
                      {pl?.name ?? id}
                    </span>
                  )
                })}
              </td>
            )}
            <td className="score">{t.score}</td>
          </tr>
        ))}
      </tbody>
    </table>
  )
}

export function LatencyBadge({ rttMs }: { rttMs: number | null }) {
  if (rttMs === null) return <span className="latency">measuring…</span>
  const quality = rttMs < 60 ? 'good' : rttMs < 150 ? 'ok' : 'poor'
  return (
    <span className={`latency ${quality}`} title="Round-trip time to the server">
      {Math.round(rttMs)} ms
    </span>
  )
}
