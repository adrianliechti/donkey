import { Database, ExternalLink, HardDrive, Network, Trash2 } from 'lucide-react'
import type { AppSummary } from './api'
import { ExposureBadge, StatusDot, ago, healthLine, healthOf, type Health } from './ui'

/**
 * One section of the paddock per space.
 *
 * A space is the project — it defaults to the repository its apps came from,
 * so a monorepo's frontend and backend land in the same one. Its apps sit on
 * the dotted field as cards: the card answers "is it up, how do I reach it,
 * what is attached" without opening anything, and clicking it slides the
 * app's panel over the fleet.
 */
export function SpaceCard({
  space,
  apps,
  onOpen,
  onRemove,
}: {
  space: string
  apps: AppSummary[]
  onOpen: (space: string, app: string) => void
  onRemove: (r: { space: string; apps: string[]; siblings: string[] }) => void
}) {
  // The project's own health is the worst of its parts: a frontend that is
  // up is not good news when the backend it calls is down.
  const worst: Health = apps.some((a) => healthOf(a.healthy, a.ready) === 'bad')
    ? 'bad'
    : apps.some((a) => healthOf(a.healthy, a.ready) === 'unknown')
      ? 'unknown'
      : 'ok'

  return (
    <section className="rounded-xl border border-line/70 bg-canvas/60 p-4">
      <div className="mb-3 flex items-center justify-between gap-3">
        <div className="flex items-center gap-2.5">
          <StatusDot health={worst} />
          <h2 className="font-mono text-base font-bold tracking-tight">{space}</h2>
          <span className="text-xs text-stone">
            {apps.length === 1 ? '1 app' : `${apps.length} apps`}
          </span>
        </div>
        <button
          type="button"
          title={apps.length > 1 ? `Remove all ${apps.length} apps in ${space}` : `Remove ${apps[0]?.name ?? space}`}
          aria-label={apps.length > 1 ? `Remove all ${apps.length} apps in ${space}` : `Remove ${apps[0]?.name ?? space}`}
          onClick={() => onRemove({ space, apps: apps.map((a) => a.name), siblings: [] })}
          className="rounded-md p-1.5 text-stone hover:bg-brick/10 hover:text-brick focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-hay-deep"
        >
          <Trash2 className="size-4" aria-hidden />
        </button>
      </div>

      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
        {apps.map((app) => (
          <AppCard key={app.name} app={app} onOpen={onOpen} siblings={apps} onRemove={onRemove} />
        ))}
      </div>
    </section>
  )
}

function AppCard({
  app,
  siblings,
  onOpen,
  onRemove,
}: {
  app: AppSummary
  siblings: AppSummary[]
  onOpen: (space: string, app: string) => void
  onRemove: (r: { space: string; apps: string[]; siblings: string[] }) => void
}) {
  const h = healthOf(app.healthy, app.ready)
  const edge =
    h === 'bad' ? 'border-brick/50 hover:border-brick' : 'border-line hover:border-line-bright'

  return (
    <div
      className={`group relative rounded-lg border bg-card transition-colors ${edge}`}
    >
      <a
        href={`#/app/${encodeURIComponent(app.space)}/${encodeURIComponent(app.name)}`}
        onClick={(e) => {
          if (e.metaKey || e.ctrlKey || e.shiftKey || e.button !== 0) return
          e.preventDefault()
          onOpen(app.space, app.name)
        }}
        className="block p-3.5 focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-hay-deep"
      >
        <div className="flex items-center justify-between gap-2">
          <span className="flex min-w-0 items-center gap-2">
            <StatusDot health={h} />
            <span className="truncate font-mono text-sm font-bold">{app.name}</span>
          </span>
          <ExposureBadge exposure={app.exposure} />
        </div>

        <div className="mt-2 space-y-1 text-xs">
          {app.url ? (
            <div className="truncate font-mono text-stone">{app.url.replace(/^https?:\/\//, '')}</div>
          ) : (
            <div
              className="flex items-center gap-1 truncate font-mono text-stone"
              title={`No address of its own — its siblings in ${app.space} reach it as http://${app.name}`}
            >
              <Network className="size-3 shrink-0" aria-hidden /> http://{app.name}
            </div>
          )}
          <div className="flex flex-wrap items-center gap-x-2 gap-y-0.5 text-stone">
            <span className={h === 'bad' ? 'text-brick' : ''}>{healthLine[h](app.ready)}</span>
            {app.startedAt && <span>· up {ago(app.startedAt)}</span>}
            {(app.restarts ?? 0) > 0 && (
              <span className="text-brick" title={`${app.restarts} restarts`}>
                · {app.restarts}↻
              </span>
            )}
          </div>
        </div>

        {((app.addons ?? []).length > 0 || app.hasStorage) && (
          <div className="mt-2.5 flex flex-wrap items-center gap-1.5 border-t border-line/70 pt-2">
            {(app.addons ?? []).map((addon) => (
              <span
                key={addon}
                className="inline-flex items-center gap-1 rounded bg-raised px-1.5 py-0.5 font-mono text-[10px] text-ink-soft"
                title={`Attached service: ${addon}`}
              >
                <Database className="size-3" aria-hidden /> {addon}
              </span>
            ))}
            {app.hasStorage && (
              <span
                className="inline-flex items-center gap-1 rounded bg-raised px-1.5 py-0.5 font-mono text-[10px] text-ink-soft"
                title="Owns a disk — browse it in the Files tab"
              >
                <HardDrive className="size-3" aria-hidden /> disk
              </span>
            )}
          </div>
        )}
      </a>

      {/* Quick actions surface on hover, out of the card's click target. */}
      <div className="absolute right-2 top-2 hidden items-center gap-0.5 rounded-md bg-card group-hover:flex">
        {app.url && (
          <a
            href={app.url}
            target="_blank"
            rel="noreferrer"
            title={`Open ${app.url}`}
            aria-label={`Open ${app.name} in a new tab`}
            className="rounded p-1.5 text-stone hover:bg-raised hover:text-ink"
            onClick={(e) => e.stopPropagation()}
          >
            <ExternalLink className="size-3.5" aria-hidden />
          </a>
        )}
        <button
          type="button"
          title={`Remove ${app.name}`}
          aria-label={`Remove ${app.name}`}
          onClick={() =>
            onRemove({
              space: app.space,
              apps: [app.name],
              siblings: siblings.filter((a) => a.name !== app.name).map((a) => a.name),
            })
          }
          className="rounded p-1.5 text-stone hover:bg-brick/10 hover:text-brick"
        >
          <Trash2 className="size-3.5" aria-hidden />
        </button>
      </div>
    </div>
  )
}
