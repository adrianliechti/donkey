import { useMemo, useState, type ReactNode } from 'react'
import { Menu, MenuButton, MenuHeading, MenuItem, MenuItems, MenuSection } from '@headlessui/react'
import { Link, useLocation, useParams } from '@tanstack/react-router'
import { Check, ChevronsUpDown, FolderPlus, Loader2, Rocket } from 'lucide-react'
import type { ActiveDeploy, AppSummary } from './api'
import { useDeploy } from './deploy'
import { NewSpaceDialog } from './NewSpaceDialog'
import { useDeploys, useFleet, useSpaceServices } from './queries'
import { serviceUI } from './serviceProtocol'
import { preloadConsole, serviceTabs } from './consoles'
import { SpaceActions } from './SpaceActions'
import { AppIcon } from './AppIcon'
import { StatusDot } from './ui'
import { healthOf } from './vocabulary'

/**
 * Where you are, and the way anywhere else: the breadcrumb is two
 * switchers. The space opens every space you can see — what is down first,
 * then the rest — and the app opens everything in that space: its apps
 * (those still being deployed for the first time too) and its services. The
 * space's own actions sit in the ⋯ between them. They are menus of links, not
 * searches: typing jumps to an entry, and ⌘K finds anything anywhere.
 */
export function Crumbs() {
  const params = useParams({ strict: false }) as { space?: string; name?: string }
  // The last crumb names an app or, on a service's page, the service.
  const onService = useLocation({ select: (l) => l.pathname.startsWith('/service/') })
  return (
    <nav aria-label="Spaces and apps" className="flex min-w-0 items-center gap-0.5">
      <Slash />
      <SpaceSwitcher current={params.space} />
      {params.space && (
        <>
          <SpaceActions space={params.space} />
          <Slash />
          <AppSwitcher space={params.space} current={params.name} service={onService} />
        </>
      )}
    </nav>
  )
}

function Slash() {
  return (
    <span className="px-1 font-mono text-sm text-line-strong select-none" aria-hidden>
      /
    </span>
  )
}

const crumb =
  'inline-flex h-8 min-w-0 items-center gap-1.5 rounded-md px-2 font-mono text-sm text-ink transition-colors hover:bg-line/60 focus-visible:outline-2 focus-visible:outline-accent-strong data-open:bg-line/60'
const panel =
  'z-50 overflow-hidden rounded-xl border border-line bg-card shadow-2xl [--anchor-gap:6px] focus:outline-none transition duration-(--duration-fast) ease-emphasized data-closed:scale-[.98] data-closed:opacity-0'
const list = 'max-h-[min(28rem,60vh)] overflow-y-auto p-1.5'
const option =
  'flex h-8 w-full items-center gap-2.5 rounded-md px-2 text-left text-sm text-ink-soft focus:outline-none data-focus:bg-accent-soft data-focus:text-ink'

type Down = { space: string; apps: AppSummary[] }

function SpaceSwitcher({ current }: { current?: string }) {
  const { data } = useFleet()
  const { deploys } = useDeploys()
  const [creating, setCreating] = useState(false)
  const down = (data?.apps ?? []).filter(
    (app) => (app.space || 'default') === current && healthOf(app.healthy, app.ready) === 'bad',
  ).length

  const { attention, spaces, count } = useMemo(() => {
    const apps = new Map<string, AppSummary[]>()
    for (const space of data?.spaces ?? []) apps.set(space, [])
    for (const app of data?.apps ?? []) {
      const space = app.space || 'default'
      apps.set(space, [...(apps.get(space) ?? []), app])
    }
    const spaces = [...apps.keys()].sort((a, b) => a.localeCompare(b))
    const attention: Down[] = spaces
      .map((space) => ({ space, apps: (apps.get(space) ?? []).filter((app) => healthOf(app.healthy, app.ready) === 'bad') }))
      .filter((d) => d.apps.length > 0)
    return { attention, spaces, count: (space: string) => apps.get(space) ?? [] }
  }, [data])

  return (
    <>
      <Menu>
        <MenuButton className={`${crumb} ${current ? 'font-semibold' : 'text-ink-soft'}`}>
          {down > 0 && <StatusDot health="bad" />}
          <span className="truncate">{current ?? 'Select a space'}</span>
          <ChevronsUpDown className="size-3.5 shrink-0 text-muted" aria-hidden />
        </MenuButton>
        <MenuItems transition anchor="bottom start" className={`${panel} w-80`}>
          <div className={list}>
            {attention.length > 0 && (
              <MenuSection>
                <Heading tone="danger">Needs attention</Heading>
                {attention.flatMap(({ space, apps }) =>
                  apps.map((app) => (
                    <MenuItem key={`${space}/${app.name}`}>
                      <Link
                        to="/app/$space/$name/{-$tab}"
                        params={{ space, name: app.name, tab: undefined }}
                        className={option}
                      >
                        <AppIcon health="bad" stack={app.stack} />
                        <span className="truncate font-mono text-ink">{app.name}</span>
                        <span className="ml-auto shrink-0 truncate font-mono text-xs text-muted">{space}</span>
                      </Link>
                    </MenuItem>
                  )),
                )}
              </MenuSection>
            )}
            <MenuSection>
              {attention.length > 0 && <Heading>Spaces</Heading>}
              {spaces.map((space) => {
                const apps = count(space)
                const downHere = apps.filter((app) => healthOf(app.healthy, app.ready) === 'bad').length
                const deploying = deploys.filter((d) => d.space === space && !d.finishedAt).length
                return (
                  <MenuItem key={space}>
                    <Link to="/space/$space" params={{ space }} className={option}>
                      <span className="flex size-4 shrink-0 items-center justify-center">
                        {space === current && <Check className="size-3.5 text-accent" aria-hidden />}
                      </span>
                      <span className="truncate font-mono text-ink">{space}</span>
                      {deploying > 0 && (
                        <Loader2 className="size-3.5 shrink-0 animate-spin text-accent" aria-label={`${deploying} deploying`} />
                      )}
                      <span className={`ml-auto shrink-0 text-xs ${downHere > 0 ? 'text-danger' : 'text-muted'}`}>
                        {downHere > 0 ? `${downHere} down` : `${apps.length} ${apps.length === 1 ? 'app' : 'apps'}`}
                      </span>
                    </Link>
                  </MenuItem>
                )
              })}
            </MenuSection>
            {spaces.length === 0 && <p className="px-2 py-5 text-center text-sm text-muted">No spaces yet.</p>}
          </div>
          {data?.platformAdmin && (
            <Footer>
              <MenuItem>
                <button type="button" onClick={() => setCreating(true)} className={option}>
                  <FolderPlus className="size-4 shrink-0 text-muted" aria-hidden />
                  New space
                </button>
              </MenuItem>
            </Footer>
          )}
        </MenuItems>
      </Menu>
      <NewSpaceDialog
        key={String(creating)}
        domain={data?.domain ?? ''}
        open={creating}
        onClose={() => setCreating(false)}
        onCreated={() => setCreating(false)}
      />
    </>
  )
}

function AppSwitcher({ space, current, service }: { space: string; current?: string; service: boolean }) {
  const { data } = useFleet()
  const { deploys, byApp } = useDeploys()
  const openDeploy = useDeploy()
  // Switching keeps the tab where the next page has it: from one app's logs
  // to the next one's, from one database's query to the next one's.
  const { tab } = useParams({ strict: false }) as { tab?: string }
  const role = data?.roles[space] ?? 'viewer'
  const { data: services } = useSpaceServices(space, role !== 'viewer')
  const app = service ? undefined : (data?.apps ?? []).find((a) => (a.space || 'default') === space && a.name === current)
  const svc = service ? services?.find((s) => s.name === current) : undefined
  const deploying = current && !service ? byApp.get(`${space}/${current}`) : undefined
  const ServiceIcon = serviceUI(svc?.protocol).Icon

  const deployed = (data?.apps ?? []).filter((a) => (a.space || 'default') === space)
  // An app being deployed for the first time is listed already, as what it
  // will be — so is one whose first deploy just failed, until it ages out.
  const firsts = deploys
    .filter((d) => d.space === space && !deployed.some((a) => a.name === d.app))
    .map((d) => ({ name: d.app }))
  const apps = [...deployed, ...firsts].sort((a, b) => a.name.localeCompare(b.name))

  return (
    <Menu>
      <MenuButton className={`${crumb} ${current ? 'font-semibold' : 'text-ink-soft'}`}>
        {deploying && !deploying.finishedAt ? (
          <Loader2 className="size-3.5 shrink-0 animate-spin text-accent" aria-hidden />
        ) : app ? (
          <AppIcon health={healthOf(app.healthy, app.ready)} stack={app.stack} className="size-3.5" />
        ) : (
          service && <ServiceIcon className="size-3.5 shrink-0 text-muted" aria-hidden />
        )}
        <span className="truncate">{current ?? 'Select an app'}</span>
        <ChevronsUpDown className="size-3.5 shrink-0 text-muted" aria-hidden />
      </MenuButton>
      <MenuItems transition anchor="bottom start" className={`${panel} w-80`}>
        <div className={list}>
          {apps.length === 0 && !services?.length && <p className="px-2 py-5 text-center text-sm text-muted">No apps yet.</p>}
          {apps.length > 0 && (
            <MenuSection>
              <Heading>Apps</Heading>
              {apps.map((a) => (
                <MenuItem key={a.name}>
                  <Link
                    to="/app/$space/$name/{-$tab}"
                    params={{ space, name: a.name, tab: service ? undefined : tab }}
                    className={option}
                  >
                    <span className="flex size-4 shrink-0 items-center justify-center">
                      <AppMark app={a} deploy={byApp.get(`${space}/${a.name}`)} />
                    </span>
                    <span className="truncate font-mono text-ink">{a.name}</span>
                    <DeployNote deploy={byApp.get(`${space}/${a.name}`)} />
                    {a.name === current && !service && <Check className="ml-auto size-3.5 shrink-0 text-accent" aria-hidden />}
                  </Link>
                </MenuItem>
              ))}
            </MenuSection>
          )}
          {!!services?.length && (
            <MenuSection>
              <Heading>Services</Heading>
              {services.map((s) => {
                const view = serviceUI(s.protocol)
                const unready = !!s.state && s.state !== 'ready'
                const keepTab = service && serviceTabs(s.protocol).some((t) => t.id === tab)
                return (
                  <MenuItem key={s.name}>
                    <Link
                      to="/service/$space/$name/{-$tab}"
                      params={{ space, name: s.name, tab: keepTab ? tab : undefined }}
                      onMouseEnter={() => preloadConsole(view.console)}
                      className={option}
                    >
                      <span className="flex size-4 shrink-0 items-center justify-center">
                        <view.Icon className="size-3.5 text-muted" aria-hidden />
                      </span>
                      <span className="truncate font-mono text-ink">{s.name}</span>
                      <span className={`ml-auto shrink-0 text-xs ${s.state === 'failed' ? 'text-danger' : 'text-muted'}`}>
                        {unready ? s.state : s.attachedTo.length > 0 ? `used by ${s.attachedTo.length}` : view.noun}
                      </span>
                      {s.name === current && service && <Check className="size-3.5 shrink-0 text-accent" aria-hidden />}
                    </Link>
                  </MenuItem>
                )
              })}
            </MenuSection>
          )}
        </div>
        {role !== 'viewer' && (
          <Footer>
            <MenuItem>
              <button type="button" onClick={() => openDeploy({ space })} className={option}>
                <Rocket className="size-4 shrink-0 text-muted" aria-hidden />
                Deploy an app here
              </button>
            </MenuItem>
          </Footer>
        )}
      </MenuItems>
    </Menu>
  )
}

// An app's mark in a list: its health, or a spinner while a deploy is under
// way, or a hollow ring for one that never ran (its first deploy failed).
function AppMark({ app, deploy }: { app: Partial<AppSummary>; deploy?: ActiveDeploy }) {
  if (deploy && !deploy.finishedAt) return <Loader2 className="size-3.5 animate-spin text-accent" aria-hidden />
  if (app.ready === undefined) return <span className="size-2.5 rounded-full border-2 border-danger" aria-hidden />
  return <AppIcon health={healthOf(!!app.healthy, app.ready)} stack={app.stack} />
}

function DeployNote({ deploy }: { deploy?: ActiveDeploy }) {
  if (!deploy || (deploy.finishedAt && deploy.stage !== 'failed')) return null
  return (
    <span className={`shrink-0 text-xs ${deploy.stage === 'failed' ? 'text-danger' : 'text-accent'}`}>
      {deploy.stage === 'failed' ? 'failed' : deploy.stage}
    </span>
  )
}

function Heading({ tone, children }: { tone?: 'danger'; children: ReactNode }) {
  return (
    <MenuHeading
      className={`px-2 pt-2.5 pb-1 font-mono text-[10px] font-semibold tracking-[0.14em] uppercase ${
        tone === 'danger' ? 'text-danger' : 'text-muted'
      }`}
    >
      {children}
    </MenuHeading>
  )
}

function Footer({ children }: { children: ReactNode }) {
  return <div className="border-t border-line p-1.5">{children}</div>
}
