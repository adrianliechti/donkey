import { useEffect, useRef, useState, type ReactNode } from 'react'
import { ArrowDownToLine, RefreshCw } from 'lucide-react'
import { api } from './api'
import { ErrorNote, IconButton, Section, SectionHeader, Select } from './ui'
import { errText } from './format'

export function LogsPanel({ space, app }: { space: string; app: string }) {
  const [tail, setTail] = useState(200)
  const [since, setSince] = useState('')
  const [follow, setFollow] = useState(false)
  // Every choice — and Refresh — is a new stream: the stream is keyed by
  // them, so it starts over with nothing in it and nothing left running.
  const [round, setRound] = useState(0)

  return (
    <LogStream
      key={`${tail}|${since}|${follow}|${round}`}
      space={space}
      app={app}
      tail={tail}
      since={since}
      follow={follow}
      controls={(busy) => (
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
          <IconButton
            icon={RefreshCw}
            label="Refresh logs"
            onClick={() => setRound(round + 1)}
            className={busy && !follow ? '[&>svg]:animate-spin' : ''}
          />
        </>
      )}
    />
  )
}

function LogStream({
  space,
  app,
  tail,
  since,
  follow,
  controls,
}: {
  space: string
  app: string
  tail: number
  since: string
  follow: boolean
  controls: (busy: boolean) => ReactNode
}) {
  const [logs, setLogs] = useState('')
  const [busy, setBusy] = useState(true)
  const [error, setError] = useState('')
  const paneRef = useRef<HTMLPreElement>(null)
  const pinnedRef = useRef(true)

  useEffect(() => {
    const ac = new AbortController()
    api
      .logs(
        space,
        app,
        { tail, follow, since: since || undefined },
        (chunk) => setLogs((prev) => prev + chunk),
        ac.signal,
      )
      .catch((err) => {
        if (!ac.signal.aborted) setError(errText(err, 'Loading the logs did not work.'))
      })
      .finally(() => {
        if (!ac.signal.aborted) setBusy(false)
      })
    return () => ac.abort()
  }, [space, app, tail, since, follow])

  // Stay pinned to the newest lines while following — unless the reader
  // scrolled up to look at something.
  useEffect(() => {
    const pane = paneRef.current
    if (pane && pinnedRef.current) pane.scrollTop = pane.scrollHeight
  }, [logs])

  // The server reports late failures inside the 200 body ("error: …").
  const streamError = logs.trim().startsWith('error:') ? logs.trim() : ''

  return (
    // The pane takes what the tab leaves under anything above it (a
    // diagnosis), and is the one thing that scrolls: logs are read by the
    // screenful, so the box grows with the window.
    <Section className="flex min-h-0 flex-1 flex-col gap-3">
      <SectionHeader title="Logs" actions={controls(busy)} />
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
          className="min-h-0 flex-1 overflow-auto rounded-lg border border-line bg-well p-3 font-mono text-xs leading-relaxed text-emphasis whitespace-pre-wrap focus-visible:outline-2 focus-visible:outline-accent-strong"
        >
          {logs || (busy ? 'Waiting for output…' : 'Nothing here yet — the app has not written any output.')}
        </pre>
      )}
    </Section>
  )
}
