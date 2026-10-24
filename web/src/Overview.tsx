import { useState } from 'react'
import { Link } from '@tanstack/react-router'
import { Database, FolderPlus, HardDrive, Loader2, Network, Rocket, Search, UploadCloud } from 'lucide-react'
import type { ActiveDeploy, AppSummary, SpaceAddonEntry } from './api'
import { preloadConsole } from './consoles'
import { useDeploy } from './deploy'
import { ago } from './format'
import { useNow } from './hooks'
import { NewSpaceDialog } from './NewSpaceDialog'
import { useDeploys, useFleet, useSpaceServices } from './queries'
import { serviceUI } from './serviceProtocol'
import { AppIcon } from './AppIcon'
import { Button, ErrorNote, ExposureBadge, Skeleton } from './ui'
import { healthLine, healthOf } from './vocabulary'

const STAGES: { id: ActiveDeploy['stage']; label: string }[] = [
  { id: 'detecting', label: 'Inspect' },
  { id: 'building', label: 'Build' },
  { id: 'releasing', label: 'Release' },
]

const stageText: Record<ActiveDeploy['stage'], string> = {
  queued: 'Waiting for the deploy before it',
  detecting: 'Inspecting the source',
  building: 'Building the image',
  releasing: 'Releasing and checking health',
  done: 'Deployed',
  failed: 'Deploy failed',
}

const spaceOf = (app: AppSummary) => app.space || 'default'

/**
 * Donkey's front page: every space you can see, how its apps are doing, and
 * what is being deployed right now — whoever started it, from wherever.
 * Built for a handful of spaces and for two hundred: a filter, and cards
 * small enough to scan.
 */
export function FleetOverview() {
  const { data, error } = useFleet()
  const { deploys } = useDeploys()
  const openDeploy = useDeploy()
  const [filter, setFilter] = useState('')
  const [creating, setCreating] = useState(false)

  if (error)
    return (
      <Page>
        <ErrorNote message={error} />
      </Page>
    )
  if (!data) {
    return (
      <Page>
        <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-4">
          <Skeleton className="h-28" />
          <Skeleton className="h-28" />
          <Skeleton className="h-28" />
        </div>
      </Page>
    )
  }

  const apps = data.apps ?? []
  const spaces = data.spaces ?? []
  const needle = filter.trim().toLowerCase()
  const down = apps.filter((app) => healthOf(app.healthy, app.ready) === 'bad')
  const running = deploys.filter((d) => !d.finishedAt)
  const failedFirsts = deploys.filter(
    (d) => d.stage === 'failed' && !apps.some((app) => spaceOf(app) === d.space && app.name === d.app),
  )
  const canDeploy = data.platformAdmin || Object.values(data.roles ?? {}).some((role) => role !== 'viewer')
  const shown = spaces.filter(
    (space) =>
      !needle || space.includes(needle) || apps.some((app) => spaceOf(app) === space && app.name.includes(needle)),
  )

  return (
    <Page>
      <div className="flex flex-wrap items-center justify-between gap-3">
        <Summary
          items={[
            [`${spaces.length} ${spaces.length === 1 ? 'space' : 'spaces'}`],
            [`${apps.length - down.length} running`],
            down.length > 0 ? [`${down.length} down`, 'text-danger'] : null,
            running.length > 0 ? [`${running.length} deploying`, 'text-accent'] : null,
          ]}
        />
        <div className="flex items-center gap-2">
          {spaces.length > 8 && (
            <label className="flex h-8 w-64 items-center gap-2 rounded-md border border-line bg-card px-2.5 text-sm focus-within:border-line-strong">
              <Search className="size-3.5 shrink-0 text-muted" aria-hidden />
              <input
                value={filter}
                onChange={(e) => setFilter(e.target.value)}
                placeholder="Filter spaces and apps…"
                aria-label="Filter spaces and apps"
                className="min-w-0 flex-1 bg-transparent outline-none placeholder:text-muted"
              />
            </label>
          )}
          {data.platformAdmin && (
            <Button kind="quiet" icon={FolderPlus} size="sm" onClick={() => setCreating(true)}>
              New space
            </Button>
          )}
          {canDeploy && (
            <Button icon={UploadCloud} size="sm" onClick={() => openDeploy()}>
              Deploy
            </Button>
          )}
        </div>
      </div>

      {(down.length > 0 || running.length > 0 || failedFirsts.length > 0) && (
        <section className="grid gap-4 lg:grid-cols-2">
          {down.length > 0 && (
            <Board title="Needs attention" tone="danger">
              {down.map((app) => (
                <BoardRow key={`${spaceOf(app)}/${app.name}`} space={spaceOf(app)} app={app.name}>
                  <AppIcon health="bad" stack={app.stack} />
                  <span className="truncate font-mono text-ink">{app.name}</span>
                  <span className="truncate font-mono text-xs text-muted">{spaceOf(app)}</span>
                  <span className="ml-auto shrink-0 text-xs text-danger">{healthLine.bad(app.ready)}</span>
                </BoardRow>
              ))}
            </Board>
          )}
          {(running.length > 0 || failedFirsts.length > 0) && (
            <Board title="Deploys">
              {[...running, ...failedFirsts].map((d) => (
                <BoardRow key={`${d.space}/${d.app}`} space={d.space} app={d.app}>
                  {d.finishedAt ? (
                    <span className="size-2.5 shrink-0 rounded-full border-2 border-danger" aria-hidden />
                  ) : (
                    <Loader2 className="size-3.5 shrink-0 animate-spin text-accent" aria-hidden />
                  )}
                  <span className="truncate font-mono text-ink">{d.app}</span>
                  <span className="truncate font-mono text-xs text-muted">{d.space}</span>
                  <span className={`ml-auto shrink-0 text-xs ${d.finishedAt ? 'text-danger' : 'text-accent'}`}>
                    {stageText[d.stage]}
                  </span>
                </BoardRow>
              ))}
            </Board>
          )}
        </section>
      )}

      {spaces.length === 0 ? (
        <Empty>
          {data.platformAdmin
            ? 'No spaces yet. Make one with New space, or deploy an app from its folder with donkey deploy.'
            : 'You have no spaces yet. Ask a platform administrator to create one and give you access.'}
        </Empty>
      ) : shown.length === 0 ? (
        <Empty>Nothing matches “{filter}”.</Empty>
      ) : (
        <section className="grid auto-rows-fr gap-4 sm:grid-cols-2 xl:grid-cols-3 2xl:grid-cols-4">
          {shown.map((space) => (
            <SpaceTile
              key={space}
              space={space}
              apps={apps.filter((app) => spaceOf(app) === space)}
              deploys={deploys.filter((d) => d.space === space)}
            />
          ))}
        </section>
      )}

      <NewSpaceDialog
        key={String(creating)}
        domain={data.domain ?? ''}
        open={creating}
        onClose={() => setCreating(false)}
        onCreated={() => setCreating(false)}
      />
    </Page>
  )
}

function SpaceTile({ space, apps, deploys }: { space: string; apps: AppSummary[]; deploys: ActiveDeploy[] }) {
  const down = apps.filter((app) => healthOf(app.healthy, app.ready) === 'bad').length
  const deploying = deploys.filter((d) => !d.finishedAt).length
  const sorted = [...apps].sort((a, b) => a.name.localeCompare(b.name))
  return (
    <Link
      to="/space/$space"
      params={{ space }}
      className={`group block rounded-lg border bg-card p-4 transition-[border-color,box-shadow,translate] duration-(--duration-fast) ease-emphasized hover:shadow-sm motion-safe:hover:-translate-y-px focus-visible:outline-2 focus-visible:outline-accent-strong ${
        down > 0 ? 'border-danger/50 hover:border-danger' : 'border-line hover:border-line-strong'
      }`}
    >
      <div className="flex items-center justify-between gap-2">
        <span className="truncate font-mono text-sm font-bold text-ink">{space}</span>
        {deploying > 0 && (
          <span className="flex shrink-0 items-center gap-1 text-xs text-accent">
            <Loader2 className="size-3.5 animate-spin" aria-hidden /> deploying
          </span>
        )}
      </div>
      <p className="mt-1 text-xs text-muted">
        {apps.length === 0 ? 'No apps yet' : `${apps.length} ${apps.length === 1 ? 'app' : 'apps'}`}
        {down > 0 && <span className="text-danger"> · {down} down</span>}
      </p>
      {sorted.length > 0 && (
        // One mark per app: a space's health readable without opening it.
        <div className="mt-3 flex flex-wrap gap-1.5" aria-label={`${apps.length - down} running, ${down} down`}>
          {sorted.slice(0, 24).map((app) => (
            <span key={app.name} title={`${app.name} — ${healthLine[healthOf(app.healthy, app.ready)](app.ready)}`}>
              <AppIcon health={healthOf(app.healthy, app.ready)} stack={app.stack} />
            </span>
          ))}
          {sorted.length > 24 && <span className="text-[11px] text-muted">+{sorted.length - 24}</span>}
        </div>
      )}
    </Link>
  )
}

/**
 * A space's page: its apps as cards — those still on their first deploy as
 * placeholders that fill in as the deploy goes — and its services.
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
        <Skeleton className="h-32" />
      </Page>
    )
  if (!(data.spaces ?? []).includes(space)) {
    return (
      <Page>
        <Empty>
          The space <span className="font-mono text-ink">{space}</span> is no longer here.
        </Empty>
      </Page>
    )
  }

  const apps = (data.apps ?? []).filter((app) => spaceOf(app) === space).sort((a, b) => a.name.localeCompare(b.name))
  const firsts = deploys.filter((d) => d.space === space && !apps.some((app) => app.name === d.app))
  const down = apps.filter((app) => healthOf(app.healthy, app.ready) === 'bad').length
  const deploying = deploys.filter((d) => d.space === space && !d.finishedAt).length

  return (
    <Page>
      <div className="flex flex-wrap items-center justify-between gap-3">
        <Summary
          items={[
            [`${apps.length} ${apps.length === 1 ? 'app' : 'apps'}`],
            services && services.length > 0
              ? [`${services.length} ${services.length === 1 ? 'service' : 'services'}`]
              : null,
            down > 0 ? [`${down} down`, 'text-danger'] : apps.length > 0 ? ['all running'] : null,
            deploying > 0 ? [`${deploying} deploying`, 'text-accent'] : null,
          ]}
        />
        {role !== 'viewer' && (
          <Button icon={UploadCloud} size="sm" onClick={() => openDeploy({ space })}>
            Deploy
          </Button>
        )}
      </div>

      {apps.length === 0 && firsts.length === 0 ? (
        <Empty>
          No apps in this space yet.
          {role !== 'viewer' && (
            <>
              {' '}
              Deploy one with the button above, or with{' '}
              <code className="rounded bg-surface px-1.5 py-0.5 font-mono text-xs">space: {space}</code> in its
              donkey.yaml.
            </>
          )}
        </Empty>
      ) : (
        <section className="grid auto-rows-fr gap-4 sm:grid-cols-2 xl:grid-cols-3 2xl:grid-cols-4">
          {apps.map((app) => (
            <AppTile key={app.name} app={app} deploy={byApp.get(`${space}/${app.name}`)} />
          ))}
          {firsts.map((d) => (
            <DeployTile key={d.app} deploy={d} />
          ))}
        </section>
      )}

      {services && services.length > 0 && (
        <section className="space-y-3">
          <h2 className="font-mono text-[11px] font-semibold tracking-[0.14em] text-muted uppercase">Services</h2>
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

function AppTile({ app, deploy }: { app: AppSummary; deploy?: ActiveDeploy }) {
  const health = healthOf(app.healthy, app.ready)
  const live = deploy && !deploy.finishedAt
  return (
    <Link
      to="/app/$space/$name/{-$tab}"
      params={{ space: spaceOf(app), name: app.name, tab: undefined }}
      className={`group flex flex-col rounded-lg border bg-card transition-[border-color,box-shadow,translate] duration-(--duration-fast) ease-emphasized hover:shadow-sm motion-safe:hover:-translate-y-px focus-visible:outline-2 focus-visible:outline-accent-strong ${
        health === 'bad' ? 'border-danger/50 hover:border-danger' : 'border-line hover:border-line-strong'
      }`}
    >
      <div className="flex-1 p-4">
        <div className="flex items-center justify-between gap-2">
          <span className="flex min-w-0 items-center gap-2">
            <AppIcon health={health} stack={app.stack} />
            <span className="truncate font-mono text-sm font-bold text-ink">{app.name}</span>
          </span>
          <ExposureBadge exposure={app.exposure} mode={app.mode} />
        </div>
        <div className="mt-2 space-y-1 text-xs text-muted">
          {app.url ? (
            <div className="truncate font-mono">{app.url.replace(/^https?:\/\//, '')}</div>
          ) : (
            <div
              className="flex items-center gap-1 truncate font-mono"
              title="No address of its own — its siblings reach it by name"
            >
              <Network className="size-3 shrink-0" aria-hidden /> http://{app.name}
            </div>
          )}
          <div className={health === 'bad' ? 'text-danger' : ''}>
            {healthLine[health](app.ready)}
            {app.startedAt && <span className="text-muted"> · up {ago(app.startedAt)}</span>}
          </div>
        </div>
        {((app.addons ?? []).length > 0 || app.hasStorage) && (
          <div className="mt-3 flex flex-wrap gap-1.5">
            {(app.addons ?? []).map((addon) => (
              <span
                key={addon}
                className="inline-flex items-center gap-1 rounded bg-raised px-1.5 py-0.5 font-mono text-[10px] text-ink-soft"
              >
                <Database className="size-3" aria-hidden /> {addon}
              </span>
            ))}
            {app.hasStorage && (
              <span className="inline-flex items-center gap-1 rounded bg-raised px-1.5 py-0.5 font-mono text-[10px] text-ink-soft">
                <HardDrive className="size-3" aria-hidden /> disk
              </span>
            )}
          </div>
        )}
      </div>
      {live && (
        <div className="border-t border-line px-4 py-2.5">
          <DeployProgress deploy={deploy} compact />
        </div>
      )}
      {deploy?.stage === 'failed' && (
        <div className="truncate border-t border-danger/30 px-4 py-2 text-xs text-danger" title={deploy.error}>
          Last deploy failed — the previous version keeps running
        </div>
      )}
    </Link>
  )
}

// An app on its first deploy: what it will be, filling in as the deploy goes.
function DeployTile({ deploy }: { deploy: ActiveDeploy }) {
  const failed = deploy.stage === 'failed'
  return (
    <Link
      to="/app/$space/$name/{-$tab}"
      params={{ space: deploy.space, name: deploy.app, tab: undefined }}
      className={`flex flex-col justify-between gap-3 rounded-lg border border-dashed p-4 transition-colors focus-visible:outline-2 focus-visible:outline-accent-strong ${
        failed
          ? 'border-danger/50 bg-danger/[.03] hover:border-danger'
          : 'border-accent/50 bg-accent-soft/30 hover:border-accent'
      }`}
    >
      <div className="flex items-center justify-between gap-2">
        <span className="flex min-w-0 items-center gap-2">
          {failed ? (
            <span className="size-2.5 shrink-0 rounded-full border-2 border-danger" aria-hidden />
          ) : (
            <Loader2 className="size-3.5 shrink-0 animate-spin text-accent" aria-hidden />
          )}
          <span className="truncate font-mono text-sm font-bold text-ink">{deploy.app}</span>
        </span>
        <span
          className={`shrink-0 font-mono text-[10px] tracking-wide uppercase ${failed ? 'text-danger' : 'text-accent'}`}
        >
          {failed ? 'failed' : 'first deploy'}
        </span>
      </div>
      <DeployProgress deploy={deploy} compact />
    </Link>
  )
}

/**
 * Where a deploy stands: its three steps, the latest line of its log, who
 * started it and from where. Compact for a card; full for an app's page.
 */
export function DeployProgress({ deploy, compact = false }: { deploy: ActiveDeploy; compact?: boolean }) {
  useNow(deploy.finishedAt ? 30_000 : 5_000) // keeps "started 2m ago" counting
  const failed = deploy.stage === 'failed'
  const at = deploy.stage === 'done' ? STAGES.length : STAGES.findIndex((s) => s.id === deploy.stage)
  const who = [deploy.by, deploy.via === 'cli' ? 'CLI' : 'console'].filter(Boolean).join(' · ')
  return (
    <div className={compact ? 'space-y-2' : 'space-y-3'}>
      <div className="flex items-center justify-between gap-3 text-xs">
        <span className={failed ? 'text-danger' : 'text-ink-soft'}>{stageText[deploy.stage]}</span>
        <span className="shrink-0 text-muted">
          {ago(deploy.startedAt)} ago{who && ` · ${who}`}
        </span>
      </div>
      {/* Three segments: inspect, build, release. The current one breathes. */}
      <div className="grid grid-cols-3 gap-1" aria-hidden>
        {STAGES.map((stage, i) => (
          <span
            key={stage.id}
            className={`h-1 rounded-full ${
              failed && i === Math.max(at, 0)
                ? 'bg-danger'
                : i < at
                  ? 'bg-accent'
                  : i === at && !failed
                    ? 'bg-accent/60 motion-safe:animate-pulse'
                    : 'bg-line'
            }`}
          />
        ))}
      </div>
      {!compact && (
        <div className="flex justify-between text-[11px] text-muted">
          {STAGES.map((stage) => (
            <span key={stage.id}>{stage.label}</span>
          ))}
        </div>
      )}
      {(failed ? deploy.error : deploy.line) && (
        <p
          className={`truncate font-mono text-[11px] ${failed ? 'text-danger' : 'text-muted'}`}
          title={failed ? deploy.error : deploy.line}
        >
          {failed ? deploy.error : deploy.line}
        </p>
      )}
    </div>
  )
}

/**
 * An app that does not run yet, on its own page: its first deploy as it
 * goes, or how it failed. The page turns into the app once it runs.
 */
export function DeployPlaceholder({ deploy }: { deploy: ActiveDeploy }) {
  const openDeploy = useDeploy()
  const failed = deploy.stage === 'failed'
  return (
    <div
      className={`rounded-xl border border-dashed p-6 ${failed ? 'border-danger/50 bg-danger/[.03]' : 'border-accent/50 bg-accent-soft/30'}`}
    >
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="flex items-center gap-2.5">
          {failed ? (
            <span className="size-3 rounded-full border-2 border-danger" aria-hidden />
          ) : (
            <Loader2 className="size-4 animate-spin text-accent" aria-hidden />
          )}
          <h2 className="text-sm font-semibold text-ink">
            {failed ? 'The first deploy failed' : 'Deploying the first version'}
          </h2>
        </div>
        {failed && (
          <Button
            kind="quiet"
            size="sm"
            icon={Rocket}
            onClick={() => openDeploy({ space: deploy.space, name: deploy.app })}
          >
            Deploy again
          </Button>
        )}
      </div>
      <div className="mt-4 max-w-2xl">
        <DeployProgress deploy={deploy} />
      </div>
      <p className="mt-4 text-xs text-muted">
        {failed
          ? 'Nothing runs yet. The build record keeps the full log.'
          : 'The rest of this page appears as soon as the app runs — you can close the deploy window or the browser meanwhile.'}
      </p>
    </div>
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
      className="flex items-center gap-3 rounded-lg border border-line bg-card p-3 text-left transition-colors hover:border-line-strong focus-visible:outline-2 focus-visible:outline-accent-strong"
    >
      <span className="flex size-8 shrink-0 items-center justify-center rounded-md bg-raised text-muted">
        <view.Icon className="size-4" aria-hidden />
      </span>
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

function Page({ children }: { children: React.ReactNode }) {
  return <main className="dotgrid flex-1 space-y-6 px-6 py-6">{children}</main>
}

function Summary({ items }: { items: ([string] | [string, string] | null)[] }) {
  return (
    <p className="flex flex-wrap items-center gap-x-2 text-sm text-ink-soft">
      {items
        .filter((item): item is [string] | [string, string] => item !== null)
        .map(([text, tone], i) => (
          <span key={text} className="flex items-center gap-2">
            {i > 0 && (
              <span className="text-line-strong" aria-hidden>
                ·
              </span>
            )}
            <span className={tone ?? ''}>{text}</span>
          </span>
        ))}
    </p>
  )
}

function Board({ title, tone, children }: { title: string; tone?: 'danger'; children: React.ReactNode }) {
  return (
    <div className="rounded-lg border border-line bg-card">
      <h2
        className={`border-b border-line px-4 py-2.5 font-mono text-[11px] font-semibold tracking-[0.14em] uppercase ${tone === 'danger' ? 'text-danger' : 'text-muted'}`}
      >
        {title}
      </h2>
      <ul className="max-h-64 overflow-y-auto p-1.5">{children}</ul>
    </div>
  )
}

function BoardRow({ space, app, children }: { space: string; app: string; children: React.ReactNode }) {
  return (
    <li>
      <Link
        to="/app/$space/$name/{-$tab}"
        params={{ space, name: app, tab: undefined }}
        className="flex h-8 items-center gap-2.5 rounded-md px-2.5 text-sm hover:bg-line/50 focus-visible:outline-2 focus-visible:outline-accent-strong"
      >
        {children}
      </Link>
    </li>
  )
}

function Empty({ children }: { children: React.ReactNode }) {
  return (
    <p className="rounded-lg border border-dashed border-line bg-card px-6 py-10 text-center text-sm text-muted">
      {children}
    </p>
  )
}
