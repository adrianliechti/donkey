import { useState } from 'react'
import { Dialog, DialogBackdrop, DialogPanel, DialogTitle } from '@headlessui/react'
import { Activity, X } from 'lucide-react'
import type { TrafficPoint, TrafficRange, TrafficSource, TrafficTotals } from './api'
import { useTraffic } from './queries'
import { Card, ErrorNote, SectionTitle, Select, Spinner, UpdatedAgo } from './ui'

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
        className={`inline-flex h-7 items-center gap-1.5 rounded-md px-1.5 font-mono text-[11px] transition-colors focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-hay-deep ${
          error
            ? 'text-brick hover:bg-brick/10'
            : 'text-stone hover:bg-line/60 hover:text-ink'
        }`}
      >
        <Activity className="size-3.5" aria-hidden />
        {data && <span>{compactInteger.format(data.totals.requests)}</span>}
        {errors > 0 && <span className="size-1.5 rounded-full bg-brick" aria-hidden />}
      </button>

      <Dialog open={open} onClose={setOpen} className="relative z-50">
        <DialogBackdrop className="fixed inset-0 bg-black/60" />
        <div className="fixed inset-0 flex justify-end">
          <DialogPanel className="flex h-full w-full max-w-4xl flex-col border-l border-line bg-surface shadow-[-24px_0_60px_rgba(0,0,0,.5)]">
            <div className="flex items-center justify-between gap-3 border-b border-line bg-card px-5 py-4">
              <div className="min-w-0">
                <div className="font-mono text-[10px] font-semibold uppercase tracking-[0.16em] text-stone">
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
                className="rounded-md p-1.5 text-stone hover:bg-raised hover:text-ink focus-visible:outline-2 focus-visible:outline-hay-deep"
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
  const { data, error, updatedAt } = useTraffic(space, app, source, range)

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
            <div className="flex items-center justify-between gap-3">
              <SectionTitle>Requests by {data.granularity}</SectionTitle>
              <UpdatedAgo at={updatedAt} />
            </div>
            {data.totals.requests > 0 ? (
              <>
                <DailyBars points={data.series} />
                <div className="mt-2 grid grid-cols-2 gap-x-3 gap-y-1 text-[10px] text-stone">
                  <span>{bucketLabel(data.series[0]?.bucket, data.granularity)}</span>
                  <span className="text-right">{bucketLabel(data.series.at(-1)?.bucket, data.granularity)}</span>
                  <span className="col-span-2 flex items-center justify-center gap-3">
                    <span><i className="mr-1 inline-block size-2 rounded-sm bg-hay" />requests</span>
                    <span><i className="mr-1 inline-block size-2 rounded-sm bg-brick" />4xx / 5xx</span>
                  </span>
                </div>
              </>
            ) : (
              <div className="flex h-36 flex-col items-center justify-center gap-2 text-center text-stone">
                <Activity className="size-5" aria-hidden />
                <p className="text-sm">No HTTP telemetry in this range.</p>
              </div>
            )}
          </Card>

          <details className="rounded-md border border-line/60 bg-surface/40 px-3 py-2 text-xs text-stone">
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

function SourceButton({ active, onClick, children }: { active: boolean; onClick: () => void; children: React.ReactNode }) {
  return (
    <button
      type="button"
      aria-pressed={active}
      onClick={onClick}
      className={`whitespace-nowrap rounded px-2.5 py-1 text-xs transition-colors ${active ? 'bg-raised font-semibold text-ink' : 'text-stone hover:text-ink'}`}
    >
      {children}
    </button>
  )
}

function Metric({ label, value, note, bad = false }: { label: string; value: string; note?: string; bad?: boolean }) {
  return (
    <Card className={`min-h-24 min-w-0 p-3.5 ${bad ? 'border-brick/30' : ''}`}>
      <div className="text-[10px] font-semibold uppercase tracking-[0.12em] text-stone">{label}</div>
      <div className={`mt-1 truncate font-mono text-2xl font-bold ${bad ? 'text-brick' : 'text-ink'}`}>{value}</div>
      {note && <div className="mt-1 text-[10px] leading-4 text-stone">{note}</div>}
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
          <p className="mt-1 text-xs text-stone">
            Views and signed-in visitors are measured at the gateway, not by app metrics.
          </p>
        </div>
        <button
          type="button"
          onClick={onShowGateway}
          className="rounded-md border border-line px-2.5 py-1.5 text-xs text-ink-soft hover:bg-line/50 hover:text-ink focus-visible:outline-2 focus-visible:outline-hay-deep"
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
        <span className="font-mono text-[10px] uppercase tracking-wider text-stone">Gateway only</span>
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
      <div className="mt-0.5 text-[10px] leading-4 text-stone">{label}</div>
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
            className="group relative flex min-w-0 flex-1 flex-col-reverse overflow-hidden rounded-t-sm bg-hay/80"
            style={{ height: `${height}%` }}
            title={`${point.bucket}: ${integer.format(point.requests)} requests, ${integer.format(errors)} errors`}
          >
            {errorHeight > 0 && <span className="block w-full bg-brick" style={{ height: `${errorHeight}%` }} />}
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
