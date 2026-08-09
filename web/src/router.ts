import { useSyncExternalStore } from 'react'

export type Route =
  | { page: 'home' }
  | { page: 'player'; code: string }
  | { page: 'moderator'; code: string }

export function parseRoute(pathname: string): Route {
  const m = /^\/r\/([A-Za-z0-9]{6})(\/mod)?\/?$/.exec(pathname)
  if (!m?.[1]) return { page: 'home' }
  const code = m[1].toUpperCase()
  return m[2] ? { page: 'moderator', code } : { page: 'player', code }
}

const listeners = new Set<() => void>()

function subscribe(fn: () => void) {
  listeners.add(fn)
  window.addEventListener('popstate', fn)
  return () => {
    listeners.delete(fn)
    window.removeEventListener('popstate', fn)
  }
}

export function navigate(path: string): void {
  history.pushState(null, '', path)
  for (const fn of listeners) fn()
}

export function useRoute(): Route {
  const path = useSyncExternalStore(subscribe, () => location.pathname)
  return parseRoute(path)
}
