import { useState } from 'react'
import { ChartNoAxesColumn, Inbox as InboxIcon, PackageOpen, Sparkles } from 'lucide-react'
import { post } from './api'
import { Inbox } from './inbox'
import { Overview } from './overview'
import { primaryButton } from './ui'

// The shell: one header, two views, and the demo's favorite button — a
// simulated customer walks in, the AI triages them, and the inbox shows the
// result. Everything else lives in inbox.tsx and overview.tsx.

export function App() {
  const [view, setView] = useState<'inbox' | 'overview'>('inbox')
  const [focus, setFocus] = useState<number | null>(null)
  const [refresh, setRefresh] = useState(0)
  const [simulating, setSimulating] = useState(false)

  async function simulate() {
    setSimulating(true)
    try {
      const r = await post<{ id: number }>('/api/simulate')
      if (r.data) {
        setFocus(r.data.id)
        setView('inbox')
        setRefresh((n) => n + 1)
      }
    } finally {
      setSimulating(false)
    }
  }

  return (
    <div className="min-h-screen">
      <header className="sticky top-0 z-10 h-14 border-b border-line bg-card/85 backdrop-blur">
        <div className="mx-auto flex h-full max-w-7xl items-center gap-3 px-4">
          <div className="flex items-center gap-2">
            <span className="grid size-7 place-items-center rounded-lg bg-accent text-white">
              <PackageOpen className="size-4" aria-hidden />
            </span>
            <span className="text-sm font-semibold tracking-tight">
              Packmule <span className="font-normal text-muted">Support</span>
            </span>
          </div>
          <nav className="ml-2 flex gap-1" aria-label="View">
            {(
              [
                ['inbox', 'Inbox', InboxIcon],
                ['overview', 'Overview', ChartNoAxesColumn],
              ] as const
            ).map(([key, label, Icon]) => (
              <button
                key={key}
                onClick={() => setView(key)}
                aria-current={view === key ? 'page' : undefined}
                className={`flex items-center gap-1.5 rounded-md px-2.5 py-1.5 text-sm transition-colors ${
                  view === key ? 'bg-accent-soft font-medium text-ink' : 'text-muted hover:text-ink-soft'
                }`}
              >
                <Icon className="size-4" aria-hidden />
                <span className="hidden sm:inline">{label}</span>
              </button>
            ))}
          </nav>
          <span className="flex-1" />
          <button onClick={simulate} disabled={simulating} className={primaryButton}>
            <Sparkles className={`size-3.5 ${simulating ? 'animate-pulse' : ''}`} aria-hidden />
            <span className="hidden sm:inline">{simulating ? 'A customer walks in…' : 'Simulate a ticket'}</span>
            <span className="sm:hidden">{simulating ? '…' : 'Simulate'}</span>
          </button>
        </div>
      </header>
      {view === 'inbox' ? <Inbox focus={focus} refresh={refresh} /> : <Overview refresh={refresh} />}
    </div>
  )
}
