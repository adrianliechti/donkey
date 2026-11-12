import { Link } from '@tanstack/react-router'
import { AppWindow, Check, Database, ExternalLink, HardDrive, Loader2, Network, UploadCloud } from 'lucide-react'
import type { ActiveDeploy, AppSummary, SpaceAddonEntry } from './api'
import { AppIcon } from './AppIcon'
import { preloadConsole } from './consoles'
import { useDeploy } from './deploy'
import { DeployState } from './DeployProgress'
import { spaceState, summarizeSpaces } from './fleet'
import { ago } from './format'
import { useDeploys, useFleet, useSpaceServices } from './queries'
import { serviceUI } from './serviceProtocol'
import { tileClass } from './controls'
import { appInsights } from './insights'
import {
  Button,
  Chip,
  EmptyState,
  ErrorNote,
  ExposureBadge,
  ScanBadge,
  InsightChips,
  Eyebrow,
  LinkButton,
  Mark,
  Page,
  PageBar,
  SectionHeader,
  SectionTitle,
  Skeleton,
  StatusLine,
} from './ui'
import { healthLine, healthOf } from './vocabulary'

const spaceOf = (app: AppSummary) => app.space || 'default'

/**
 * A space's page: its apps as tiles — those still on their first deploy as
 * dashed ones that fill in as the deploy goes — and its services. The apps'
 * title sits in the bar beside Deploy, with the space's news when there is
 * any, in the words the fleet's row uses; so the tiles start where the
 * fleet's list starts, and nothing moves on the way from one to the other.
 */
export function SpaceOverview({ space }: { space: string }) {
  const { data, error } = useFleet()
  const { deploys, byApp } = useDeploys()
  const openDeploy = useDeploy()
  const role = data?.roles[space] ?? 'viewer'
  const { data: services } = useSpaceServices(space, !!data && role !== 'viewer')

  if (error)
    return (
      <Page>
        <ErrorNote message={error} />
      </Page>
    )
  if (!data)
    return (
      <Page>
        <Skeleton className="h-8 w-40" />
        <Skeleton className="h-32" />
      </Page>
    )
  if (!(data.spaces ?? []).includes(space)) {
    return (
      <Page>
        <div className="rounded-lg border border-dashed border-line bg-card">
          <EmptyState title="This space is no longer here">
            <span className="font-mono text-ink">{space}</span> was removed, or you no longer have access to it.
          </EmptyState>
        </div>
      </Page>
    )
  }

  const summary = summarizeSpaces([space], data.apps ?? [], deploys)[0]
  const apps = summary.apps
  const firsts = summary.deploys.filter((d) => !apps.some((app) => app.name === d.app))
  const state = spaceState(summary)
  const count = apps.length + firsts.length

  return (
    <Page>
      <PageBar
        actions={
          role !== 'viewer' && (
            <Button icon={UploadCloud} onClick={() => openDeploy({ space })}>
              Deploy
            </Button>
          )
        }
      >
        <SectionTitle>
          <Titled count={count}>Apps</Titled>
        </SectionTitle>
        {state.news && <StatusLine size="md" {...state} />}
      </PageBar>

      {count === 0 ? (
        <div className="rounded-lg border border-dashed border-line bg-card">
          <EmptyState icon={AppWindow} title="No apps in this space yet">
            {role !== 'viewer' ? (
              <>
                Deploy one with the button above, or with{' '}
                <code className="rounded bg-raised px-1.5 py-0.5 font-mono text-xs text-ink-soft">space: {space}</code>{' '}
                in its donkey.yaml.
              </>
            ) : (
              'Apps deployed into this space appear here.'
            )}
          </EmptyState>
        </div>
      ) : (
        <div className="grid auto-rows-fr gap-4 sm:grid-cols-2 xl:grid-cols-3 2xl:grid-cols-4">
          {apps.map((app) => (
            <AppTile key={app.name} app={app} deploy={byApp.get(`${space}/${app.name}`)} />
          ))}
          {firsts.map((d) => (
            <DeployTile key={d.app} deploy={d} />
          ))}
        </div>
      )}

      {services && services.length > 0 && (
        // A step more room above the second group than within it.
        <section className="space-y-6 pt-4" aria-label="Services">
          <SectionHeader title={<Titled count={services.length}>Services</Titled>} />
          <div className="grid auto-rows-fr gap-4 sm:grid-cols-2 xl:grid-cols-3 2xl:grid-cols-4">
            {services.map((svc) => (
              <ServiceTile key={svc.name} space={space} service={svc} />
            ))}
          </div>
        </section>
      )}
    </Page>
  )
}

// A group's title with how many are in it.
function Titled({ children, count }: { children: string; count: number }) {
  return (
    <>
      {children}
      <span className="ml-2 font-mono text-xs font-normal text-muted">{count}</span>
    </>
  )
}

// A tile keeps its shape whatever the app is doing: the header row is a
// button tall whether or not there is an address to open, and the two
// lines under it carry the address and the health — or, while a deploy
// runs, where it stands.
function AppTile({ app, deploy }: { app: AppSummary; deploy?: ActiveDeploy }) {
  const health = healthOf(app.healthy, app.ready)
  const live = deploy && !deploy.finishedAt
  const failed = deploy?.stage === 'failed'
  return (
    <div className={`relative flex min-w-0 flex-col p-4 ${tileClass(health === 'bad' ? 'danger' : 'default')}`}>
      <div className="flex min-h-7 items-center justify-between gap-2">
        <span className="flex min-w-0 items-center gap-2">
          <AppIcon health={health} stack={app.stack} />
          <Link
            to="/app/$space/$name/{-$tab}"
            params={{ space: spaceOf(app), name: app.name, tab: undefined }}
            className="truncate font-mono text-sm font-semibold text-ink after:absolute after:inset-0 after:rounded-lg focus-visible:outline-none focus-visible:after:outline-2 focus-visible:after:outline-accent-strong"
          >
            {app.name}
          </Link>
        </span>
        <span className="flex shrink-0 items-center gap-2">
          <ScanBadge security={app.security} />
          <ExposureBadge exposure={app.exposure} mode={app.mode} />
          {app.url && (
            <LinkButton href={app.url} icon={ExternalLink} kind="quiet" size="sm" className="relative z-10">
              Open<span className="sr-only"> {app.name}</span>
            </LinkButton>
          )}
        </span>
      </div>
      {live ? (
        <DeployState deploy={deploy} />
      ) : (
        <div className="mt-2 space-y-1 text-xs text-muted">
          {app.url ? (
            <div className="truncate font-mono">{app.url.replace(/^https?:\/\//, '')}</div>
          ) : (
            // No address of its own: its siblings reach it by name.
            <div className="flex items-center gap-1 truncate font-mono">
              <Network className="size-3 shrink-0" aria-hidden /> http://{app.name} · internal only
            </div>
          )}
          {failed ? (
            <div className="truncate text-danger">Last deploy failed — the previous version keeps running</div>
          ) : (
            <div className={`truncate ${health === 'bad' ? 'text-danger' : ''}`}>
              {healthLine[health](app.ready)}
              {app.startedAt && <span className="text-muted"> · up {ago(app.startedAt)}</span>}
            </div>
          )}
        </div>
      )}
      {((app.addons ?? []).length > 0 || app.hasStorage || appInsights(app).length > 0) && (
        // The chips sit on the raised tint; under the tile's hover tint they
        // turn to the card colour, like a row's mark.
        <div className="mt-3 flex flex-wrap gap-1.5 [&>span]:transition-colors group-hover:[&>span]:bg-card">
          <InsightChips app={app} />
          {(app.addons ?? []).map((addon) => (
            <Chip key={addon}>
              <Database className="size-3" aria-hidden /> {addon}
            </Chip>
          ))}
          {app.hasStorage && (
            <Chip>
              <HardDrive className="size-3" aria-hidden /> disk
            </Chip>
          )}
        </div>
      )}
    </div>
  )
}

// An app on its first deploy: what it will be, filling in as the deploy goes.
function DeployTile({ deploy }: { deploy: ActiveDeploy }) {
  const failed = deploy.stage === 'failed'
  const done = deploy.stage === 'done'
  return (
    <Link
      to="/app/$space/$name/{-$tab}"
      params={{ space: deploy.space, name: deploy.app, tab: undefined }}
      className={`block p-4 focus-visible:outline-2 focus-visible:outline-accent-strong ${tileClass(failed ? 'failed' : done ? 'default' : 'pending')}`}
    >
      <div className="flex min-h-7 items-center justify-between gap-2">
        <span className="flex min-w-0 items-center gap-2">
          {failed ? (
            <span className="size-2.5 shrink-0 rounded-full border-2 border-danger" aria-hidden />
          ) : done ? (
            <Check className="size-3.5 shrink-0 text-success" aria-hidden />
          ) : (
            <Loader2 className="size-3.5 shrink-0 animate-spin text-accent" aria-hidden />
          )}
          <span className="truncate font-mono text-sm font-semibold text-ink">{deploy.app}</span>
        </span>
        <Eyebrow tone={failed ? 'danger' : done ? undefined : 'accent'} className="shrink-0">
          {failed ? 'failed' : done ? 'deployed' : 'first deploy'}
        </Eyebrow>
      </div>
      <DeployState deploy={deploy} />
    </Link>
  )
}

function ServiceTile({ space, service }: { space: string; service: SpaceAddonEntry }) {
  const view = serviceUI(service.protocol)
  const unready = !!service.state && service.state !== 'ready'
  return (
    <Link
      to="/service/$space/$name/{-$tab}"
      params={{ space, name: service.name, tab: undefined }}
      onMouseEnter={() => preloadConsole(view.console)}
      className={`flex items-center gap-3 p-4 text-left focus-visible:outline-2 focus-visible:outline-accent-strong ${tileClass()}`}
    >
      <Mark>
        <view.Icon className="size-4" aria-hidden />
      </Mark>
      <span className="min-w-0">
        <span className="block truncate font-mono text-sm font-semibold text-ink">{service.name}</span>
        <span className={`block truncate text-xs ${service.state === 'failed' ? 'text-danger' : 'text-muted'}`}>
          {unready
            ? `${service.state}${service.message ? `: ${service.message}` : ''}`
            : service.attachedTo.length > 0
              ? `used by ${service.attachedTo.join(', ')}`
              : `${view.noun} · not connected`}
        </span>
      </span>
    </Link>
  )
}
