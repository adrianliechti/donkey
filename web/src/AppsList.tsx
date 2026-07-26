import { useCallback, useState } from 'react'
import { RefreshCw } from 'lucide-react'
import { api, type AppSummary } from './api'
import { usePoll } from './hooks'
import { RemoveApps } from './RemoveApps'
import { SpaceCard } from './SpaceCard'
import { ErrorNote, Skeleton, healthLine, healthOf, type Health } from './ui'

const healthRank: Record<Health, number> = { bad: 0, unknown: 1, ok: 2 }

export function AppsList({ onOpen }: { onOpen: (space: string, app: string) => void }) {
  const fetcher = useCallback(() => api.apps(), [])
  const { data, error, reload } = usePoll(fetcher, 10_000)
  const apps = data?.apps ?? null
  // What the remove dialog is asking about: one app, or a whole space.
  const [removing, setRemoving] = useState<{ space: string; apps: string[]; siblings: string[] } | null>(null)

  // Apps belong to a space; the paddock mirrors that hierarchy instead of
  // flattening everything into one wall of cards.
  const grouped = new Map<string, AppSummary[]>()
  for (const app of apps ?? []) {
    const space = app.space || 'default'
    grouped.set(space, [...(grouped.get(space) ?? []), app])
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

      {apps !== null && apps.length === 0 && (
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
    </div>
  )
}
