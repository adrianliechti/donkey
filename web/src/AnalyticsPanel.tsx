import { useState, type KeyboardEvent, type PointerEvent, type ReactNode } from 'react'
import { ArrowDown, ArrowUp, ChartColumn, Info, Monitor, Smartphone, Tablet, X, type LucideIcon } from 'lucide-react'
import type {
  AnalyticsDimension,
  AnalyticsEventProperty,
  AnalyticsFilter,
  AnalyticsLive,
  AnalyticsPoint,
  AnalyticsReport,
  AnalyticsRow,
  AnalyticsTotals,
  TrafficRange,
} from './api'
import { useAnalytics, useAnalyticsLive, useNetwork } from './queries'
import { TrafficPanel } from './TrafficPanel'
import {
  Button,
  Callout,
  Card,
  CopyButton,
  Disclosure,
  ErrorNote,
  Eyebrow,
  focusRing,
  Section,
  SectionHeader,
  SectionTitle,
  Select,
  Spinner,
  StatGrid,
  wellClass,
} from './ui'

const integer = new Intl.NumberFormat()
const compact = new Intl.NumberFormat(undefined, { notation: 'compact', maximumFractionDigits: 1 })
const percent = new Intl.NumberFormat(undefined, { maximumFractionDigits: 1 })

const rangeOptions: { value: TrafficRange; label: string }[] = [
  { value: '48h', label: 'last 48 hours' },
  { value: '7d', label: 'last 7 days' },
  { value: '30d', label: 'last 30 days' },
]
const previousLabel: Record<TrafficRange, string> = {
  '48h': 'the previous 48 hours',
  '7d': 'the previous 7 days',
  '30d': 'the previous 30 days',
}

// The long names label filters and column heads; the short ones are the
// tabs inside a breakdown card, where five of them share one line.
const DIMENSION_LABEL: Record<AnalyticsDimension, string> = {
  path: 'Page',
  entry: 'Entry page',
  exit: 'Exit page',
  referrer: 'Referrer',
  channel: 'Channel',
  utm_source: 'UTM source',
  utm_medium: 'UTM medium',
  utm_campaign: 'UTM campaign',
  browser: 'Browser',
  os: 'OS',
  device: 'Device',
  country: 'Country',
  language: 'Language',
  event: 'Event',
}
const TAB_LABEL: Record<AnalyticsDimension, string> = {
  path: 'Pages',
  entry: 'Entry',
  exit: 'Exit',
  referrer: 'Referrers',
  channel: 'Channels',
  utm_source: 'Source',
  utm_medium: 'Medium',
  utm_campaign: 'Campaign',
  browser: 'Browsers',
  os: 'OS',
  device: 'Devices',
  country: 'Countries',
  language: 'Languages',
  event: 'Events',
}
const GROUPS: { title: string; dimensions: AnalyticsDimension[] }[] = [
  { title: 'Pages', dimensions: ['path', 'entry', 'exit'] },
  { title: 'Sources', dimensions: ['channel', 'referrer', 'utm_source', 'utm_medium', 'utm_campaign'] },
  { title: 'Locations', dimensions: ['country', 'language'] },
  { title: 'Technology', dimensions: ['browser', 'os', 'device'] },
]
// Identifiers — paths, event names, hosts — are mono, like everywhere else.
const MONO: AnalyticsDimension[] = ['path', 'entry', 'exit', 'referrer', 'event', 'utm_source', 'utm_medium', 'utm_campaign']

const SNIPPET = '<script defer src="/_donkey/s.js"></script>'
const TRACK_EXAMPLE = `// from code
window.donkey?.track('signup', { plan: 'pro' })

<!-- or declaratively, on anything clickable -->
<button data-donkey-event="signup" data-donkey-event-plan="pro">Sign up</button>`
const IGNORE_EXAMPLE = `localStorage.setItem('donkey:ignore', '1')`

/**
 * An app's one view of how it is used: the audience the gateway's script
 * counted on its pages (in the shape umami and rybbit made familiar — who
 * came, from where, to which pages, and what they did there), then the
 * requests the gateway served and what the app's containers used. One range
 * drives all of it; filters narrow the audience.
 */
export function AnalyticsPanel({
  space,
  app,
  canManage,
  onOpenSettings,
}: {
  space: string
  app: string
  canManage: boolean
  onOpenSettings: () => void
}) {
  const [range, setRange] = useState<TrafficRange>('7d')
  const [filters, setFilters] = useState<AnalyticsFilter[]>([])
  const { data, error, stale } = useAnalytics(space, app, range, filters)
  const { data: live } = useAnalyticsLive(space, app)
  // Viewers may read analytics but not the app's settings; for them the
  // report speaks for itself.
  const { data: network } = useNetwork(space, app, canManage)

  const hasData = !!data && (data.totals.visits > 0 || data.totals.pageviews > 0 || data.totals.events > 0)
  // Pages are counted on web apps with an address that did not opt out.
  // APIs and internal apps have no audience to show, only traffic; viewers
  // cannot read the settings and go by what was recorded.
  const counts = network ? network.exposure !== 'internal' && network.mode !== 'api' && network.analytics !== false : undefined
  // An empty audience shrinks to a line: a web-mode app that serves JSON
  // would otherwise carry a block of zeros forever.
  const showAudience = hasData || (!data && !error) || filters.length > 0
  const filter = (dimension: AnalyticsDimension, value: string) =>
    setFilters((current) => [...current.filter((f) => f.dimension !== dimension), { dimension, value }])
  const unfilter = (dimension: AnalyticsDimension) =>
    setFilters((current) => current.filter((f) => f.dimension !== dimension))

  if (!showAudience) {
    const optedOut = network?.analytics === false && network.mode !== 'api' && network.exposure !== 'internal'
    return (
      <>
        {optedOut && (
          <Section>
            <OffNotice onOpenSettings={onOpenSettings} />
          </Section>
        )}
        {counts !== false && (
          <Section>
            <SectionHeader
              title="Audience"
              description="No page visits in this range. The gateway counts them on the app's HTML pages."
            />
          </Section>
        )}
        <TrafficPanel space={space} app={app} range={range} onRange={setRange} />
      </>
    )
  }

  return (
    <>
      <Section className="space-y-5">
        <SectionHeader
          title="Audience"
          actions={
            <>
              {live && <LivePill live={live} />}
              <Select variant="quiet" ariaLabel="Time range" value={range} onChange={setRange} options={rangeOptions} />
            </>
          }
        />
        {network?.analytics === false && <OffNotice onOpenSettings={onOpenSettings} />}
        {filters.length > 0 && <FilterBar filters={filters} onRemove={unfilter} onClear={() => setFilters([])} />}
        <ErrorNote message={error} />
        {!data && !error && <Spinner label="Reading analytics…" />}
        {data && (
          // A new range or filter keeps this report up, dimmed, until the
          // next one arrives — no flash, no jump.
          <div
            className={`space-y-5 transition-opacity duration-(--duration-fast) ${stale ? 'opacity-60' : ''}`}
            aria-busy={stale}
          >
            <Kpis totals={data.totals} previous={data.previous} range={data.range} />
            {hasData ? (
              <VisitsChart series={data.series} granularity={data.granularity} />
            ) : (
              <EmptyState filtered={filters.length > 0} onClear={() => setFilters([])} />
            )}
          </div>
        )}
      </Section>

      {data && hasData && (
        <Section className={`transition-opacity duration-(--duration-fast) ${stale ? 'opacity-60' : ''}`}>
          <div className="grid gap-4 sm:grid-cols-2">
            {GROUPS.map((group) => (
              <BreakdownCard
                key={group.title}
                title={group.title}
                dimensions={group.dimensions}
                report={data}
                filters={filters}
                onFilter={filter}
              />
            ))}
            <div className="sm:col-span-2">
              <BreakdownCard
                title="Events"
                dimensions={['event']}
                report={data}
                filters={filters}
                onFilter={filter}
                properties={data.eventProperties}
              />
            </div>
          </div>
        </Section>
      )}

      <Section>
        <HowItsCounted report={data} />
      </Section>

      <TrafficPanel space={space} app={app} range={range} />
    </>
  )
}

// ─── Header pieces ───────────────────────────────────────────────────────

function LivePill({ live }: { live: AnalyticsLive }) {
  const title = live.pages.length
    ? `On the app right now: ${live.pages.map((p) => `${p.value || '/'} (${integer.format(p.visitors)})`).join(', ')}`
    : `Nobody in the last ${live.windowMinutes} minutes`
  return (
    <span
      title={title}
      aria-label={`${integer.format(live.visitors)} visitors in the last ${live.windowMinutes} minutes`}
      className="mr-1 inline-flex h-6 items-center gap-1.5 rounded-full bg-raised px-2 font-mono text-[11px] text-ink-soft"
    >
      <span
        className={`size-1.5 rounded-full ${live.visitors > 0 ? 'bg-success motion-safe:animate-pulse' : 'bg-line-strong'}`}
        aria-hidden
      />
      {integer.format(live.visitors)} online
    </span>
  )
}

// A web app that opted out: its pages are left untouched, and what was
// recorded earlier stays until it ages out.
function OffNotice({ onOpenSettings }: { onOpenSettings: () => void }) {
  return (
    <Callout
      icon={Info}
      title="Web analytics is off for this app"
      action={
        <Button kind="quiet" size="sm" onClick={onOpenSettings}>
          Open settings
        </Button>
      }
    >
      The gateway adds no script to its pages, so visits are not counted.
    </Callout>
  )
}

function FilterBar({
  filters,
  onRemove,
  onClear,
}: {
  filters: AnalyticsFilter[]
  onRemove: (dimension: AnalyticsDimension) => void
  onClear: () => void
}) {
  return (
    <div className="flex flex-wrap items-center gap-1.5" aria-label="Active filters">
      <Eyebrow className="mr-1">Filtered by</Eyebrow>
      {filters.map((f) => (
        <button
          key={f.dimension}
          type="button"
          onClick={() => onRemove(f.dimension)}
          title={`Remove the ${DIMENSION_LABEL[f.dimension].toLowerCase()} filter`}
          className={`inline-flex max-w-full items-center gap-1.5 rounded-md bg-accent-soft/60 py-1 pl-2 pr-1.5 text-xs text-ink transition-colors hover:bg-accent-soft ${focusRing}`}
        >
          <span className="text-muted">{DIMENSION_LABEL[f.dimension]}</span>
          <span className={`truncate ${MONO.includes(f.dimension) ? 'font-mono' : ''}`}>{valueText(f.dimension, f.value)}</span>
          <X className="size-3.5 shrink-0 text-muted" aria-hidden />
        </button>
      ))}
      <button
        type="button"
        onClick={onClear}
        className={`rounded px-1 text-xs text-muted underline-offset-2 hover:text-ink hover:underline ${focusRing}`}
      >
        Clear all
      </button>
    </div>
  )
}

// ─── Headline numbers ────────────────────────────────────────────────────

type Kpi = {
  label: string
  value: (t: AnalyticsTotals) => number
  format: (n: number) => string
  /** Bounce rate goes down when things go well. */
  lowerIsBetter?: boolean
  /** Rates compare in percentage points, counts in percent. */
  points?: boolean
  /** An average over visits: nothing to say about a period without any. */
  perVisit?: boolean
}

const KPIS: Kpi[] = [
  { label: 'Visitors', value: (t) => t.visitors, format: (n) => integer.format(n) },
  { label: 'Visits', value: (t) => t.visits, format: (n) => integer.format(n) },
  { label: 'Pageviews', value: (t) => t.pageviews, format: (n) => integer.format(n) },
  { label: 'Views per visit', value: (t) => t.viewsPerVisit, format: (n) => n.toFixed(1), perVisit: true },
  { label: 'Bounce rate', value: (t) => t.bounceRate, format: (n) => `${percent.format(n)}%`, lowerIsBetter: true, points: true, perVisit: true },
  { label: 'Visit duration', value: (t) => t.visitDurationSeconds, format: duration, perVisit: true },
]

function Kpis({ totals, previous, range }: { totals: AnalyticsTotals; previous?: AnalyticsTotals; range: TrafficRange }) {
  const empty = totals.visits === 0
  return (
    <div className="space-y-1.5">
      <StatGrid
        columns="grid-cols-2 sm:grid-cols-3"
        stats={KPIS.map((kpi) => ({
          label: kpi.label,
          // A rate over no visits is not 0% — it is nothing to say.
          value: empty && kpi.perVisit ? '—' : kpi.format(kpi.value(totals)),
          note: previous && !empty ? <Delta kpi={kpi} current={kpi.value(totals)} previous={previous} /> : undefined,
        }))}
      />
      {(previous || range === '30d') && (
        <p className="text-[11px] text-muted">
          {previous
            ? `Changes compare with ${previousLabel[range]}.`
            : 'Analytics keep 30 days, so there is no earlier month to compare with.'}
        </p>
      )}
    </div>
  )
}

function Delta({ kpi, current, previous }: { kpi: Kpi; current: number; previous: AnalyticsTotals }) {
  const before = kpi.value(previous)
  // Growth from nothing has no percentage, and an average over no earlier
  // visits has no baseline at all.
  if (previous.visits === 0 || (!kpi.points && before === 0)) {
    return <span className="text-muted">new in this period</span>
  }
  const change = current - before
  if (change === 0) {
    return <span className="text-muted">no change</span>
  }
  const up = change > 0
  const good = up !== !!kpi.lowerIsBetter
  const Arrow = up ? ArrowUp : ArrowDown
  const amount = kpi.points
    ? `${percent.format(Math.abs(change))} pp`
    : `${percent.format(Math.abs((change / before) * 100))}%`
  return (
    <span className={`inline-flex items-center gap-0.5 font-mono ${good ? 'text-success' : 'text-danger'}`}>
      <Arrow className="size-3" aria-hidden />
      <span className="sr-only">{up ? 'up' : 'down'} </span>
      {amount}
    </span>
  )
}

function EmptyState({ filtered, onClear }: { filtered: boolean; onClear: () => void }) {
  return (
    <div className="flex flex-col items-center gap-2 py-8 text-center">
      <ChartColumn className="size-5 text-muted" aria-hidden />
      <p className="text-sm text-ink-soft">{filtered ? 'No visits match these filters.' : 'No visits in this range yet.'}</p>
      {filtered ? (
        <Button kind="quiet" size="sm" onClick={onClear}>
          Clear filters
        </Button>
      ) : (
        <p className="max-w-sm text-xs leading-relaxed text-muted">
          Visits appear here a few seconds after someone opens one of the app’s pages.
        </p>
      )}
    </div>
  )
}

// ─── Visits over time ────────────────────────────────────────────────────

// Two nested measures on one scale: every visitor's views sit inside the
// pageview column, so visitors overlay pageviews from the same baseline —
// one axis, two steps of one hue, a legend, and the exact numbers in the
// tooltip and the table behind the chart.
function VisitsChart({ series, granularity }: { series: AnalyticsPoint[]; granularity: 'hour' | 'day' }) {
  const [active, setActive] = useState<number | null>(null)
  const top = niceMax(Math.max(0, ...series.map((p) => Math.max(p.pageviews, p.visitors))))
  const half = top / 2
  const point = active === null ? null : series[active]

  function pick(e: PointerEvent<HTMLDivElement>) {
    const box = e.currentTarget.getBoundingClientRect()
    if (!series.length || box.width === 0) return
    const index = Math.floor(((e.clientX - box.left) / box.width) * series.length)
    setActive(Math.min(series.length - 1, Math.max(0, index)))
  }
  function step(e: KeyboardEvent<HTMLDivElement>) {
    const last = series.length - 1
    const from = active ?? last
    const next =
      e.key === 'ArrowLeft' ? from - 1 : e.key === 'ArrowRight' ? from + 1 : e.key === 'Home' ? 0 : e.key === 'End' ? last : null
    if (next === null) return
    e.preventDefault()
    setActive(Math.min(last, Math.max(0, next)))
  }

  return (
    <div className="space-y-2">
      <div className="flex items-center justify-between gap-3">
        <Eyebrow>Visitors and pageviews by {granularity}</Eyebrow>
        <span className="flex items-center gap-3 text-[11px] text-muted">
          <span className="flex items-center gap-1"><i className="inline-block size-2 rounded-sm bg-chart-visitors" />visitors</span>
          <span className="flex items-center gap-1"><i className="inline-block size-2 rounded-sm bg-chart-views" />pageviews</span>
        </span>
      </div>

      <div className="flex gap-2">
        {/* Recessive scale: the top and the middle, nothing more. */}
        <div className="relative h-40 w-8 shrink-0 font-mono text-[10px] tabular-nums text-muted" aria-hidden>
          <span className="absolute right-0 top-0 -translate-y-1/2">{compact.format(top)}</span>
          {Number.isInteger(half) && <span className="absolute right-0 top-1/2 -translate-y-1/2">{compact.format(half)}</span>}
          <span className="absolute bottom-0 right-0 translate-y-1/2">0</span>
        </div>
        <div className="relative min-w-0 flex-1">
          <div className="pointer-events-none absolute inset-x-0 top-0 border-t border-line" aria-hidden />
          <div className="pointer-events-none absolute inset-x-0 top-1/2 border-t border-line/70" aria-hidden />
          <div
            role="img"
            aria-label={`Visitors and pageviews per ${granularity}; use the arrow keys to read each ${granularity}`}
            tabIndex={0}
            onPointerMove={pick}
            onPointerLeave={() => setActive(null)}
            onKeyDown={step}
            onFocus={() => setActive((current) => current ?? series.length - 1)}
            onBlur={() => setActive(null)}
            className="relative flex h-40 cursor-crosshair items-end gap-[2px] border-b border-line-strong focus-visible:outline-2 focus-visible:outline-offset-4 focus-visible:outline-accent-strong"
          >
            {series.map((p, i) => (
              <div key={p.bucket} className="relative flex h-full min-w-0 flex-1 items-end justify-center">
                {active === i && <span className="absolute inset-y-0 left-1/2 w-px bg-line-strong" aria-hidden />}
                <span
                  className={`relative block h-full w-full max-w-6 ${active !== null && active !== i ? 'opacity-70' : ''}`}
                  aria-hidden
                >
                  <Bar value={p.pageviews} top={top} tone="bg-chart-views" />
                  <Bar value={p.visitors} top={top} tone="bg-chart-visitors" />
                </span>
              </div>
            ))}
          </div>
          {point && active !== null && (
            <ChartTooltip point={point} granularity={granularity} position={(active + 0.5) / series.length} />
          )}
          <div className="mt-1.5 flex justify-between gap-3 text-[11px] text-muted">
            <span>{bucketLabel(series[0]?.bucket, granularity)}</span>
            <span>{bucketLabel(series.at(-1)?.bucket, granularity)}</span>
          </div>
        </div>
      </div>

      <table className="sr-only">
        <caption>Visitors, visits and pageviews per {granularity}</caption>
        <thead>
          <tr><th scope="col">{granularity}</th><th scope="col">Visitors</th><th scope="col">Visits</th><th scope="col">Pageviews</th></tr>
        </thead>
        <tbody>
          {series.map((p) => (
            <tr key={p.bucket}>
              <th scope="row">{bucketLabel(p.bucket, granularity)}</th>
              <td>{p.visitors}</td>
              <td>{p.visits}</td>
              <td>{p.pageviews}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

function Bar({ value, top, tone }: { value: number; top: number; tone: string }) {
  if (value <= 0) return null
  // Rounded at the data end, square on the baseline; never thinner than a
  // sliver, so a single visit still shows.
  const height = Math.max(2, (value / top) * 100)
  return <span className={`absolute inset-x-0 bottom-0 rounded-t-[4px] ${tone}`} style={{ height: `${height}%` }} />
}

function ChartTooltip({ point, granularity, position }: { point: AnalyticsPoint; granularity: 'hour' | 'day'; position: number }) {
  // Kept inside the plot: near either edge it hangs from that edge instead
  // of centring on the column.
  const align = position < 0.2 ? 'translate-x-0' : position > 0.8 ? '-translate-x-full' : '-translate-x-1/2'
  const rows = [
    { label: 'visitors', value: point.visitors, key: 'bg-chart-visitors' },
    { label: 'pageviews', value: point.pageviews, key: 'bg-chart-views' },
    { label: 'visits', value: point.visits, key: '' },
  ]
  return (
    <div
      className={`pointer-events-none absolute top-0 z-10 min-w-36 rounded-md border border-line bg-card px-3 py-2 text-xs shadow-lg ${align}`}
      style={{ left: `${position * 100}%` }}
      role="status"
    >
      <p className="mb-1 text-[11px] text-muted">{bucketLabel(point.bucket, granularity, true)}</p>
      {rows.map((row) => (
        <p key={row.label} className="flex items-center gap-2">
          <i className={`inline-block h-0.5 w-3 rounded-full ${row.key || 'bg-transparent'}`} aria-hidden />
          <span className="font-mono font-semibold tabular-nums text-ink">{integer.format(row.value)}</span>
          <span className="text-muted">{row.label}</span>
        </p>
      ))}
    </div>
  )
}

// ─── Breakdowns ──────────────────────────────────────────────────────────

const SHOWN = 8

function BreakdownCard({
  title,
  dimensions,
  report,
  filters,
  onFilter,
  properties,
}: {
  title: string
  dimensions: AnalyticsDimension[]
  report: AnalyticsReport
  filters: AnalyticsFilter[]
  onFilter: (dimension: AnalyticsDimension, value: string) => void
  properties?: AnalyticsEventProperty[]
}) {
  const [dimension, setDimension] = useState(dimensions[0])
  const [all, setAll] = useState(false)
  const rows = report.breakdowns[dimension] ?? []
  const shown = all ? rows : rows.slice(0, SHOWN)
  const active = filters.find((f) => f.dimension === dimension)?.value
  // Shares are of the whole, not of the leader: bars across a card add up
  // to what the headline numbers say.
  const whole = dimension === 'path' ? report.totals.pageviews : dimension === 'event' ? report.totals.events : report.totals.visits
  const countLabel = dimension === 'path' ? 'Views' : dimension === 'event' ? 'Count' : 'Visits'

  return (
    <Card className="flex flex-col overflow-hidden">
      <div className="flex flex-wrap items-center justify-between gap-x-3 gap-y-1 border-b border-line px-3 py-2">
        <SectionTitle>{title}</SectionTitle>
        {dimensions.length > 1 && (
          <div role="tablist" aria-label={`${title} breakdown`} className="-mr-1 flex flex-wrap gap-0.5">
            {dimensions.map((d) => (
              <button
                key={d}
                type="button"
                role="tab"
                aria-selected={d === dimension}
                onClick={() => {
                  setDimension(d)
                  setAll(false)
                }}
                className={`rounded px-1.5 py-0.5 text-xs transition-colors ${focusRing} ${
                  d === dimension ? 'bg-raised font-medium text-ink' : 'text-muted hover:text-ink'
                }`}
              >
                {TAB_LABEL[d]}
              </button>
            ))}
          </div>
        )}
      </div>

      <div className="flex gap-2 px-3 pb-1 pt-2">
        <Eyebrow className="min-w-0 flex-1">{DIMENSION_LABEL[dimension]}</Eyebrow>
        <Eyebrow className="w-16 text-right">Visitors</Eyebrow>
        <Eyebrow className="w-14 text-right">{countLabel}</Eyebrow>
      </div>
      {rows.length === 0 ? (
        <p className="px-3 py-6 text-center text-xs text-muted">Nothing recorded for this range.</p>
      ) : (
        <ul className="pb-1.5">
          {shown.map((row) => (
            <BreakdownRow
              key={row.value}
              dimension={dimension}
              row={row}
              whole={whole}
              active={active === row.value}
              onFilter={() => onFilter(dimension, row.value)}
            />
          ))}
        </ul>
      )}
      {rows.length > SHOWN && (
        <button
          type="button"
          onClick={() => setAll((v) => !v)}
          className={`mx-3 mb-2 self-start rounded px-1 text-xs text-muted underline-offset-2 hover:text-ink hover:underline ${focusRing}`}
        >
          {all ? 'Show fewer' : `Show all ${rows.length}`}
        </button>
      )}
      {properties && properties.length > 0 && <EventProperties properties={properties} />}
    </Card>
  )
}

function BreakdownRow({
  dimension,
  row,
  whole,
  active,
  onFilter,
}: {
  dimension: AnalyticsDimension
  row: AnalyticsRow
  whole: number
  active: boolean
  onFilter: () => void
}) {
  const share = whole > 0 ? Math.min(100, (row.count / whole) * 100) : 0
  const text = valueText(dimension, row.value)
  return (
    <li>
      <button
        type="button"
        onClick={onFilter}
        disabled={active}
        title={active ? `Filtered by this ${DIMENSION_LABEL[dimension].toLowerCase()}` : `${text} — ${percent.format(share)}% · click to filter`}
        className={`group relative flex w-full items-center gap-2 px-3 py-1 text-left text-sm transition-colors hover:bg-line/30 disabled:cursor-default ${focusRing} focus-visible:-outline-offset-2`}
      >
        <span
          className={`absolute inset-y-0.5 left-1 rounded-sm ${active ? 'bg-accent/25' : 'bg-accent/10 group-hover:bg-accent/15'}`}
          style={{ width: `calc(${share}% - 0.5rem)` }}
          aria-hidden
        />
        <ValueLabel dimension={dimension} value={row.value} text={text} />
        <span className="relative w-16 shrink-0 text-right font-mono text-xs tabular-nums text-muted">{integer.format(row.visitors)}</span>
        <span className="relative w-14 shrink-0 text-right font-mono text-xs tabular-nums text-ink">{integer.format(row.count)}</span>
      </button>
    </li>
  )
}

function EventProperties({ properties }: { properties: AnalyticsEventProperty[] }) {
  return (
    <div className="space-y-3 border-t border-line px-3 py-3">
      <Eyebrow>Properties of this event</Eyebrow>
      <div className="grid gap-4 sm:grid-cols-2">
        {properties.map((property) => {
          const total = property.values.reduce((sum, v) => sum + v.count, 0)
          return (
            <div key={property.key} className="min-w-0 space-y-1">
              <p className="font-mono text-xs text-ink-soft">{property.key}</p>
              <ul>
                {property.values.map((v) => {
                  const share = total > 0 ? (v.count / total) * 100 : 0
                  return (
                    <li key={v.value} className="relative flex items-center gap-2 py-0.5 text-xs" title={`${percent.format(share)}%`}>
                      <span className="absolute inset-y-0 left-0 rounded-sm bg-accent/10" style={{ width: `${share}%` }} aria-hidden />
                      <span className="relative min-w-0 flex-1 truncate pl-1 font-mono text-ink">{v.value || <span className="text-muted">(empty)</span>}</span>
                      <span className="relative shrink-0 pr-1 font-mono tabular-nums text-ink">{integer.format(v.count)}</span>
                    </li>
                  )
                })}
              </ul>
            </div>
          )
        })}
      </div>
    </div>
  )
}

const DEVICE_ICON: Record<string, LucideIcon> = { desktop: Monitor, mobile: Smartphone, tablet: Tablet }

function ValueLabel({ dimension, value, text }: { dimension: AnalyticsDimension; value: string; text: string }) {
  const Icon = dimension === 'device' ? DEVICE_ICON[value] : undefined
  return (
    <span className="relative flex min-w-0 flex-1 items-center gap-1.5">
      {Icon && <Icon className="size-3.5 shrink-0 text-muted" aria-hidden />}
      <span className={`truncate ${value ? 'text-ink' : 'text-muted'} ${MONO.includes(dimension) && value ? 'font-mono text-xs' : ''}`}>
        {text}
      </span>
    </span>
  )
}

// ─── How it's counted ────────────────────────────────────────────────────

function HowItsCounted({ report }: { report: AnalyticsReport | null }) {
  const definitions = report?.definitions
  return (
    <Disclosure summary="How visits are counted">
      <div className="space-y-4 text-xs leading-relaxed text-ink-soft">
        <p>
          donkey-gateway adds one small script to the <code className="font-mono">&lt;head&gt;</code> of this app’s HTML
          pages. It sets no cookie and stores no IP address, user agent or query string: an anonymous visitor is a hash
          that changes every day, a signed-in one a hash of the sign-in that only this app’s numbers use. Navigations
          inside a single-page app count as pageviews, and links to other sites as <code className="font-mono">outbound-link</code> events.
        </p>
        {definitions && (
          <dl className="grid gap-x-4 gap-y-1.5 sm:grid-cols-[max-content_1fr]">
            <dt className="font-medium text-ink">Visitor</dt>
            <dd>{definitions.visitor}</dd>
            <dt className="font-medium text-ink">Visit</dt>
            <dd>{definitions.visit}</dd>
            <dt className="font-medium text-ink">Bounce</dt>
            <dd>{definitions.bounce}</dd>
            <dt className="font-medium text-ink">Duration</dt>
            <dd>{definitions.duration}</dd>
          </dl>
        )}
        <Snippet label="Custom events" code={TRACK_EXAMPLE} />
        <Snippet
          label="Adding the script yourself"
          note="Pages whose Content-Security-Policy does not allow same-origin scripts are left untouched — include the tag in those, or in pages the gateway does not see as HTML. Loading it twice is harmless."
          code={SNIPPET}
        />
        <Snippet
          label="Leaving yourself out"
          note="Run this in the browser console on one of the app’s pages; that browser is not counted from then on."
          code={IGNORE_EXAMPLE}
        />
      </div>
    </Disclosure>
  )
}

function Snippet({ label, note, code }: { label: string; note?: ReactNode; code: string }) {
  return (
    <div className="space-y-1.5">
      <Eyebrow>{label}</Eyebrow>
      {note && <p>{note}</p>}
      <div className="relative">
        <pre className={`${wellClass} whitespace-pre-wrap break-all pr-11`}>{code}</pre>
        <div className="absolute right-1 top-1">
          <CopyButton text={code} label={`Copy: ${label.toLowerCase()}`} />
        </div>
      </div>
    </div>
  )
}

// ─── Formatting ──────────────────────────────────────────────────────────

const regionNames = displayNames('region')
const languageNames = displayNames('language')

function displayNames(type: 'region' | 'language') {
  try {
    return new Intl.DisplayNames(undefined, { type })
  } catch {
    return null
  }
}

function nameOf(names: Intl.DisplayNames | null, code: string) {
  try {
    return names?.of(code) ?? code
  } catch {
    return code
  }
}

function flag(country: string) {
  if (!/^[A-Z]{2}$/.test(country)) return ''
  return String.fromCodePoint(...[...country].map((c) => 0x1f1e6 + c.charCodeAt(0) - 65))
}

/** The words a value reads as — in a row, a chip and a tooltip alike. */
function valueText(dimension: AnalyticsDimension, value: string): string {
  if (!value) return 'Unknown'
  switch (dimension) {
    case 'country':
      return `${flag(value)} ${nameOf(regionNames, value)}`.trim()
    case 'language':
      return nameOf(languageNames, value)
    case 'device':
      return value.charAt(0).toUpperCase() + value.slice(1)
    default:
      return value
  }
}

function duration(seconds: number) {
  const s = Math.round(seconds)
  if (s <= 0) return '0s'
  if (s < 60) return `${s}s`
  const m = Math.floor(s / 60)
  if (m < 60) return `${m}m ${s % 60}s`
  return `${Math.floor(m / 60)}h ${m % 60}m`
}

// niceMax rounds the scale up to 1, 2 or 5 of a power of ten, so the two
// labelled lines read as round numbers.
function niceMax(value: number) {
  if (value <= 1) return 1
  const power = 10 ** Math.floor(Math.log10(value))
  for (const step of [1, 2, 5, 10]) {
    if (step * power >= value) return step * power
  }
  return 10 * power
}

function bucketLabel(bucket: string | undefined, granularity: 'hour' | 'day', long = false) {
  if (!bucket) return ''
  const at = new Date(bucket)
  if (Number.isNaN(at.getTime())) return bucket
  if (granularity === 'day') {
    // Day buckets are UTC dates; formatting them in local time would show
    // the day before west of Greenwich.
    return new Intl.DateTimeFormat(undefined, {
      timeZone: 'UTC',
      ...(long ? { weekday: 'short' } : {}),
      month: 'short',
      day: 'numeric',
    }).format(at)
  }
  return new Intl.DateTimeFormat(undefined, {
    month: 'short',
    day: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
    ...(long ? { timeZoneName: 'short' } : {}),
  }).format(at)
}
