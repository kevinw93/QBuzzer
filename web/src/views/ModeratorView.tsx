import { useEffect, useState, type SyntheticEvent } from 'react'
import { isTyping } from '../input'
import { ConnectionBanner, ErrorToast, Scoreboard } from '../components/common'
import { names, teamLabel } from '../roomInfo'
import type { RoomConnection } from '../net/connection'
import { roomExists, useRoom, wsURL } from '../net/useRoom'
import { MaxNameLen, MaxProtestNoteLen, type GameView, type RoomState } from '../protocol'
import { downloadCSV, scoresheetCSV } from '../scoresheet'
import { playBeep } from '../sound'
import { passwordKey } from '../storageKeys'
import { awaitingBuzz, keyAction, keyHelp, rule, type Action } from './moderatorKeys'

export function ModeratorView({ code }: { code: string }) {
  const [password, setPassword] = useState(localStorage.getItem(passwordKey(code)))
  const [attempt, setAttempt] = useState(0)
  if (!password) return <PasswordForm code={code} onPassword={(pw) => { setPassword(pw); setAttempt((n) => n + 1) }} />
  return (
    <Console
      key={attempt}
      code={code}
      password={password}
      onBadPassword={() => {
        localStorage.removeItem(passwordKey(code))
        setPassword(null)
      }}
    />
  )
}

function PasswordForm({ code, onPassword }: { code: string; onPassword: (pw: string) => void }) {
  const [value, setValue] = useState('')
  const submit = (e: SyntheticEvent) => {
    e.preventDefault()
    localStorage.setItem(passwordKey(code), value)
    onPassword(value)
  }
  return (
    <main className="center">
      <form onSubmit={submit}>
        <h2>Moderate room {code}</h2>
        <input aria-label="Moderator password" type="password" placeholder="Moderator password" value={value} onChange={(e) => { setValue(e.target.value) }} required autoFocus />
        <button type="submit">Moderate</button>
      </form>
    </main>
  )
}

function Console({ code, password, onBadPassword }: { code: string; password: string; onBadPassword: () => void }) {
  const [conn, snap] = useRoom(`mod:${code}:${password}`, () => ({
    url: wsURL(code),
    code,
    role: 'moderator',
    password,
    storage: sessionStorage,
    roomExists: () => roomExists(code),
  }))
  const room = snap.room
  const act = (a: Action | null) => {
    if (a) conn.send(a.type, a.payload)
  }

  useEffect(() => conn.onEffect((e) => { if (e.type === 'buzz_locked') playBeep() }), [conn])
  useEffect(() => {
    if (snap.problem === 'bad-password') onBadPassword()
  }, [snap.problem, onBadPassword])
  useEffect(() => {
    if (!room) return
    const onKey = (e: KeyboardEvent) => {
      if (isTyping(e.target) || e.metaKey || e.ctrlKey || e.altKey) return
      const a = keyAction(e, room.game)
      if (a) {
        e.preventDefault()
        conn.send(a.type, a.payload)
      }
    }
    window.addEventListener('keydown', onKey)
    return () => {
      window.removeEventListener('keydown', onKey)
    }
  }, [conn, room])

  if (!room) return <ConnectionBanner snap={snap} />
  const g = room.game

  return (
    <main className="moderator">
      <header>
        <span>
          Room <b>{room.code}</b> · players join at <code>{location.origin}/r/{room.code}</code>
        </span>
        <button type="button" onClick={() => { downloadCSV(`qbuzzer-${room.code}.csv`, scoresheetCSV(room)) }}>
          Export scoresheet (CSV, draft format)
        </button>
      </header>
      <ConnectionBanner snap={snap} />
      <section className="phase">
        <h2>
          Tossup {g.tossupNumber} · {phaseLabel(g)}
        </h2>
        <Controls room={room} act={act} />
      </section>
      <BuzzTable room={room} />
      <Scoreboard room={room} />
      {!g.rules.individual && <TeamAdmin room={room} conn={conn} />}
      <details>
        <summary>Keyboard shortcuts</summary>
        <dl className="keys">
          {keyHelp.map(([k, what]) => (
            <div key={k}>
              <dt>{k}</dt>
              <dd>{what}</dd>
            </div>
          ))}
        </dl>
      </details>
      <ErrorToast snap={snap} />
    </main>
  )
}

function phaseLabel(g: GameView): string {
  switch (g.phase) {
    case 'idle':
      return 'ready'
    case 'reading':
      return g.lockedOut.length ? 'reading (after a wrong answer)' : 'reading'
    case 'buzz_window':
      return 'collecting buzzes…'
    case 'awaiting_ruling':
      return 'awaiting ruling'
    case 'bonus':
      return 'bonus'
    case 'tossup_dead':
      return 'dead tossup'
  }
}

function Controls({ room, act }: { room: RoomState; act: (a: Action | null) => void }) {
  const g = room.game
  const t = { tossupId: g.tossupId }
  const undo = g.lastActionId ? (
    <button type="button" onClick={() => { act({ type: 'undo', payload: { actionId: g.lastActionId ?? '' } }) }}>
      Undo (U)
    </button>
  ) : null
  const protest = (
    <button
      type="button"
      onClick={() => {
        const note = prompt('Protest note for this tossup')
        if (note !== null) act({ type: 'protest', payload: { tossupId: g.tossupId, note: note.slice(0, MaxProtestNoteLen) } })
      }}
    >
      Protest
    </button>
  )

  switch (g.phase) {
    case 'idle':
      return <div className="controls">{[<button key="s" type="button" className="primary" onClick={() => { act({ type: 'start_tossup', payload: t }) }}>Start tossup {g.tossupNumber} (Space)</button>, undo]}</div>
    case 'reading':
      return <div className="controls">{[<button key="d" type="button" onClick={() => { act({ type: 'next', payload: t }) }}>Dead tossup (D)</button>, undo, protest]}</div>
    case 'buzz_window':
      return <div className="controls">{undo}</div>
    case 'awaiting_ruling': {
      const b = awaitingBuzz(g)
      const who = b ? names(room).get(b.playerId) : undefined
      return (
        <div className="controls ruling">
          <p className="holder">
            {who?.name ?? b?.playerId}
            {b && !g.rules.individual && <span> · {teamLabel(room, b.teamId)}</span>}
            {who && !who.connected && <span className="tag">disconnected</span>}
          </p>
          {g.rules.powerPoints > 0 && <button type="button" onClick={() => { act(rule(g, 'power')) }}>Power +{g.rules.powerPoints} (P)</button>}
          <button type="button" onClick={() => { act(rule(g, 'correct')) }}>+{g.rules.tossupPoints} (C)</button>
          {g.rules.negPoints !== 0 && <button type="button" onClick={() => { act(rule(g, 'neg')) }}>Neg {g.rules.negPoints} (N)</button>}
          <button type="button" onClick={() => { act(rule(g, 'no_penalty')) }}>0 (Z)</button>
          {undo}
        </div>
      )
    }
    case 'bonus':
      return (
        <div className="controls">
          <BonusParts room={room} act={act} />
          <button type="button" className="primary" onClick={() => { act({ type: 'next', payload: t }) }}>Next (Space)</button>
          {undo}
          {protest}
        </div>
      )
    case 'tossup_dead':
      return <div className="controls">{[<button key="n" type="button" className="primary" onClick={() => { act({ type: 'next', payload: t }) }}>Next (Space)</button>, undo, protest]}</div>
  }
}

function BonusParts({ room, act }: { room: RoomState; act: (a: Action | null) => void }) {
  const g = room.game
  const b = g.bonus
  if (!b) return null
  const others = g.teams.filter((t) => t.id !== b.teamId)
  return (
    <ol className="bonus">
      {b.parts.map((part, i) => {
        const ruleIt = (correct: boolean, bouncebackTeamId?: string) => {
          act({ type: 'rule_bonus', payload: { bonusId: b.id, part: i, correct, bouncebackTeamId } })
        }
        if (part.ruled) {
          return (
            <li key={i}>
              {part.correct ? '✓' : part.bouncebackTeamId ? `bounced to ${teamLabel(room, part.bouncebackTeamId)}` : '✗'}
            </li>
          )
        }
        return (
          <li key={i}>
            <button type="button" onClick={() => { ruleIt(true) }}>✓ ({i + 1})</button>
            <button type="button" onClick={() => { ruleIt(false) }}>✗</button>
            {g.rules.bounceback &&
              others.map((t) => (
                <button key={t.id} type="button" onClick={() => { ruleIt(false, t.id) }}>
                  → {t.name}
                </button>
              ))}
          </li>
        )
      })}
    </ol>
  )
}

function BuzzTable({ room }: { room: RoomState }) {
  const g = room.game
  if (g.buzzes.length === 0) return null
  const players = names(room)
  return (
    <table className="buzzes">
      <thead>
        <tr>
          <th>Player</th>
          {!g.rules.individual && <th>Team</th>}
          <th title="When the press was judged, after correcting for network delay">Pressed</th>
          <th title="How much earlier than its arrival the buzz was judged">Correction</th>
          <th>Status</th>
        </tr>
      </thead>
      <tbody>
        {g.buzzes.map((b) => (
          <tr key={b.id} className={b.status}>
            <td>{players.get(b.playerId)?.name ?? b.playerId}</td>
            {!g.rules.individual && <td>{teamLabel(room, b.teamId)}</td>}
            <td>{(b.offsetMs / 1000).toFixed(3)} s</td>
            <td>
              {b.correctionMs.toFixed(0)} ms
              {b.clamped && (
                <span className="tag warn" title="The client's timestamp was implausible and was clamped">
                  clamped
                </span>
              )}
            </td>
            <td>{b.status === 'correct' && b.power ? 'power' : b.status.replace('_', ' ')}</td>
          </tr>
        ))}
      </tbody>
    </table>
  )
}

function TeamAdmin({ room, conn }: { room: RoomState; conn: RoomConnection }) {
  const [name, setName] = useState('')
  const g = room.game
  const editable = g.phase === 'idle' || g.phase === 'bonus' || g.phase === 'tossup_dead'
  const teamOf = (pid: string) => g.teams.find((t) => t.playerIds.includes(pid))?.id ?? ''
  return (
    <section className="team-admin">
      <h3>Teams</h3>
      <form
        onSubmit={(e) => {
          e.preventDefault()
          conn.send('create_team', { name })
          setName('')
        }}
      >
        <input aria-label="New team name" placeholder="New team" value={name} onChange={(e) => { setName(e.target.value) }} maxLength={MaxNameLen} required disabled={!editable} />
        <button type="submit" disabled={!editable}>Add team</button>
      </form>
      <ul>
        {room.players.map((pl) => (
          <li key={pl.id}>
            <span className={pl.connected ? undefined : 'away'}>{pl.name}</span>
            <select
              aria-label={`Team for ${pl.name}`}
              value={teamOf(pl.id)}
              disabled={!editable}
              onChange={(e) => conn.send('assign_team', { playerId: pl.id, teamId: e.target.value || undefined })}
            >
              <option value="">No team</option>
              {g.teams.map((t) => (
                <option key={t.id} value={t.id}>
                  {t.name}
                </option>
              ))}
            </select>
          </li>
        ))}
      </ul>
      {!editable && <p className="hint">Teams can change between tossups.</p>}
    </section>
  )
}
