import { useCallback, useEffect, useState, type SyntheticEvent } from 'react'
import { BuzzButton } from '../components/BuzzButton'
import { ConnectionBanner, ErrorToast, LatencyBadge, Scoreboard } from '../components/common'
import { roomExists, useRoom, wsURL } from '../net/useRoom'
import { MaxNameLen, type RoomState } from '../protocol'
import { playBeep } from '../sound'
import { nameKey } from '../storageKeys'

export function PlayerView({ code }: { code: string }) {
  const [name, setName] = useState(sessionStorage.getItem(nameKey) ?? '')
  if (!name) return <NameForm onName={setName} />
  return <Player code={code} name={name} />
}

function NameForm({ onName }: { onName: (n: string) => void }) {
  const [value, setValue] = useState('')
  const submit = (e: SyntheticEvent) => {
    e.preventDefault()
    sessionStorage.setItem(nameKey, value.trim())
    onName(value.trim())
  }
  return (
    <main className="center">
      <form onSubmit={submit}>
        <input aria-label="Your name" placeholder="Your name" value={value} onChange={(e) => { setValue(e.target.value) }} maxLength={MaxNameLen} required autoFocus />
        <button type="submit">Join</button>
      </form>
    </main>
  )
}

function Player({ code, name }: { code: string; name: string }) {
  const [conn, snap] = useRoom(`player:${code}`, () => ({
    url: wsURL(code),
    code,
    role: 'player',
    name,
    storage: sessionStorage,
    roomExists: () => roomExists(code),
  }))
  useEffect(() => conn.onEffect((e) => { if (e.type === 'buzz_locked') playBeep() }), [conn])

  const room = snap.room
  const me = snap.welcome?.playerId
  const buzz = useCallback(
    (t: number) => {
      if (room) conn.send('buzz', { tossupId: room.game.tossupId, clientTimeMs: t })
    },
    [conn, room],
  )
  if (!room || !me) return <ConnectionBanner snap={snap} />

  const g = room.game
  const myTeam = g.rules.individual ? me : g.teams.find((t) => t.playerIds.includes(me))?.id
  const mine = g.buzzes.find((b) => b.playerId === me && (b.status === 'pending' || b.status === 'awaiting'))
  const locked = myTeam !== undefined && (g.lockedOut.includes(myTeam) || g.lockedOut.includes(me))
  const open = g.phase === 'reading' || g.phase === 'buzz_window'
  const holder = g.buzzes.find((b) => b.status === 'awaiting')

  let label = 'BUZZ'
  if (!myTeam) label = 'Choose a team'
  else if (mine?.status === 'awaiting') label = 'Your answer'
  else if (mine) label = 'Buzzed…'
  else if (locked) label = 'Locked out'
  else if (holder) label = `${room.players.find((pl) => pl.id === holder.playerId)?.name ?? 'Someone'} buzzed`
  else if (!open) label = g.phase === 'bonus' ? 'Bonus' : 'Waiting'

  return (
    <main className="player">
      <header>
        <span>
          Room <b>{room.code}</b> · {name}
        </span>
        <LatencyBadge rttMs={snap.rttMs} />
      </header>
      <ConnectionBanner snap={snap} />
      <BuzzButton enabled={open && !!myTeam && !locked && !mine && snap.status === 'open'} label={label} onBuzz={buzz} />
      {!g.rules.individual && (g.phase === 'idle' || g.phase === 'bonus' || g.phase === 'tossup_dead') && (
        <TeamPicker room={room} myTeam={myTeam} onPick={(teamId) => conn.send('choose_team', { teamId })} />
      )}
      <Scoreboard room={room} highlight={myTeam} />
      <ErrorToast snap={snap} />
    </main>
  )
}

function TeamPicker({ room, myTeam, onPick }: { room: RoomState; myTeam?: string; onPick: (id: string) => void }) {
  if (room.game.teams.length === 0) return <p className="hint">Waiting for the moderator to create teams.</p>
  return (
    <div className="teams-pick">
      {room.game.teams.map((t) => (
        <button key={t.id} type="button" className={t.id === myTeam ? 'selected' : undefined} onClick={() => { onPick(t.id) }}>
          {t.name}
        </button>
      ))}
    </div>
  )
}
