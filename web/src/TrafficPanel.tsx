import { useState } from 'react'
import { Dialog, DialogBackdrop, DialogPanel, DialogTitle } from '@headlessui/react'
import { Activity, Cpu, MemoryStick, X } from 'lucide-react'
import type { ResourcePoint, ResourceRange, TrafficPoint, TrafficRange, TrafficSource, TrafficTotals } from './api'
import { useResources, useTraffic } from './queries'
import { ago, Card, ErrorNote, SectionTitle, Select, Spinner } from './ui'

const integer = new Intl.NumberFormat()
const compactInteger = new Intl.NumberFormat(undefined, {
  notation: 'compact',
  maximumFractionDigits: 1,
})
const rangeOptions: { value: TrafficRange; label: string }[] = [
  { value: '48h', label: 'Last 48 hours' },
  { value: '7d', label: 'Last 7 days' },
  { value: '30d', label: 'Last 30 days' },
]
const resourceRangeOptions: { value: ResourceRange; label: string }[] = [
  { value: '1h', label: 'Last hour' },
  { value: '4h', label: 'Last 4 hours' },
  { value: '48h', label: 'Last 48 hours' },
  { value: '7d', label: 'Last 7 days' },
  { value: '30d', label: 'Last 30 days' },
]

/**
 * Space cards only need a quiet signal that telemetry exists. The full
 * dashboard opens on demand, keeping the fleet about apps and services.
 */
export function SpaceTelemetry({ space }: { space: string }) {
  const [open, setOpen] = useState(false)
  const { data, error } = useTraffic(space)
  const errors = (data?.totals.clientErrors ?? 0) + (data?.totals.serverErrors ?? 0)
  const title = error
    ? `Telemetry unavailable: ${error}`
    : data
      ? `Telemetry — ${integer.format(data.totals.requests)} requests, ${integer.format(errors)} errors in the last 30 days`
      : 'Open space telemetry'

  return (
    <>
      <button
        type="button"
        title={title}
        aria-label={title}
        onClick={() => setOpen(true)}
        className={`inline-flex h-7 items-center gap-1.5 rounded-md px-1.5 font-mono text-[11px] transition-colors focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-accent-strong ${
          error
            ? 'text-danger hover:bg-danger/10'
            : 'text-muted hover:bg-line/60 hover:text-ink'
        }`}
      >
        <Activity className="size-3.5" aria-hidden />
        {data && <span>{compactInteger.format(data.totals.requests)}</span>}
        {errors > 0 && <span className="size-1.5 rounded-full bg-danger" aria-hidden />}
      </button>

      <Dialog open={open} onClose={setOpen} className="relative z-50">
        <DialogBackdrop className="fixed inset-0 bg-black/60 transition-opacity duration-150 starting:opacity-0" />
        <div className="fixed inset-0 flex justify-end">
          <DialogPanel className="flex h-full w-full max-w-4xl flex-col border-l border-line bg-surface transition-transform duration-200 ease-out will-change-transform starting:translate-x-full shadow-[-24px_0_60px_rgba(0,0,0,.5)]">
            <div className="flex items-center justify-between gap-3 border-b border-line bg-card px-5 py-4">
              <div className="min-w-0">
                <div className="font-mono text-[10px] font-semibold uppercase tracking-[0.16em] text-muted">
                  Space telemetry
                </div>
                <DialogTitle className="truncate font-mono text-xl font-bold tracking-tight">
                  {space}
                </DialogTitle>
              </div>
              <button
                type="button"
                title="Close"
                aria-label="Close space telemetry"
                onClick={() => setOpen(false)}
                className="rounded-md p-1.5 text-muted hover:bg-raised hover:text-ink focus-visible:outline-2 focus-visible:outline-accent-strong"
              >
                <X className="size-4" aria-hidden />
              </button>
            </div>
            <div className="min-h-0 flex-1 overflow-y-auto p-5 sm:p-6">
              <TrafficPanel space={space} app="" />
            </div>
          </DialogPanel>
        </div>
      </Dialog>
    </>
  )
}

export function TrafficPanel({ space, app }: { space: string; app: string }) {
  const [source, setSource] = useState<TrafficSource>('edge')
  const [range, setRange] = useState<TrafficRange>('30d')
  const { data, error } = useTraffic(space, app, source, range)

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center justify-end gap-2">
        <Select
          value={range}
          onChange={setRange}
          options={rangeOptions}
          ariaLabel="Telemetry time range"
          size="sm"
        />
        <div
          className="flex items-center gap-1 rounded-md border border-line bg-surface p-1"
          role="group"
          aria-label="Telemetry source"
        >
          <SourceButton active={source === 'edge'} onClick={() => setSource('edge')}>
            Gateway
          </SourceButton>
          <SourceButton active={source === 'app'} onClick={() => setSource('app')}>
            In-app OTEL
          </SourceButton>
        </div>
      </div>

      {app && <ResourceUsage space={space} app={app} />}

      <ErrorNote message={error} />
      {!data && !error && <Spinner label="Reading telemetry…" />}
      {data && (
        <>
          <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
            <Metric label="Requests" value={integer.format(data.totals.requests)} note="HTTP responses" />
            <Metric
              label="Successful"
              value={integer.format(data.totals.successful)}
              note={`${successRate(data.totals)} of requests · HTTP 2xx–3xx`}
            />
            <Metric
              label="Errors"
              value={integer.format(data.totals.clientErrors + data.totals.serverErrors)}
              note={`${integer.format(data.totals.clientErrors)} client 4xx · ${integer.format(data.totals.serverErrors)} server 5xx`}
              bad={data.totals.clientErrors + data.totals.serverErrors > 0}
            />
            <Metric
              label="P95 latency"
              value={latency(data.totals.p95LatencyMs)}
              note={`p50 ${latency(data.totals.p50LatencyMs)} · average ${latency(data.totals.averageLatencyMs)}`}
            />
          </div>

          <AudienceCard totals={data.totals} source={source} onShowGateway={() => setSource('edge')} />

          <Card className="p-4">
            <SectionTitle>Requests by {data.granularity}</SectionTitle>
            {data.totals.requests > 0 ? (
              <>
                <DailyBars points={data.series} />
                <div className="mt-2 grid grid-cols-2 gap-x-3 gap-y-1 text-[10px] text-muted">
                  <span>{bucketLabel(data.series[0]?.bucket, data.granularity)}</span>
                  <span className="text-right">{bucketLabel(data.series.at(-1)?.bucket, data.granularity)}</span>
                  <span className="col-span-2 flex items-center justify-center gap-3">
                    <span><i className="mr-1 inline-block size-2 rounded-sm bg-accent" />requests</span>
                    <span><i className="mr-1 inline-block size-2 rounded-sm bg-danger" />4xx / 5xx</span>
                  </span>
                </div>
              </>
            ) : (
              <div className="flex h-36 flex-col items-center justify-center gap-2 text-center text-muted">
                <Activity className="size-5" aria-hidden />
                <p className="text-sm">No HTTP telemetry in this range.</p>
              </div>
            )}
          </Card>

          <details className="rounded-md border border-line/60 bg-surface/40 px-3 py-2 text-xs text-muted">
            <summary className="cursor-pointer select-none font-medium text-ink-soft">
              How these numbers are counted
            </summary>
            <div className="mt-2 space-y-1.5 leading-relaxed">
              <p><span className="text-ink-soft">Successful:</span> {data.definitions.success}.</p>
              {source === 'edge' && (
                <>
                  <p><span className="text-ink-soft">Views:</span> {data.definitions.view}.</p>
                  <p><span className="text-ink-soft">Signed-in visitors:</span> {data.definitions.uniqueVisitors}.</p>
                </>
              )}
            </div>
          </details>
        </>
      )}
    </div>
  )
}

function ResourceUsage({ space, app }: { space: string; app: string }) {
  const [range, setRange] = useState<ResourceRange>('4h')
  const { data, error } = useResources(space, app, range)

  if (error) return <ErrorNote message={`Runtime usage is unavailable: ${error}`} />
  if (!data) {
    return (
      <Card className="p-4">
        <Spinner label="Reading runtime usage…" />
      </Card>
    )
  }
  if (!data.available) {
    return (
      <Card className="px-4 py-5">
        <SectionTitle>Runtime resources</SectionTitle>
        <p className="mt-2 text-sm text-muted">
          No container CPU or memory samples yet. Usage appears after the next runtime sample.
        </p>
      </Card>
    )
  }

  const current = data.current
  return (
    <Card className="p-4">
      <div className="flex flex-wrap items-start justify-between gap-2">
        <div>
          <SectionTitle>Runtime resources</SectionTitle>
          <p className="mt-1 text-xs text-muted">Container usage across the app’s running instances.</p>
        </div>
        <div className="flex items-center gap-2">
          {current && !current.fresh && (
            <span className="text-xs text-danger">Last sample {ago(current.sampledAt)} ago</span>
          )}
          <Select
            value={range}
            onChange={setRange}
            options={resourceRangeOptions}
            ariaLabel="Runtime resource time range"
            size="sm"
          />
        </div>
      </div>

      {current && (
        <div className="mt-4 grid grid-cols-3 divide-x divide-line rounded-md border border-line/70 bg-surface/40 py-3">
          <RuntimeValue label="CPU" value={formatCPU(current.cpuCores)} />
          <RuntimeValue label="Memory" value={formatBytes(current.memoryBytes)} />
          <RuntimeValue label="Instances" value={integer.format(current.instances)} />
        </div>
      )}

      <div className="mt-4 grid gap-4 sm:grid-cols-2">
        <ResourceBars
          icon={<Cpu className="size-3.5" aria-hidden />}
          label="CPU"
          points={data.series}
          value={(point) => point.averageCpuCores}
          format={formatCPU}
          tone="bg-accent/80"
        />
        <ResourceBars
          icon={<MemoryStick className="size-3.5" aria-hidden />}
          label="Memory"
          points={data.series}
          value={(point) => point.averageMemoryBytes}
          format={formatBytes}
          tone="bg-success/75"
        />
      </div>
    </Card>
  )
}

function RuntimeValue({ label, value }: { label: string; value: string }) {
  return (
    <div className="min-w-0 px-3 first:pl-3">
      <div className="truncate font-mono text-lg font-bold text-ink">{value}</div>
      <div className="mt-0.5 text-[10px] uppercase tracking-wider text-muted">{label}</div>
    </div>
  )
}

function ResourceBars({
  icon,
  label,
  points,
  value,
  format,
  tone,
}: {
  icon: React.ReactNode
  label: string
  points: ResourcePoint[]
  value: (point: ResourcePoint) => number
  format: (value: number) => string
  tone: string
}) {
  const max = Math.max(0, ...points.map(value))
  const measured = points.filter((point) => point.samples > 0)
  const latest = measured.length ? value(measured.at(-1)!) : 0
  return (
    <div className="min-w-0">
      <div className="flex items-center justify-between gap-2 text-xs text-muted">
        <span className="flex items-center gap-1.5">{icon}{label} history</span>
        <span className="font-mono text-ink-soft">{measured.length ? format(latest) : '—'}</span>
      </div>
      <div
        className="mt-2 flex h-16 items-end gap-px"
        role="img"
        aria-label={`${label} average over the selected time range`}
      >
        {points.map((point) => {
          const amount = value(point)
          const height = point.samples === 0 || max === 0 ? 1 : Math.max(3, (amount / max) * 100)
          return (
            <span
              key={point.bucket}
              className={`min-w-0 flex-1 rounded-t-[1px] ${point.samples ? tone : 'bg-line/35'}`}
              style={{ height: `${height}%` }}
              title={point.samples ? `${point.bucket}: ${format(amount)}` : `${point.bucket}: no sample`}
            />
          )
        })}
      </div>
    </div>
  )
}

function formatCPU(cores: number) {
  if (!cores) return '0 cores'
  const digits = cores >= 10 ? 1 : cores >= 1 ? 2 : 3
  return `${cores.toFixed(digits).replace(/\.?0+$/, '')} cores`
}

function formatBytes(bytes: number) {
  if (!bytes) return '0 B'
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB']
  const power = Math.min(units.length - 1, Math.floor(Math.log(bytes) / Math.log(1024)))
  const value = bytes / 1024 ** power
  return `${value.toFixed(value >= 10 || power === 0 ? 0 : 1)} ${units[power]}`
}

function SourceButton({ active, onClick, children }: { active: boolean; onClick: () => void; children: React.ReactNode }) {
  return (
    <button
      type="button"
      aria-pressed={active}
      onClick={onClick}
      className={`whitespace-nowrap rounded px-2.5 py-1 text-xs transition-colors ${active ? 'bg-raised font-semibold text-ink' : 'text-muted hover:text-ink'}`}
    >
      {children}
    </button>
  )
}

function Metric({ label, value, note, bad = false }: { label: string; value: string; note?: string; bad?: boolean }) {
  return (
    <Card className={`min-h-24 min-w-0 p-3.5 ${bad ? 'border-danger/30' : ''}`}>
      <div className="text-[10px] font-semibold uppercase tracking-[0.12em] text-muted">{label}</div>
      <div className={`mt-1 truncate font-mono text-2xl font-bold ${bad ? 'text-danger' : 'text-ink'}`}>{value}</div>
      {note && <div className="mt-1 text-[10px] leading-4 text-muted">{note}</div>}
    </Card>
  )
}

function AudienceCard({
  totals,
  source,
  onShowGateway,
}: {
  totals: TrafficTotals
  source: TrafficSource
  onShowGateway: () => void
}) {
  if (source === 'app') {
    return (
      <Card className="flex min-h-24 flex-wrap items-center justify-between gap-3 px-4 py-3">
        <div>
          <SectionTitle>Audience</SectionTitle>
          <p className="mt-1 text-xs text-muted">
            Views and signed-in visitors are measured at the gateway, not by app metrics.
          </p>
        </div>
        <button
          type="button"
          onClick={onShowGateway}
          className="rounded-md border border-line px-2.5 py-1.5 text-xs text-ink-soft hover:bg-line/50 hover:text-ink focus-visible:outline-2 focus-visible:outline-accent-strong"
        >
          Show gateway data
        </button>
      </Card>
    )
  }

  return (
    <Card className="min-h-24 p-4">
      <div className="flex items-center justify-between gap-3">
        <SectionTitle>Audience</SectionTitle>
        <span className="font-mono text-[10px] uppercase tracking-wider text-muted">Gateway only</span>
      </div>
      <div className="mt-3 grid grid-cols-3 divide-x divide-line">
        <AudienceMetric label="Views" value={integer.format(totals.views)} />
        <AudienceMetric label="Signed-in visitors" value={integer.format(totals.uniqueVisitors)} />
        <AudienceMetric
          label="Identified views"
          value={totals.views === 0 ? '—' : `${totals.visitorCoveragePercent.toFixed(1)}%`}
        />
      </div>
    </Card>
  )
}

function AudienceMetric({ label, value }: { label: string; value: string }) {
  return (
    <div className="min-w-0 px-3 first:pl-0 last:pr-0">
      <div className="truncate font-mono text-lg font-bold text-ink">{value}</div>
      <div className="mt-0.5 text-[10px] leading-4 text-muted">{label}</div>
    </div>
  )
}

function DailyBars({ points }: { points: TrafficPoint[] }) {
  const max = Math.max(1, ...points.map((point) => point.requests))
  return (
    <div className="mt-4 flex h-32 items-end gap-[3px]" role="img" aria-label="Requests over the selected time range; error portions are red">
      {points.map((point) => {
        const errors = point.clientErrors + point.serverErrors
        const height = point.requests === 0 ? 1 : Math.max(4, (point.requests / max) * 100)
        const errorHeight = point.requests === 0 ? 0 : (errors / point.requests) * 100
        return (
          <div
            key={point.bucket}
            className="group relative flex min-w-0 flex-1 flex-col-reverse overflow-hidden rounded-t-sm bg-accent/80"
            style={{ height: `${height}%` }}
            title={`${point.bucket}: ${integer.format(point.requests)} requests, ${integer.format(errors)} errors`}
          >
            {errorHeight > 0 && <span className="block w-full bg-danger" style={{ height: `${errorHeight}%` }} />}
          </div>
        )
      })}
    </div>
  )
}

function bucketLabel(bucket: string | undefined, granularity: 'hour' | 'day') {
  if (!bucket) return ''
  if (granularity === 'day') return bucket
  return new Intl.DateTimeFormat(undefined, {
    month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', timeZoneName: 'short',
  }).format(new Date(bucket))
}

function successRate(totals: TrafficTotals) {
  if (totals.requests === 0) return '—'
  return `${((totals.successful / totals.requests) * 100).toFixed(1)}%`
}

function latency(ms: number) {
  if (!ms) return '—'
  if (ms >= 1000) return `${(ms / 1000).toFixed(ms >= 10_000 ? 0 : 1)} s`
  return `${ms.toFixed(ms >= 100 ? 0 : 1)} ms`
}
