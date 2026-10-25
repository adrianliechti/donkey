import { useState } from 'react'
import { useSearch } from '@tanstack/react-router'
import {
  BadgeCheck,
  Box,
  ChartColumn,
  ExternalLink,
  FileText,
  GitCommitHorizontal,
  Globe2,
  HardDrive,
  Loader2,
  Rocket,
  RotateCw,
  ScrollText,
  SlidersHorizontal,
  UploadCloud,
  type LucideIcon,
} from 'lucide-react'
import { api, type AppSecurity, type EndpointState, type EndpointStatus } from './api'
import { AddonsCard } from './AddonsCard'
import { AnalyticsPanel } from './AnalyticsPanel'
import { DangerZone } from './DangerZone'
import { DiagnosisSection } from './DiagnoseCard'
import { EnvEditor } from './EnvEditor'
import { FilesPanel } from './FilesPanel'
import { LogsPanel } from './LogsPanel'
import { NetworkCard } from './NetworkCard'
import { Sheet } from './Sheet'
import type { SourceSearch } from './sourceSearch'
import { SourcePanel } from './SourcePanel'
import { StoredRelease } from './StoredRelease'
import { DeployPlaceholder, DeployProgress } from './DeployProgress'
import { useAppBuild, useAppBuilds, useAppStatus, useAppSecurity, useDeploys, useFleet } from './queries'
import { ScanSection } from './ScanSection'
import { SignalChips } from './ReleaseView'
import {
  Button,
  Callout,
  Card,
  ErrorNote,
  IconButton,
  LinkButton,
  List,
  ListRow,
  Section,
  SectionHeader,
  Spinner,
  StateChip,
  TabBar,
  ScanBadge,
} from './ui'
import { ago, errText, scanCounts } from './format'

// An app is a page of its own. Each tab is one job — glance, read,
// browse, configure, change how it's reached — and the header that answers
// "is it up?" stays above all of them.
export type AppTab = 'overview' | 'release' | 'source' | 'traffic' | 'logs' | 'files' | 'variables' | 'settings'

const TABS: { id: AppTab; label: string; icon: LucideIcon }[] = [
  { id: 'overview', label: 'Overview', icon: FileText },
  { id: 'source', label: 'Source', icon: GitCommitHorizontal },
  { id: 'release', label: 'Deployment', icon: Rocket },
  // One view of how the app is used: its audience, requests and resources.
  // The id stays 'traffic' so links to #/app/…/traffic keep working.
  { id: 'traffic', label: 'Analytics', icon: ChartColumn },
  { id: 'logs', label: 'Logs', icon: ScrollText },
  { id: 'files', label: 'Files', icon: HardDrive },
  // Settings is everything about how the app runs: how it is reached, its
  // variables, and removing it. 'variables' links land here too.
  { id: 'settings', label: 'Settings', icon: SlidersHorizontal },
]

// Viewers read how an app is doing, never how it is configured.
const VIEWER_TABS: AppTab[] = ['overview', 'traffic']

export function AppDetail({
  space,
  app,
  tab,
  onTab,
  onDeploy,
  onRemoved,
}: {
  space: string
  app: string
  tab: AppTab
  onTab: (tab: AppTab) => void
  onDeploy: () => void
  onRemoved: () => void
}) {
  const { data: status, error, reload } = useAppStatus(space, app)
  // What the last action from the toolbar had to say, on the tab it was
  // said on: an error shows in the page, not in the toolbar, so the toolbar
  // never changes shape — and leaving the tab leaves it behind.
  const [actionError, setActionError] = useState<{ tab: AppTab; message: string } | null>(null)
  const search = useSearch({ strict: false }) as SourceSearch
  const { data: fleet } = useFleet()
  const deploy = useDeploys().byApp.get(`${space}/${app}`)
  const canManage = fleet?.roles[space] === 'admin' || fleet?.roles[space] === 'developer'
  // The tab also comes from the URL, so it can name one that is not offered:
  // .../files for an app with no disk. Fall back rather than leave the panel
  // blank with nothing selected.
  const asked: AppTab = tab === 'variables' ? 'settings' : tab
  const shown: AppTab =
    !canManage && !VIEWER_TABS.includes(asked)
      ? 'overview'
      : asked === 'files' && !status?.hasStorage
        ? 'overview'
        : asked
  // The log pane and the file browser are workbenches: the page stands
  // still and they scroll inside (see Sheet). Everything else is a document.
  const fill = shown === 'logs' || (shown === 'source' && !!search.files)

  return (
    // The breadcrumb above names the app and shows its health; the sheet's
    // top edge is its tabs and what can be done to it, and stays in reach
    // however far a tab scrolls.
    <Sheet
      fill={fill}
      tabs={
        status?.found && (
          // Files is only offered when there is a disk to browse — an app
          // without one has nothing on it a redeploy would not rebuild. An
          // app in trouble: the way to its diagnosis is Logs, so that tab
          // carries the dot.
          <TabBar
            tabs={TABS.filter(
              (t) => (canManage || VIEWER_TABS.includes(t.id)) && (t.id !== 'files' || status.hasStorage),
            ).map((t) => (t.id === 'logs' && !status.healthy ? { ...t, alert: 'unhealthy' } : t))}
            value={shown}
            onChange={onTab}
            actions={
              <>
                {canManage && (
                  <RestartButton
                    space={space}
                    app={app}
                    onRestarted={reload}
                    onError={(message) => setActionError(message ? { tab: shown, message } : null)}
                  />
                )}
                {status.url && (
                  <LinkButton href={status.url} icon={ExternalLink} kind="quiet">
                    Open
                  </LinkButton>
                )}
                {canManage && (
                  <Button icon={UploadCloud} onClick={onDeploy}>
                    Deploy
                  </Button>
                )}
              </>
            }
          />
        )
      }
    >
      <div className={`flex w-full flex-col gap-5 px-6 py-6 ${fill ? 'min-h-0 flex-1' : ''}`}>
        <ErrorNote message={error || (actionError?.tab === shown ? actionError.message : '')} />
        {status === null && !error && <Spinner label="Asking the cluster…" />}

        {/* Not running (yet): its first deploy, as it goes or how it failed.
            A deploy that went through for an app that is gone again (removed
            since) is no longer news: the app is simply not here. */}
        {status && !status.found && deploy && (!deploy.finishedAt || deploy.stage === 'failed') && (
          <DeployPlaceholder deploy={deploy} />
        )}
        {status && !status.found && (!deploy || (deploy.finishedAt && deploy.stage !== 'failed')) && (
          <Card className="p-8 text-center">
            <h2 className="font-mono text-xl font-bold tracking-tight">{app}</h2>
            <p className="mt-2 text-sm text-muted">
              Nothing with this name is deployed in the space <span className="font-mono">{space}</span>.
            </p>
            {canManage && (
              <p className="mt-1 text-sm">
                Deploy it from its project folder: <code className="font-mono text-xs">donkey deploy</code>
              </p>
            )}
          </Card>
        )}
        {/* A new version on its way to a running app. */}
        {status?.found && deploy && !deploy.finishedAt && (
          <Callout tone="info" icon={Loader2} role="status">
            <DeployProgress deploy={deploy} compact />
          </Callout>
        )}

        {status?.found && (
          <>
            {shown === 'overview' && (
              <>
                {canManage && <AboutApp space={space} app={app} onOpenBuild={() => onTab('release')} />}
                {canManage && status.security && <SecuritySection space={space} app={app} security={status.security} />}
                {canManage && <InstancesCard pods={status.pods} startedAt={status.startedAt} />}
                {!canManage && (
                  <Section>
                    <SectionHeader
                      title="App overview"
                      description="Open the app using the link above, or see how it is used in Analytics."
                    />
                  </Section>
                )}
                {canManage && status.endpoint && <EndpointCard endpoint={status.endpoint} />}
                {canManage && <AddonsCard space={space} app={app} />}
              </>
            )}
            {shown === 'source' && <SourcePanel space={space} app={app} />}
            {shown === 'release' && <StoredRelease space={space} app={app} />}
            {shown === 'logs' && (
              // The log pane takes what the sheet leaves; it is the one thing
              // that scrolls.
              <div className="flex min-h-0 flex-1 flex-col gap-5">
                {!status.healthy && <DiagnosisSection space={space} app={app} image={status.image} />}
                <LogsPanel space={space} app={app} />
              </div>
            )}
            {shown === 'traffic' && (
              <AnalyticsPanel space={space} app={app} canManage={canManage} onOpenSettings={() => onTab('settings')} />
            )}
            {shown === 'files' && <FilesPanel space={space} app={app} />}
            {shown === 'settings' && (
              <>
                <NetworkCard space={space} app={app} onChanged={reload} />
                <EnvEditor space={space} app={app} onSaved={reload} />
                <DangerZone space={space} app={app} onDestroyed={onRemoved} />
              </>
            )}
          </>
        )}
      </div>
    </Sheet>
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

// What the scan made of the running image, in one line, with the way to
// the latest findings below it.
const securityText: Record<AppSecurity['state'], string> = {
  clean: 'Nothing with a fix available at or above the policy’s threshold.',
  fixable: 'Fixes are available below the threshold; they do not refuse a deploy.',
  warning: 'The last scan reported a secret or an unsupported operating system — see the findings.',
  waived: 'Runs on a waiver, or on the assistant’s reading that the app is not affected — see the findings.',
  'needs-action': 'Would be refused by today’s policy.',
}

function SecuritySection({ space, app, security }: { space: string; app: string; security: AppSecurity }) {
  const [expanded, setExpanded] = useState(false)
  const dates = [
    security.scannedAt && `Scanned ${readableDate(security.scannedAt)}`,
    security.dbUpdatedAt && `database from ${readableDate(security.dbUpdatedAt)}`,
  ].filter(Boolean)
  return (
    <Section className="space-y-2">
      <SectionHeader
        title="Security"
        actions={
          <button
            type="button"
            onClick={() => setExpanded(!expanded)}
            className="rounded px-1 text-xs text-muted underline-offset-2 transition-colors hover:text-ink hover:underline focus-visible:outline-2 focus-visible:outline-accent-strong"
          >
            {expanded ? 'Hide findings' : 'Latest findings'}
          </button>
        }
      />
      <p className="flex flex-wrap items-center gap-2 text-sm text-ink">
        <ScanBadge security={security} />
        <span>
          {scanCounts(security.summary)}
          {security.summary.fixable > 0 && <span className="text-muted"> · {security.summary.fixable} with a fix</span>}
          {security.summary.secrets > 0 && (
            <span className="text-muted">
              {' '}
              · {security.summary.secrets} {security.summary.secrets === 1 ? 'secret' : 'secrets'}
            </span>
          )}
        </span>
      </p>
      <p className="text-xs leading-relaxed text-muted">
        {dates.length > 0 && dates.join(' · ') + '. '}
        {securityText[security.state]}
      </p>
      {security.refreshError && <ErrorNote message={security.refreshError} />}
      {expanded && <CurrentScan space={space} app={app} scannedAt={security.scannedAt} />}
    </Section>
  )
}

function CurrentScan({ space, app, scannedAt }: { space: string; app: string; scannedAt?: string }) {
  const { data, error } = useAppSecurity(space, app, scannedAt)
  if (error) return <ErrorNote message={error} />
  if (!data) return <Spinner label="Loading the latest scan…" />
  return data.scan ? (
    <ScanSection scan={data.scan} current />
  ) : (
    <p className="text-sm text-muted">No scan is stored for this image.</p>
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

function EndpointStateChip({ state, renewing = false }: { state: EndpointState; renewing?: boolean }) {
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
            {certificate.message && <p className="mt-1 text-xs leading-relaxed text-ink-soft">{certificate.message}</p>}
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

// The pod summaries carry no start time of their own; the status knows when
// the newest instance came up — exact for a single one, and said as the
// newest for several.
function InstancesCard({ pods, startedAt }: { pods: string[]; startedAt?: string }) {
  const one = pods.length === 1
  return (
    <Section className="space-y-3">
      <SectionHeader
        title="Instances"
        actions={!one && startedAt ? <span className="text-xs text-muted">newest up {ago(startedAt)}</span> : undefined}
      />
      {pods.length === 0 ? (
        <p className="text-sm text-muted">
          No instances running — the app may be starting, or its rollout is stuck. The logs usually say why.
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
                {one && startedAt && <span className="shrink-0 text-xs text-muted">up {ago(startedAt)}</span>}
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

function RestartButton({
  space,
  app,
  onRestarted,
  onError,
}: {
  space: string
  app: string
  onRestarted: () => void
  onError: (message: string) => void
}) {
  const [busy, setBusy] = useState(false)

  async function restart() {
    setBusy(true)
    onError('')
    try {
      await api.restart(space, app)
      onRestarted()
    } catch (err) {
      onError(errText(err, 'Restarting did not work.'))
    } finally {
      setBusy(false)
    }
  }

  return (
    <IconButton
      icon={RotateCw}
      label={busy ? 'Restarting…' : 'Restart the app'}
      onClick={restart}
      disabled={busy}
      className={busy ? '[&>svg]:animate-spin' : ''}
    />
  )
}
