import { useState } from 'react'
import {
  Activity,
  ExternalLink,
  FileText,
  HardDrive,
  KeyRound,
  Rocket,
  RotateCw,
  ScrollText,
  SlidersHorizontal,
  UploadCloud,
  X,
} from 'lucide-react'
import { api } from './api'
import { AddonsCard } from './AddonsCard'
import { DangerZone } from './DangerZone'
import { DiagnoseCard } from './DiagnoseCard'
import { EnvEditor } from './EnvEditor'
import { FilesPanel } from './FilesPanel'
import { LogsPanel } from './LogsPanel'
import { NetworkCard } from './NetworkCard'
import { StoredRelease } from './StoredRelease'
import { TrafficPanel } from './TrafficPanel'
import { useAppStatus } from './queries'
import {
  ago,
  Button,
  Card,
  ErrorNote,
  errText,
  ExposureBadge,
  healthLine,
  healthOf,
  SectionTitle,
  Spinner,
  StateChip,
  StatusDot,
} from './ui'

// The app panel slides over the fleet. Each tab is one job — glance, read,
// browse, configure, change how it's reached — and the header that answers
// "is it up?" stays above all of them.
export type AppTab = 'overview' | 'release' | 'traffic' | 'logs' | 'files' | 'variables' | 'settings'

const TABS: { id: AppTab; label: string; icon: React.ReactNode }[] = [
  { id: 'overview', label: 'Overview', icon: <FileText className="size-4" aria-hidden /> },
  { id: 'release', label: 'Deployment', icon: <Rocket className="size-4" aria-hidden /> },
  { id: 'traffic', label: 'Telemetry', icon: <Activity className="size-4" aria-hidden /> },
  { id: 'logs', label: 'Logs', icon: <ScrollText className="size-4" aria-hidden /> },
  { id: 'files', label: 'Files', icon: <HardDrive className="size-4" aria-hidden /> },
  { id: 'variables', label: 'Variables', icon: <KeyRound className="size-4" aria-hidden /> },
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
  const { data: status, error, reload } = useAppStatus(space, app)
  // The tab also comes from the URL, so it can name one that is not offered:
  // .../files for an app with no disk. Fall back rather than leave the panel
  // blank with nothing selected.
  const shown: AppTab =
    (tab === 'files' && !status?.hasStorage) || (tab === 'release' && status !== null && !status.hasRelease)
      ? 'overview'
      : tab

  return (
    <div className="flex min-h-full flex-col">
      <div className="sticky top-0 z-10 border-b border-line bg-card/95 backdrop-blur">
        <div className="flex items-start justify-between gap-3 px-5 pt-4">
          <div className="min-w-0">
            <div className="flex items-center gap-2.5">
              <h1 className="truncate font-mono text-xl font-bold tracking-tight">{app}</h1>
              {status?.found && <ExposureBadge exposure={status.exposure} />}
            </div>
            {status?.found && (
              <div className="mt-1 flex flex-wrap items-center gap-x-3 gap-y-1 text-sm text-ink-soft">
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
            )}
          </div>
          <div className="flex shrink-0 items-center gap-1.5">
            <button
              onClick={onBack}
              title="Close (Esc)"
              aria-label="Close the app panel"
              className="rounded-md p-1.5 text-stone hover:bg-raised hover:text-ink focus-visible:outline-2 focus-visible:outline-hay-deep"
            >
              <X className="size-4" aria-hidden />
            </button>
          </div>
        </div>

        {status?.found && (
          <div className="flex flex-wrap items-center gap-2 px-5 pt-3">
            {status.url && (
              <a
                href={status.url}
                target="_blank"
                rel="noreferrer"
                className="flex items-center gap-1.5 rounded-md bg-hay px-3 py-1.5 text-sm font-semibold text-on-hay transition-colors hover:bg-hay-deep focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-hay-deep"
              >
                Open app <ExternalLink className="size-3.5" aria-hidden />
              </a>
            )}
            <RestartButton space={space} app={app} onRestarted={reload} />
            <Button
              kind="quiet"
              onClick={() => {
                location.hash = `#/deploy/${encodeURIComponent(space)}/${encodeURIComponent(app)}`
              }}
            >
              <span className="flex items-center gap-1.5">
                <UploadCloud className="size-3.5" aria-hidden /> Deploy
              </span>
            </Button>
          </div>
        )}

        {status?.found && (
          /* Files is only offered when there is a disk to browse — an app
             without one has nothing on it a redeploy would not rebuild. */
          <nav className="mt-3 flex gap-1 overflow-x-auto px-5" role="tablist">
            {TABS.filter(
              (t) => (t.id !== 'files' || status.hasStorage) && (t.id !== 'release' || status.hasRelease),
            ).map((t) => (
              <button
                key={t.id}
                role="tab"
                aria-selected={shown === t.id}
                onClick={() => onTab(t.id)}
                className={`-mb-px flex shrink-0 items-center gap-1.5 border-b-2 px-3 py-2 text-sm transition-colors focus-visible:outline-2 focus-visible:outline-hay-deep ${
                  shown === t.id
                    ? 'border-hay font-medium text-ink'
                    : 'border-transparent text-stone hover:border-line hover:text-ink'
                }`}
              >
                {t.icon}
                {t.label}
              </button>
            ))}
          </nav>
        )}
      </div>

      <div className="flex-1 space-y-5 px-5 py-5">
        <ErrorNote message={error} />
        {status === null && !error && <Spinner label="Asking the cluster…" />}

        {status && !status.found && (
          <Card className="p-8 text-center">
            <h2 className="font-mono text-xl font-bold tracking-tight">{app}</h2>
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
            {/* A broken app is the reason people open this panel — the offer
                to explain it belongs above every tab. */}
            {!status.healthy && <DiagnoseCard space={space} app={app} />}

            {shown === 'overview' && (
              <>
                <InstancesCard pods={status.pods} />
                <AddonsCard space={space} app={app} />
              </>
            )}
            {shown === 'release' && <StoredRelease space={space} app={app} />}
            {shown === 'logs' && <LogsPanel space={space} app={app} />}
            {shown === 'traffic' && <TrafficPanel space={space} app={app} />}
            {shown === 'files' && <FilesPanel space={space} app={app} />}
            {shown === 'variables' && <EnvEditor space={space} app={app} onSaved={reload} />}
            {shown === 'settings' && (
              <>
                <NetworkCard space={space} app={app} onChanged={reload} />
                <DangerZone space={space} app={app} onDestroyed={onBack} />
              </>
            )}
          </>
        )}
      </div>
    </div>
  )
}

function InstancesCard({ pods }: { pods: string[] }) {
  return (
    <section>
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
    </section>
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
