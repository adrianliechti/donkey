import { useEffect, useState } from 'react'
import { HardDrive, Plug } from 'lucide-react'
import { ago, call, duration } from './api'
import type { Stats } from './api'
import { BarRow, TrendLine } from './charts'
import { Card, Hint, PriorityChip, Stat } from './ui'
import type { Priority } from './api'

interface Activity {
  at: string
  ticket: number | null
  text: string
}

const PRIORITY_ORDER: Priority[] = ['urgent', 'high', 'normal', 'low']
// Priority is a state, so its bars wear the status palette — always beside
// the labeled chip, never color alone.
const priorityBar: Record<Priority, string> = {
  urgent: 'bg-critical',
  high: 'bg-serious',
  normal: 'bg-muted',
  low: 'bg-line',
}

export function Overview({ refresh }: { refresh: number }) {
  const [stats, setStats] = useState<Stats | null>(null)
  const [activity, setActivity] = useState<Activity[]>([])
  const [hint, setHint] = useState('')
  const [error, setError] = useState('')

  useEffect(() => {
    call<Stats>('/api/stats')
      .then((r) => {
        setHint(r.hint ?? '')
        if (r.data) setStats(r.data)
      })
      .catch((err) => setError(err instanceof Error ? err.message : 'The backend did not answer.'))
    call<Activity[]>('/api/activity')
      .then((r) => setActivity(r.data ?? []))
      .catch(() => {})
  }, [refresh])

  if (error) {
    return (
      <p role="alert" className="mx-auto max-w-xl px-5 py-10 text-sm">
        {error}
      </p>
    )
  }
  if (hint) {
    return (
      <div className="mx-auto max-w-xl px-5 py-10">
        <Hint text={hint} />
      </div>
    )
  }
  if (!stats) return <p className="mx-auto max-w-5xl px-5 py-10 text-sm text-muted">Counting tickets…</p>

  const maxCategory = Math.max(0, ...stats.categories.map((c) => c.total))
  const maxPriority = Math.max(1, ...PRIORITY_ORDER.map((p) => stats.priorities[p] ?? 0))

  return (
    <main className="mx-auto max-w-5xl space-y-5 px-5 py-6">
      <section className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
        <Stat label="Open tickets" value={String(stats.open)} />
        <Stat label="Needs attention" value={String(stats.attention)} detail="urgent and high, still open" />
        <Stat label="New this week" value={String(stats.new_this_week)} />
        <Stat
          label="Median time to resolution"
          value={stats.median_resolution_seconds ? duration(stats.median_resolution_seconds) : '—'}
          detail="tickets closed in the last 30 days"
        />
      </section>

      <Card title="New tickets per week">
        <TrendLine points={stats.weekly} />
      </Card>

      <section className="grid gap-5 lg:grid-cols-2">
        <Card title="Tickets by category — all time">
          {stats.categories.map((c) => (
            <BarRow key={c.category} label={c.category} value={c.total} max={maxCategory} />
          ))}
        </Card>
        <Card title="Open tickets by priority">
          <div className="space-y-1">
            {PRIORITY_ORDER.map((p) => {
              const n = stats.priorities[p] ?? 0
              return (
                <div key={p} className="grid grid-cols-[6rem_1fr_3rem] items-center gap-3 py-1.5">
                  <PriorityChip value={p} />
                  <div className="h-4">
                    <div
                      className={`h-4 rounded-r-[4px] ${priorityBar[p]} transition-[width] duration-500`}
                      style={{ width: `${Math.max((n / maxPriority) * 100, 1.5)}%` }}
                      role="img"
                      aria-label={`${p}: ${n} open`}
                    />
                  </div>
                  <span className="text-right text-sm tabular-nums">{n}</span>
                </div>
              )
            })}
          </div>
        </Card>
      </section>

      <section className="grid gap-5 lg:grid-cols-2">
        <Card title="Recent activity">
          <p className="mb-3 flex items-center gap-1.5 text-xs text-muted">
            <HardDrive className="size-3.5" aria-hidden />
            An append-only log on the app's own disk — it survives every redeploy.
          </p>
          <ul className="space-y-2">
            {activity.map((a, i) => (
              <li key={i} className="flex items-baseline justify-between gap-3 border-b border-line/60 pb-2 text-sm last:border-0">
                <span className="min-w-0 break-words">{a.text}</span>
                <span className="shrink-0 text-xs text-muted">{ago(a.at)}</span>
              </li>
            ))}
            {activity.length === 0 && <li className="text-sm text-muted">Nothing yet — the desk just woke up.</li>}
          </ul>
        </Card>
        <Card title="The same desk, for an assistant">
          <p className="text-sm leading-relaxed text-ink-soft">
            The backend also speaks <span className="font-medium">MCP</span>, so a model can work the queue
            like any other agent: <code className="font-mono text-xs">tickets_queue</code>,{' '}
            <code className="font-mono text-xs">ticket_read</code>,{' '}
            <code className="font-mono text-xs">ticket_reply</code>,{' '}
            <code className="font-mono text-xs">ticket_triage</code>,{' '}
            <code className="font-mono text-xs">attachment_read</code> and{' '}
            <code className="font-mono text-xs">desk_overview</code> — the same cores this dashboard calls,
            so both faces always agree.
          </p>
          <p className="mt-3 flex items-center gap-1.5 text-sm text-muted">
            <Plug className="size-4 shrink-0" aria-hidden />
            Point your MCP client at <code className="font-mono text-xs">/mcp</code> on this address.
          </p>
        </Card>
      </section>
    </main>
  )
}
