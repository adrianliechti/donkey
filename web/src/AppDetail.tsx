import { useState } from 'react'
import {
  Activity,
  BadgeCheck,
  Box,
  ExternalLink,
  FileText,
  Globe2,
  HardDrive,
  KeyRound,
  Rocket,
  RotateCw,
  ScrollText,
  SlidersHorizontal,
  Stethoscope,
  UploadCloud,
  X,
} from 'lucide-react'
import { api, type EndpointState, type EndpointStatus } from './api'
import { AddonsCard } from './AddonsCard'
import { DangerZone } from './DangerZone'
import { DiagnosisSection } from './DiagnoseCard'
import { EnvEditor } from './EnvEditor'
import { FilesPanel } from './FilesPanel'
import { LogsPanel } from './LogsPanel'
import { NetworkCard } from './NetworkCard'
import { StoredRelease } from './StoredRelease'
import { TrafficPanel } from './TrafficPanel'
import { DrawerBody } from './dialog'
import { useAppBuild, useAppBuilds, useAppStatus, useFleet } from './queries'
import { SignalChips } from './ReleaseView'
import {
  ago,
  Card,
  ErrorNote,
  errText,
  ExposureBadge,
  healthLine,
  healthOf,
  IconButton,
  List,
  ListRow,
  Section,
  SectionHeader,
  Spinner,
  StateChip,
  StatusDot,
  Toolbar,
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
  onDeploy,
  onBack,
}: {
  space: string
  app: string
  tab: AppTab
  onTab: (tab: AppTab) => void
  onDeploy: () => void
  onBack: () => void
}) {
  const { data: status, error, reload } = useAppStatus(space, app)
  const { data: fleet } = useFleet()
  const canManage = fleet?.roles[space] === 'admin' || fleet?.roles[space] === 'developer'
  // The tab also comes from the URL, so it can name one that is not offered:
  // .../files for an app with no disk. Fall back rather than leave the panel
  // blank with nothing selected.
  const shown: AppTab = !canManage && tab !== 'overview' && tab !== 'traffic' ? 'overview' : tab === 'files' && !status?.hasStorage ? 'overview' : tab

  return (
    <>
      {/* The header never scrolls; only the tab body below it does. */}
      <div className={`shrink-0 ${status?.found ? '' : 'border-b border-line'}`}>
        {/* The same title row as every dialog: identity left, portlet right. */}
        <div className="flex items-start justify-between gap-3 px-5 pt-4">
          <div className="min-w-0">
            <div className="flex items-center gap-2.5">
              <h1 className="truncate font-mono text-xl font-bold tracking-tight">{app}</h1>
              {status?.found && <ExposureBadge exposure={status.exposure} mode={status.mode} />}
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
                {/* The one way to the diagnosis, the same on every tab: it
                    lives on Logs, next to the evidence it reads. */}
                {canManage && !status.healthy && (
                  <button
                    type="button"
                    onClick={() => onTab('logs')}
                    className="flex items-center gap-1 text-xs text-danger underline-offset-2 hover:underline focus-visible:outline-2 focus-visible:outline-accent-strong"
                  >
                    <Stethoscope className="size-3.5" aria-hidden /> Diagnose
                  </button>
                )}
              </div>
            )}
          </div>
          {/* The same icon portlet as every dialog header. -mr-2: the close
              glyph lines up with the content edge, not its hit area. */}
          <Toolbar className="-mr-2">
            {status?.found && status.url && (
              /* An anchor (opens a new tab), dressed as an icon button. */
              <a
                href={status.url}
                target="_blank"
                rel="noreferrer"
                title="Open the app in a new tab"
                aria-label="Open the app in a new tab"
                className="flex size-8 shrink-0 items-center justify-center rounded-md text-muted transition-colors hover:bg-line/60 hover:text-ink focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-accent-strong"
              >
                <ExternalLink className="size-4" aria-hidden />
              </a>
            )}
            {canManage && status?.found && <RestartButton space={space} app={app} onRestarted={reload} />}
            {canManage && status?.found && <IconButton icon={UploadCloud} label="Deploy a new version" onClick={onDeploy} />}
            <span className="mx-0.5 h-4 w-px bg-line" aria-hidden />
            <IconButton icon={X} label="Close the app panel (Esc)" onClick={onBack} />
          </Toolbar>
        </div>

        {status?.found && (
          /* Files is only offered when there is a disk to browse — an app
             without one has nothing on it a redeploy would not rebuild. */
          // The bar's bottom line is an inset shadow, painted under the tabs,
          // so the active underline sits on it without spilling out: nothing
          // overflows vertically, and the bar only scrolls sideways when the
          // tabs are wider than the panel (phones).
          <nav
            className="mt-3 flex gap-1 overflow-x-auto overflow-y-hidden overscroll-x-contain px-5 shadow-[inset_0_-1px_0_var(--color-line)] [scrollbar-width:none] [&::-webkit-scrollbar]:hidden"
            role="tablist"
          >
            {TABS.filter((t) => (canManage || t.id === 'overview' || t.id === 'traffic') && (t.id !== 'files' || status.hasStorage)).map((t) => (
              <button
                key={t.id}
                role="tab"
                aria-selected={shown === t.id}
                onClick={() => onTab(t.id)}
                className={`flex h-10 shrink-0 items-center gap-1.5 border-b-2 px-3 text-sm transition-colors focus-visible:outline-2 focus-visible:outline-accent-strong ${
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

      {/* Keyed by tab: each tab opens scrolled to its top. */}
      <DrawerBody key={shown}>
        <ErrorNote message={error} />
        {status === null && !error && <Spinner label="Asking the cluster…" />}

        {status && !status.found && (
          <Card className="p-8 text-center">
            <h2 className="font-mono text-xl font-bold tracking-tight">{app}</h2>
            <p className="mt-2 text-sm text-muted">
              Nothing with this name is deployed in the space <span className="font-mono">{space}</span>.
            </p>
            {canManage && <p className="mt-1 text-sm">
              Deploy it from its project folder: <code className="font-mono text-xs">donkey deploy</code>
            </p>}
          </Card>
        )}

        {status?.found && (
          <>
            {shown === 'overview' && (
              <>
                {canManage && <AboutApp space={space} app={app} onOpenBuild={() => onTab('release')} />}
                {canManage && <InstancesCard pods={status.pods} />}
                {!canManage && <Section><SectionHeader title="App overview" description="Open the app using the link above, or view its health and usage in Telemetry." /></Section>}
                {canManage && status.endpoint && <EndpointCard endpoint={status.endpoint} />}
                {canManage && <AddonsCard space={space} app={app} />}
              </>
            )}
            {shown === 'release' && <StoredRelease space={space} app={app} />}
            {shown === 'logs' && (
              <>
                {!status.healthy && <DiagnosisSection space={space} app={app} image={status.image} />}
                <LogsPanel space={space} app={app} />
              </>
            )}
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
      </DrawerBody>
    </>
  )
}

// About: what the app is, in the words donkey used when it read the code
// for the latest build. Nothing shows until a build with a plan exists.
function AboutApp({ space, app, onOpenBuild }: { space: string; app: string; onOpenBuild: () => void }) {
  const { data: history } = useAppBuilds(space, app)
  const latest = history?.builds?.[0]
  const { data: release } = useAppBuild(space, app, latest?.sequence ?? null)
  const plan = release?.plan
  if (!latest || !plan || (!plan.appType && !plan.notes)) return null
  const stack = [...(plan.build.runtimes ?? []), ...(plan.build.frameworks ?? [])]

  return (
    <Section className="space-y-2">
      <SectionHeader
        title="About"
        actions={
          <button
            type="button"
            onClick={onOpenBuild}
            className="rounded px-1 text-xs text-muted underline-offset-2 transition-colors hover:text-ink hover:underline focus-visible:outline-2 focus-visible:outline-accent-strong"
          >
            Build #{latest.sequence}
          </button>
        }
      />
      {plan.appType && <p className="text-sm text-ink">{plan.appType}</p>}
      {plan.notes && <p className="text-xs leading-relaxed text-muted">{plan.notes}</p>}
      {stack.length > 0 && (
        <div className="pt-1">
          <SignalChips signals={stack} />
        </div>
      )}
    </Section>
  )
}

const endpointStyle: Record<EndpointState, { chip: string; icon: string }> = {
  ready: { chip: 'bg-success/10 text-success', icon: 'text-success' },
  issuing: { chip: 'bg-accent-soft text-accent', icon: 'text-accent' },
  propagating: { chip: 'bg-accent-soft text-accent', icon: 'text-accent' },
  error: { chip: 'bg-danger/10 text-danger', icon: 'text-danger' },
  unknown: { chip: 'bg-raised text-ink-soft', icon: 'text-muted' },
  unmanaged: { chip: 'bg-raised text-muted', icon: 'text-muted' },
}

function EndpointStateChip({
  state,
  renewing = false,
}: {
  state: EndpointState
  renewing?: boolean
}) {
  return (
    <span className={`rounded px-1.5 py-0.5 font-mono text-[11px] leading-4 ${endpointStyle[state].chip}`}>
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
    <Section className="space-y-3">
      <SectionHeader
        title="Address"
        actions={<span className="break-all font-mono text-xs text-ink-soft">{endpoint.host}</span>}
        description="Refreshed with app status. Managed DNS answers are checked from the control plane, including private zones."
      />
      <List>
        <ListRow className="items-start">
          <BadgeCheck className={`mt-0.5 size-4 shrink-0 ${endpointStyle[certificate.state].icon}`} aria-hidden />
          <div className="min-w-0" aria-live="polite">
            <div className="flex flex-wrap items-center gap-2">
              <span className="text-sm">TLS certificate</span>
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
        </ListRow>
        <ListRow className="items-start">
          <Globe2 className={`mt-0.5 size-4 shrink-0 ${endpointStyle[dns.state].icon}`} aria-hidden />
          <div className="min-w-0" aria-live="polite">
            <div className="flex flex-wrap items-center gap-2">
              <span className="text-sm">DNS</span>
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
            {dns.message && <p className="mt-1 text-xs leading-relaxed text-ink-soft">{dns.message}</p>}
          </div>
        </ListRow>
      </List>
    </Section>
  )
}

function InstancesCard({ pods }: { pods: string[] }) {
  return (
    <Section className="space-y-3">
      <SectionHeader title="Instances" />
      {pods.length === 0 ? (
        <p className="text-sm text-muted">
          No instances running — the app may be starting, or its rollout is stuck. The logs usually
          say why.
        </p>
      ) : (
        <List>
          {pods.map((p) => {
            // podSummary strings look like "name: Running (CrashLoopBackOff, 3 restarts)".
            const [name, ...rest] = p.split(': ')
            const state = rest.join(': ')
            const m = state.match(/^([^(]+?)(?:\s*\((.*)\))?$/)
            return (
              <ListRow key={p}>
                <Box className="size-4 shrink-0 text-muted" aria-hidden />
                <span className="min-w-0 flex-1 truncate font-mono text-sm">{name}</span>
                <span className="flex flex-wrap justify-end gap-1.5">
                  {m && <StateChip state={m[1].trim()} />}
                  {m?.[2] && m[2].split(', ').map((note) => <StateChip key={note} state={note} />)}
                </span>
              </ListRow>
            )
          })}
        </List>
      )}
    </Section>
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
    <>
      {error && <span className="text-xs text-danger">{error}</span>}
      <IconButton
        icon={RotateCw}
        label={busy ? 'Restarting…' : 'Restart the app'}
        onClick={restart}
        disabled={busy}
        className={busy ? '[&>svg]:animate-spin' : ''}
      />
    </>
  )
}
