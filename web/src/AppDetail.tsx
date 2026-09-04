import { useState } from 'react'
import {
  Activity,
  BadgeCheck,
  ExternalLink,
  FileText,
  Globe2,
  HardDrive,
  KeyRound,
  Rocket,
  RotateCw,
  ScrollText,
  SlidersHorizontal,
  UploadCloud,
  X,
} from 'lucide-react'
import { api, type EndpointState, type EndpointStatus } from './api'
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
  const shown: AppTab = tab === 'files' && !status?.hasStorage ? 'overview' : tab

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
                {status.startedAt && <span className="text-xs text-muted">up {ago(status.startedAt)}</span>}
                {(status.restarts ?? 0) > 0 && (
                  <span className="text-xs text-danger">{status.restarts} restarts</span>
                )}
                {status.image && (
                  <span className="hidden font-mono text-xs text-muted sm:inline">
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
              className="rounded-md p-1.5 text-muted hover:bg-raised hover:text-ink focus-visible:outline-2 focus-visible:outline-accent-strong"
            >
              <X className="size-4" aria-hidden />
            </button>
          </div>
        </div>

        {status?.found && (
          <div className="flex flex-wrap items-center gap-2 px-5 pt-3">
            {/* An anchor (opens a new tab), dressed exactly like the quiet
                Restart/Deploy buttons beside it so the action row is uniform. */}
            {status.url && (
              <a
                href={status.url}
                target="_blank"
                rel="noreferrer"
                className="flex items-center gap-1.5 rounded-md border border-line bg-transparent px-3 py-1.5 text-sm text-ink-soft transition-colors hover:bg-line/50 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-accent-strong"
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
            {TABS.filter((t) => t.id !== 'files' || status.hasStorage).map((t) => (
              <button
                key={t.id}
                role="tab"
                aria-selected={shown === t.id}
                onClick={() => onTab(t.id)}
                className={`-mb-px flex shrink-0 items-center gap-1.5 border-b-2 px-3 py-2 text-sm transition-colors focus-visible:outline-2 focus-visible:outline-accent-strong ${
                  shown === t.id
                    ? 'border-accent font-medium text-ink'
                    : 'border-transparent text-muted hover:border-line hover:text-ink'
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
            <p className="mt-2 text-sm text-muted">
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
                {status.endpoint && <EndpointCard endpoint={status.endpoint} />}
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

const endpointStyle: Record<EndpointState, { chip: string; icon: string }> = {
  ready: { chip: 'border-success/30 bg-success/10 text-success', icon: 'text-success' },
  issuing: { chip: 'border-accent/30 bg-accent-soft text-accent', icon: 'text-accent' },
  propagating: { chip: 'border-accent/30 bg-accent-soft text-accent', icon: 'text-accent' },
  error: { chip: 'border-danger/30 bg-danger/10 text-danger', icon: 'text-danger' },
  unknown: { chip: 'border-line bg-line/50 text-ink-soft', icon: 'text-muted' },
  unmanaged: { chip: 'border-line bg-line/50 text-muted', icon: 'text-muted' },
}

function EndpointStateChip({
  state,
  renewing = false,
}: {
  state: EndpointState
  renewing?: boolean
}) {
  return (
    <span className={`rounded border px-1.5 py-0.5 font-mono text-[11px] leading-4 ${endpointStyle[state].chip}`}>
      {renewing && state === 'ready' ? 'ready · renewing' : state}
    </span>
  )
}

function readableDate(value: string): string {
  return new Intl.DateTimeFormat(undefined, { dateStyle: 'medium' }).format(new Date(value))
}

function EndpointCard({ endpoint }: { endpoint: EndpointStatus }) {
  const certificate = endpoint.certificate
  const dns = endpoint.dns

  return (
    <section>
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <SectionTitle>Address readiness</SectionTitle>
        <span className="break-all font-mono text-xs text-ink-soft">{endpoint.host}</span>
      </div>
      <div className="mt-2 divide-y divide-line/70 rounded-lg border border-line" aria-live="polite">
        <div className="grid grid-cols-[1.25rem_minmax(0,1fr)] gap-2.5 px-3 py-3">
          <BadgeCheck
            className={`mt-0.5 size-4 ${endpointStyle[certificate.state].icon}`}
            aria-hidden
          />
          <div className="min-w-0">
            <div className="flex flex-wrap items-center gap-2">
              <span className="text-sm font-medium">TLS certificate</span>
              <EndpointStateChip state={certificate.state} renewing={certificate.renewing} />
              <span className="font-mono text-[11px] text-muted">
                {certificate.mode}
                {certificate.issuer ? ` · ${certificate.issuer}` : ''}
                {certificate.reason && certificate.reason.toLowerCase() !== certificate.state
                  ? ` · ${certificate.reason}`
                  : ''}
              </span>
            </div>
            {certificate.message && (
              <p className="mt-1 text-xs leading-relaxed text-ink-soft">{certificate.message}</p>
            )}
            {(certificate.expiresAt || certificate.renewalTime) && (
              <p className="mt-1 text-xs text-muted">
                {certificate.expiresAt && `expires ${readableDate(certificate.expiresAt)}`}
                {certificate.expiresAt && certificate.renewalTime && ' · '}
                {certificate.renewalTime && `renewal ${readableDate(certificate.renewalTime)}`}
              </p>
            )}
          </div>
        </div>
        <div className="grid grid-cols-[1.25rem_minmax(0,1fr)] gap-2.5 px-3 py-3">
          <Globe2 className={`mt-0.5 size-4 ${endpointStyle[dns.state].icon}`} aria-hidden />
          <div className="min-w-0">
            <div className="flex flex-wrap items-center gap-2">
              <span className="text-sm font-medium">DNS</span>
              <EndpointStateChip state={dns.state} />
              {dns.recordType && dns.expected && dns.expected.length > 0 && (
                <span className="font-mono text-[11px] text-muted">
                  {dns.recordType} → {dns.expected.join(', ')}
                </span>
              )}
              {dns.state !== 'unmanaged' && (
                <span className="font-mono text-[11px] text-muted">
                  DNSEndpoint {dns.observed ? 'observed' : 'waiting'}
                </span>
              )}
            </div>
            {dns.message && (
              <p className="mt-1 text-xs leading-relaxed text-ink-soft">{dns.message}</p>
            )}
          </div>
        </div>
      </div>
      <p className="mt-1.5 text-xs text-muted">
        Refreshed with app status. Managed DNS answers are checked from the control plane, including
        private zones.
      </p>
    </section>
  )
}

function InstancesCard({ pods }: { pods: string[] }) {
  return (
    <section>
      <SectionTitle>Instances</SectionTitle>
      {pods.length === 0 ? (
        <p className="mt-2 text-sm text-muted">
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
      {error && <span className="text-xs text-danger">{error}</span>}
      <Button kind="quiet" onClick={restart} disabled={busy}>
        <span className="flex items-center gap-1.5">
          <RotateCw className={`size-3.5 ${busy ? 'animate-spin' : ''}`} aria-hidden />
          {busy ? 'Restarting…' : 'Restart'}
        </span>
      </Button>
    </div>
  )
}
