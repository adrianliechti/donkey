import { useCallback, useEffect, useRef, useState } from 'react'
import { RefreshCw } from 'lucide-react'
import { api } from './api'
import { Card, ErrorNote, errText, SectionTitle } from './ui'

export function LogsPanel({ space, app }: { space: string; app: string }) {
  const [logs, setLogs] = useState('')
  const [tail, setTail] = useState(200)
  const [since, setSince] = useState('')
  const [follow, setFollow] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const paneRef = useRef<HTMLPreElement>(null)
  const pinnedRef = useRef(true)
  const abortRef = useRef<AbortController | null>(null)

  const load = useCallback(async () => {
    abortRef.current?.abort()
    const ac = new AbortController()
    abortRef.current = ac
    setBusy(true)
    setLogs('')
    setError('')
    try {
      await api.logs(
        space,
        app,
        { tail, follow, since: since || undefined },
        (chunk) => setLogs((prev) => prev + chunk),
        ac.signal,
      )
    } catch (err) {
      if (!ac.signal.aborted) {
        setError(errText(err, 'Loading the logs did not work.'))
      }
    } finally {
      if (abortRef.current === ac) setBusy(false)
    }
  }, [space, app, tail, since, follow])

  useEffect(() => {
    load()
    return () => abortRef.current?.abort()
  }, [load])

  // Stay pinned to the newest lines while following — unless the reader
  // scrolled up to look at something.
  useEffect(() => {
    const pane = paneRef.current
    if (pane && pinnedRef.current) pane.scrollTop = pane.scrollHeight
  }, [logs])

  // The server reports late failures inside the 200 body ("error: …").
  const streamError = logs.trim().startsWith('error:') ? logs.trim() : ''

  return (
    <Card className="p-4">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <SectionTitle>Logs</SectionTitle>
        <div className="flex items-center gap-2">
          <label className="flex items-center gap-1.5 text-xs text-ink-soft">
            <input
              type="checkbox"
              checked={follow}
              onChange={(e) => setFollow(e.target.checked)}
              className="size-3.5 accent-hay-deep"
            />
            Follow
          </label>
          <select
            value={since}
            onChange={(e) => setSince(e.target.value)}
            aria-label="How far back"
            className="rounded-md border border-line bg-surface px-2 py-1 text-xs focus:outline-2 focus:outline-hay-deep"
          >
            <option value="">all</option>
            <option value="5m">last 5 min</option>
            <option value="1h">last hour</option>
          </select>
          <select
            value={tail}
            onChange={(e) => setTail(Number(e.target.value))}
            aria-label="How many lines"
            className="rounded-md border border-line bg-surface px-2 py-1 text-xs focus:outline-2 focus:outline-hay-deep"
          >
            <option value={100}>last 100</option>
            <option value={200}>last 200</option>
            <option value={500}>last 500</option>
          </select>
          <button
            onClick={load}
            className="flex items-center gap-1.5 rounded-md px-2 py-1 text-xs text-stone hover:bg-line/50 focus-visible:outline-2 focus-visible:outline-hay-deep"
          >
            <RefreshCw className={`size-3.5 ${busy && !follow ? 'animate-spin' : ''}`} aria-hidden />
          </button>
        </div>
      </div>
      <ErrorNote message={error || streamError} />
      {!streamError && (
        <pre
          ref={paneRef}
          tabIndex={0}
          aria-label="Application logs"
          onScroll={() => {
            const pane = paneRef.current
            if (pane) pinnedRef.current = pane.scrollHeight - pane.scrollTop - pane.clientHeight < 40
          }}
          className="mt-3 h-80 overflow-auto rounded-md bg-ink-well p-3 font-mono text-xs leading-relaxed text-parchment whitespace-pre-wrap focus-visible:outline-2 focus-visible:outline-hay-deep"
        >
          {logs ||
            (busy
              ? 'Waiting for output…'
              : 'Nothing here yet — the app has not written any output.')}
        </pre>
      )}
    </Card>
  )
}
