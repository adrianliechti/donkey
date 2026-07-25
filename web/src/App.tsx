import { useEffect, useState } from 'react'
import { Boxes, LogOut, Server, UploadCloud } from 'lucide-react'
import { clearToken, getToken, setUnauthorizedHandler } from './api'
import { AppDetail, type AppTab } from './AppDetail'
import { AppsList } from './AppsList'
import { DeployView } from './DeployView'
import { Login } from './Login'
import { SystemView } from './SystemView'

type View =
  | { page: 'apps' }
  | { page: 'system' }
  | { page: 'app'; space: string; name: string; tab: AppTab }
  | { page: 'deploy'; space: string; name: string }

const APP_TABS: AppTab[] = ['overview', 'logs', 'files', 'settings']

// The view lives in the URL hash so refresh keeps your place and an
// unhealthy app can be linked to a teammate.
function parseHash(): View {
  if (location.hash === '#/system') return { page: 'system' }
  // The tab rides in the URL too, so "look at this app's logs" is a link.
  const m = location.hash.match(/^#\/app\/([^/]+)\/([^/]+)(?:\/([^/]+))?$/)
  if (m) {
    const tab = APP_TABS.find((t) => t === m[3]) ?? 'overview'
    return { page: 'app', space: decodeURIComponent(m[1]), name: decodeURIComponent(m[2]), tab }
  }
  const d = location.hash.match(/^#\/deploy(?:\/([^/]+)\/([^/]+))?$/)
  if (d) {
    return {
      page: 'deploy',
      space: d[1] ? decodeURIComponent(d[1]) : '',
      name: d[2] ? decodeURIComponent(d[2]) : '',
    }
  }
  return { page: 'apps' }
}

function hashFor(view: View): string {
  switch (view.page) {
    case 'system':
      return '#/system'
    case 'app': {
      const base = `#/app/${encodeURIComponent(view.space)}/${encodeURIComponent(view.name)}`
      return view.tab === 'overview' ? base : `${base}/${view.tab}`
    }
    case 'deploy':
      return view.name
        ? `#/deploy/${encodeURIComponent(view.space)}/${encodeURIComponent(view.name)}`
        : '#/deploy'
    default:
      return '#/apps'
  }
}

export function navigate(view: View) {
  location.hash = hashFor(view)
}

export default function App() {
  const [connected, setConnected] = useState(() => getToken() !== '')
  const [view, setView] = useState<View>(parseHash)

  useEffect(() => {
    const onHash = () => setView(parseHash())
    window.addEventListener('hashchange', onHash)
    // When the token stops being accepted, return to the login screen
    // instead of leaving every panel polling a dead session.
    setUnauthorizedHandler(() => setConnected(false))
    return () => window.removeEventListener('hashchange', onHash)
  }, [])

  useEffect(() => {
    document.title =
      view.page === 'app'
        ? `${view.name} · Donkey`
        : view.page === 'system'
          ? 'System · Donkey'
          : view.page === 'deploy'
            ? 'Deploy · Donkey'
            : 'Donkey — admin'
  }, [view])

  if (!connected) {
    return <Login onConnected={() => setConnected(true)} />
  }

  const tab = (page: 'apps' | 'system', label: string, icon: React.ReactNode) => {
    const active = view.page === page || (page === 'apps' && view.page === 'app')
    return (
      <button
        onClick={() => navigate({ page })}
        aria-current={active ? 'page' : undefined}
        className={`flex items-center gap-1.5 rounded-md px-3 py-1.5 text-sm transition-colors focus-visible:outline-2 focus-visible:outline-hay-deep ${
          active ? 'bg-ink text-surface' : 'text-ink-soft hover:bg-line/60'
        }`}
      >
        {icon}
        {label}
      </button>
    )
  }

  return (
    <div className="min-h-screen">
      <header className="border-b border-line bg-card">
        <div className="mx-auto flex max-w-5xl items-center justify-between gap-4 px-4 py-3">
          <div className="flex items-center gap-6">
            <button
              onClick={() => navigate({ page: 'apps' })}
              className="flex items-center gap-2 font-mono text-lg font-bold tracking-tight focus-visible:outline-2 focus-visible:outline-hay-deep"
            >
              <span aria-hidden>🫏</span> Donkey
            </button>
            <nav className="flex items-center gap-1">
              {tab('apps', 'Apps', <Boxes className="size-4" aria-hidden />)}
              {tab('system', 'System', <Server className="size-4" aria-hidden />)}
            </nav>
          </div>
          <div className="flex items-center gap-2">
            <button
              onClick={() => navigate({ page: 'deploy', space: '', name: '' })}
              className="flex items-center gap-1.5 rounded-md bg-hay px-3 py-1.5 text-sm font-semibold text-on-hay transition-colors hover:bg-hay-deep hover:text-white focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-hay-deep"
            >
              <UploadCloud className="size-4" aria-hidden /> Deploy
            </button>
            <button
              onClick={() => {
                clearToken()
                setConnected(false)
              }}
              className="flex items-center gap-1.5 rounded-md px-2 py-1.5 text-sm text-stone hover:bg-line/60 hover:text-ink focus-visible:outline-2 focus-visible:outline-hay-deep"
            >
              <LogOut className="size-4" aria-hidden /> Sign out
            </button>
          </div>
        </div>
      </header>
      <main className="mx-auto max-w-5xl px-4 py-8">
        {view.page === 'apps' && (
          <AppsList onOpen={(space, name) => navigate({ page: 'app', space, name, tab: 'overview' })} />
        )}
        {view.page === 'system' && <SystemView />}
        {view.page === 'app' && (
          <AppDetail
            space={view.space}
            app={view.name}
            tab={view.tab}
            onTab={(tab) => navigate({ page: 'app', space: view.space, name: view.name, tab })}
            onBack={() => navigate({ page: 'apps' })}
          />
        )}
        {view.page === 'deploy' && (
          <DeployView
            space={view.space}
            name={view.name}
            onDeployed={(space, name) => navigate({ page: 'app', space, name, tab: 'overview' })}
          />
        )}
      </main>
    </div>
  )
}
