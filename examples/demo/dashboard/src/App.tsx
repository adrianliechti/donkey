import { useEffect, useState } from 'react'
import { Boxes, Database, Download, Plug, RefreshCw, Sparkles, Trash2, TrendingUp, Upload } from 'lucide-react'
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
interface Note {
  id: number
  text: string
  created: string
}
interface FileRow {
  key: string
  size: number
  modified: string
}

async function get<T>(path: string): Promise<T> {
  const res = await fetch(path)
  if (!res.ok) throw new Error(`${path} answered ${res.status}`)
  return res.json() as Promise<T>
}

// The service-backed endpoints answer 503 with a hint while their addon is
// missing — that is a state to show, not an error to throw.
async function call<T>(path: string, init?: RequestInit): Promise<{ data?: T; hint?: string }> {
  const res = await fetch(path, init)
  if (res.status === 503) {
    const body = (await res.json().catch(() => null)) as { detail?: string } | null
    return { hint: body?.detail ?? 'This service is not connected yet.' }
  }
  if (!res.ok) throw new Error(`${path} answered ${res.status}`)
  return { data: (await res.json()) as T }
}

const money = (n: number) => `$${n.toLocaleString('en-US', { maximumFractionDigits: 0 })}`
const bytes = (n: number) =>
  n >= 1_048_576 ? `${(n / 1_048_576).toFixed(1)} MB` : n >= 1024 ? `${Math.round(n / 1024)} kB` : `${n} B`
const button =
  'rounded-md border border-line px-3 py-1.5 text-sm text-ink-soft transition-colors hover:bg-line/50 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-data'
// Demo rows earn no ceremony: the delete sits in each row, but stays quiet
// until the row is pointed at.
const rowDelete =
  'rounded p-0.5 text-muted opacity-0 transition-opacity hover:text-ink focus-visible:opacity-100 group-hover:opacity-100'

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

        {/* The space's attached services, doing real work: notes in postgres,
            files in rustfs, and the AI summary through the openai proxy. */}
        <section className="grid gap-5 lg:grid-cols-2">
          <NotesCard />
          <FilesCard />
        </section>

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

function NotesCard() {
  const [notes, setNotes] = useState<Note[]>([])
  const [hint, setHint] = useState('')
  const [draft, setDraft] = useState('')
  const [summary, setSummary] = useState('')
  const [aiHint, setAiHint] = useState('')
  const [thinking, setThinking] = useState(false)

  async function load() {
    const r = await call<Note[]>('/api/notes')
    setHint(r.hint ?? '')
    if (!r.hint) setNotes(r.data ?? [])
  }

  useEffect(() => {
    load().catch(() => setHint('The backend did not answer.'))
  }, [])

  async function add(e: React.FormEvent) {
    e.preventDefault()
    const text = draft.trim()
    if (!text) return
    const r = await call('/api/notes', {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ text }),
    })
    if (r.hint) return setHint(r.hint)
    setDraft('')
    await load()
  }

  async function remove(id: number) {
    const r = await call(`/api/notes/${id}`, { method: 'DELETE' }).catch(() => ({}) as { hint?: string })
    if (r.hint) setHint(r.hint)
    else await load()
  }

  async function summarize() {
    setThinking(true)
    try {
      const r = await call<{ summary: string }>('/api/notes/summary', { method: 'POST' })
      setAiHint(r.hint ?? '')
      if (!r.hint) setSummary(r.data?.summary ?? '')
    } catch {
      setAiHint('The summary did not arrive — try again.')
    } finally {
      setThinking(false)
    }
  }

  return (
    <Card title="Shared notes, in the space's postgres">
      {hint ? (
        <Hint text={hint} />
      ) : (
        <>
          <form onSubmit={add} className="flex gap-2">
            <input
              value={draft}
              onChange={(e) => setDraft(e.target.value)}
              placeholder="Write a note for the team…"
              aria-label="New note"
              className="min-w-0 flex-1 rounded-md border border-line bg-surface px-3 py-1.5 text-sm placeholder:text-muted focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-data"
            />
            <button type="submit" className={button}>
              Add
            </button>
          </form>
          <ul className="mt-3 max-h-52 space-y-2 overflow-y-auto">
            {notes.map((n) => (
              <li
                key={n.id}
                className="group flex items-baseline justify-between gap-3 border-b border-line/60 pb-2 text-sm last:border-0"
              >
                <span className="min-w-0 break-words">{n.text}</span>
                <span className="flex shrink-0 items-center gap-1.5">
                  <span className="font-mono text-xs text-muted">{n.created.slice(0, 10)}</span>
                  <button onClick={() => remove(n.id)} aria-label={`Delete note ${n.id}`} className={rowDelete}>
                    <Trash2 className="size-3.5" aria-hidden />
                  </button>
                </span>
              </li>
            ))}
            {notes.length === 0 && <li className="text-sm text-muted">Nothing yet — the first note is yours.</li>}
          </ul>
          <div className="mt-3 border-t border-line pt-3">
            <button onClick={summarize} disabled={thinking} className={`flex items-center gap-1.5 ${button}`}>
              <Sparkles className={`size-3.5 ${thinking ? 'animate-pulse' : ''}`} aria-hidden />
              {thinking ? 'Reading the notes…' : 'Summarize with AI'}
            </button>
            {aiHint && <Hint text={aiHint} />}
            {summary && !aiHint && <p className="mt-2 rounded-md bg-data-soft px-3 py-2 text-sm">{summary}</p>}
          </div>
        </>
      )}
    </Card>
  )
}

function FilesCard() {
  const [files, setFiles] = useState<FileRow[]>([])
  const [hint, setHint] = useState('')
  const [sending, setSending] = useState(false)

  async function load() {
    const r = await call<FileRow[]>('/api/files')
    setHint(r.hint ?? '')
    if (!r.hint) setFiles(r.data ?? [])
  }

  useEffect(() => {
    load().catch(() => setHint('The backend did not answer.'))
  }, [])

  async function remove(key: string) {
    const r = await call(`/api/files/${encodeURIComponent(key)}`, { method: 'DELETE' }).catch(
      () => ({}) as { hint?: string },
    )
    if (r.hint) setHint(r.hint)
    else await load()
  }

  async function upload(list: FileList | null) {
    const file = list?.[0]
    if (!file) return
    setSending(true)
    try {
      const body = new FormData()
      body.append('file', file)
      const r = await call('/api/files', { method: 'POST', body })
      if (r.hint) setHint(r.hint)
      else await load()
    } finally {
      setSending(false)
    }
  }

  return (
    <Card title="Attachments, in the space's bucket">
      {hint ? (
        <Hint text={hint} />
      ) : (
        <>
          <label className={`inline-flex cursor-pointer items-center gap-1.5 ${button}`}>
            <Upload className={`size-3.5 ${sending ? 'animate-pulse' : ''}`} aria-hidden />
            {sending ? 'Uploading…' : 'Upload a file'}
            <input
              type="file"
              className="sr-only"
              onChange={(e) => {
                upload(e.target.files)
                e.target.value = ''
              }}
            />
          </label>
          <ul className="mt-3 max-h-52 space-y-2 overflow-y-auto">
            {files.map((f) => (
              <li
                key={f.key}
                className="group flex items-baseline justify-between gap-3 border-b border-line/60 pb-2 text-sm last:border-0"
              >
                <a
                  href={`/api/files/${encodeURIComponent(f.key)}`}
                  download
                  className="flex min-w-0 items-center gap-1.5 hover:text-data"
                >
                  <Download className="size-3.5 shrink-0 text-muted" aria-hidden />
                  <span className="truncate">{f.key}</span>
                </a>
                <span className="flex shrink-0 items-center gap-1.5">
                  <span className="font-mono text-xs tabular-nums text-muted">
                    {f.modified.slice(0, 10)} · {bytes(f.size)}
                  </span>
                  <button onClick={() => remove(f.key)} aria-label={`Delete ${f.key}`} className={rowDelete}>
                    <Trash2 className="size-3.5" aria-hidden />
                  </button>
                </span>
              </li>
            ))}
            {files.length === 0 && (
              <li className="text-sm text-muted">Nothing here yet — every upload lands in the space's bucket.</li>
            )}
          </ul>
        </>
      )}
    </Card>
  )
}

function Hint({ text }: { text: string }) {
  return (
    <p className="flex items-center gap-1.5 py-3 text-sm text-muted">
      <Plug className="size-4 shrink-0" aria-hidden />
      {text}
    </p>
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
