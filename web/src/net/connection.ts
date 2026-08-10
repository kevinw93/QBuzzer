import type { ClientMessage, PayloadOf, ServerMessage, StateEffect } from '../messages'
import * as p from '../protocol'

/** Client clock: monotonic, Unix-epoch milliseconds. Matches what the server's
 * clock sync expects, and what event.timeStamp is measured against. */
export const clientNow = (): number => performance.timeOrigin + performance.now()

export type Status = 'connecting' | 'open' | 'reconnecting' | 'closed'

/** Why the connection stopped for good; null while it is (re)connecting. */
export type Problem =
  | 'reload'
  | 'bad-password'
  | 'rate-limited'
  | 'replaced'
  | 'room-full'
  | 'room-gone'
  | null

export interface Snapshot {
  status: Status
  problem: Problem
  welcome: p.Welcome | null
  room: p.RoomState | null
  lastError: p.ErrorMessage | null
  rttMs: number | null
}

type SeqMessage = Extract<ClientMessage, { seq: number }>
export type SeqType = SeqMessage['type']
export type Effect = StateEffect

export interface KV {
  getItem(key: string): string | null
  setItem(key: string, value: string): void
  removeItem(key: string): void
}

export interface Options {
  url: string
  code: string
  role: p.Role
  name?: string
  password?: string
  /** Where the player session token lives; one seat per tab with sessionStorage. */
  storage: KV
  WebSocketImpl?: new (url: string) => WebSocket
  now?: () => number
  random?: () => number
  /** Resolves false when the room no longer exists. */
  roomExists?: () => Promise<boolean>
}

const OPEN = 1
const maxPending = 100
const roomCheckAfterAttempts = 3

/**
 * RoomConnection owns one room socket outside React: join, reconnect with
 * backoff, seq numbering and resend, ping replies, and the epoch/version rule
 * for states. React reads it through subscribe/getSnapshot.
 */
export class RoomConnection {
  private snap: Snapshot = { status: 'connecting', problem: null, welcome: null, room: null, lastError: null, rttMs: null }
  private readonly listeners = new Set<() => void>()
  private readonly effectListeners = new Set<(e: Effect) => void>()
  private ws: WebSocket | null = null
  private seq = 0
  private pending: { seq: number; data: string }[] = [] // sent since the last welcome
  private attempts = 0
  private running = false
  private timer: ReturnType<typeof setTimeout> | undefined
  private readonly o: Required<Omit<Options, 'name' | 'password'>> & Pick<Options, 'name' | 'password'>

  constructor(opts: Options) {
    this.o = {
      WebSocketImpl: WebSocket,
      now: clientNow,
      random: Math.random,
      roomExists: () => Promise.resolve(true),
      ...opts,
    }
  }

  private get tokenKey(): string {
    return `qbuzzer:${this.o.code}:session`
  }

  start(): void {
    if (this.running) return
    this.running = true
    this.connect()
  }

  stop(): void {
    this.running = false
    clearTimeout(this.timer)
    this.ws?.close(1000)
    this.ws = null
  }

  subscribe = (fn: () => void): (() => void) => {
    this.listeners.add(fn)
    return () => this.listeners.delete(fn)
  }

  getSnapshot = (): Snapshot => this.snap

  onEffect(fn: (e: Effect) => void): () => void {
    this.effectListeners.add(fn)
    return () => this.effectListeners.delete(fn)
  }

  /**
   * Sends a numbered message. While disconnected, messages are queued and
   * resent after the next welcome, except buzzes: a buzz that can't be sent
   * now is stale by the time it could be, so it is dropped (returns false).
   */
  send<T extends SeqType>(type: T, payload: PayloadOf<ClientMessage, T>): boolean {
    const open = this.ws?.readyState === OPEN && this.snap.welcome !== null
    if (!open && type === 'buzz') return false
    this.seq++
    const data = JSON.stringify({ type, seq: this.seq, payload })
    this.pending.push({ seq: this.seq, data })
    if (this.pending.length > maxPending) this.pending.shift()
    if (open) this.ws?.send(data)
    return true
  }

  private update(patch: Partial<Snapshot>): void {
    this.snap = { ...this.snap, ...patch }
    for (const fn of this.listeners) fn()
  }

  private connect(): void {
    const ws = new this.o.WebSocketImpl(this.o.url)
    this.ws = ws
    ws.onopen = () => {
      const join: p.Join = { protocolVersion: p.ProtocolVersion, role: this.o.role }
      if (this.o.name) join.name = this.o.name
      if (this.o.role === p.RoleModerator) join.password = this.o.password
      const token = this.o.role === p.RolePlayer ? this.o.storage.getItem(this.tokenKey) : null
      if (token) join.token = token
      ws.send(JSON.stringify({ type: 'join', payload: join }))
    }
    ws.onmessage = (ev: MessageEvent<string>) => {
      this.handle(ws, JSON.parse(ev.data) as ServerMessage)
    }
    ws.onclose = (ev: CloseEvent) => {
      if (this.ws !== ws) return // superseded or stopped
      this.ws = null
      this.handleClose(ev.code)
    }
  }

  private handle(ws: WebSocket, msg: ServerMessage): void {
    switch (msg.type) {
      case 'welcome': {
        const w = msg.payload
        if (this.o.role === p.RolePlayer) this.o.storage.setItem(this.tokenKey, w.sessionToken)
        this.attempts = 0
        // The server processed everything up to lastSeq; resend the rest in
        // order with their original seqs so the server can still dedupe.
        this.pending = this.pending.filter((m) => m.seq > w.lastSeq)
        this.seq = Math.max(this.seq, w.lastSeq)
        for (const m of this.pending) ws.send(m.data)
        this.update({ status: 'open', problem: null, welcome: w })
        return
      }
      case 'state': {
        const s = msg.payload
        const cur = this.snap.room
        if (cur === null || s.epoch !== cur.epoch || s.version > cur.version) {
          this.update({ room: s })
          // Effects ride inside the state they describe; a stale state's are dropped with it.
          for (const e of (s.effects ?? []) as Effect[]) for (const fn of this.effectListeners) fn(e)
        }
        return
      }
      case 'ping': {
        const recv = this.o.now()
        const pong = { type: 'pong', payload: { pingId: msg.payload.id, clientRecvMs: recv, clientSendMs: this.o.now() } }
        ws.send(JSON.stringify(pong))
        if (msg.payload.rttMs) this.update({ rttMs: msg.payload.rttMs })
        return
      }
      case 'error':
        this.update({ lastError: msg.payload })
        return
    }
  }

  private handleClose(code: number): void {
    const stopWith = (problem: Problem) => {
      this.running = false
      this.update({ status: 'closed', problem })
    }
    switch (code) {
      case p.CloseCodeReloadRequired:
      case p.CloseCodeJoinRequired:
        stopWith('reload')
        return
      case p.CloseCodeUnauthorized:
        stopWith('bad-password')
        return
      case p.CloseCodeRateLimited:
        stopWith('rate-limited')
        return
      case p.CloseCodeReplaced:
        stopWith('replaced')
        return
      case 1013: // try again later: room full
        stopWith('room-full')
        return
      case p.CloseCodeUnknownSession:
        this.o.storage.removeItem(this.tokenKey)
        this.pending = []
        this.seq = 0
        break
    }
    if (!this.running) return
    this.update({ status: 'reconnecting', welcome: null })
    void this.scheduleReconnect()
  }

  private async scheduleReconnect(): Promise<void> {
    this.attempts++
    if (this.attempts >= roomCheckAfterAttempts && !(await this.o.roomExists())) {
      this.running = false
      this.update({ status: 'closed', problem: 'room-gone' })
      return
    }
    const base = Math.min(5000, 250 * 2 ** (this.attempts - 1))
    const delay = base * (0.5 + this.o.random() / 2)
    this.timer = setTimeout(() => {
      if (this.running) this.connect()
    }, delay)
  }
}
