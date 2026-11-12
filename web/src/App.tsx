import { useEffect, useState } from 'react'
import {
  createHashHistory,
  createRootRouteWithContext,
  createRoute,
  createRouter,
  HeadContent,
  Navigate,
  Outlet,
  redirect,
  RouterProvider,
  useNavigate,
} from '@tanstack/react-router'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { Activity, Search } from 'lucide-react'
import { setUnauthorizedHandler } from './api'
import { AppDetail, type AppTab } from './AppDetail'
import type { SourceSearch } from './sourceSearch'
import { CommandPalette } from './CommandPalette'
import { useDeploy } from './deploy'
import { DeployProvider } from './DeployDrawer'
import { FleetOverview } from './FleetOverview'
import { SpaceOverview } from './Overview'
import { fleetQuery, statusQuery, useFleet } from './queries'
import { Crumbs } from './Switcher'
import { SystemView } from './SystemView'
import { DonkeyLogo, IconButton, Page, Toolbar } from './ui'
import { UserMenu } from './UserMenu'
import { PlatformSettings } from './PlatformSettings'
import { ServiceDetail } from './ServiceDetail'
import { cleanServiceSearch, type ServiceSearch } from './serviceSearch'

// The routes live in the hash (#/app/acme/shop/logs) so refresh keeps your
// place, an unhealthy app can be linked to a teammate, and bookmarks from
// the previous console keep working.
const APP_TABS: AppTab[] = ['overview', 'release', 'source', 'traffic', 'logs', 'files', 'variables', 'settings']
const pageTitle = (page: string) => page + ' · Donkey'

// One cache for the whole console. Short staleness keeps panels honest
// while still letting a remounted view show what it knew instantly. The
// router carries the client in its context, so route loaders can warm the
// cache before a view renders.
const queryClient = new QueryClient({
  defaultOptions: { queries: { retry: 1, staleTime: 5_000 } },
})

function Shell() {
  return (
    <DeployProvider>
      <Frame />
    </DeployProvider>
  )
}

// The console's frame: the bar on top — whose breadcrumb is how you get
// anywhere — and the page under it. Every page is a space's app (or the
// system view); the fleet has no page of its own, its map is the breadcrumb.
function Frame() {
  const navigate = useNavigate()
  const { data: fleet } = useFleet()
  const [paletteOpen, setPaletteOpen] = useState(false)
  const [platformSettingsOpen, setPlatformSettingsOpen] = useState(false)

  // ⌘K / Ctrl-K opens the palette from anywhere. Registered once at the shell
  // so it works on every page and over open switchers.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') {
        e.preventDefault()
        setPaletteOpen((v) => !v)
      }
    }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  }, [])

  return (
    <div className="flex min-h-screen flex-col">
      <HeadContent />
      {/* A fixed height (h-13), border included: the app header sticks at
          top-13, exactly where it already sits, so it never moves when the
          page scrolls. Header, tabs and page share one gutter (px-6), so the
          logo, the first tab and the content start on the same line. */}
      <header className="sticky top-0 z-30 h-13 border-b border-line bg-card/95 backdrop-blur">
        <div className="flex h-full items-center justify-between gap-4 px-6">
          <div className="flex min-w-0 items-center gap-1">
            <button
              onClick={() => navigate({ to: '/' })}
              className="flex shrink-0 items-center gap-2 font-mono text-base font-bold tracking-tight focus-visible:outline-2 focus-visible:outline-accent-strong"
            >
              <DonkeyLogo className="size-5 text-ink" />
              Donkey
            </button>
            <Crumbs />
          </div>
          {/* -mr-2: the last icon's glyph, not its hover box, meets the
              gutter the Deploy button and the page content end on. */}
          <Toolbar className="-mr-2">
            <IconButton icon={Search} label="Search apps and spaces — ⌘K" onClick={() => setPaletteOpen(true)} />
            {fleet?.platformAdmin && (
              <IconButton icon={Activity} label="System health" onClick={() => navigate({ to: '/system' })} />
            )}
            <UserMenu
              onPlatformSettings={fleet?.platformAdmin ? () => setPlatformSettingsOpen(true) : undefined}
              onSignOut={() => {
                // The gateway owns the session — its logout page clears the
                // cookie for the console and every app at once.
                window.location.href = '/oauth2/logout'
              }}
            />
          </Toolbar>
        </div>
      </header>

      <CommandPalette open={paletteOpen} onClose={() => setPaletteOpen(false)} />
      {platformSettingsOpen && <PlatformSettings onClose={() => setPlatformSettingsOpen(false)} />}

      <div className="flex min-w-0 flex-1 flex-col">
        <Outlet />
      </div>
    </div>
  )
}

// An app is a page of its own: the room its dashboards, logs and diffs need,
// a real place in the browser's history, and the breadcrumb's switcher for the
// next one. A redeploy slides over it, so the app stays in view meanwhile.
function AppPage({ space, name, tab }: { space: string; name: string; tab: AppTab }) {
  const navigate = useNavigate()
  const openDeploy = useDeploy()

  return (
    <AppDetail
      // A different app resets the page; a different tab must not.
      key={`${space}/${name}`}
      space={space}
      app={name}
      tab={tab}
      onTab={(next) =>
        navigate({
          to: '/app/$space/$name/{-$tab}',
          params: { space, name, tab: next === 'overview' ? undefined : next },
        })
      }
      onDeploy={() => openDeploy({ space, name })}
      onRemoved={() => navigate({ to: '/space/$space', params: { space } })}
    />
  )
}

// A service is a page too: its console in the first tabs, then Connection
// and Settings. Crossing tabs keeps the console's place in the query.
function ServicePage({ space, name, tab }: { space: string; name: string; tab?: string }) {
  const navigate = useNavigate()
  return (
    <ServiceDetail
      key={`${space}/${name}`}
      space={space}
      service={name}
      tab={tab}
      onTab={(next) =>
        navigate({ to: '/service/$space/$name/{-$tab}', params: { space, name, tab: next }, search: true })
      }
      onRemoved={() => navigate({ to: '/space/$space', params: { space } })}
    />
  )
}

// Old deploy links open the deploy drawer over the front page.
function DeployLink({ space = '', name = '' }: { space?: string; name?: string }) {
  const openDeploy = useDeploy()
  useEffect(() => {
    openDeploy({ space, name })
  }, [openDeploy, space, name])
  return <Navigate to="/" replace />
}

// Code-style routes: the whole map in one place, typed params included.
// Titles live on the routes (HeadContent renders them); loaders warm the
// query cache so a view opens with its data already there — hovering an app
// in the switcher already warms its status.
const rootRoute = createRootRouteWithContext<{ queryClient: QueryClient }>()({
  component: Shell,
  loader: ({ context }) => context.queryClient.ensureQueryData(fleetQuery),
  head: () => ({ meta: [{ title: pageTitle('Apps') }] }),
})

// Donkey's front page: every space, how its apps are doing, what is being
// deployed right now.
const homeRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/',
  component: FleetOverview,
  head: () => ({ meta: [{ title: pageTitle('Spaces') }] }),
})

// The previous console called the fleet "#/apps" — old bookmarks land here.
const legacyAppsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/apps',
  beforeLoad: () => {
    throw redirect({ to: '/' })
  },
})

const systemRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/system',
  component: () => (
    <Page>
      <SystemView />
    </Page>
  ),
  head: () => ({ meta: [{ title: pageTitle('System') }] }),
})

const spaceRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/space/$space',
  head: ({ params }) => ({ meta: [{ title: pageTitle(params.space) }] }),
  component: () => {
    const { space } = spaceRoute.useParams()
    return <SpaceOverview space={space} />
  },
})

const deployRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/deploy',
  component: () => <DeployLink />,
})

const deploySpaceRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/deploy/$space',
  component: () => <DeployLink space={deploySpaceRoute.useParams().space} />,
})

const deployAppRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/deploy/$space/$name',
  component: () => {
    const { space, name } = deployAppRoute.useParams()
    return <DeployLink space={space} name={name} />
  },
})

// One route for the app and every one of its tabs ({-$tab} is optional,
// so old "/app/space/name" links still land on the overview). Crossing tabs
// stays inside this route — nothing above it re-renders, nothing reloads.
const appRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/app/$space/$name/{-$tab}',
  // Where the Source tab has drilled down to — a commit, its files, one
  // file — so back and forward walk the drilldown and every step can be
  // linked. Anything else in the query is dropped.
  validateSearch: (search: Record<string, unknown>): SourceSearch => ({
    commit: typeof search.commit === 'string' && search.commit ? search.commit : undefined,
    files: search.files === true || search.files === 1 || search.files === '1' ? 1 : undefined,
    path: typeof search.path === 'string' && search.path ? search.path : undefined,
  }),
  loader: ({ context, params }) => context.queryClient.ensureQueryData(statusQuery(params.space, params.name)),
  head: ({ params }) => ({ meta: [{ title: pageTitle(params.name) }] }),
  component: () => {
    const { space, name, tab } = appRoute.useParams()
    // The tab comes from the URL, so it can name something that doesn't
    // exist — fall back rather than render an empty page.
    const known = APP_TABS.find((t) => t === tab) ?? 'overview'
    return <AppPage space={space} name={name} tab={known} />
  },
})

// A service and every one of its tabs. Its console keeps its place — the
// table, the folder, the key — in the query, in keys of its own choosing.
const serviceRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/service/$space/$name/{-$tab}',
  validateSearch: (search: Record<string, unknown>): ServiceSearch => cleanServiceSearch(search),
  head: ({ params }) => ({ meta: [{ title: pageTitle(params.name) }] }),
  component: () => {
    const { space, name, tab } = serviceRoute.useParams()
    return <ServicePage space={space} name={name} tab={tab} />
  },
})

const routeTree = rootRoute.addChildren([
  homeRoute,
  appRoute,
  serviceRoute,
  spaceRoute,
  deployRoute,
  deploySpaceRoute,
  deployAppRoute,
  legacyAppsRoute,
  systemRoute,
])

const router = createRouter({
  routeTree,
  history: createHashHistory(),
  context: { queryClient },
  // Hovering a link warms its data through the loader; staleness is
  // Query's business, so the router's own preload timer is switched off.
  defaultPreload: 'intent',
  defaultPreloadStaleTime: 0,
  // An unknown hash is an old or mistyped link — the start page is always a
  // sensible place to land.
  defaultNotFoundComponent: () => <Navigate to="/" replace />,
})

declare module '@tanstack/react-router' {
  interface Register {
    router: typeof router
  }
}

export default function App() {
  useEffect(() => {
    // The gateway owns the session. When it expires, a full navigation is
    // the sign-in: reload, the gateway redirects through the IdP (instant
    // while its session lives) and lands back here.
    setUnauthorizedHandler(() => window.location.reload())
  }, [])

  return (
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  )
}
