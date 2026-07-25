import { useCallback, useEffect, useRef, useState } from 'react'
import { errText } from './ui'

// usePoll fetches now and on an interval, pausing while the tab is hidden
// and refreshing the moment it becomes visible again. Out-of-order
// responses are dropped, so a slow answer never overwrites a newer one.
export function usePoll<T>(fetcher: () => Promise<T>, intervalMs: number) {
  const [data, setData] = useState<T | null>(null)
  const [error, setError] = useState('')
  const [updatedAt, setUpdatedAt] = useState<number | null>(null)
  const seq = useRef(0)

  const load = useCallback(async () => {
    const mySeq = ++seq.current
    try {
      const res = await fetcher()
      if (mySeq !== seq.current) return
      setData(res)
      setUpdatedAt(Date.now())
      setError('')
    } catch (err) {
      if (mySeq !== seq.current) return
      setError(errText(err, 'Loading did not work.'))
    }
  }, [fetcher])

  useEffect(() => {
    // A new fetcher means a new subject (another app) — drop the old data
    // instead of showing it under the new name while loading.
    setData(null)
    setUpdatedAt(null)
    load()
    const tick = () => {
      if (!document.hidden) load()
    }
    const t = setInterval(tick, intervalMs)
    document.addEventListener('visibilitychange', tick)
    return () => {
      seq.current++ // drop whatever is still in flight
      clearInterval(t)
      document.removeEventListener('visibilitychange', tick)
    }
  }, [load, intervalMs])

  return { data, error, updatedAt, reload: load }
}
