import { useCallback, useEffect, useState } from 'react'
import {
  ArrowLeft,
  ExternalLink,
  Eye,
  EyeOff,
  Plus,
  RefreshCw,
  Trash2,
  TriangleAlert,
} from 'lucide-react'
import { api, type AppStatus } from './api'
import { Button, Card, ErrorNote, SectionTitle, Spinner, StatusDot } from './ui'

export function AppDetail({ space, app, onBack }: { space: string; app: string; onBack: () => void }) {
  const [status, setStatus] = useState<AppStatus | null>(null)
  const [error, setError] = useState('')

  const load = useCallback(async () => {
    try {
      setStatus(await api.status(space, app))
      setError('')
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Loading the app did not work.')
    }
  }, [space, app])

  useEffect(() => {
    load()
    const t = setInterval(load, 10_000)
    return () => clearInterval(t)
  }, [load])

  return (
    <div className="space-y-6">
      <button
        onClick={onBack}
        className="flex items-center gap-1.5 text-sm text-stone hover:text-ink focus-visible:outline-2 focus-visible:outline-hay-deep"
      >
        <ArrowLeft className="size-4" aria-hidden /> All apps
      </button>

      <ErrorNote message={error} />
      {status === null && !error && <Spinner label="Asking the cluster…" />}

      {status && (
        <>
          <div className="flex flex-wrap items-center justify-between gap-3">
            <div>
              <h1 className="font-mono text-2xl font-bold tracking-tight">{app}</h1>
              <div className="mt-0.5 font-mono text-xs text-stone">space {space}</div>
              <div className="mt-1 flex items-center gap-2 text-sm text-ink-soft">
                <StatusDot healthy={status.healthy} />
                {!status.found
                  ? 'not deployed'
                  : status.healthy
                    ? `running (${status.ready})`
                    : `not fully up (${status.ready})`}
                {status.image && (
                  <span className="ml-2 hidden font-mono text-xs text-stone sm:inline">
                    {status.image.split('/').pop()}
                  </span>
                )}
              </div>
            </div>
            {status.url && (
              <a
                href={status.url}
                target="_blank"
                rel="noreferrer"
                className="flex items-center gap-1.5 rounded-md bg-hay px-3 py-1.5 text-sm font-semibold text-ink transition-colors hover:bg-hay-deep hover:text-white focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-hay-deep"
              >
                Open app <ExternalLink className="size-3.5" aria-hidden />
              </a>
            )}
          </div>

          {status.pods.length > 0 && (
            <Card className="p-4">
              <SectionTitle>Instances</SectionTitle>
              <ul className="mt-2 space-y-1 font-mono text-xs text-ink-soft">
                {status.pods.map((p) => (
                  <li key={p}>{p}</li>
                ))}
              </ul>
            </Card>
          )}

          <EnvEditor space={space} app={app} />
          <LogsPanel space={space} app={app} />
          <DangerZone space={space} app={app} onDestroyed={onBack} />
        </>
      )}
    </div>
  )
}

function EnvEditor({ space, app }: { space: string; app: string }) {
  const [env, setEnv] = useState<Record<string, string> | null>(null)
  const [dirty, setDirty] = useState(false)
  const [reveal, setReveal] = useState(false)
  const [newKey, setNewKey] = useState('')
  const [newValue, setNewValue] = useState('')
  const [busy, setBusy] = useState(false)
  const [note, setNote] = useState('')
  const [error, setError] = useState('')

  useEffect(() => {
    api
      .env(space, app)
      .then((res) => setEnv(res.env ?? {}))
      .catch((err) => setError(err instanceof Error ? err.message : 'Loading failed.'))
  }, [space, app])

  function addVar() {
    const key = newKey.trim()
    if (!key || env === null) return
    setEnv({ ...env, [key]: newValue })
    setNewKey('')
    setNewValue('')
    setDirty(true)
    setNote('')
  }

  function removeVar(key: string) {
    if (env === null) return
    const next = { ...env }
    delete next[key]
    setEnv(next)
    setDirty(true)
    setNote('')
  }

  async function save() {
    if (env === null) return
    setBusy(true)
    setError('')
    try {
      const res = await api.setEnv(space, app, env, true)
      setDirty(false)
      setNote(res.restarted ? 'Saved — the app is restarting with the new values.' : 'Saved.')
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Saving did not work.')
    } finally {
      setBusy(false)
    }
  }

  const keys = env ? Object.keys(env).sort() : []

  return (
    <Card className="p-4">
      <div className="flex items-center justify-between">
        <SectionTitle>Environment variables</SectionTitle>
        <button
          onClick={() => setReveal(!reveal)}
          className="flex items-center gap-1 text-xs text-stone hover:text-ink focus-visible:outline-2 focus-visible:outline-hay-deep"
        >
          {reveal ? (
            <>
              <EyeOff className="size-3.5" aria-hidden /> Hide values
            </>
          ) : (
            <>
              <Eye className="size-3.5" aria-hidden /> Show values
            </>
          )}
        </button>
      </div>

      {env === null && !error && <Spinner label="Loading…" />}
      {env !== null && (
        <div className="mt-3 space-y-2">
          {keys.length === 0 && (
            <p className="text-sm text-stone">No variables yet — add the first one below.</p>
          )}
          {keys.map((key) => (
            <div key={key} className="flex items-center gap-2">
              <span className="w-1/3 truncate font-mono text-sm font-medium">{key}</span>
              <span className="flex-1 truncate font-mono text-sm text-ink-soft">
                {reveal ? env[key] : '••••••••'}
              </span>
              <button
                onClick={() => removeVar(key)}
                aria-label={`Remove ${key}`}
                className="rounded p-1 text-stone hover:bg-brick/10 hover:text-brick focus-visible:outline-2 focus-visible:outline-hay-deep"
              >
                <Trash2 className="size-4" aria-hidden />
              </button>
            </div>
          ))}
          <div className="flex items-center gap-2 border-t border-line pt-3">
            <input
              value={newKey}
              onChange={(e) => setNewKey(e.target.value)}
              placeholder="KEY"
              className="w-1/3 rounded-md border border-line bg-surface px-2 py-1.5 font-mono text-sm focus:outline-2 focus:outline-hay-deep"
            />
            <input
              value={newValue}
              onChange={(e) => setNewValue(e.target.value)}
              placeholder="value"
              className="flex-1 rounded-md border border-line bg-surface px-2 py-1.5 font-mono text-sm focus:outline-2 focus:outline-hay-deep"
            />
            <Button kind="quiet" onClick={addVar} disabled={newKey.trim() === ''}>
              <span className="flex items-center gap-1">
                <Plus className="size-3.5" aria-hidden /> Add
              </span>
            </Button>
          </div>
          <div className="flex items-center justify-between pt-2">
            <p className="text-xs text-stone">Saving restarts the app so it picks up the change.</p>
            <Button onClick={save} disabled={!dirty || busy}>
              {busy ? 'Saving…' : 'Save changes'}
            </Button>
          </div>
          {note && <p className="text-sm text-moss">{note}</p>}
          <ErrorNote message={error} />
        </div>
      )}
    </Card>
  )
}

function LogsPanel({ space, app }: { space: string; app: string }) {
  const [logs, setLogs] = useState('')
  const [tail, setTail] = useState(200)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  const load = useCallback(async () => {
    setBusy(true)
    try {
      setLogs(await api.logs(space, app, tail))
      setError('')
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Loading the logs did not work.')
    } finally {
      setBusy(false)
    }
  }, [space, app, tail])

  useEffect(() => {
    load()
  }, [load])

  return (
    <Card className="p-4">
      <div className="flex items-center justify-between">
        <SectionTitle>Logs</SectionTitle>
        <div className="flex items-center gap-2">
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
            <RefreshCw className={`size-3.5 ${busy ? 'animate-spin' : ''}`} aria-hidden /> Refresh
          </button>
        </div>
      </div>
      <ErrorNote message={error} />
      <pre className="mt-3 h-80 overflow-auto rounded-md bg-ink p-3 font-mono text-xs leading-relaxed text-line whitespace-pre-wrap">
        {logs || 'Nothing here yet — the app has not written any output.'}
      </pre>
    </Card>
  )
}

function DangerZone({ space, app, onDestroyed }: { space: string; app: string; onDestroyed: () => void }) {
  const [confirming, setConfirming] = useState(false)
  const [keepData, setKeepData] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  async function destroy() {
    setBusy(true)
    setError('')
    try {
      await api.destroy(space, app, keepData)
      onDestroyed()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Removing did not work.')
      setBusy(false)
    }
  }

  return (
    <Card className="border-brick/30 p-4">
      <SectionTitle>Remove</SectionTitle>
      {!confirming ? (
        <div className="mt-2 flex items-center justify-between gap-4">
          <p className="text-sm text-ink-soft">
            Takes <span className="font-mono">{app}</span> off the cluster. A later{' '}
            <code className="font-mono text-xs">donkey deploy</code> brings it back.
          </p>
          <Button kind="danger" onClick={() => setConfirming(true)}>
            <span className="flex items-center gap-1.5">
              <Trash2 className="size-3.5" aria-hidden /> Remove app
            </span>
          </Button>
        </div>
      ) : (
        <div className="mt-2 space-y-3">
          <p className="flex items-center gap-2 text-sm font-medium text-brick">
            <TriangleAlert className="size-4" aria-hidden />
            Remove {app} and everything donkey created for it?
          </p>
          <label className="flex items-center gap-2 text-sm text-ink-soft">
            <input
              type="checkbox"
              checked={keepData}
              onChange={(e) => setKeepData(e.target.checked)}
              className="size-4 accent-hay-deep"
            />
            Keep stored data (databases)
          </label>
          <div className="flex gap-2">
            <Button kind="danger" onClick={destroy} disabled={busy}>
              {busy ? 'Removing…' : 'Yes, remove it'}
            </Button>
            <Button kind="quiet" onClick={() => setConfirming(false)} disabled={busy}>
              Cancel
            </Button>
          </div>
          <ErrorNote message={error} />
        </div>
      )}
    </Card>
  )
}
