import { useState, type SyntheticEvent } from 'react'
import { MaxNameLen, MaxPasswordLen, MinPasswordLen, type CreateRoomResponse } from '../protocol'
import { navigate } from '../router'
import { nameKey, passwordKey } from '../storageKeys'

export function Home() {
  const [password, setPassword] = useState('')
  const [code, setCode] = useState('')
  const [name, setName] = useState(sessionStorage.getItem(nameKey) ?? '')
  const [error, setError] = useState('')

  const create = async (e: SyntheticEvent) => {
    e.preventDefault()
    setError('')
    const resp = await fetch('/api/rooms', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ moderatorPassword: password }),
    })
    if (!resp.ok) {
      setError(await resp.text())
      return
    }
    const room = (await resp.json()) as CreateRoomResponse
    localStorage.setItem(passwordKey(room.code), password)
    navigate(`/r/${room.code}/mod`)
  }

  const join = (e: SyntheticEvent) => {
    e.preventDefault()
    sessionStorage.setItem(nameKey, name.trim())
    navigate(`/r/${code.trim().toUpperCase()}`)
  }

  return (
    <main className="home">
      <h1>QBuzzer</h1>
      <form onSubmit={join}>
        <h2>Join a game</h2>
        <input aria-label="Room code" placeholder="Room code" value={code} onChange={(e) => { setCode(e.target.value) }} maxLength={6} required pattern="[A-Za-z0-9]{6}" />
        <input aria-label="Your name" placeholder="Your name" value={name} onChange={(e) => { setName(e.target.value) }} maxLength={MaxNameLen} required />
        <button type="submit">Join</button>
      </form>
      <form onSubmit={(e) => void create(e)}>
        <h2>Moderate a game</h2>
        <input
          aria-label="Moderator password"
          type="password"
          placeholder={`Choose a moderator password (${String(MinPasswordLen)}+ characters)`}
          value={password}
          onChange={(e) => { setPassword(e.target.value) }}
          minLength={MinPasswordLen}
          maxLength={MaxPasswordLen}
          required
        />
        <p className="hint">Share it only with other moderators: anyone with it can run the room.</p>
        <button type="submit">Create room</button>
      </form>
      {error && <p className="toast">{error}</p>}
    </main>
  )
}
