import { useCallback } from 'react'
import { ExternalLink, RefreshCw } from 'lucide-react'
import { api, type AppSummary } from './api'
import { usePoll } from './hooks'
import { Card, ErrorNote, Skeleton, StatusDot, UpdatedAgo, ago, healthLine, healthOf, type Health } from './ui'

const healthRank: Record<Health, number> = { bad: 0, unknown: 1, ok: 2 }


export function AppsList({ onOpen }: { onOpen: (space: string, app: string) => void }) {
  const fetcher = useCallback(() => api.apps(), [])
  const { data, error, updatedAt, reload } = usePoll(fetcher, 10_000)
  const apps = data?.apps ?? null

  // Apps belong to a space; the list mirrors that hierarchy instead of
  // flattening everything into one wall of cards. Within a space, the apps
  // that need attention come first.
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
        <h1 className="text-lg font-semibold">Apps</h1>
        <div className="flex items-center gap-3">
          <UpdatedAgo at={updatedAt} />
          <button
            onClick={reload}
            className="flex items-center gap-1.5 rounded-md px-2 py-1 text-xs text-stone hover:bg-line/50 focus-visible:outline-2 focus-visible:outline-hay-deep"
          >
            <RefreshCw className="size-3.5" aria-hidden /> Refresh
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
                const tone = h === 'ok' ? 'bg-moss/80' : h === 'bad' ? 'bg-brick' : 'bg-stone/60'
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
          <Skeleton className="h-28" />
          <Skeleton className="h-28" />
          <Skeleton className="h-28" />
        </div>
      )}

      {apps !== null && apps.length === 0 && (
        <Card className="p-10 text-center">
          <p className="text-sm text-stone">No apps yet.</p>
          <p className="mt-2 text-sm">
            Deploy your first one from a project folder:{' '}
            <code className="rounded bg-surface px-2 py-0.5 font-mono text-sm">donkey deploy</code>
          </p>
          <p className="mt-1 text-sm text-stone">
            — or use <span className="font-medium text-ink">Deploy</span> above and drop the project
            folder right here.
          </p>
        </Card>
      )}

      {spaces.map(([space, spaceApps]) => (
        <section key={space} className="space-y-3">
          <h2 className="flex items-baseline gap-2 border-b border-line pb-1">
            <span className="font-mono text-sm font-bold tracking-tight">{space}</span>
            <span className="text-xs text-stone">
              {spaceApps.length} {spaceApps.length === 1 ? 'app' : 'apps'}
            </span>
          </h2>
          <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
            {spaceApps.map((app) => {
              const h = healthOf(app.healthy, app.ready)
              const notch = h === 'ok' ? 'border-l-moss' : h === 'bad' ? 'border-l-brick' : 'border-l-stone/50'
              return (
                <div
                  key={`${app.space}/${app.name}`}
                  className={`group relative rounded-lg border border-line bg-card p-4 transition-colors hover:bg-hay-wash focus-within:outline-2 focus-within:outline-offset-2 focus-within:outline-hay-deep border-l-4 ${notch}`}
                >
                  {/* A real link, so the app opens in a new tab on middle-click
                      — stretched to keep the whole card clickable. */}
                  <a
                    href={`#/app/${encodeURIComponent(app.space)}/${encodeURIComponent(app.name)}`}
                    onClick={(e) => {
                      if (e.metaKey || e.ctrlKey || e.shiftKey || e.button !== 0) return
                      e.preventDefault()
                      onOpen(app.space, app.name)
                    }}
                    className="font-mono text-lg font-bold tracking-tight after:absolute after:inset-0 group-hover:underline focus:outline-none"
                  >
                    {app.name}
                  </a>
                  <div className="mt-1.5 flex items-center gap-2 text-sm text-ink-soft">
                    <StatusDot health={h} />
                    {healthLine[h](app.ready)}
                    {app.startedAt && <span className="text-xs text-stone">up {ago(app.startedAt)}</span>}
                    {(app.restarts ?? 0) > 0 && (
                      <span className="text-xs text-brick" title={`${app.restarts} restarts`}>
                        {app.restarts}↻
                      </span>
                    )}
                  </div>
                  {app.url && (
                    <a
                      href={app.url}
                      target="_blank"
                      rel="noreferrer"
                      // Above the stretched link, so this opens the app itself.
                      className="relative z-10 mt-2 flex w-fit items-center gap-1 truncate font-mono text-xs text-stone hover:text-hay-deep hover:underline"
                    >
                      <ExternalLink className="size-3 shrink-0" aria-hidden />
                      {app.url.replace(/^https?:\/\//, '')}
                    </a>
                  )}
                  {app.image && (
                    <div className="mt-1 truncate font-mono text-xs text-stone/70">
                      {app.image.split('/').pop()}
                    </div>
                  )}
                </div>
              )
            })}
          </div>
        </section>
      ))}
    </div>
  )
}
