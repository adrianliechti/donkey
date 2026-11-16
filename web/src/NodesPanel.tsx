import { Server } from 'lucide-react'
import type { NodeResource, SystemNode } from './api'
import { humanSize } from './format'
import { useNodes } from './queries'
import { cores, fullness, percent, share, sumResource, type Fullness } from './reports'
import { HeadCell, ListHead, PanelBar } from './ReportParts'
import { EmptyState, ErrorNote, Skeleton, StatGrid, StatusLine, panelClass } from './ui'

// The machines apps run on: a cluster's nodes, or the one host of a Docker
// engine — the runtime says which, this page never asks. Each row is a node
// with two meters, processors and memory: how much of what workloads can
// have is in use right now, and, where the runtime places workloads by what
// they ask for, how much of it is already spoken for.

// The list's columns, shared by its header and its rows. Narrower than md
// the meters drop under the name.
const columns = 'grid items-center gap-x-6 gap-y-2 md:grid-cols-[minmax(0,1.3fr)_minmax(0,1fr)_minmax(0,1fr)_5.5rem]'

// A meter's fill says how full: quiet in the accent, amber when there is
// little room left, red when there is none. The track is the same colour,
// faint, so the state reads across the whole bar.
const fill: Record<Fullness, { bar: string; track: string }> = {
  ok: { bar: 'bg-accent', track: 'bg-accent/15' },
  high: { bar: 'bg-warning', track: 'bg-warning/20' },
  full: { bar: 'bg-danger', track: 'bg-danger/20' },
}

type Unit = { amount: (value: number) => string; name: string }
const cpuUnit: Unit = { amount: cores, name: 'cores' }
const memoryUnit: Unit = { amount: humanSize, name: '' }

const figure = (unit: Unit, value: number) => (unit.name ? `${unit.amount(value)} ${unit.name}` : unit.amount(value))

// Meter is one resource of one node. In use is the bar; reserved is a tick
// on it, since that is a line the scheduler will not cross rather than
// something that is happening. A node whose usage nobody measures shows what
// is reserved as the bar instead, in the quiet ink — known, but not load.
function Meter({ label, resource, unit }: { label: string; resource: NodeResource; unit: Unit }) {
  const used = share(resource.used, resource.capacity)
  const reserved = share(resource.reserved, resource.capacity)
  const level = fullness(used)
  const look = used === undefined ? { bar: 'bg-ink-soft/60', track: 'bg-line' } : fill[level]
  const shown = used ?? reserved
  const words = [
    used !== undefined && resource.used !== undefined
      ? `${figure(unit, resource.used)} of ${figure(unit, resource.capacity)} in use (${percent(used)})`
      : `${figure(unit, resource.capacity)}, usage not measured`,
    reserved !== undefined && resource.reserved !== undefined
      ? `${figure(unit, resource.reserved)} reserved (${percent(reserved)})`
      : '',
  ]
    .filter(Boolean)
    .join(', ')
  return (
    <div className="min-w-0" title={`${label}: ${words}`}>
      <span className="sr-only">
        {label}: {words}
      </span>
      <div aria-hidden>
        <div className="flex items-baseline justify-between gap-2 text-xs">
          <span className="truncate text-ink-soft">
            {/* The label is in the column header from md up. */}
            <span className="text-muted md:hidden">{label} </span>
            {resource.used !== undefined ? (
              <>
                <span className="font-mono text-ink">{unit.amount(resource.used)}</span> of{' '}
                {figure(unit, resource.capacity)}
              </>
            ) : (
              <span className="font-mono text-ink">{figure(unit, resource.capacity)}</span>
            )}
          </span>
          {used !== undefined && (
            <span
              className={`shrink-0 font-mono ${level === 'full' ? 'text-danger' : level === 'high' ? 'text-warning' : 'text-ink-soft'}`}
            >
              {percent(used)}
            </span>
          )}
        </div>
        <div className={`relative mt-1.5 h-1.5 rounded-full ${look.track}`}>
          {shown !== undefined && shown > 0 && (
            <div className={`h-full min-w-1 rounded-full ${look.bar}`} style={{ width: percent(shown) }} />
          )}
          {used !== undefined && reserved !== undefined && (
            <div
              className="absolute -top-0.5 h-2.5 w-0.5 -translate-x-1/2 rounded-full bg-ink ring-1 ring-card"
              style={{ left: percent(reserved) }}
            />
          )}
        </div>
        <div className="mt-1 h-4 truncate text-xs text-muted">
          {reserved !== undefined ? `${percent(reserved)} reserved` : used === undefined ? 'usage not measured' : ''}
        </div>
      </div>
    </div>
  )
}

// What a node's row says about its state: what is wrong first, then that it
// is closed to new workloads, and otherwise only a dot.
function nodeState(node: SystemNode): { tone: 'danger' | 'warning' | 'success'; label: string; news: boolean } {
  const problems = node.problems ?? []
  if (!node.ready) return { tone: 'danger', label: problems[0] ?? 'Not ready', news: true }
  if (problems.length > 0) return { tone: 'warning', label: problems.join(', '), news: true }
  if (!node.schedulable) return { tone: 'warning', label: 'Closed to new workloads', news: true }
  return { tone: 'success', label: 'Ready', news: false }
}

function NodeRow({ node }: { node: SystemNode }) {
  const state = nodeState(node)
  const facts = [...(node.roles ?? []), node.version, [node.os, node.arch].filter(Boolean).join(' · ')].filter(Boolean)
  return (
    <li className={`px-4 py-3 ${columns}`}>
      <div className="min-w-0">
        <div className="flex min-w-0 items-center gap-2">
          <span className="truncate font-mono text-sm font-semibold text-ink" title={node.name}>
            {node.name}
          </span>
          <StatusLine tone={state.tone} label={state.label} quiet={!state.news} />
        </div>
        <p className="truncate text-xs text-muted" title={facts.join(' · ')}>
          {facts.join(' · ')}
        </p>
        {/* Everything that is wrong, when the line above only had room for one. */}
        {(node.problems ?? []).length > 1 && (
          <p className="mt-0.5 text-xs text-danger">{(node.problems ?? []).slice(1).join(' · ')}</p>
        )}
      </div>
      <Meter label="Processors" resource={node.cpu} unit={cpuUnit} />
      <Meter label="Memory" resource={node.memory} unit={memoryUnit} />
      <div className="text-xs text-ink-soft md:text-right">
        <span className="text-muted md:hidden">Instances </span>
        <span className="font-mono text-ink">{node.instances}</span>
        {node.instanceCapacity ? <span className="text-muted"> of {node.instanceCapacity}</span> : null}
      </div>
    </li>
  )
}

// One cell of the totals. The share in use is the headline, since a long
// figure would not fit a narrow cell; how much that is, and what is
// reserved, is the line under it.
function total(resource: NodeResource, unit: Unit) {
  const used = share(resource.used, resource.capacity)
  const reserved = share(resource.reserved, resource.capacity)
  const spoken = reserved !== undefined ? `${percent(reserved)} reserved` : ''
  if (used === undefined || resource.used === undefined) {
    return {
      value: figure(unit, resource.capacity),
      note: ['usage not measured', spoken].filter(Boolean).join(' · '),
    }
  }
  return {
    value: percent(used),
    note: [`${unit.amount(resource.used)} of ${figure(unit, resource.capacity)} in use`, spoken]
      .filter(Boolean)
      .join(' · '),
    tone: fullness(used) === 'full' ? ('danger' as const) : undefined,
  }
}

export function NodesPanel() {
  const { data, error, updatedAt, reload } = useNodes()
  const nodes = data?.nodes ?? []
  const troubled = nodes.filter((node) => !node.ready)
  const cpu = sumResource(nodes.map((node) => node.cpu))
  const memory = sumResource(nodes.map((node) => node.memory))
  const instances = nodes.reduce((sum, node) => sum + node.instances, 0)
  const capacity = nodes.every((node) => node.instanceCapacity)
    ? nodes.reduce((sum, node) => sum + (node.instanceCapacity ?? 0), 0)
    : 0

  return (
    <>
      <PanelBar updatedAt={updatedAt} onReload={reload}>
        {troubled.length > 0 && (
          <StatusLine size="md" tone="danger" label={`${troubled.length} of ${nodes.length} not ready`} />
        )}
      </PanelBar>
      <ErrorNote message={error} />
      {!data && !error && (
        <>
          <Skeleton className="h-20" />
          <Skeleton className="h-40" />
        </>
      )}
      {data && nodes.length === 0 && (
        <div className="rounded-lg border border-dashed border-line">
          <EmptyState icon={Server} title={data.note ? 'The nodes are not this installation’s to see' : 'No nodes'}>
            {data.note ?? 'The runtime reports no machine to run apps on.'}
          </EmptyState>
        </div>
      )}
      {nodes.length > 0 && (
        <>
          <StatGrid
            stats={[
              {
                label: 'Nodes',
                value: nodes.length,
                note: troubled.length > 0 ? `${troubled.length} not ready` : nodes.length === 1 ? 'ready' : 'all ready',
                tone: troubled.length > 0 ? 'danger' : undefined,
              },
              { label: 'Processors', ...total(cpu, cpuUnit) },
              { label: 'Memory', ...total(memory, memoryUnit) },
              {
                label: 'Instances',
                value: instances,
                note: capacity ? `room for ${capacity}` : 'running on the node',
              },
            ]}
          />
          <section aria-label="Nodes" className={`overflow-hidden ${panelClass}`}>
            <ListHead className={`max-md:hidden ${columns}`}>
              <HeadCell>Node</HeadCell>
              <HeadCell>Processors</HeadCell>
              <HeadCell>Memory</HeadCell>
              <HeadCell className="text-right">Instances</HeadCell>
            </ListHead>
            <ul className="divide-y divide-line">
              {nodes.map((node) => (
                <NodeRow key={node.name} node={node} />
              ))}
            </ul>
          </section>
          {nodes.some((node) => node.cpu.reserved !== undefined) && (
            <p className="text-xs leading-relaxed text-muted">
              The bar is what a node uses right now; the mark on it is what its workloads have reserved. A new app needs
              room past the mark, however idle the node is.
            </p>
          )}
        </>
      )}
    </>
  )
}
