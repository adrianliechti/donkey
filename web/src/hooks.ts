import { useEffect, useState, useSyncExternalStore } from 'react'

// useColorScheme follows the OS light/dark preference — the console has no
// theme toggle and stores nothing, so this reads `prefers-color-scheme` live.
// It exists for the rare place a value can't be a CSS token: the code
// highlighter's theme is a token→hex map chosen in JS.
const darkQuery = () => window.matchMedia('(prefers-color-scheme: dark)')
export function useColorScheme(): 'light' | 'dark' {
  return useSyncExternalStore(
    (cb) => {
      const mq = darkQuery()
      mq.addEventListener('change', cb)
      return () => mq.removeEventListener('change', cb)
    },
    () => (darkQuery().matches ? 'dark' : 'light'),
    () => 'dark',
  )
}

// useMediaQuery answers a CSS media query live — for the few layouts that
// have to branch in JS (a tab list that is vertical on desktop, say).
export function useMediaQuery(query: string): boolean {
  return useSyncExternalStore(
    (cb) => {
      const mq = window.matchMedia(query)
      mq.addEventListener('change', cb)
      return () => mq.removeEventListener('change', cb)
    },
    () => window.matchMedia(query).matches,
    () => false,
  )
}

// useScrolledPast says whether the page has scrolled at least `px` — for
// chrome that changes shape once it sticks. It re-renders only when the
// answer flips, not on every scroll event.
export function useScrolledPast(px: number): boolean {
  return useSyncExternalStore(
    (cb) => {
      window.addEventListener('scroll', cb, { passive: true })
      return () => window.removeEventListener('scroll', cb)
    },
    () => window.scrollY >= px,
    () => false,
  )
}

// useNow re-renders every `every` ms, for relative times ("2m ago") that
// should keep counting while nothing else on the page changes.
export function useNow(every = 15_000): number {
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    const timer = window.setInterval(() => setNow(Date.now()), every)
    return () => window.clearInterval(timer)
  }, [every])
  return now
}
