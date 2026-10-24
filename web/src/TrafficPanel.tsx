import { useState } from 'react'
import { Cpu, MemoryStick } from 'lucide-react'
import type { ResourcePoint, ResourceRange, TrafficPoint, TrafficRange, TrafficSource, TrafficTotals } from './api'
import { useResources, useTraffic } from './queries'
import { Drawer, DrawerBody, PaneHeader } from './dialog'
import { ErrorNote, Eyebrow, Section, SectionHeader, Select, Spinner, StatGrid } from './ui'
import { ago } from './format'

const integer = new Intl.NumberFormat()
const rangeOptions: { value: TrafficRange; label: string }[] = [
  { value: '48h', label: 'last 48 hours' },
  { value: '7d', label: 'last 7 days' },
  { value: '30d', label: 'last 30 days' },
]
const resourceRangeOptions: { value: ResourceRange; label: string }[] = [
  { value: '1h', label: 'last hour' },
  { value: '4h', label: 'last 4 hours' },
  { value: '48h', label: 'last 48 hours' },
  { value: '7d', label: 'last 7 days' },
  { value: '30d', label: 'last 30 days' },
]

/**
 * A whole space's traffic, opened from its ⋯ menu in the breadcrumb. The
 * report is only asked for while the drawer is open.
 */
export function SpaceTrafficDrawer({ space, open, onClose }: { space: string; open: boolean; onClose: () => void }) {
  return (
    <Drawer open={open} onClose={onClose} label={`${space} — traffic`} width="max-w-4xl">
      <PaneHeader prominent eyebrow="Space traffic" title={space} onClose={onClose} closeLabel="Close space traffic" />
      <DrawerBody>{open && <TrafficPanel space={space} app="" />}</DrawerBody>
    </Drawer>
  )
}

/**
 * Requests as donkey-gateway saw them: volume, errors and latency. In an
 * app's Analytics tab the range belongs to the tab: given with onRange, this
 * section shows the tab's picker; given alone, the audience above shows it.
 * The space drawer keeps a picker of its own.
 */
export function TrafficPanel({
  space,
  app,
  range: shared,
  onRange,
}: {
  space: string
  app: string
  range?: TrafficRange
  onRange?: (range: TrafficRange) => void
}) {
  const [own, setOwn] = useState<TrafficRange>('30d')
  const range = shared ?? own
  const setRange = onRange ?? setOwn
  const [source, setSource] = useState<TrafficSource>('edge')
  const { data, error } = useTraffic(space, app, source, range)
  const errors = data ? data.totals.clientErrors + data.totals.serverErrors : 0

  return (
    <>
      <Section className="space-y-5">
        <SectionHeader
          title="Traffic"
          description={
            source === 'edge'
              ? 'HTTP traffic through the gateway, including sign-in and access responses.'
              : 'HTTP traffic reported by the application.'
          }
          actions={
            <>
              <Select
                variant="quiet"
                ariaLabel="Traffic source"
                value={source}
                onChange={setSource}
                options={[
                  { value: 'edge', label: 'gateway' },
                  { value: 'app', label: 'application' },
                ]}
              />
              {(!shared || onRange) && (
                <Select
                  variant="quiet"
                  ariaLabel="Time range"
                  value={range}
                  onChange={setRange}
                  options={rangeOptions}
                />
              )}
            </>
          }
        />
        <ErrorNote message={error} />
        {!data && !error && <Spinner label="Reading telemetry…" />}
        {data && (
          <>
            <StatGrid
              stats={[
                { label: 'Requests', value: integer.format(data.totals.requests), note: 'HTTP responses' },
                {
                  label: 'Successful',
                  value: integer.format(data.totals.successful),
                  note: `${successRate(data.totals)} · HTTP 2xx–3xx`,
                },
                {
                  label: 'Errors',
                  value: integer.format(errors),
                  note: `${integer.format(data.totals.clientErrors)} 4xx · ${integer.format(data.totals.serverErrors)} 5xx`,
                  tone: errors > 0 ? 'danger' : undefined,
                },
                {
                  label: 'P95 latency',
                  value: latency(data.totals.p95LatencyMs),
                  note: `p50 ${latency(data.totals.p50LatencyMs)} · avg ${latency(data.totals.averageLatencyMs)}`,
                },
              ]}
            />

            {source === 'edge' && (
              <p className="text-xs text-muted" title={data.definitions.authenticatedCallers}>
                <span className="font-mono text-ink">{integer.format(data.totals.authenticatedCallers ?? 0)}</span>{' '}
                distinct authenticated callers · anonymous callers excluded
              </p>
            )}

            <div className="space-y-2">
              <div className="flex items-center justify-between gap-3">
                <Eyebrow>Requests by {data.granularity}</Eyebrow>
                <span className="flex items-center gap-3 text-[11px] text-muted">
                  <span className="flex items-center gap-1">
                    <i className="inline-block size-2 rounded-sm bg-accent" />
                    requests
                  </span>
                  <span className="flex items-center gap-1">
                    <i className="inline-block size-2 rounded-sm bg-danger" />
                    4xx / 5xx
                  </span>
                </span>
              </div>
              {data.totals.requests > 0 ? (
                <>
                  <DailyBars points={data.series} />
                  <div className="flex justify-between gap-3 text-[11px] text-muted">
                    <span>{bucketLabel(data.series[0]?.bucket, data.granularity)}</span>
                    <span>{bucketLabel(data.series.at(-1)?.bucket, data.granularity)}</span>
                  </div>
                </>
              ) : (
                <p className="py-8 text-center text-sm text-muted">No HTTP telemetry in this range.</p>
              )}
            </div>
            {!!data.endpoints?.length && (
              <div className="space-y-2">
                <Eyebrow>Top HTTP routes</Eyebrow>
                <div className="overflow-x-auto">
                  <table className="w-full text-left text-xs">
                    <thead className="text-muted">
                      <tr>
                        {!app && <th className="py-2 pr-3 font-normal">App</th>}
                        <th className="py-2 pr-3 font-normal">Route</th>
                        <th className="py-2 px-3 text-right font-normal">Requests</th>
                        <th className="py-2 px-3 text-right font-normal">4xx / 5xx</th>
                        <th className="py-2 pl-3 text-right font-normal">P95</th>
                      </tr>
                    </thead>
                    <tbody>
                      {data.endpoints.map((row) => (
                        <tr key={`${row.app}:${row.method}:${row.route}`} className="border-t border-line/50">
                          {!app && <td className="py-2 pr-3">{row.app}</td>}
                          <td className="py-2 pr-3 font-mono">
                            {row.method} {row.route || 'Unknown route'}
                          </td>
                          <td className="py-2 px-3 text-right font-mono">{integer.format(row.requests)}</td>
                          <td className="py-2 px-3 text-right font-mono">
                            {integer.format(row.clientErrors)} / {integer.format(row.serverErrors)}
                          </td>
                          <td className="py-2 pl-3 text-right font-mono">{latency(row.p95LatencyMs)}</td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              </div>
            )}
          </>
        )}
      </Section>

      {!!data?.mcp?.totals.operations && (
        <Section className="space-y-4">
          <SectionHeader
            title="MCP operations"
            description="Operations reported by MCP servers, including tool calls, prompts and resources."
          />
          <StatGrid
            stats={[
              { label: 'Operations', value: integer.format(data.mcp.totals.operations) },
              {
                label: 'Operation errors',
                value: integer.format(data.mcp.totals.errors),
                note: 'Tool, server and transport failures',
              },
              {
                label: 'Caller errors',
                value: integer.format(data.mcp.totals.callerErrors),
                note: 'Invalid requests or missing resources',
              },
              {
                label: 'P95 latency',
                value: latency(data.mcp.totals.p95LatencyMs),
                note: `avg ${latency(data.mcp.totals.averageLatencyMs)}`,
              },
            ]}
          />
          <div className="overflow-x-auto">
            <table className="w-full text-left text-xs">
              <thead className="text-muted">
                <tr>
                  {!app && <th className="py-2 pr-3 font-normal">App</th>}
                  <th className="py-2 pr-3 font-normal">Operation</th>
                  <th className="py-2 px-3 text-right font-normal">Count</th>
                  <th className="py-2 px-3 text-right font-normal">Errors</th>
                  <th className="py-2 px-3 text-right font-normal">Caller errors</th>
                  <th className="py-2 pl-3 text-right font-normal">P95</th>
                </tr>
              </thead>
              <tbody>
                {data.mcp.operations.map((row) => (
                  <tr key={`${row.app}:${row.method}:${row.target}`} className="border-t border-line/50">
                    {!app && <td className="py-2 pr-3">{row.app}</td>}
                    <td className="py-2 pr-3 font-mono">
                      {row.method}
                      {row.target && ` ${row.target}`}
                    </td>
                    <td className="py-2 px-3 text-right font-mono">{integer.format(row.operations)}</td>
                    <td className="py-2 px-3 text-right font-mono">{integer.format(row.errors)}</td>
                    <td className="py-2 px-3 text-right font-mono">{integer.format(row.callerErrors)}</td>
                    <td className="py-2 pl-3 text-right font-mono">{latency(row.p95LatencyMs)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </Section>
      )}

      {app && <ResourceUsage space={space} app={app} />}
    </>
  )
}

function ResourceUsage({ space, app }: { space: string; app: string }) {
  const [range, setRange] = useState<ResourceRange>('4h')
  const { data, error } = useResources(space, app, range)
  const current = data?.current

  return (
    <Section className="space-y-4">
      <SectionHeader
        title="Runtime resources"
        description="Container usage across the app’s running instances."
        actions={
          <>
            {current && !current.fresh && (
              <span className="text-xs text-danger">Last sample {ago(current.sampledAt)} ago</span>
            )}
            <Select
              variant="quiet"
              ariaLabel="Runtime resource time range"
              value={range}
              onChange={setRange}
              options={resourceRangeOptions}
            />
          </>
        }
      />
      {error && <ErrorNote message={`Runtime usage is unavailable: ${error}`} />}
      {!data && !error && <Spinner label="Reading runtime usage…" />}
      {data && !data.available && (
        <p className="text-sm text-muted">
          No container CPU or memory samples yet. Usage appears after the next runtime sample.
        </p>
      )}
      {data?.available && (
        <>
          {current && (
            <StatGrid
              columns="grid-cols-3"
              stats={[
                { label: 'CPU', value: formatCPU(current.cpuCores) },
                { label: 'Memory', value: formatBytes(current.memoryBytes) },
                { label: 'Instances', value: integer.format(current.instances) },
              ]}
            />
          )}
          <div className="grid gap-4 sm:grid-cols-2">
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
        </>
      )}
    </Section>
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
        <span className="flex items-center gap-1.5">
          {icon}
          {label} history
        </span>
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

function DailyBars({ points }: { points: TrafficPoint[] }) {
  const max = Math.max(1, ...points.map((point) => point.requests))
  return (
    <div
      className="flex h-32 items-end gap-[3px]"
      role="img"
      aria-label="Requests over the selected time range; error portions are red"
    >
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
  if (granularity === 'day') {
    const day = new Date(bucket)
    return Number.isNaN(day.getTime())
      ? bucket
      : new Intl.DateTimeFormat(undefined, { dateStyle: 'medium' }).format(day)
  }
  return new Intl.DateTimeFormat(undefined, {
    month: 'short',
    day: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
    timeZoneName: 'short',
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
