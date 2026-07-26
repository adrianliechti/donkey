import { useEffect, useState } from 'react'
import {
  createHashHistory,
  createRootRoute,
  createRoute,
  createRouter,
  Outlet,
  redirect,
  RouterProvider,
  useNavigate,
  useParams,
} from '@tanstack/react-router'
import { Activity, UploadCloud } from 'lucide-react'
import { clearToken, getToken, setUnauthorizedHandler } from './api'
import { AppDetail, type AppTab } from './AppDetail'
import { AppsList } from './AppsList'
import { DeployView } from './DeployView'
import { Login } from './Login'
import { SystemView } from './SystemView'
import { UserMenu } from './UserMenu'

// The routes live in the hash (#/app/acme/shop/logs) so refresh keeps your
// place, an unhealthy app can be linked to a teammate, and bookmarks from
// the previous console keep working.
const APP_TABS: AppTab[] = ['overview', 'logs', 'files', 'variables', 'settings']

// Signing out is triggered inside the router's tree but owned by App (it
// holds the connected state) — this hook is the one thread between them.
let signOut = () => {}

function Shell() {
  const navigate = useNavigate()
  return (
    <div className="flex min-h-screen flex-col">
      <header className="sticky top-0 z-30 border-b border-line bg-card/95 backdrop-blur">
        <div className="mx-auto flex max-w-6xl items-center justify-between gap-4 px-4 py-2.5">
          <div className="flex min-w-0 items-center gap-3">
            <button
              onClick={() => navigate({ to: '/' })}
              className="flex shrink-0 items-center gap-2 font-mono text-base font-bold tracking-tight focus-visible:outline-2 focus-visible:outline-hay-deep"
            >
              <span className="text-xl" aria-hidden>
                🫏
              </span>
              donkey
            </button>
            <Breadcrumb />
          </div>
          <div className="flex shrink-0 items-center gap-1.5">
            <button
              onClick={() => navigate({ to: '/system' })}
              className="flex items-center gap-1.5 rounded-md px-2.5 py-1.5 text-sm text-ink-soft transition-colors hover:bg-raised focus-visible:outline-2 focus-visible:outline-hay-deep"
            >
              <Activity className="size-4" aria-hidden /> System
            </button>
            <button
              onClick={() => navigate({ to: '/deploy' })}
              className="flex items-center gap-1.5 rounded-md bg-hay px-3 py-1.5 text-sm font-semibold text-on-hay transition-colors hover:bg-hay-deep focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-hay-deep"
            >
              <UploadCloud className="size-4" aria-hidden /> Deploy
            </button>
            <UserMenu
              onSignOut={() => {
                clearToken()
                signOut()
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

function useTitle(title: string) {
  useEffect(() => {
    document.title = title
  }, [title])
}

function FleetPage() {
  useTitle('Donkey — admin')
  const navigate = useNavigate()
  return (
    <AppsList
      onOpen={(space, name) => navigate({ to: '/app/$space/$name', params: { space, name } })}
    />
  )
}

// The app panel slides over the fleet instead of replacing it — the canvas
// behind it keeps saying how everything else is doing.
function AppOverlay({ space, name, tab }: { space: string; name: string; tab: AppTab }) {
  useTitle(`${name} · Donkey`)
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
      <AppsList onOpen={(s, n) => navigate({ to: '/app/$space/$name', params: { space: s, name: n } })} />
      <div className="fixed inset-0 z-40 bg-black/50" onClick={close} aria-hidden />
      <aside
        role="dialog"
        aria-modal="true"
        aria-label={`${name} — app details`}
        className="fixed inset-y-0 right-0 z-50 flex w-full max-w-3xl flex-col overflow-y-auto border-l border-line bg-card shadow-[-24px_0_60px_rgba(0,0,0,.5)]"
      >
        <AppDetail
          space={space}
          app={name}
          tab={tab}
          onTab={(next) =>
            next === 'overview'
              ? navigate({ to: '/app/$space/$name', params: { space, name } })
              : navigate({ to: '/app/$space/$name/$tab', params: { space, name, tab: next } })
          }
          onBack={close}
        />
      </aside>
    </>
  )
}

function SystemPage() {
  useTitle('System · Donkey')
  return <SystemView />
}

function DeployPage({ space, name }: { space: string; name: string }) {
  useTitle('Deploy · Donkey')
  const navigate = useNavigate()
  return (
    <DeployView
      space={space}
      name={name}
      onDeployed={(s, n) => navigate({ to: '/app/$space/$name', params: { space: s, name: n } })}
    />
  )
}

// Code-style routes: the whole map in one place, typed params included.
const rootRoute = createRootRoute({ component: Shell })

const fleetRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/',
  component: FleetPage,
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
  component: SystemPage,
})

const deployRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/deploy',
  component: () => <DeployPage space="" name="" />,
})

const deployAppRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/deploy/$space/$name',
  component: () => {
    const { space, name } = deployAppRoute.useParams()
    return <DeployPage space={space} name={name} />
  },
})

const appRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/app/$space/$name',
  component: () => {
    const { space, name } = appRoute.useParams()
    return <AppOverlay space={space} name={name} tab="overview" />
  },
})

const appTabRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/app/$space/$name/$tab',
  component: () => {
    const { space, name, tab } = appTabRoute.useParams()
    // The tab comes from the URL, so it can name something that doesn't
    // exist — fall back rather than render an empty panel.
    const known = APP_TABS.find((t) => t === tab) ?? 'overview'
    return <AppOverlay space={space} name={name} tab={known} />
  },
})

const routeTree = rootRoute.addChildren([
  fleetRoute,
  legacyAppsRoute,
  systemRoute,
  deployRoute,
  deployAppRoute,
  appRoute,
  appTabRoute,
])

const router = createRouter({
  routeTree,
  history: createHashHistory(),
  defaultNotFoundComponent: () => {
    // An unknown hash is an old or mistyped link — the fleet is always a
    // sensible place to land.
    location.hash = '#/'
    return null
  },
})

declare module '@tanstack/react-router' {
  interface Register {
    router: typeof router
  }
}

export default function App() {
  const [connected, setConnected] = useState(() => getToken() !== '')

  useEffect(() => {
    // When the token stops being accepted, return to the login screen
    // instead of leaving every panel polling a dead session.
    setUnauthorizedHandler(() => setConnected(false))
    signOut = () => setConnected(false)
  }, [])

  if (!connected) {
    return <Login onConnected={() => setConnected(true)} />
  }
  return <RouterProvider router={router} />
}
