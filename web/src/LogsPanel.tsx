import { useCallback, useEffect, useRef, useState } from 'react'
import { ArrowDownToLine, RefreshCw } from 'lucide-react'
import { api } from './api'
import { ErrorNote, errText, IconButton, Section, SectionHeader, Select } from './ui'

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
    // The pane takes whatever height the panel has left: logs are read by
    // the screenful, so the box grows with the window instead of staying a
    // fixed strip.
    // Exactly as tall as the panel's visible area, below whatever comes
    // first (a diagnosis): scrolling the panel moves the diagnosis away and
    // leaves the logs filling the whole height. The pane hands its scroll on
    // at either end, so scrolling up past the first line brings it back.
    // After a diagnosis it is taller by its divider and top padding, so once
    // scrolled those sit just out of view and the tab looks like it does
    // for a healthy app.
    <Section className="flex h-full min-h-72 shrink-0 flex-col gap-3 [&:not(:first-child)]:h-[calc(100%+1.25rem+1px)]">
      <SectionHeader
        title="Logs"
        actions={
          <>
            <Select
              variant="quiet"
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
              variant="quiet"
              ariaLabel="How many lines"
              value={tail}
              onChange={setTail}
              options={[100, 200, 500].map((n) => ({ value: n, label: `last ${n}` }))}
            />
            <span className="mx-1 h-4 w-px bg-line" aria-hidden />
            <IconButton
              icon={ArrowDownToLine}
              label={follow ? 'Stop following new lines' : 'Follow new lines'}
              active={follow}
              onClick={() => setFollow(!follow)}
            />
            <IconButton icon={RefreshCw} label="Refresh logs" onClick={load} className={busy && !follow ? '[&>svg]:animate-spin' : ''} />
          </>
        }
      />
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
          className="min-h-0 flex-1 overflow-auto overscroll-y-auto rounded-lg border border-line bg-well p-3 font-mono text-xs leading-relaxed text-emphasis whitespace-pre-wrap focus-visible:outline-2 focus-visible:outline-accent-strong"
        >
          {logs ||
            (busy
              ? 'Waiting for output…'
              : 'Nothing here yet — the app has not written any output.')}
        </pre>
      )}
    </Section>
  )
}
