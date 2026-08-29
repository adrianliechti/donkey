import { useCallback, useEffect, useRef, useState } from 'react'
import { RefreshCw } from 'lucide-react'
import { api } from './api'
import { ErrorNote, errText, Select } from './ui'

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
    <div className="space-y-3">
      <div className="flex flex-wrap items-center justify-end gap-2">
        <label className="flex items-center gap-1.5 text-xs text-ink-soft">
          <input
            type="checkbox"
            checked={follow}
            onChange={(e) => setFollow(e.target.checked)}
            className="size-3.5 accent-accent-strong"
          />
          Follow
        </label>
        <Select
          size="sm"
          ariaLabel="How far back"
          value={since}
          onChange={setSince}
          options={[
            { value: '', label: 'all' },
            { value: '5m', label: 'last 5 min' },
            { value: '1h', label: 'last hour' },
          ]}
        />
        <Select
          size="sm"
          ariaLabel="How many lines"
          value={tail}
          onChange={setTail}
          options={[100, 200, 500].map((n) => ({ value: n, label: `last ${n}` }))}
        />
        <button
          type="button"
          onClick={load}
          title="Refresh logs"
          aria-label="Refresh logs"
          className="flex items-center rounded-md p-1.5 text-xs text-muted hover:bg-line/50 focus-visible:outline-2 focus-visible:outline-accent-strong"
        >
          <RefreshCw className={`size-3.5 ${busy && !follow ? 'animate-spin' : ''}`} aria-hidden />
        </button>
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
          className="h-80 overflow-auto rounded-md bg-well p-3 font-mono text-xs leading-relaxed text-emphasis whitespace-pre-wrap focus-visible:outline-2 focus-visible:outline-accent-strong"
        >
          {logs ||
            (busy
              ? 'Waiting for output…'
              : 'Nothing here yet — the app has not written any output.')}
        </pre>
      )}
    </div>
  )
}
