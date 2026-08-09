import { useEffect, useMemo, useSyncExternalStore } from 'react'
import { RoomConnection, type Options, type Snapshot } from './connection'

interface Entry {
  conn: RoomConnection
  refs: number
}

const live = new Map<string, Entry>()

/**
 * Returns the connection for key, creating it on first use. Connections are
 * reference-counted and stopped a tick after the last user unmounts, so
 * React StrictMode's unmount/remount in development reuses one socket.
 */
export function useRoom(key: string, opts: () => Options): [RoomConnection, Snapshot] {
  const conn = useMemo(() => {
    const existing = live.get(key)
    if (existing) return existing.conn
    const entry = { conn: new RoomConnection(opts()), refs: 0 }
    live.set(key, entry)
    return entry.conn
    // opts is read once per key by design.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key])

  useEffect(() => {
    const entry = live.get(key)
    if (!entry) return
    entry.refs++
    entry.conn.start()
    return () => {
      entry.refs--
      setTimeout(() => {
        if (entry.refs === 0 && live.get(key) === entry) {
          entry.conn.stop()
          live.delete(key)
        }
      }, 0)
    }
  }, [key])

  return [conn, useSyncExternalStore(conn.subscribe, conn.getSnapshot)]
}

export function wsURL(code: string): string {
  const scheme = location.protocol === 'https:' ? 'wss' : 'ws'
  return `${scheme}://${location.host}/api/rooms/${code}/ws`
}

export async function roomExists(code: string): Promise<boolean> {
  try {
    const resp = await fetch(`/api/rooms/${code}`)
    return resp.status !== 404
  } catch {
    return true // network trouble isn't proof the room is gone
  }
}
