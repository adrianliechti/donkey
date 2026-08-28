import { useEffect } from 'react'
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
  useParams,
} from '@tanstack/react-router'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { Activity, UploadCloud } from 'lucide-react'
import { setUnauthorizedHandler } from './api'
import { AppDetail, type AppTab } from './AppDetail'
import { AppsList } from './AppsList'
import { DeployView } from './DeployView'
import { fleetQuery, statusQuery } from './queries'
import { SystemView } from './SystemView'
import { DonkeyLogo } from './ui'
import { UserMenu } from './UserMenu'

// The routes live in the hash (#/app/acme/shop/logs) so refresh keeps your
// place, an unhealthy app can be linked to a teammate, and bookmarks from
// the previous console keep working.
const APP_TABS: AppTab[] = ['overview', 'traffic', 'logs', 'files', 'variables', 'settings']

// One cache for the whole console. Short staleness keeps panels honest
// while still letting a remounted view show what it knew instantly. The
// router carries the client in its context, so route loaders can warm the
// cache before a view renders.
const queryClient = new QueryClient({
  defaultOptions: { queries: { retry: 1, staleTime: 5_000 } },
})

function Shell() {
  const navigate = useNavigate()
  return (
    <div className="flex min-h-screen flex-col">
      <HeadContent />
      <header className="sticky top-0 z-30 border-b border-line bg-card/95 backdrop-blur">
        <div className="mx-auto flex max-w-6xl items-center justify-between gap-4 px-4 py-2.5">
          <div className="flex min-w-0 items-center gap-3">
            <button
              onClick={() => navigate({ to: '/' })}
              className="flex shrink-0 items-center gap-2 font-mono text-base font-bold tracking-tight focus-visible:outline-2 focus-visible:outline-hay-deep"
            >
              <DonkeyLogo className="size-5 text-white" />
              Donkey
            </button>
            <Breadcrumb />
          </div>
          <div className="flex shrink-0 items-center gap-1.5">
            <button
              onClick={() => navigate({ to: '/deploy' })}
              className="flex items-center gap-1.5 rounded-md bg-hay px-3 py-1.5 text-sm font-semibold text-on-hay transition-colors hover:bg-hay-deep focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-hay-deep"
            >
              <UploadCloud className="size-4" aria-hidden /> Deploy
            </button>
            <button
              type="button"
              title="System health"
              aria-label="System health"
              onClick={() => navigate({ to: '/system' })}
              className="flex items-center rounded-md p-1.5 text-stone hover:bg-line/60 hover:text-ink focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-hay-deep"
            >
              <Activity className="size-5" aria-hidden />
            </button>
            <UserMenu
              onSignOut={() => {
                // The gateway owns the session — its logout page clears the
                // cookie for the console and every app at once.
                window.location.href = '/oauth2/logout'
              }}
            />
          </div>
        </div>
      </header>

      {/* The paddock: the fleet lives on a dotted field. */}
      <main className="dotgrid flex-1">
        <div className="mx-auto max-w-6xl px-4 py-8">
          <Outlet />
        </div>
      </main>
    </div>
  )
}

// Breadcrumb says where you are: fleet, or fleet / space / app.
function Breadcrumb() {
  const params = useParams({ strict: false }) as { space?: string; name?: string }
  if (!params.space || !params.name) return null
  return (
    <span className="flex min-w-0 items-center gap-1.5 font-mono text-sm text-stone">
      <span aria-hidden>/</span>
      <span className="truncate text-ink-soft">{params.space}</span>
      <span aria-hidden>/</span>
      <span className="truncate font-semibold text-ink">{params.name}</span>
    </span>
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

// The app panel slides over the fleet instead of replacing it — the canvas
// behind it keeps saying how everything else is doing.
function AppOverlay({ space, name, tab }: { space: string; name: string; tab: AppTab }) {
  const navigate = useNavigate()
  const close = () => navigate({ to: '/' })

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') close()
    }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  return (
    <>
      <div className="fixed inset-0 z-40 bg-black/50" onClick={close} aria-hidden />
      <aside
        role="dialog"
        aria-modal="true"
        aria-label={`${name} — app details`}
        className="fixed inset-y-0 right-0 z-50 flex w-full max-w-3xl flex-col overflow-y-auto border-l border-line bg-card shadow-[-24px_0_60px_rgba(0,0,0,.5)]"
      >
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
            })
          }
          onBack={close}
        />
      </aside>
    </>
  )
}

function DeployPage({ space, name }: { space: string; name: string }) {
  const navigate = useNavigate()
  return (
    <DeployView
      space={space}
      name={name}
      onDeployed={(s, n) =>
        navigate({ to: '/app/$space/$name/{-$tab}', params: { space: s, name: n, tab: undefined } })
      }
    />
  )
}

// Code-style routes: the whole map in one place, typed params included.
// Titles live on the routes (HeadContent renders them); loaders warm the
// query cache so a view opens with its data already there.
const rootRoute = createRootRouteWithContext<{ queryClient: QueryClient }>()({
  component: Shell,
  head: () => ({ meta: [{ title: 'Donkey — admin' }] }),
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
  head: () => ({ meta: [{ title: 'System · Donkey' }] }),
})

const deployRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/deploy',
  component: () => <DeployPage space="" name="" />,
  head: () => ({ meta: [{ title: 'Deploy · Donkey' }] }),
})

const deployAppRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/deploy/$space/$name',
  component: () => {
    const { space, name } = deployAppRoute.useParams()
    return <DeployPage space={space} name={name} />
  },
  head: () => ({ meta: [{ title: 'Deploy · Donkey' }] }),
})

// One route for the panel and every one of its tabs ({-$tab} is optional,
// so old "/app/space/name" links still land on the overview). Crossing tabs
// stays inside this route — nothing above it re-renders, nothing reloads.
const appRoute = createRoute({
  getParentRoute: () => fleetRoute,
  path: '/app/$space/$name/{-$tab}',
  loader: ({ context, params }) =>
    context.queryClient.ensureQueryData(statusQuery(params.space, params.name)),
  head: ({ params }) => ({ meta: [{ title: `${params.name} · Donkey` }] }),
  component: () => {
    const { space, name, tab } = appRoute.useParams()
    // The tab comes from the URL, so it can name something that doesn't
    // exist — fall back rather than render an empty panel.
    const known = APP_TABS.find((t) => t === tab) ?? 'overview'
    return <AppOverlay space={space} name={name} tab={known} />
  },
})

const routeTree = rootRoute.addChildren([
  fleetRoute.addChildren([fleetIndexRoute, appRoute]),
  legacyAppsRoute,
  systemRoute,
  deployRoute,
  deployAppRoute,
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
