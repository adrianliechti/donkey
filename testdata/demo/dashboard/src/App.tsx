import { useEffect, useState } from 'react'
import { Boxes, Database, Plug, RefreshCw, TrendingUp } from 'lucide-react'
import { BarRow, TrendLine } from './charts'

interface Summary {
  orders: number
  revenue: number
  average_order: number
  units: number
}
interface RegionRow {
  region: string
  revenue: number
  orders: number
}
interface MonthRow {
  month: string
  revenue: number
}
interface Order {
  id: number
  ordered_at: string
  region: string
  product: string
  units: number
  revenue: number
}

async function get<T>(path: string): Promise<T> {
  const res = await fetch(path)
  if (!res.ok) throw new Error(`${path} answered ${res.status}`)
  return res.json() as Promise<T>
}

const money = (n: number) => `$${n.toLocaleString('en-US', { maximumFractionDigits: 0 })}`

export function App() {
  const [summary, setSummary] = useState<Summary | null>(null)
  const [regions, setRegions] = useState<RegionRow[]>([])
  const [months, setMonths] = useState<MonthRow[]>([])
  const [orders, setOrders] = useState<Order[]>([])
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  async function load() {
    setBusy(true)
    try {
      const [s, r, m, o] = await Promise.all([
        get<Summary>('/api/summary'),
        get<RegionRow[]>('/api/revenue-by-region'),
        get<MonthRow[]>('/api/revenue-by-month'),
        get<Order[]>('/api/orders?limit=12'),
      ])
      setSummary(s)
      setRegions(r)
      setMonths(m)
      setOrders(o)
      setError('')
    } catch (err) {
      setError(err instanceof Error ? err.message : 'The backend did not answer.')
    } finally {
      setBusy(false)
    }
  }

  useEffect(() => {
    load()
  }, [])

  const maxRegion = Math.max(0, ...regions.map((r) => r.revenue))

  return (
    <div className="min-h-screen">
      <header className="border-b border-line bg-card">
        <div className="mx-auto flex max-w-5xl flex-wrap items-center justify-between gap-3 px-5 py-4">
          <div>
            <h1 className="text-lg font-semibold tracking-tight">Sales</h1>
            <p className="mt-0.5 flex items-center gap-1.5 text-xs text-muted">
              <Database className="size-3.5" aria-hidden />
              Read live from DuckDB — the same numbers the MCP tools answer with.
            </p>
          </div>
          <button
            onClick={load}
            className="flex items-center gap-1.5 rounded-md border border-line px-3 py-1.5 text-sm text-ink-soft transition-colors hover:bg-line/50 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-data"
          >
            <RefreshCw className={`size-3.5 ${busy ? 'animate-spin' : ''}`} aria-hidden />
            Refresh
          </button>
        </div>
      </header>

      <main className="mx-auto max-w-5xl space-y-5 px-5 py-6">
        {error && (
          <p role="alert" className="rounded-md border border-line bg-card px-3 py-2 text-sm">
            {error}
          </p>
        )}

        {/* Headline numbers are not charts. */}
        <section className="grid gap-4 sm:grid-cols-3">
          <Stat label="Revenue, all time" value={summary ? money(summary.revenue) : '—'} icon={<TrendingUp className="size-4" aria-hidden />} />
          <Stat label="Orders" value={summary ? summary.orders.toLocaleString('en-US') : '—'} icon={<Boxes className="size-4" aria-hidden />} />
          <Stat label="Average order" value={summary ? money(summary.average_order) : '—'} icon={<TrendingUp className="size-4" aria-hidden />} />
        </section>

        <section className="grid gap-5 lg:grid-cols-2">
          <Card title="Revenue by region">
            {regions.map((r) => (
              <BarRow key={r.region} label={r.region} value={r.revenue} max={maxRegion} />
            ))}
          </Card>
          <Card title="Revenue per month">
            <TrendLine points={months} />
          </Card>
        </section>

        <Card title="Latest orders">
          <div className="-mx-1 overflow-x-auto">
            <table className="w-full min-w-lg text-sm">
              <thead>
                <tr className="border-b border-line text-left text-xs text-muted">
                  <th className="py-2 pl-1 font-medium">Date</th>
                  <th className="py-2 font-medium">Region</th>
                  <th className="py-2 font-medium">Product</th>
                  <th className="py-2 text-right font-medium">Units</th>
                  <th className="py-2 pr-1 text-right font-medium">Revenue</th>
                </tr>
              </thead>
              <tbody>
                {orders.map((o) => (
                  <tr key={o.id} className="border-b border-line/60 last:border-0">
                    <td className="py-1.5 pl-1 font-mono text-xs text-muted">{o.ordered_at}</td>
                    <td className="py-1.5">{o.region}</td>
                    <td className="py-1.5">{o.product}</td>
                    <td className="py-1.5 text-right font-mono tabular-nums">{o.units}</td>
                    <td className="py-1.5 pr-1 text-right font-mono tabular-nums">{money(o.revenue)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </Card>

        <Card title="Same data, for an assistant">
          <p className="text-sm text-ink-soft">
            The backend also speaks <span className="font-medium">MCP</span>, so a model can ask these
            questions itself — <code className="font-mono text-xs">sales_summary</code>,{' '}
            <code className="font-mono text-xs">revenue_by_region</code>,{' '}
            <code className="font-mono text-xs">top_products</code> and a read-only{' '}
            <code className="font-mono text-xs">run_sql</code>.
          </p>
          <p className="mt-2 flex items-center gap-1.5 text-sm text-muted">
            <Plug className="size-4 shrink-0" aria-hidden />
            Point your MCP client at <code className="font-mono text-xs">/mcp</code> on the backend.
          </p>
        </Card>
      </main>
    </div>
  )
}

function Stat({ label, value, icon }: { label: string; value: string; icon: React.ReactNode }) {
  return (
    <div className="rounded-lg border border-line bg-card p-4">
      <div className="flex items-center gap-1.5 text-xs text-muted">
        {icon}
        {label}
      </div>
      <div className="mt-1 font-mono text-2xl font-semibold tabular-nums tracking-tight">{value}</div>
    </div>
  )
}

function Card({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <section className="rounded-lg border border-line bg-card p-4">
      <h2 className="mb-2 text-xs font-semibold uppercase tracking-wider text-muted">{title}</h2>
      {children}
    </section>
  )
}
