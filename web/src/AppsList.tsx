import { useState } from 'react'
import { Dialog, DialogBackdrop, DialogPanel, DialogTitle } from '@headlessui/react'
import { FolderPlus, RefreshCw } from 'lucide-react'
import { api, type AppSummary, type SpaceConfig } from './api'
import { useFleet } from './queries'
import { SpaceCard } from './SpaceCard'
import { SpaceConfigEditor } from './SpaceSettings'
import { Button, DonkeyLogo, ErrorNote, Skeleton, StatusDot, errText, healthOf, type Health } from './ui'

const healthRank: Record<Health, number> = { bad: 0, unknown: 1, ok: 2 }

export function AppsList() {
  const { data, error, reload } = useFleet()
  const apps = data?.apps ?? null
  const [creating, setCreating] = useState(false)
  // Triage: the fleet is where an operator asks "is anything wrong?" — so it
  // can narrow to just the apps that need attention rather than scanning every
  // card. Off by default; the summary counts stay honest either way.
  const [onlyProblems, setOnlyProblems] = useState(false)

  // Apps belong to a space; the paddock mirrors that hierarchy instead of
  // flattening everything into one wall of cards. Spaces without apps are
  // real too — they may hold services, or were just created for them.
  const grouped = new Map<string, AppSummary[]>()
  for (const app of apps ?? []) {
    const space = app.space || 'default'
    grouped.set(space, [...(grouped.get(space) ?? []), app])
  }
  for (const space of data?.spaces ?? []) {
    if (!grouped.has(space)) grouped.set(space, [])
  }
  const spaces = [...grouped.entries()].sort(([a], [b]) => a.localeCompare(b))

  for (const [, list] of spaces) {
    list.sort((a, b) => {
      const ra = healthRank[healthOf(a.healthy, a.ready)]
      const rb = healthRank[healthOf(b.healthy, b.ready)]
      return ra !== rb ? ra - rb : a.name.localeCompare(b.name)
    })
  }

  // Fleet-wide health at a glance. "attention" is the triage bucket: down or
  // status-unknown, the two an operator would want to look at.
  const counts = { ok: 0, attention: 0 }
  for (const app of apps ?? []) {
    if (healthOf(app.healthy, app.ready) === 'ok') counts.ok++
    else counts.attention++
  }
  const total = counts.ok + counts.attention

  // With the filter on, keep only spaces that hold an app needing attention,
  // and within them only those apps — everything healthy steps out of the way.
  // The full per-space list is kept alongside so a space's delete confirmation
  // still counts every app it would destroy, not just the ones on screen.
  const fullBySpace = new Map(spaces)
  const shownSpaces = onlyProblems
    ? spaces
        .map(([space, list]) => [space, list.filter((a) => healthOf(a.healthy, a.ready) !== 'ok')] as const)
        .filter(([, list]) => list.length > 0)
    : spaces

  return (
    <div className="space-y-5">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="flex flex-wrap items-center gap-x-3 gap-y-1.5">
          <h1 className="font-mono text-[11px] font-semibold uppercase tracking-[0.18em] text-muted">
            Fleet
          </h1>
          {total > 0 && (
            <FleetSummary
              ok={counts.ok}
              attention={counts.attention}
              onlyProblems={onlyProblems}
              onToggle={() => setOnlyProblems((v) => !v)}
            />
          )}
        </div>
        <div className="flex items-center gap-0.5">
          {data?.platformAdmin && <button
            type="button"
            onClick={() => setCreating(true)}
            title="New space"
            aria-label="New space"
            className="rounded-md p-1.5 text-muted hover:bg-raised focus-visible:outline-2 focus-visible:outline-accent-strong"
          >
            <FolderPlus className="size-4" aria-hidden />
          </button>}
          <button
            type="button"
            onClick={reload}
            title="Refresh"
            aria-label="Refresh"
            className="rounded-md p-1.5 text-muted hover:bg-raised focus-visible:outline-2 focus-visible:outline-accent-strong"
          >
            <RefreshCw className="size-4" aria-hidden />
          </button>
        </div>
      </div>

      <ErrorNote message={error} />

      {apps === null && !error && (
        <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
          <Skeleton className="h-32" />
          <Skeleton className="h-32" />
          <Skeleton className="h-32" />
        </div>
      )}

      {onlyProblems && shownSpaces.length === 0 && (
        <div className="rounded-xl border border-line bg-card p-8 text-center">
          <p className="flex items-center justify-center gap-2 text-sm text-ink">
            <StatusDot health="ok" /> Nothing needs attention — all {total} apps are running.
          </p>
          <button
            type="button"
            onClick={() => setOnlyProblems(false)}
            className="mt-2 text-sm text-accent hover:text-accent-strong"
          >
            Show the whole fleet
          </button>
        </div>
      )}

      {apps !== null && spaces.length === 0 && !data?.platformAdmin && (
        <p className="rounded-xl border border-line bg-card p-8 text-center text-sm text-muted">You have no spaces yet. Ask an platform administrator to create a space and give you access.</p>
      )}
      {apps !== null && spaces.length === 0 && data?.platformAdmin && (
        <div className="rounded-xl border border-line bg-card p-12 text-center">
          <DonkeyLogo className="mx-auto size-10 text-muted" />
          <p className="mt-3 text-sm text-muted">The paddock is empty.</p>
          <p className="mt-2 text-sm">
            Deploy your first app from its project folder:{' '}
            <code className="rounded bg-surface px-2 py-0.5 font-mono text-sm">donkey deploy</code>
          </p>
          <p className="mt-1 text-sm text-muted">— or make a space, then add an app to it:</p>
          <button
            type="button"
            onClick={() => setCreating(true)}
            className="mt-3 inline-flex items-center gap-1.5 rounded-md border border-line px-3 py-1.5 text-sm text-ink-soft transition-colors hover:bg-line/50 focus-visible:outline-2 focus-visible:outline-accent-strong"
          >
            <FolderPlus className="size-4" aria-hidden /> New space
          </button>
        </div>
      )}

      {shownSpaces.length > 0 && (
        <div className="divide-y divide-line/70 border-t border-line/70">
          {shownSpaces.map(([space, spaceApps]) => (
            <SpaceCard
              key={space}
              space={space}
              apps={spaceApps}
              allApps={fullBySpace.get(space) ?? spaceApps}
              role={data?.roles[space] ?? 'developer'}
              onChanged={reload}
            />
          ))}
        </div>
      )}

      <NewSpaceDialog
        key={String(creating)}
        domain={data?.domain ?? ''}
        open={creating}
        onClose={() => setCreating(false)}
        onCreated={() => {
          setCreating(false)
          reload()
        }}
      />
    </div>
  )
}

/**
 * The fleet's health in one line: how many apps are running, how many want a
 * look, and a one-click switch to just those. When everything is up it reads
 * as a calm all-clear rather than a wall of green.
 */
function FleetSummary({
  ok,
  attention,
  onlyProblems,
  onToggle,
}: {
  ok: number
  attention: number
  onlyProblems: boolean
  onToggle: () => void
}) {
  // When everything is healthy, the fleet says nothing next to its title —
  // the summary earns its place only when something needs a look.
  if (attention === 0) return null
  return (
    <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-sm">
      <span className="flex items-center gap-1.5 font-medium text-danger">
        <StatusDot health="bad" />
        {attention} {attention === 1 ? 'app needs attention' : 'apps need attention'}
      </span>
      {ok > 0 && (
        <span className="flex items-center gap-1.5 text-ink-soft">
          <StatusDot health="ok" />
          {ok} running
        </span>
      )}
      <button
        type="button"
        onClick={onToggle}
        aria-pressed={onlyProblems}
        className={`rounded-md border px-2 py-0.5 text-xs transition-colors focus-visible:outline-2 focus-visible:outline-accent-strong ${
          onlyProblems
            ? 'border-accent-strong bg-accent-soft text-accent-strong'
            : 'border-line text-ink-soft hover:border-line-strong hover:text-ink'
        }`}
      >
        {onlyProblems ? 'Showing problems' : 'Only problems'}
      </button>
    </div>
  )
}

/**
 * A space usually appears by deploying into it; this makes one ahead of
 * time — a home for services (or teammates) before any app arrives.
 */
function NewSpaceDialog({
  domain,
  open,
  onClose,
  onCreated,
}: {
  domain: string
  open: boolean
  onClose: () => void
  onCreated: () => void
}) {
  const [name, setName] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [settings, setSettings] = useState<SpaceConfig>({ admins: {}, developers: {}, domains: [] })
  const [customDomains, setCustomDomains] = useState(false)
  const defaults = name.trim() && domain ? [`${name.trim()}.${domain}`] : []
  const effective = { ...settings, domains: customDomains ? settings.domains : defaults }

  async function create() {
    if (['donkey', 'platform', 'system'].includes(name.trim())) {
      setError('This name is reserved for Donkey. Choose another space name.')
      return
    }
    setBusy(true)
    setError('')
    try {
      await api.createSpace(name.trim(), effective)
      setName('')
      onCreated()
    } catch (err) {
      setError(errText(err, 'Creating did not work.'))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Dialog open={open} onClose={() => !busy && onClose()} className="relative z-50">
      <DialogBackdrop
        transition
        className="fixed inset-0 bg-black/60 transition-opacity duration-(--duration-base) ease-emphasized data-closed:opacity-0 data-leave:duration-(--duration-fast) data-leave:ease-exit"
      />
      <div className="fixed inset-0 overflow-y-auto p-4 pt-[8vh]">
        <DialogPanel
          transition
          className="mx-auto w-full max-w-lg rounded-xl border border-line bg-card p-6 shadow-2xl transition duration-(--duration-base) ease-emphasized data-closed:opacity-0 data-closed:scale-95 data-leave:duration-(--duration-fast) data-leave:ease-exit"
        >
          <DialogTitle className="text-lg font-semibold tracking-tight">Create space</DialogTitle>
          <p className="mt-1 text-sm text-muted">A home for your apps and services.</p>
          <ErrorNote message={error} />
          <form
            onSubmit={(e) => {
              e.preventDefault()
              if (name.trim()) create()
            }}
            className="mt-6 flex flex-col gap-5"
          >
            <label className="block text-sm font-medium">
              Space name
              <input
                autoFocus
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="my-project"
                pattern="[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?"
                maxLength={40}
                required
                title="Use lowercase letters, digits and dashes, starting and ending with a letter or digit."
                aria-describedby="space-name-help"
                aria-label="Space name"
                autoCapitalize="none"
                spellCheck={false}
                disabled={busy}
                className="mt-2 block w-full rounded-lg border border-line bg-surface px-3 py-2.5 font-mono text-sm font-normal focus:outline-2 focus:outline-accent-strong"
              />
              <span id="space-name-help" className="mt-2 block text-xs font-normal text-muted">Lowercase letters, digits and dashes, up to 40 characters.</span>
            </label>
            <fieldset disabled={busy}>
              <SpaceConfigEditor creating settings={effective} domainPlaceholder={!customDomains && !name.trim() && domain ? `<space>.${domain}` : undefined} onChange={(next) => {
                if (next.domains !== effective.domains) setCustomDomains(true)
                setSettings(next)
              }} />
            </fieldset>
            <div className="flex justify-end gap-2">
              <Button kind="quiet" onClick={onClose} disabled={busy}>
                Cancel
              </Button>
              <Button type="submit" disabled={busy || name.trim() === ''}>
                {busy ? 'Creating…' : 'Create space'}
              </Button>
            </div>
          </form>
        </DialogPanel>
      </div>
    </Dialog>
  )
}
