import { useCallback, useState } from 'react'
import {
  ArrowLeft,
  ExternalLink,
  FileText,
  HardDrive,
  RotateCw,
  ScrollText,
  SlidersHorizontal,
  UploadCloud,
} from 'lucide-react'
import { api } from './api'
import { AddonsCard } from './AddonsCard'
import { DangerZone } from './DangerZone'
import { DiagnoseCard } from './DiagnoseCard'
import { EnvEditor } from './EnvEditor'
import { FilesPanel } from './FilesPanel'
import { LogsPanel } from './LogsPanel'
import { usePoll } from './hooks'
import {
  ago,
  Button,
  Card,
  ErrorNote,
  errText,
  healthLine,
  healthOf,
  SectionTitle,
  Spinner,
  StateChip,
  StatusDot,
  UpdatedAgo,
} from './ui'

// The app page used to be one long scroll of eight cards. Each tab is now one
// job — glance, read, change, manage — and the header that answers "is it up?"
// stays above all of them.
export type AppTab = 'overview' | 'logs' | 'files' | 'settings'

const TABS: { id: AppTab; label: string; icon: React.ReactNode }[] = [
  { id: 'overview', label: 'Overview', icon: <FileText className="size-4" aria-hidden /> },
  { id: 'logs', label: 'Logs', icon: <ScrollText className="size-4" aria-hidden /> },
  { id: 'files', label: 'Files', icon: <HardDrive className="size-4" aria-hidden /> },
  { id: 'settings', label: 'Settings', icon: <SlidersHorizontal className="size-4" aria-hidden /> },
]

export function AppDetail({
  space,
  app,
  tab,
  onTab,
  onBack,
}: {
  space: string
  app: string
  tab: AppTab
  onTab: (tab: AppTab) => void
  onBack: () => void
}) {
  const fetcher = useCallback(() => api.status(space, app), [space, app])
  const { data: status, error, updatedAt, reload } = usePoll(fetcher, 10_000)
  // The tab also comes from the URL, so it can name one that is not offered:
  // .../files for an app with no disk. Fall back rather than leave the panel
  // blank with nothing selected.
  const shown: AppTab = tab === 'files' && !status?.hasStorage ? 'overview' : tab

  return (
    <div className="space-y-5">
      <div className="flex items-center justify-between">
        <button
          onClick={onBack}
          className="flex items-center gap-1.5 text-sm text-stone hover:text-ink focus-visible:outline-2 focus-visible:outline-hay-deep"
        >
          <ArrowLeft className="size-4" aria-hidden /> All apps
        </button>
        <UpdatedAgo at={updatedAt} />
      </div>

      <ErrorNote message={error} />
      {status === null && !error && <Spinner label="Asking the cluster…" />}

      {status && !status.found && (
        <Card className="p-8 text-center">
          <h1 className="font-mono text-2xl font-bold tracking-tight">{app}</h1>
          <p className="mt-2 text-sm text-stone">
            Nothing with this name is deployed in the space <span className="font-mono">{space}</span>.
          </p>
          <p className="mt-1 text-sm">
            Deploy it from its project folder: <code className="font-mono text-xs">donkey deploy</code>
          </p>
        </Card>
      )}

      {status?.found && (
        <>
          <div className="flex flex-wrap items-start justify-between gap-3">
            <div>
              <h1 className="font-mono text-2xl font-bold tracking-tight">{app}</h1>
              <div className="mt-0.5 font-mono text-xs text-stone">space {space}</div>
              <div className="mt-1.5 flex flex-wrap items-center gap-x-3 gap-y-1 text-sm text-ink-soft">
                <span className="flex items-center gap-2">
                  <StatusDot health={healthOf(status.healthy, status.ready)} />
                  {healthLine[healthOf(status.healthy, status.ready)](status.ready)}
                </span>
                {status.startedAt && <span className="text-xs text-stone">up {ago(status.startedAt)}</span>}
                {(status.restarts ?? 0) > 0 && (
                  <span className="text-xs text-brick">{status.restarts} restarts</span>
                )}
                {status.image && (
                  <span className="hidden font-mono text-xs text-stone sm:inline">
                    {status.image.split('/').pop()}
                  </span>
                )}
              </div>
            </div>
            <div className="flex items-center gap-2">
              <Button
                kind="quiet"
                onClick={() => {
                  location.hash = `#/deploy/${encodeURIComponent(space)}/${encodeURIComponent(app)}`
                }}
              >
                <span className="flex items-center gap-1.5">
                  <UploadCloud className="size-3.5" aria-hidden /> Deploy update
                </span>
              </Button>
              <RestartButton space={space} app={app} onRestarted={reload} />
              {status.url && (
                <a
                  href={status.url}
                  target="_blank"
                  rel="noreferrer"
                  className="flex items-center gap-1.5 rounded-md bg-hay px-3 py-1.5 text-sm font-semibold text-on-hay transition-colors hover:bg-hay-deep hover:text-white focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-hay-deep"
                >
                  Open app <ExternalLink className="size-3.5" aria-hidden />
                </a>
              )}
            </div>
          </div>

          {/* A broken app is the reason people open this page — the offer to
              explain it belongs above the tabs, on every one of them. */}
          {!status.healthy && <DiagnoseCard space={space} app={app} />}

          {/* Files is only offered when there is a disk to browse. An app
              without one has nothing on it a redeploy would not rebuild, so
              the tab could only ever open onto an empty container.
              `shown` matters because the tab also comes from the URL: a link
              to .../files for an app with no disk would otherwise select a tab
              that is not in the list and render nothing at all. */}
          <nav className="flex gap-1 border-b border-line" role="tablist">
            {TABS.filter((t) => t.id !== 'files' || status.hasStorage).map((t) => (
              <button
                key={t.id}
                role="tab"
                aria-selected={shown === t.id}
                onClick={() => onTab(t.id)}
                className={`-mb-px flex items-center gap-1.5 border-b-2 px-3 py-2 text-sm transition-colors focus-visible:outline-2 focus-visible:outline-hay-deep ${
                  tab === t.id
                    ? 'border-hay-deep font-medium text-ink'
                    : 'border-transparent text-stone hover:border-line hover:text-ink'
                }`}
              >
                {t.icon}
                {t.label}
              </button>
            ))}
          </nav>

          {shown === 'overview' && (
            <>
              <InstancesCard pods={status.pods} />
              <AddonsCard space={space} app={app} />
            </>
          )}
          {shown === 'logs' && <LogsPanel space={space} app={app} />}
          {shown === 'files' && <FilesPanel space={space} app={app} />}
          {shown === 'settings' && (
            <>
              <EnvEditor space={space} app={app} onSaved={reload} />
              <DangerZone space={space} app={app} onDestroyed={onBack} />
            </>
          )}
        </>
      )}
    </div>
  )
}

function InstancesCard({ pods }: { pods: string[] }) {
  return (
    <Card className="p-4">
      <SectionTitle>Instances</SectionTitle>
      {pods.length === 0 ? (
        <p className="mt-2 text-sm text-stone">
          No instances running — the app may be starting, or its rollout is stuck. The logs usually
          say why.
        </p>
      ) : (
        <ul className="mt-2 space-y-1.5">
          {pods.map((p) => {
            // podSummary strings look like "name: Running (CrashLoopBackOff, 3 restarts)".
            const [name, ...rest] = p.split(': ')
            const state = rest.join(': ')
            const m = state.match(/^([^(]+?)(?:\s*\((.*)\))?$/)
            return (
              <li key={p} className="flex flex-wrap items-center gap-2 font-mono text-xs text-ink-soft">
                <span className="truncate">{name}</span>
                {m && <StateChip state={m[1].trim()} />}
                {m?.[2] && m[2].split(', ').map((note) => <StateChip key={note} state={note} />)}
              </li>
            )
          })}
        </ul>
      )}
    </Card>
  )
}

function RestartButton({ space, app, onRestarted }: { space: string; app: string; onRestarted: () => void }) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  async function restart() {
    setBusy(true)
    setError('')
    try {
      await api.restart(space, app)
      onRestarted()
    } catch (err) {
      setError(errText(err, 'Restarting did not work.'))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="flex items-center gap-2">
      {error && <span className="text-xs text-brick">{error}</span>}
      <Button kind="quiet" onClick={restart} disabled={busy}>
        <span className="flex items-center gap-1.5">
          <RotateCw className={`size-3.5 ${busy ? 'animate-spin' : ''}`} aria-hidden />
          {busy ? 'Restarting…' : 'Restart'}
        </span>
      </Button>
    </div>
  )
}
