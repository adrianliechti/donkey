import { useCallback, useState } from 'react'
import { Dialog, DialogBackdrop, DialogPanel, DialogTitle } from '@headlessui/react'
import { FolderPlus, RefreshCw } from 'lucide-react'
import { api, type AppSummary } from './api'
import { usePoll } from './hooks'
import { RemoveApps } from './RemoveApps'
import { SpaceCard } from './SpaceCard'
import { Button, ErrorNote, Skeleton, errText, healthLine, healthOf, type Health } from './ui'

const healthRank: Record<Health, number> = { bad: 0, unknown: 1, ok: 2 }

export function AppsList({ onOpen }: { onOpen: (space: string, app: string) => void }) {
  const fetcher = useCallback(async () => {
    const [apps, spaces] = await Promise.all([api.apps(), api.spaces()])
    return { apps: apps.apps, spaces: spaces.spaces }
  }, [])
  const { data, error, reload } = usePoll(fetcher, 10_000)
  const apps = data?.apps ?? null
  // What the remove dialog is asking about: one app, or a whole space.
  const [removing, setRemoving] = useState<{ space: string; apps: string[]; siblings: string[] } | null>(null)
  const [creating, setCreating] = useState(false)

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

  const all = apps ?? []
  const troubled = all.filter((a) => healthOf(a.healthy, a.ready) === 'bad')
  const summary =
    all.length === 0
      ? ''
      : troubled.length === 0
        ? `${all.length} ${all.length === 1 ? 'app' : 'apps'} in ${spaces.length} ${spaces.length === 1 ? 'space' : 'spaces'} — all running.`
        : `${troubled.length} of ${all.length} ${all.length === 1 ? 'app' : 'apps'} ${troubled.length === 1 ? 'needs' : 'need'} attention.`

  return (
    <div className="space-y-5">
      <div className="flex items-center justify-between">
        <h1 className="font-mono text-[11px] font-semibold uppercase tracking-[0.18em] text-stone">
          Fleet
        </h1>
        <div className="flex items-center gap-0.5">
          <button
            type="button"
            onClick={() => setCreating(true)}
            title="New space"
            aria-label="New space"
            className="rounded-md p-1.5 text-stone hover:bg-raised focus-visible:outline-2 focus-visible:outline-hay-deep"
          >
            <FolderPlus className="size-4" aria-hidden />
          </button>
          <button
            type="button"
            onClick={reload}
            title="Refresh"
            aria-label="Refresh"
            className="rounded-md p-1.5 text-stone hover:bg-raised focus-visible:outline-2 focus-visible:outline-hay-deep"
          >
            <RefreshCw className="size-4" aria-hidden />
          </button>
        </div>
      </div>

      {/* The herd at a glance: one tick per app, colored by health. */}
      {all.length > 0 && (
        <div>
          <div className="flex flex-wrap gap-1" role="list" aria-label="All apps by health">
            {[...all]
              .sort((a, b) => healthRank[healthOf(a.healthy, a.ready)] - healthRank[healthOf(b.healthy, b.ready)])
              .map((app) => {
                const h = healthOf(app.healthy, app.ready)
                const tone =
                  h === 'ok'
                    ? 'bg-moss/70 hover:bg-moss'
                    : h === 'bad'
                      ? 'bg-brick shadow-[0_0_10px_1px] shadow-brick/40'
                      : 'bg-stone/50'
                return (
                  <button
                    key={`${app.space}/${app.name}`}
                    role="listitem"
                    title={`${app.name} (${app.space}) — ${healthLine[h](app.ready)}`}
                    onClick={() => onOpen(app.space, app.name)}
                    className={`h-3.5 w-6 rounded-sm ${tone} transition-transform hover:scale-y-125 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-hay-deep`}
                  />
                )
              })}
          </div>
          <p className="mt-1.5 text-sm text-ink-soft">{summary}</p>
        </div>
      )}

      <ErrorNote message={error} />

      {apps === null && !error && (
        <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
          <Skeleton className="h-32" />
          <Skeleton className="h-32" />
          <Skeleton className="h-32" />
        </div>
      )}

      {apps !== null && spaces.length === 0 && (
        <div className="rounded-xl border border-line bg-card p-12 text-center">
          <div className="text-4xl" aria-hidden>
            🫏
          </div>
          <p className="mt-3 text-sm text-stone">The paddock is empty.</p>
          <p className="mt-2 text-sm">
            Deploy your first app from its project folder:{' '}
            <code className="rounded bg-surface px-2 py-0.5 font-mono text-sm">donkey deploy</code>
          </p>
          <p className="mt-1 text-sm text-stone">
            — or use <span className="font-medium text-ink">Deploy</span> above and drop the project
            folder right here.
          </p>
        </div>
      )}

      {spaces.map(([space, spaceApps]) => (
        <SpaceCard key={space} space={space} apps={spaceApps} onOpen={onOpen} onRemove={setRemoving} />
      ))}

      {removing && (
        <RemoveApps
          space={removing.space}
          apps={removing.apps}
          siblings={removing.siblings}
          onDone={() => {
            setRemoving(null)
            reload()
          }}
          onCancel={() => setRemoving(null)}
        />
      )}

      <NewSpaceDialog
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
 * A space usually appears by deploying into it; this makes one ahead of
 * time — a home for services (or teammates) before any app arrives.
 */
function NewSpaceDialog({
  open,
  onClose,
  onCreated,
}: {
  open: boolean
  onClose: () => void
  onCreated: () => void
}) {
  const [name, setName] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  async function create() {
    setBusy(true)
    setError('')
    try {
      await api.createSpace(name.trim())
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
      <DialogBackdrop className="fixed inset-0 bg-black/60" />
      <div className="fixed inset-0 flex items-start justify-center p-4 pt-[18vh]">
        <DialogPanel className="w-full max-w-sm rounded-lg border border-line bg-card p-4 shadow-2xl">
          <DialogTitle className="font-mono text-[11px] font-semibold uppercase tracking-[0.14em] text-stone">
            New space
          </DialogTitle>
          <ErrorNote message={error} />
          <form
            onSubmit={(e) => {
              e.preventDefault()
              if (name.trim()) create()
            }}
            className="mt-3 flex flex-col gap-2"
          >
            <input
              autoFocus
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="my-project"
              aria-label="Space name"
              className="rounded-md border border-line bg-surface px-2 py-1.5 font-mono text-sm focus:outline-2 focus:outline-hay-deep"
            />
            <p className="text-xs text-stone">
              A space groups apps that belong together — they reach each other by name and share its
              services.
            </p>
            <div className="flex justify-end gap-2">
              <Button kind="quiet" onClick={onClose} disabled={busy}>
                Cancel
              </Button>
              <Button type="submit" disabled={busy || name.trim() === ''}>
                {busy ? 'Creating…' : 'Create'}
              </Button>
            </div>
          </form>
        </DialogPanel>
      </div>
    </Dialog>
  )
}
