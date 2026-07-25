import { useState } from 'react'
import { Boxes, LogOut, Server } from 'lucide-react'
import { clearToken, getToken } from './api'
import { AppDetail } from './AppDetail'
import { AppsList } from './AppsList'
import { Login } from './Login'
import { SystemView } from './SystemView'

type View = { page: 'apps' } | { page: 'system' } | { page: 'app'; space: string; name: string }

export default function App() {
  const [connected, setConnected] = useState(() => getToken() !== '')
  const [view, setView] = useState<View>({ page: 'apps' })

  if (!connected) {
    return <Login onConnected={() => setConnected(true)} />
  }

  const tab = (page: 'apps' | 'system', label: string, icon: React.ReactNode) => (
    <button
      onClick={() => setView({ page })}
      className={`flex items-center gap-1.5 rounded-md px-3 py-1.5 text-sm transition-colors focus-visible:outline-2 focus-visible:outline-hay-deep ${
        view.page === page || (page === 'apps' && view.page === 'app')
          ? 'bg-ink text-surface'
          : 'text-ink-soft hover:bg-line/60'
      }`}
    >
      {icon}
      {label}
    </button>
  )

  return (
    <div className="min-h-screen">
      <header className="border-b border-line bg-card">
        <div className="mx-auto flex max-w-5xl items-center justify-between gap-4 px-4 py-3">
          <div className="flex items-center gap-6">
            <button
              onClick={() => setView({ page: 'apps' })}
              className="flex items-center gap-2 font-mono text-lg font-bold tracking-tight focus-visible:outline-2 focus-visible:outline-hay-deep"
            >
              <span aria-hidden>🫏</span> donkey
            </button>
            <nav className="flex items-center gap-1">
              {tab('apps', 'Apps', <Boxes className="size-4" aria-hidden />)}
              {tab('system', 'System', <Server className="size-4" aria-hidden />)}
            </nav>
          </div>
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
      </header>
      <main className="mx-auto max-w-5xl px-4 py-8">
        {view.page === 'apps' && (
          <AppsList onOpen={(space, name) => setView({ page: 'app', space, name })} />
        )}
        {view.page === 'system' && <SystemView />}
        {view.page === 'app' && (
          <AppDetail space={view.space} app={view.name} onBack={() => setView({ page: 'apps' })} />
        )}
      </main>
    </div>
  )
}
