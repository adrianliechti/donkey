import { useEffect, useState } from 'react'
import { ExternalLink, RefreshCw } from 'lucide-react'
import { api, type AppSummary } from './api'
import { Card, ErrorNote, Spinner, StatusDot } from './ui'

export function AppsList({ onOpen }: { onOpen: (app: string) => void }) {
  const [apps, setApps] = useState<AppSummary[] | null>(null)
  const [error, setError] = useState('')

  async function load() {
    try {
      const res = await api.apps()
      setApps(res.apps)
      setError('')
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Loading the apps did not work.')
    }
  }

  useEffect(() => {
    load()
    const t = setInterval(load, 10_000)
    return () => clearInterval(t)
  }, [])

  if (apps === null && !error) return <Spinner label="Asking the cluster…" />

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <h1 className="text-lg font-semibold">Apps</h1>
        <button
          onClick={load}
          className="flex items-center gap-1.5 rounded-md px-2 py-1 text-xs text-stone hover:bg-line/50 focus-visible:outline-2 focus-visible:outline-hay-deep"
        >
          <RefreshCw className="size-3.5" aria-hidden /> Refresh
        </button>
      </div>
      <ErrorNote message={error} />
      {apps !== null && apps.length === 0 && (
        <Card className="p-10 text-center">
          <p className="text-sm text-stone">No apps yet.</p>
          <p className="mt-2 text-sm">
            Deploy your first one from a project folder:{' '}
            <code className="rounded bg-surface px-2 py-0.5 font-mono text-sm">donkey deploy</code>
          </p>
        </Card>
      )}
      <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
        {apps?.map((app) => (
          <button
            key={app.name}
            onClick={() => onOpen(app.name)}
            className={`group rounded-lg border border-line bg-card p-4 text-left shadow-sm transition-shadow hover:shadow-md focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-hay-deep border-l-4 ${
              app.healthy ? 'border-l-moss' : 'border-l-brick'
            }`}
          >
            <div className="font-mono text-lg font-bold tracking-tight group-hover:underline">
              {app.name}
            </div>
            <div className="mt-1.5 flex items-center gap-2 text-sm text-ink-soft">
              <StatusDot healthy={app.healthy} />
              {app.healthy ? `running (${app.ready})` : `not fully up (${app.ready})`}
            </div>
            {app.url && (
              <div className="mt-2 flex items-center gap-1 truncate font-mono text-xs text-stone">
                <ExternalLink className="size-3 shrink-0" aria-hidden />
                {app.url.replace(/^https?:\/\//, '')}
              </div>
            )}
            {app.image && (
              <div className="mt-1 truncate font-mono text-xs text-stone/70">
                {app.image.split('/').pop()}
              </div>
            )}
          </button>
        ))}
      </div>
    </div>
  )
}
