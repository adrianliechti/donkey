import { Lock } from 'lucide-react'
import type { DoctorCheck, SystemComponent } from './api'
import { ago } from './format'
import { NodesPanel } from './NodesPanel'
import { useDoctor, useFleet, useSystem } from './queries'
import { PanelBar } from './ReportParts'
import { Sheet } from './Sheet'
import { SYSTEM_TABS, type SystemTab } from './systemTabs'
import { TechnologiesPanel } from './TechnologiesPanel'
import {
  CheckIcon,
  EmptyState,
  ErrorNote,
  List,
  ListRow,
  Page,
  Section,
  SectionHeader,
  Skeleton,
  Spinner,
  StatGrid,
  StatusLine,
  TabBar,
} from './ui'
import { VulnerabilitiesPanel } from './VulnerabilitiesPanel'

export function SystemView({ tab, onTab }: { tab: SystemTab; onTab: (tab: SystemTab) => void }) {
  const { data: fleet } = useFleet()
  // The System tab's dot is the control plane's own state, which is cheap to
  // know; the Vulnerabilities tab's is what the fleet already carries.
  const { data: system } = useSystem(!!fleet?.platformAdmin)
  if (fleet && !fleet.platformAdmin) {
    return (
      <Page>
        <div className="rounded-lg border border-dashed border-line bg-card">
          <EmptyState icon={Lock} title="For platform administrators">
            This page shows the installation itself. Ask a platform administrator if you need something from it.
          </EmptyState>
        </div>
      </Page>
    )
  }
  const alerts: Partial<Record<SystemTab, string>> = {
    system: system?.components.some((component) => !component.healthy)
      ? 'a component of donkey is not running'
      : undefined,
    vulnerabilities: fleet?.apps.some((app) => app.security?.state === 'needs-action')
      ? 'findings that need action'
      : undefined,
  }
  return (
    <Sheet
      tabs={
        <TabBar
          tabs={SYSTEM_TABS.map((t) => (alerts[t.id] ? { ...t, alert: alerts[t.id] } : t))}
          value={tab}
          onChange={onTab}
        />
      }
    >
      <div className="flex w-full flex-col gap-5 px-6 py-6">
        {tab === 'system' && <SystemPanel />}
        {tab === 'nodes' && <NodesPanel />}
        {tab === 'vulnerabilities' && <VulnerabilitiesPanel />}
        {tab === 'technologies' && <TechnologiesPanel />}
      </div>
    </Sheet>
  )
}

// What each component of the control plane is for, by the role every
// runtime gives it. One this console has not heard of is listed by name
// alone.
const roles: Record<string, string> = {
  server: 'The API and this console',
  orchestrator: 'Builds, deploys and the periodic scans',
  gateway: 'The edge: routes, certificates and the login',
  ai: 'The assistant’s endpoint for apps and the console',
  proxy: 'Shared HTTP services for apps',
  telemetry: 'Traffic, analytics and resource history',
  packages: 'The package mirror builds install through',
  buildkitd: 'The build engine',
  registry: 'The image registry',
}

function ComponentRow({ component }: { component: SystemComponent }) {
  const facts = [
    component.startedAt ? `up ${ago(component.startedAt)}` : '',
    component.restarts ? `${component.restarts} ${component.restarts === 1 ? 'restart' : 'restarts'}` : '',
  ].filter(Boolean)
  return (
    <ListRow className="items-start">
      <span className="mt-0.5">
        <CheckIcon ok={component.healthy} />
      </span>
      <div className="min-w-0 flex-1">
        <div className="flex min-w-0 flex-wrap items-baseline gap-x-2">
          <span className="font-mono text-sm font-medium text-ink">{component.name}</span>
          {component.version && (
            <span className="font-mono text-xs text-muted" title={component.image}>
              {component.version}
            </span>
          )}
        </div>
        <p className="text-xs text-muted">{roles[component.role] ?? component.role}</p>
        {component.message && <p className="mt-0.5 text-xs break-words text-danger">{component.message}</p>}
      </div>
      <div className="shrink-0 text-right text-xs">
        <div className={component.healthy ? 'text-ink-soft' : 'text-danger'}>
          <span className="font-mono">
            {component.ready} of {component.want}
          </span>{' '}
          running
        </div>
        {facts.length > 0 && <div className="text-muted">{facts.join(' · ')}</div>}
      </div>
    </ListRow>
  )
}

function CheckRow({ check }: { check: DoctorCheck }) {
  return (
    <ListRow className="items-start">
      <span className="mt-0.5">
        <CheckIcon ok={check.ok} />
      </span>
      <div className="min-w-0">
        <div className="text-sm font-medium text-ink">{check.name}</div>
        {check.detail && (
          <div className={`text-xs break-words ${check.ok ? 'text-muted' : 'text-danger'}`}>{check.detail}</div>
        )}
      </div>
    </ListRow>
  )
}

// System is two questions about donkey itself. Is each of its components
// running — which the runtime answers at once — and does each thing it
// depends on answer, which means asking every one of them and takes a moment.
function SystemPanel() {
  const { data: system, error: systemError, updatedAt, reload: reloadSystem } = useSystem()
  const { data: doctor, error: doctorError, reload: reloadDoctor, fetching } = useDoctor()
  const components = system?.components ?? []
  const stopped = components.filter((component) => !component.healthy)
  const checks = doctor?.checks ?? []
  const failing = checks.filter((check) => !check.ok)
  const news = [
    stopped.length > 0 ? `${stopped.length} ${stopped.length === 1 ? 'component' : 'components'} not running` : '',
    failing.length > 0 ? `${failing.length} ${failing.length === 1 ? 'check' : 'checks'} failing` : '',
  ].filter(Boolean)
  const domain = system?.domain ?? doctor?.domain

  return (
    <>
      <PanelBar
        updatedAt={updatedAt}
        reloadLabel="Check again"
        busy={fetching}
        onReload={() => {
          reloadSystem()
          reloadDoctor()
        }}
      >
        {/* Only news speaks: a well system is a dot. */}
        {news.length > 0 ? (
          <StatusLine size="md" tone="danger" label={news.join(' · ')} />
        ) : (
          system && doctor && <StatusLine size="md" tone="success" label="Everything answers" quiet />
        )}
      </PanelBar>
      <ErrorNote message={systemError} />

      <StatGrid
        stats={[
          {
            label: 'Runs on',
            value: system ? system.runtime.name || '—' : '…',
            note: system?.runtime.version,
          },
          {
            label: 'Donkey',
            value: system ? system.version || '—' : '…',
            note: domain ? `apps on *.${domain}` : undefined,
          },
          {
            label: 'Components',
            value: system ? `${components.length - stopped.length} / ${components.length}` : '…',
            note: system
              ? stopped.length > 0
                ? `${stopped.map((c) => c.name).join(', ')} not running`
                : 'running'
              : undefined,
            tone: stopped.length > 0 ? 'danger' : undefined,
          },
          {
            label: 'Checks',
            value: doctor ? `${checks.length - failing.length} / ${checks.length}` : '…',
            note: doctor ? (failing.length > 0 ? `${failing.length} failing` : 'passing') : 'asking, up to 15s',
            tone: failing.length > 0 ? 'danger' : undefined,
          },
        ]}
      />

      <Section className="space-y-3">
        <SectionHeader
          title="Components"
          description={`Donkey’s own workloads, as ${system?.runtime.name || 'the runtime'} runs them.`}
        />
        {!system && !systemError && <Skeleton className="h-40" />}
        {system?.componentsError && <ErrorNote message={system.componentsError} />}
        {system && !system.componentsError && components.length === 0 && (
          <p className="text-sm text-muted">The runtime lists none of donkey’s own workloads.</p>
        )}
        {components.length > 0 && (
          <List>
            {components.map((component) => (
              <ComponentRow key={component.name} component={component} />
            ))}
          </List>
        )}
      </Section>

      <Section className="space-y-3">
        <SectionHeader
          title="Checks"
          description="What donkey depends on to build, run and serve apps, asked just now."
        />
        <ErrorNote message={doctorError} />
        {!doctor && !doctorError && <Spinner label="Asking everything donkey depends on… (up to 15s)" />}
        {checks.length > 0 && (
          <List>
            {checks.map((check) => (
              <CheckRow key={check.name} check={check} />
            ))}
          </List>
        )}
      </Section>

      {domain && (
        <Section className="space-y-1">
          <SectionHeader title="Apps are served on" />
          <p className="font-mono text-sm">
            {'<app>'}.{'<space>'}.{domain}
          </p>
          <p className="text-xs leading-relaxed text-muted">
            Every exposed app gets an exact hostname at the shared gateway. When external-dns is configured, donkey
            publishes a matching DNSEndpoint for each one.
          </p>
        </Section>
      )}
    </>
  )
}
