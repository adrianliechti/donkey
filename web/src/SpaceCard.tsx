import { useState } from 'react'
import { ChevronRight, ExternalLink, Lock, Trash2 } from 'lucide-react'
import type { AppSummary } from './api'
import { StatusDot, ago, healthLine, healthOf, type Health } from './ui'

/**
 * One card per space.
 *
 * A space is the project — it defaults to the repository its apps came from,
 * so a monorepo's frontend and backend land in the same one. Giving each app
 * an equal card said the opposite: that a product and the internal service it
 * depends on are two unrelated things that happen to sit near each other.
 *
 * So the card is the project, and the apps are inside it. Collapsed it still
 * says what is in there and whether anything is wrong — a summary you have to
 * open to trust is worse than no summary — and opening it gives every app its
 * address and its controls.
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
  const [open, setOpen] = useState(false)

  const exposed = apps.filter((a) => a.url)
  const internal = apps.filter((a) => !a.url)
  // The project's own health is the worst of its parts: a frontend that is up
  // is not good news when the backend it calls is down.
  const worst: Health = apps.some((a) => healthOf(a.healthy, a.ready) === 'bad')
    ? 'bad'
    : apps.some((a) => healthOf(a.healthy, a.ready) === 'unknown')
      ? 'unknown'
      : 'ok'
  const notch = worst === 'ok' ? 'border-l-moss' : worst === 'bad' ? 'border-l-brick' : 'border-l-stone/50'
  const address = exposed[0]?.url

  return (
    <section className={`rounded-lg border border-line bg-card border-l-4 ${notch}`}>
      <div className="flex items-start gap-3 p-4">
        <button
          type="button"
          onClick={() => setOpen(!open)}
          aria-expanded={open}
          className="flex flex-1 items-start gap-2 text-left focus:outline-2 focus:outline-offset-2 focus:outline-hay-deep"
        >
          <ChevronRight
            className={`mt-1 size-4 shrink-0 text-stone transition-transform ${open ? 'rotate-90' : ''}`}
            aria-hidden
          />
          <span className="min-w-0">
            <span className="font-mono text-lg font-bold tracking-tight">{space}</span>
            <span className="mt-1 flex flex-wrap items-center gap-x-2 gap-y-1 text-sm text-ink-soft">
              <StatusDot health={worst} />
              {apps.length === 1 ? '1 app' : `${apps.length} apps`}
              {internal.length > 0 && <span className="text-xs text-stone">{internal.length} internal</span>}
              {worst !== 'ok' && (
                <span className="text-xs text-brick">
                  {apps.filter((a) => healthOf(a.healthy, a.ready) !== 'ok').length} need attention
                </span>
              )}
            </span>
            {/* Collapsed, the names are what tells you whether this is the
                project you were looking for. */}
            {!open && (
              <span className="mt-1.5 flex flex-wrap gap-1.5">
                {apps.map((a) => (
                  <span
                    key={a.name}
                    className={`rounded px-1.5 py-0.5 font-mono text-xs ${
                      healthOf(a.healthy, a.ready) === 'ok' ? 'bg-surface text-ink-soft' : 'bg-brick/10 text-brick'
                    }`}
                  >
                    {a.name}
                  </span>
                ))}
              </span>
            )}
          </span>
        </button>

        <div className="flex shrink-0 items-center gap-2">
          {address && (
            <a
              href={address}
              target="_blank"
              rel="noreferrer"
              className="flex items-center gap-1 truncate font-mono text-xs text-stone hover:text-hay-deep hover:underline"
            >
              <ExternalLink className="size-3 shrink-0" aria-hidden />
              {address.replace(/^https?:\/\//, '')}
            </a>
          )}
          {/* Icon only, and the same icon the rows below use: one meaning,
              one shape. The label lives in the tooltip because the target of
              a delete matters more than the verb — "Remove" alone next to a
              project name is ambiguous about how much it takes. */}
          <button
            type="button"
            title={
              apps.length > 1
                ? `Remove all ${apps.length} apps in ${space}`
                : `Remove ${apps[0]?.name ?? space}`
            }
            aria-label={
              apps.length > 1
                ? `Remove all ${apps.length} apps in ${space}`
                : `Remove ${apps[0]?.name ?? space}`
            }
            onClick={() => onRemove({ space, apps: apps.map((a) => a.name), siblings: [] })}
            className="rounded-md p-1.5 text-stone hover:bg-brick/10 hover:text-brick focus:outline-2 focus:outline-offset-2 focus:outline-hay-deep"
          >
            <Trash2 className="size-4" aria-hidden />
          </button>
        </div>
      </div>

      {open && (
        <ul className="border-t border-line">
          {[...exposed, ...internal].map((app) => {
            const h = healthOf(app.healthy, app.ready)
            return (
              <li
                key={`${app.space}/${app.name}`}
                className="group flex flex-wrap items-center gap-x-3 gap-y-1 border-b border-line px-4 py-2.5 last:border-b-0 hover:bg-hay-wash"
              >
                <StatusDot health={h} />
                <a
                  href={`#/app/${encodeURIComponent(app.space)}/${encodeURIComponent(app.name)}`}
                  onClick={(e) => {
                    if (e.metaKey || e.ctrlKey || e.shiftKey || e.button !== 0) return
                    e.preventDefault()
                    onOpen(app.space, app.name)
                  }}
                  className="font-mono font-bold hover:underline"
                >
                  {app.name}
                </a>
                <span className="text-sm text-ink-soft">{healthLine[h](app.ready)}</span>
                {app.startedAt && <span className="text-xs text-stone">up {ago(app.startedAt)}</span>}
                {(app.restarts ?? 0) > 0 && (
                  <span className="text-xs text-brick" title={`${app.restarts} restarts`}>
                    {app.restarts}↻
                  </span>
                )}

                {app.url ? (
                  <a
                    href={app.url}
                    target="_blank"
                    rel="noreferrer"
                    className="ml-auto flex items-center gap-1 truncate font-mono text-xs text-stone hover:text-hay-deep hover:underline"
                  >
                    <ExternalLink className="size-3 shrink-0" aria-hidden />
                    {app.url.replace(/^https?:\/\//, '')}
                  </a>
                ) : (
                  <span
                    className="ml-auto flex items-center gap-1 truncate font-mono text-xs text-stone"
                    title={`No address of its own — reached by its siblings in ${space} as http://${app.name}`}
                  >
                    <Lock className="size-3 shrink-0" aria-hidden />
                    http://{app.name}
                  </span>
                )}

                <button
                  type="button"
                  title={`Remove ${app.name}`}
                  aria-label={`Remove ${app.name}`}
                  onClick={() =>
                    onRemove({
                      space: app.space,
                      apps: [app.name],
                      siblings: apps.filter((a) => a.name !== app.name).map((a) => a.name),
                    })
                  }
                  className="rounded-md p-1.5 text-stone opacity-0 transition-opacity hover:bg-brick/10 hover:text-brick focus:opacity-100 group-hover:opacity-100"
                >
                  <Trash2 className="size-4" aria-hidden />
                </button>
              </li>
            )
          })}
        </ul>
      )}
    </section>
  )
}
