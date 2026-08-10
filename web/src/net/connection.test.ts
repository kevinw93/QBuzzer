import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { ServerMessage } from '../messages'
import { RoomConnection, type KV, type Options } from './connection'

class FakeWS {
  static all: FakeWS[] = []
  readyState = 0
  sent: unknown[] = []
  onopen: (() => void) | null = null
  onmessage: ((ev: { data: string }) => void) | null = null
  onclose: ((ev: { code: number }) => void) | null = null
  url: string
  constructor(url: string) {
    this.url = url
    FakeWS.all.push(this)
  }
  send(data: string) {
    this.sent.push(JSON.parse(data))
  }
  close(code = 1000) {
    this.serverClose(code)
  }
  open() {
    this.readyState = 1
    this.onopen?.()
  }
  receive(msg: ServerMessage) {
    this.onmessage?.({ data: JSON.stringify(msg) })
  }
  serverClose(code: number) {
    this.readyState = 3
    this.onclose?.({ code })
  }
  last() {
    return this.sent.at(-1) as { type: string; seq?: number; payload: Record<string, unknown> }
  }
}

const latest = () => {
  const ws = FakeWS.all.at(-1)
  if (!ws) throw new Error('no socket')
  return ws
}

function memoryKV(): KV & { data: Map<string, string> } {
  const data = new Map<string, string>()
  return {
    data,
    getItem: (k) => data.get(k) ?? null,
    setItem: (k, v) => data.set(k, v),
    removeItem: (k) => data.delete(k),
  }
}

function make(extra: Partial<Options> = {}) {
  const storage = memoryKV()
  let clock = 1000
  const conn = new RoomConnection({
    url: 'ws://test/api/rooms/ABC234/ws',
    code: 'ABC234',
    role: 'player',
    name: 'Alice',
    storage,
    WebSocketImpl: FakeWS as unknown as new (url: string) => WebSocket,
    now: () => (clock += 0.5),
    random: () => 1,
    ...extra,
  })
  return { conn, storage }
}

const welcome = (lastSeq = 0, token = 'tok'): ServerMessage => ({
  type: 'welcome',
  payload: { playerId: 'p1', role: 'player', sessionToken: token, lastSeq },
})

const state = (epoch: string, version: number): ServerMessage => ({
  type: 'state',
  payload: {
    epoch, version, code: 'ABC234', moderatorConnected: true, players: [],
    game: {
      phase: 'idle', tossupId: 't1', tossupNumber: 1, buzzes: [], lockedOut: [], teams: [], protests: [], history: [],
      rules: { individual: false, tossupPoints: 10, powerPoints: 15, negPoints: -5, bonusParts: 3, bonusPartPoints: 10, bounceback: false },
    },
  },
})

function connected(extra: Partial<Options> = {}) {
  const made = make(extra)
  made.conn.start()
  latest().open()
  latest().receive(welcome())
  return made
}

beforeEach(() => {
  FakeWS.all = []
  vi.useFakeTimers()
})
afterEach(() => {
  vi.useRealTimers()
})

describe('join', () => {
  it('sends a player join with the stored session token', () => {
    const { conn, storage } = make()
    storage.setItem('qbuzzer:ABC234:session', 'saved')
    conn.start()
    latest().open()
    expect(latest().last()).toEqual({ type: 'join', payload: { protocolVersion: 1, role: 'player', name: 'Alice', token: 'saved' } })
  })

  it('sends the moderator password and never a session token', () => {
    const { conn, storage } = make({ role: 'moderator', name: undefined, password: 'hunter22' })
    storage.setItem('qbuzzer:ABC234:session', 'saved')
    conn.start()
    latest().open()
    expect(latest().last()).toEqual({ type: 'join', payload: { protocolVersion: 1, role: 'moderator', password: 'hunter22' } })
  })

  it('stores the session token from welcome', () => {
    const { conn, storage } = connected()
    expect(storage.data.get('qbuzzer:ABC234:session')).toBe('tok')
    expect(conn.getSnapshot().status).toBe('open')
  })

  it('does not open two sockets when started twice', () => {
    const { conn } = make()
    conn.start()
    conn.start()
    expect(FakeWS.all).toHaveLength(1)
  })
})

describe('state', () => {
  it('accepts newer versions and any new epoch, ignores stale ones', () => {
    const { conn } = connected()
    const ws = latest()
    ws.receive(state('e1', 5))
    ws.receive(state('e1', 4))
    expect(conn.getSnapshot().room?.version).toBe(5)
    ws.receive(state('e1', 5))
    expect(conn.getSnapshot().room?.version).toBe(5)
    ws.receive(state('e2', 1)) // the room was recreated
    expect(conn.getSnapshot().room?.epoch).toBe('e2')
  })
})

describe('ping', () => {
  it('answers with receive and send times and shows the RTT', () => {
    const { conn } = connected()
    latest().receive({ type: 'ping', payload: { id: 7, rttMs: 21.5 } })
    const pong = latest().last()
    expect(pong.type).toBe('pong')
    expect(pong.seq).toBeUndefined()
    expect(pong.payload.pingId).toBe(7)
    expect(pong.payload.clientSendMs as number).toBeGreaterThanOrEqual(pong.payload.clientRecvMs as number)
    expect(conn.getSnapshot().rttMs).toBe(21.5)
  })
})

describe('seq and resend', () => {
  it('numbers messages and resends the unacknowledged ones after reconnecting', () => {
    const { conn } = connected()
    conn.send('set_name', { name: 'A' })
    conn.send('set_name', { name: 'B' })
    conn.send('set_name', { name: 'C' })
    expect(latest().sent.slice(1).map((m) => (m as { seq: number }).seq)).toEqual([1, 2, 3])

    latest().serverClose(1006)
    vi.runOnlyPendingTimers()
    latest().open()
    latest().receive(welcome(2)) // server got 1 and 2
    const resent = latest().sent.slice(1) as { seq: number; payload: { name: string } }[]
    expect(resent.map((m) => [m.seq, m.payload.name])).toEqual([[3, 'C']])

    conn.send('set_name', { name: 'D' })
    expect(latest().last().seq).toBe(4)
  })

  it('continues numbering above the server’s lastSeq (e.g. after a moderator handoff)', () => {
    const { conn } = make({ role: 'moderator', password: 'hunter22' })
    conn.start()
    latest().open()
    latest().receive(welcome(41))
    conn.send('next', { tossupId: 't1' })
    expect(latest().last().seq).toBe(42)
  })

  it('drops buzzes while disconnected but queues other messages', () => {
    const { conn } = connected()
    latest().serverClose(1006)
    expect(conn.send('buzz', { tossupId: 't1', clientTimeMs: 1 })).toBe(false)
    expect(conn.send('set_name', { name: 'Queued' })).toBe(true)
    vi.runOnlyPendingTimers()
    latest().open()
    latest().receive(welcome(0))
    expect(latest().last()).toMatchObject({ type: 'set_name', seq: 1, payload: { name: 'Queued' } })
  })
})

describe('close codes', () => {
  it.each([
    [4000, 'reload'],
    [4001, 'bad-password'],
    [4003, 'replaced'],
    [4006, 'rate-limited'],
    [1013, 'room-full'],
  ])('%i stops with %s and does not reconnect', (code, problem) => {
    const { conn } = connected()
    latest().serverClose(code)
    vi.runAllTimers()
    expect(conn.getSnapshot()).toMatchObject({ status: 'closed', problem })
    expect(FakeWS.all).toHaveLength(1)
  })

  it('unknown session: forgets the token and rejoins fresh', () => {
    const { storage } = connected()
    latest().serverClose(4002)
    expect(storage.data.has('qbuzzer:ABC234:session')).toBe(false)
    vi.runOnlyPendingTimers()
    latest().open()
    expect(latest().last().payload.token).toBeUndefined()
  })

  it('reconnects with growing backoff after a dropped connection', () => {
    const { conn } = connected()
    latest().serverClose(1006)
    expect(conn.getSnapshot().status).toBe('reconnecting')
    vi.advanceTimersByTime(249)
    expect(FakeWS.all).toHaveLength(1)
    vi.advanceTimersByTime(1)
    expect(FakeWS.all).toHaveLength(2)
    latest().serverClose(1006)
    vi.advanceTimersByTime(499)
    expect(FakeWS.all).toHaveLength(2)
    vi.advanceTimersByTime(1)
    expect(FakeWS.all).toHaveLength(3)
  })

  it('reports a room that no longer exists', async () => {
    const { conn } = connected({ roomExists: () => Promise.resolve(false) })
    for (let i = 0; i < 3; i++) {
      latest().serverClose(1006)
      await vi.runOnlyPendingTimersAsync()
    }
    expect(conn.getSnapshot()).toMatchObject({ status: 'closed', problem: 'room-gone' })
  })

  it('stop closes the socket and never reconnects', () => {
    const { conn } = connected()
    conn.stop()
    vi.runAllTimers()
    expect(FakeWS.all).toHaveLength(1)
  })
})

describe('effects', () => {
  it('delivers the effects of each applied state, and none from stale ones', () => {
    const { conn } = connected()
    const seen: string[] = []
    conn.onEffect((e) => seen.push(e.type))
    const withEffect = (version: number): ServerMessage => {
      const s = state('e1', version)
      if (s.type === 'state') s.payload.effects = [{ type: 'buzz_locked', payload: { tossupId: 't1', buzzId: 'b1', playerId: 'p1' } }]
      return s
    }
    latest().receive(withEffect(3))
    latest().receive(withEffect(2)) // stale: neither the state nor its effects apply
    expect(seen).toEqual(['buzz_locked'])
  })
})
