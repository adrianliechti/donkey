import { useEffect, useState } from 'react'
import {
  createHashHistory,
  createRootRouteWithContext,
  createRoute,
  createRouter,
  HeadContent,
  Link,
  Navigate,
  Outlet,
  redirect,
  RouterProvider,
  useNavigate,
  useParams,
} from '@tanstack/react-router'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { Activity, ArrowLeft, Search } from 'lucide-react'
import { setUnauthorizedHandler, type AppSummary } from './api'
import { AppDetail, type AppTab } from './AppDetail'
import { AppsList } from './AppsList'
import { CommandPalette } from './CommandPalette'
import { DeployView } from './DeployView'
import { Drawer, DrawerBody, PaneHeader } from './dialog'
import { fleetQuery, statusQuery, useFleet } from './queries'
import { SpaceCard } from './SpaceCard'
import { SystemView } from './SystemView'
import { DonkeyLogo, ErrorNote, IconButton, Spinner, Toolbar, healthOf, type Health } from './ui'
import { UserMenu } from './UserMenu'
import { PlatformSettings } from './PlatformSettings'

const healthRank: Record<Health, number> = { bad: 0, unknown: 1, ok: 2 }

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
  const navigate = useNavigate()
  const { data: fleet } = useFleet()
  const [paletteOpen, setPaletteOpen] = useState(false)
  const [platformSettingsOpen, setPlatformSettingsOpen] = useState(false)

  // ⌘K / Ctrl-K opens the palette from anywhere. Registered once at the shell
  // so it works over the fleet, an app panel, or any page.
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
      <header className="sticky top-0 z-30 border-b border-line bg-card/95 backdrop-blur">
        <div className="mx-auto flex max-w-6xl items-center justify-between gap-4 px-4 py-2.5">
          <div className="flex min-w-0 items-center gap-3">
            <button
              onClick={() => navigate({ to: '/' })}
              className="flex shrink-0 items-center gap-2 font-mono text-base font-bold tracking-tight focus-visible:outline-2 focus-visible:outline-accent-strong"
            >
              <DonkeyLogo className="size-5 text-ink" />
              Donkey
            </button>
            <Breadcrumb />
          </div>
          {/* A uniform icon toolbar. Deploying lives inside a space now (its
              Add menu), so there is no global Deploy here. */}
          <Toolbar>
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

      {/* The paddock: the fleet lives on a dotted field. */}
      <main className="dotgrid flex-1">
        <div className="mx-auto max-w-6xl px-4 py-8">
          <Outlet />
        </div>
      </main>
    </div>
  )
}

// Breadcrumb says where you are and lets you step back up: the space is a
// link to its own page, the app name is where you are. It shows on both the
// space page (space only) and inside an app (space / app).
function Breadcrumb() {
  const params = useParams({ strict: false }) as { space?: string; name?: string }
  if (!params.space) return null
  return (
    <span className="flex min-w-0 items-center gap-1.5 font-mono text-sm text-muted">
      <span aria-hidden>/</span>
      {params.name ? (
        <Link
          to="/space/$space"
          params={{ space: params.space }}
          className="truncate text-ink-soft hover:text-ink hover:underline"
        >
          {params.space}
        </Link>
      ) : (
        <span className="truncate font-semibold text-ink">{params.space}</span>
      )}
      {params.name && (
        <>
          <span aria-hidden>/</span>
          <span className="truncate font-semibold text-ink">{params.name}</span>
        </>
      )}
    </span>
  )
}

// A space as a place of its own: its services and apps, linkable and
// bookmarkable, reached from the breadcrumb or the command palette. It reuses
// the same SpaceCard the fleet shows, so there is one way a space looks.
function SpacePage({ space }: { space: string }) {
  const { data, error, reload } = useFleet()
  const apps = (data?.apps ?? [])
    .filter((a: AppSummary) => (a.space || 'default') === space)
    .sort((a, b) => {
      const ra = healthRank[healthOf(a.healthy, a.ready)]
      const rb = healthRank[healthOf(b.healthy, b.ready)]
      return ra !== rb ? ra - rb : a.name.localeCompare(b.name)
    })
  // Known if it still has apps or is a declared (possibly empty) space. After
  // a delete it is neither, so we send the operator back to the fleet.
  const known = !data || apps.length > 0 || (data.spaces ?? []).includes(space)

  return (
    <div className="space-y-5">
      <Link
        to="/"
        className="inline-flex items-center gap-1.5 text-sm text-muted hover:text-ink"
      >
        <ArrowLeft className="size-4" aria-hidden /> Fleet
      </Link>
      <ErrorNote message={error} />
      {!data && !error && <Spinner label="Asking the cluster…" />}
      {data && !known && (
        <div className="rounded-xl border border-line bg-card p-8 text-center text-sm text-muted">
          The space <span className="font-mono">{space}</span> is no longer here.
        </div>
      )}
      {data && known && (
        <SpaceCard
          space={space}
          apps={apps}
          role={data.roles[space] ?? 'viewer'}
          onChanged={reload}
        />
      )}
    </div>
  )
}

// The fleet is a layout, not a page: the app panel renders into its Outlet,
// so opening an app or switching its tabs never unmounts the canvas behind
// it — no refetch, no skeleton flash.
function FleetLayout() {
  return (
    <>
      <AppsList />
      <Outlet />
    </>
  )
}

// A drawer bound to a route: it opens with the route and, on close, slides
// out first and navigates back to the fleet when the exit has finished.
// Closing never resets the fleet's scroll — the panel sat on top of it.
function useRouteDrawer() {
  const navigate = useNavigate()
  const [open, setOpen] = useState(true)
  return {
    open,
    close: () => setOpen(false),
    afterClose: () => navigate({ to: '/', resetScroll: false }),
  }
}

// The app panel slides over the fleet instead of replacing it — the canvas
// behind it keeps saying how everything else is doing.
function AppOverlay({ space, name, tab }: { space: string; name: string; tab: AppTab }) {
  const navigate = useNavigate()
  const drawer = useRouteDrawer()

  return (
    // Reading code wants the room a file tree and a source pane need side by
    // side; every other tab keeps the panel's usual width.
    <Drawer {...drawer} onClose={drawer.close} label={`${name} — app details`} width={tab === 'source' ? 'max-w-5xl' : undefined}>
      <AppDetail
        // A different app resets the panel; a different tab must not.
        key={`${space}/${name}`}
        space={space}
        app={name}
        tab={tab}
        onTab={(next) =>
          navigate({
            to: '/app/$space/$name/{-$tab}',
            params: { space, name, tab: next === 'overview' ? undefined : next },
            // Tabs change the panel, not the page: the fleet behind must
            // not jump to the top.
            resetScroll: false,
          })
        }
        onDeploy={() =>
          navigate({ to: '/deploy/$space/$name', params: { space, name }, resetScroll: false })
        }
        onBack={drawer.close}
      />
    </Drawer>
  )
}

// Deploy slides over the fleet like the app panel, so the canvas behind keeps
// saying how everything else is doing while a new app comes up. Wider than the
// app panel, to hold the live build's split view.
function DeployOverlay({ space, name }: { space: string; name: string }) {
  const navigate = useNavigate()
  const drawer = useRouteDrawer()

  return (
    <Drawer {...drawer} onClose={drawer.close} label="Deploy an app" width="max-w-4xl">
      <PaneHeader
        prominent
        eyebrow={name ? `Redeploy · ${space}` : space ? `Deploy into ${space}` : 'Deploy'}
        title={name || 'New app'}
        onClose={drawer.close}
        closeLabel="Close deploy"
      />
      <DrawerBody>
        <DeployView
          space={space}
          name={name}
          onDeployed={(s, n) =>
            navigate({
              to: '/app/$space/$name/{-$tab}',
              params: { space: s, name: n, tab: undefined },
              resetScroll: false,
            })
          }
        />
      </DrawerBody>
    </Drawer>
  )
}

// Code-style routes: the whole map in one place, typed params included.
// Titles live on the routes (HeadContent renders them); loaders warm the
// query cache so a view opens with its data already there.
const rootRoute = createRootRouteWithContext<{ queryClient: QueryClient }>()({
  component: Shell,
  head: () => ({ meta: [{ title: pageTitle('Apps') }] }),
})

// Pathless layout: it has no path of its own, so it stays mounted for both
// the bare fleet ('/') and an open app panel ('/app/…') — that persistence
// is the whole point (see FleetLayout).
const fleetRoute = createRoute({
  getParentRoute: () => rootRoute,
  id: 'fleet',
  component: FleetLayout,
  loader: ({ context }) => context.queryClient.ensureQueryData(fleetQuery),
})

const fleetIndexRoute = createRoute({
  getParentRoute: () => fleetRoute,
  path: '/',
  component: () => null,
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
  component: SystemView,
  head: () => ({ meta: [{ title: pageTitle('System') }] }),
})

const spaceRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/space/$space',
  loader: ({ context }) => context.queryClient.ensureQueryData(fleetQuery),
  head: ({ params }) => ({ meta: [{ title: pageTitle(params.space) }] }),
  component: () => {
    const { space } = spaceRoute.useParams()
    return <SpacePage space={space} />
  },
})

// Deploy overlays the fleet in three shapes: space-less (the palette's
// entry — the space is chosen in the review), into a named space (a space's
// Add menu), and a redeploy of a specific app. All slide over the fleet, so
// they hang off the fleet layout.
const deployRoute = createRoute({
  getParentRoute: () => fleetRoute,
  path: '/deploy',
  component: () => <DeployOverlay space="" name="" />,
  head: () => ({ meta: [{ title: pageTitle('Deploy') }] }),
})

const deploySpaceRoute = createRoute({
  getParentRoute: () => fleetRoute,
  path: '/deploy/$space',
  component: () => {
    const { space } = deploySpaceRoute.useParams()
    return <DeployOverlay space={space} name="" />
  },
  head: () => ({ meta: [{ title: pageTitle('Deploy') }] }),
})

const deployAppRoute = createRoute({
  getParentRoute: () => fleetRoute,
  path: '/deploy/$space/$name',
  component: () => {
    const { space, name } = deployAppRoute.useParams()
    return <DeployOverlay space={space} name={name} />
  },
  head: () => ({ meta: [{ title: pageTitle('Deploy') }] }),
})

// One route for the panel and every one of its tabs ({-$tab} is optional,
// so old "/app/space/name" links still land on the overview). Crossing tabs
// stays inside this route — nothing above it re-renders, nothing reloads.
const appRoute = createRoute({
  getParentRoute: () => fleetRoute,
  path: '/app/$space/$name/{-$tab}',
  loader: ({ context, params }) =>
    context.queryClient.ensureQueryData(statusQuery(params.space, params.name)),
  head: ({ params }) => ({ meta: [{ title: pageTitle(params.name) }] }),
  component: () => {
    const { space, name, tab } = appRoute.useParams()
    // The tab comes from the URL, so it can name something that doesn't
    // exist — fall back rather than render an empty panel.
    const known = APP_TABS.find((t) => t === tab) ?? 'overview'
    return <AppOverlay space={space} name={name} tab={known} />
  },
})

const routeTree = rootRoute.addChildren([
  fleetRoute.addChildren([
    fleetIndexRoute,
    appRoute,
    deployRoute,
    deploySpaceRoute,
    deployAppRoute,
  ]),
  legacyAppsRoute,
  systemRoute,
  spaceRoute,
])

const router = createRouter({
  routeTree,
  history: createHashHistory(),
  context: { queryClient },
  // Hovering a card warms its data through the loader; staleness is
  // Query's business, so the router's own preload timer is switched off.
  defaultPreload: 'intent',
  defaultPreloadStaleTime: 0,
  // An unknown hash is an old or mistyped link — the fleet is always a
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
