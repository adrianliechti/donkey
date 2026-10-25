import { useMemo, useState, type ReactNode } from 'react'
import { Link } from '@tanstack/react-router'
import { ArrowUpRight, CircleCheck, FolderPlus, Layers, Loader2, Search, Star, UploadCloud } from 'lucide-react'
import type { ActiveDeploy, SpaceList } from './api'
import { AppIcon } from './AppIcon'
import { useDeploy } from './deploy'
import { DeployProgress } from './DeployProgress'
import {
  deployTime,
  filterSpaces,
  isDeploying,
  recentDeploys,
  spaceState,
  summarizeSpaces,
  spaceSecurity,
  type SpaceFilter,
  type SpaceIssue,
  type SpaceSummary,
} from './fleet'
import { ago } from './format'
import { useNow } from './hooks'
import { box, useBox } from './httpStore'
import { NewSpaceDialog } from './NewSpaceDialog'
import { useDeploys, useFleet } from './queries'
import {
  Button,
  EmptyState,
  ErrorNote,
  Eyebrow,
  IconButton,
  Mark,
  Page,
  PageBar,
  panelClass,
  SearchField,
  SectionTitle,
  Segmented,
  Skeleton,
  StatusLine,
  ScanBadge,
} from './ui'
import { healthLine, healthOf } from './vocabulary'

// The spaces pinned in this browser, kept across visits.
const pins = box('donkey.pinned-spaces', (raw): string[] =>
  Array.isArray(raw) ? raw.filter((name): name is string => typeof name === 'string') : [],
)
const roles: Record<SpaceList['roles'][string], string> = { admin: 'Admin', developer: 'Developer', viewer: 'Viewer' }

// The list's columns, shared by its header and its rows: the name takes
// what is left, status and the last deploy keep their width, the pin has
// an icon button's. The apps and the last deploy wait for room.
const columns =
  'grid grid-cols-[minmax(0,1fr)_7.5rem_1.75rem] items-center gap-x-4 sm:grid-cols-[minmax(0,1fr)_7.5rem_4.5rem_1.75rem] md:grid-cols-[minmax(0,1.5fr)_minmax(7.5rem,1fr)_4.5rem_9rem_1.75rem]'

/**
 * Donkey's front page: every space you can see, how its apps are doing, and
 * what is being deployed right now — whoever started it, from wherever.
 * Built for a handful of spaces and for two hundred: a filter, a search,
 * and one row per space. On a desktop the page fills the window and stands
 * still — the list scrolls inside its panel, under its column header — and
 * the activity rail beside it is exactly as tall, in every state: a deploy
 * starting or an app failing changes what it says, not where anything is.
 */
export function FleetOverview() {
  const { data, error } = useFleet()
  const { deploys, error: deployError, loading: deployLoading } = useDeploys()
  const openDeploy = useDeploy()
  const [query, setQuery] = useState('')
  const [filter, setFilter] = useState<SpaceFilter>('all')
  const [creating, setCreating] = useState(false)
  const pinned = new Set(useBox(pins))
  useNow()

  const spaces = useMemo(() => summarizeSpaces(data?.spaces ?? [], data?.apps ?? [], deploys), [data, deploys])
  const shown = filterSpaces(spaces, query, filter, pinned)
  const attention = spaces.filter((space) => space.issues.length > 0)
  const issues = attention.flatMap((space) => space.issues.map((issue) => ({ ...issue, space: space.name })))
  const activity = recentDeploys(spaces.flatMap((space) => space.deploys))
  const canDeploy = data?.platformAdmin || Object.values(data?.roles ?? {}).some((role) => role !== 'viewer')

  const togglePin = (space: string) =>
    pins.set((previous) =>
      previous.includes(space) ? previous.filter((name) => name !== space) : [...previous, space],
    )

  if (error)
    return (
      <Page>
        <ErrorNote message={error} />
      </Page>
    )
  if (!data)
    return (
      <Page>
        <Skeleton className="h-8 w-80" />
        <Skeleton className="h-64" />
      </Page>
    )

  const reset = () => {
    setQuery('')
    setFilter('all')
  }

  // Why the list is empty, when it is: no spaces at all, or none left by
  // the search or the filter.
  const empty =
    spaces.length === 0 ? (
      <EmptyState icon={Layers} title={data.platformAdmin ? 'Create your first space' : 'No spaces yet'}>
        {data.platformAdmin
          ? 'A space is a home for apps and services. Make one with New space, or deploy an app from its folder with donkey deploy.'
          : 'Ask a platform administrator to create a space and give you access.'}
      </EmptyState>
    ) : shown.length > 0 ? null : query.trim() ? (
      <EmptyState
        icon={Search}
        title={`Nothing matches “${query.trim()}”`}
        action={
          <Button kind="quiet" size="sm" onClick={reset}>
            Show all spaces
          </Button>
        }
      >
        Spaces, their apps and first deploys are searched by name.
      </EmptyState>
    ) : filter === 'pinned' ? (
      <EmptyState icon={Star} title="No pinned spaces">
        Use the star beside a space to keep it at the top, in this browser.
      </EmptyState>
    ) : (
      <EmptyState icon={CircleCheck} title="Nothing needs attention">
        Every app is running and no deploy has failed.
      </EmptyState>
    )

  return (
    <Page fill>
      <PageBar
        actions={
          canDeploy && (
            <>
              {data.platformAdmin && (
                // A quiet button on the field wears the card colour, like
                // the search beside it — the dots stay behind it.
                <Button kind="quiet" icon={FolderPlus} className="bg-card" onClick={() => setCreating(true)}>
                  New space
                </Button>
              )}
              {canDeploy && (
                <Button icon={UploadCloud} onClick={() => openDeploy()}>
                  Deploy
                </Button>
              )}
            </>
          )
        }
      >
        {spaces.length > 0 && (
          <>
            <Segmented
              label="Show"
              value={filter}
              onChange={setFilter}
              options={[
                { value: 'all', label: 'All', count: spaces.length },
                { value: 'pinned', label: 'Pinned', count: spaces.filter((space) => pinned.has(space.name)).length },
                { value: 'attention', label: 'Needs attention', count: attention.length },
              ]}
            />
            <SearchField value={query} onChange={setQuery} placeholder="Find a space or an app…" />
          </>
        )}
      </PageBar>

      {/* One grid row, as tall as what the window leaves under the bar: the
          list and the rail share it, whatever is in either. */}
      <div className="grid gap-6 xl:min-h-0 xl:flex-1 xl:grid-cols-[minmax(0,1fr)_20rem] xl:grid-rows-[minmax(0,1fr)]">
        <section aria-label="Spaces" className={`flex min-w-0 flex-col overflow-hidden xl:min-h-0 ${panelClass}`}>
          <div aria-hidden className={`h-11 shrink-0 border-b border-line px-4 ${columns}`}>
            <Eyebrow>Space</Eyebrow>
            <Eyebrow>Status</Eyebrow>
            <span className="hidden sm:block">
              <Eyebrow>Apps</Eyebrow>
            </span>
            <span className="hidden md:block">
              <Eyebrow>Last deploy</Eyebrow>
            </span>
            <span />
          </div>
          {/* The rows scroll under the header; an empty state sits in the
              middle of the room they would take. */}
          <div className="flex min-h-0 flex-1 flex-col xl:overflow-y-auto">
            {empty ? (
              <div className="my-auto">{empty}</div>
            ) : (
              <ul className="divide-y divide-line">
                {shown.map((space) => (
                  <SpaceRow
                    key={space.name}
                    space={space}
                    role={roles[data.roles[space.name]]}
                    pinned={pinned.has(space.name)}
                    onPin={() => togglePin(space.name)}
                  />
                ))}
              </ul>
            )}
          </div>
        </section>

        <FleetActivity issues={issues} activity={activity} error={deployError} loading={deployLoading} />
      </div>

      <NewSpaceDialog
        key={String(creating)}
        domain={data.domain ?? ''}
        open={creating}
        onClose={() => setCreating(false)}
        onCreated={() => {
          setCreating(false)
          reset()
        }}
      />
    </Page>
  )
}

function SpaceRow({
  space,
  role,
  pinned,
  onPin,
}: {
  space: SpaceSummary
  role: string
  pinned: boolean
  onPin: () => void
}) {
  const last = space.deploys[0]
  const state = spaceState(space)
  return (
    <li className={`group relative px-4 py-3 transition-colors duration-(--duration-fast) hover:bg-raised ${columns}`}>
      <div className="flex min-w-0 items-center gap-3">
        {/* The space's initial: two hundred rows keep a steady left edge, and
            no colour in the list means anything but health. */}
        <Mark className="font-mono text-xs font-semibold text-ink-soft uppercase">{space.name.slice(0, 1)}</Mark>
        <div className="min-w-0">
          <Link
            to="/space/$space"
            params={{ space: space.name }}
            title={space.name}
            className="block truncate font-mono text-sm font-semibold text-ink after:absolute after:inset-0 focus-visible:outline-none focus-visible:after:outline-2 focus-visible:after:-outline-offset-2 focus-visible:after:outline-accent-strong"
          >
            {space.name}
          </Link>
          {/* The role, and the space's scan word beside it — the one cell
              with room for a word. */}
          <p className="flex items-center gap-2 text-xs text-muted">
            {role}
            <ScanBadge security={spaceSecurity(space.apps)} />
          </p>
        </div>
      </div>
      {/* Only news speaks: a healthy space is a green dot, an empty one says
          nothing — the words are there for screen readers. */}
      {state.news || state.tone === 'success' ? (
        <StatusLine {...state} quiet={!state.news} />
      ) : (
        // The cell stays in the grid (sr-only alone would leave it, being
        // absolutely positioned) and holds the words for screen readers.
        <span>
          <span className="sr-only">{state.label}</span>
        </span>
      )}
      <div
        className="hidden items-center gap-1.5 sm:flex"
        aria-label={`${space.apps.length} apps: ${space.running} running, ${space.down} not fully up, ${space.unknown} unknown`}
      >
        {/* One bar per app, up to eight: a space's health readable without opening it. */}
        <span className="flex gap-0.5" aria-hidden>
          {space.apps.slice(0, 8).map((app) => {
            const health = healthOf(app.healthy, app.ready)
            return (
              <span
                key={app.name}
                className={`h-3.5 w-1 rounded-sm ${health === 'bad' ? 'bg-danger' : health === 'unknown' ? 'bg-warning' : 'bg-success/60'}`}
              />
            )
          })}
        </span>
        <span className="font-mono text-xs text-ink-soft">{space.apps.length}</span>
      </div>
      {/* The last deploy, and which app it was: said in the cell, as the
          row's link covers it and would swallow a tooltip. */}
      <div className="hidden min-w-0 md:block">
        {last ? (
          <>
            {isDeploying(last) ? (
              <StatusLine tone="accent" label="Deploying" busy />
            ) : (
              <span className={`block truncate text-xs ${last.stage === 'failed' ? 'text-danger' : 'text-muted'}`}>
                {last.stage === 'failed' ? 'Failed' : 'Deployed'}{' '}
                <time dateTime={deployTime(last)}>{ago(deployTime(last))} ago</time>
              </span>
            )}
            <span className="block truncate font-mono text-xs text-muted">{last.app}</span>
          </>
        ) : (
          <span className="text-xs text-muted">—</span>
        )}
      </div>
      <IconButton
        icon={Star}
        size="sm"
        label={`${pinned ? 'Unpin' : 'Pin'} ${space.name}`}
        onClick={onPin}
        className={`relative z-10 ${pinned ? 'text-ink-soft [&>svg]:fill-current' : ''}`}
      />
    </li>
  )
}

/**
 * The rail beside the list: what needs attention, what is deploying, what
 * just finished — and, when nothing is, that nothing is. It is there in
 * every state, exactly as tall as the list, and scrolls inside when it has
 * more to say than fits.
 */
function FleetActivity({
  issues,
  activity,
  error,
  loading,
}: {
  issues: (SpaceIssue & { space: string })[]
  activity: ActiveDeploy[]
  error: string
  loading: boolean
}) {
  const active = activity.filter(isDeploying)
  // Failed deploys already appear under Needs attention.
  const completed = activity.filter((deploy) => !isDeploying(deploy) && deploy.stage !== 'failed')
  const quiet = issues.length === 0 && active.length === 0 && completed.length === 0
  const summary = [
    issues.length ? `${issues.length} ${issues.length === 1 ? 'issue' : 'issues'}` : '',
    active.length ? `${active.length} deploying` : '',
  ]
    .filter(Boolean)
    .join(' · ')
  return (
    <aside aria-label="Activity" className="min-w-0 xl:min-h-0">
      <div className={`flex h-full min-w-0 flex-col overflow-hidden ${panelClass}`}>
        <div className="flex h-11 shrink-0 items-center justify-between gap-3 border-b border-line px-4">
          <SectionTitle>Activity</SectionTitle>
          <span className="truncate text-xs text-muted">{summary || (loading && !error ? 'Checking…' : '')}</span>
        </div>
        <div className="flex min-h-0 flex-1 flex-col divide-y divide-line xl:overflow-y-auto">
          {error && (
            <div className="p-4">
              <ErrorNote message={error} />
            </div>
          )}
          {issues.length > 0 && (
            <ActivitySection title="Needs attention" tone="danger">
              {issues.map((issue) => {
                const health = issue.app ? healthOf(issue.app.healthy, issue.app.ready) : 'bad'
                return (
                  <ActivityRow
                    key={`${issue.space}/${issue.name}`}
                    space={issue.space}
                    app={issue.name}
                    // An app that is down: its diagnosis is in Logs. One whose
                    // deploy failed: its page says so, and offers to deploy again.
                    tab={issue.deploy ? undefined : 'logs'}
                    icon={
                      issue.app ? (
                        <AppIcon health={health} stack={issue.app.stack} />
                      ) : (
                        <span className="size-2.5 shrink-0 rounded-full border-2 border-danger" aria-hidden />
                      )
                    }
                    note={
                      <span className="text-danger">
                        {issue.deploy ? 'Deploy failed' : healthLine[health](issue.app?.ready ?? '')}
                      </span>
                    }
                  />
                )
              })}
            </ActivitySection>
          )}
          {active.length > 0 && (
            <ActivitySection title="Deploying" tone="accent">
              {active.map((deploy) => (
                <ActivityRow
                  key={`${deploy.space}/${deploy.app}`}
                  space={deploy.space || 'default'}
                  app={deploy.app}
                  icon={<Loader2 className="size-4 shrink-0 animate-spin text-accent" aria-hidden />}
                >
                  <DeployProgress deploy={deploy} compact />
                </ActivityRow>
              ))}
            </ActivitySection>
          )}
          {completed.length > 0 && (
            <ActivitySection title="Recent deploys">
              {completed.map((deploy) => (
                <ActivityRow
                  key={`${deploy.space}/${deploy.app}`}
                  space={deploy.space || 'default'}
                  app={deploy.app}
                  icon={
                    <>
                      <CircleCheck className="size-4 shrink-0 text-success" aria-hidden />
                      <span className="sr-only">Deployed: </span>
                    </>
                  }
                  right={
                    <time className="shrink-0 text-xs text-muted" dateTime={deployTime(deploy)}>
                      {ago(deployTime(deploy))} ago
                    </time>
                  }
                />
              ))}
            </ActivitySection>
          )}
          {quiet &&
            !error &&
            (loading ? (
              <div className="p-4" role="status" aria-label="Loading activity">
                <Skeleton className="h-16" />
              </div>
            ) : (
              <div className="my-auto">
                <EmptyState icon={CircleCheck} title="All quiet">
                  Nothing is deploying and nothing needs attention.
                </EmptyState>
              </div>
            ))}
        </div>
      </div>
    </aside>
  )
}

function ActivitySection({
  title,
  tone,
  children,
}: {
  title: string
  tone?: 'danger' | 'accent'
  children: ReactNode
}) {
  return (
    <section aria-label={title} className="py-2">
      <Eyebrow tone={tone} className="px-4 py-1.5">
        {title}
      </Eyebrow>
      <ul>{children}</ul>
    </section>
  )
}

// Every entry in the rail is one link to an app, tinted on hover like a row
// of the list: a mark, the name, the space it is in and a note on what is up,
// then the time or an arrow at the right — and, under all that, room for a
// deploy's progress.
function ActivityRow({
  space,
  app,
  tab,
  icon,
  note,
  right,
  children,
}: {
  space: string
  app: string
  tab?: string
  icon: ReactNode
  note?: ReactNode
  right?: ReactNode
  children?: ReactNode
}) {
  return (
    <li>
      <Link
        to="/app/$space/$name/{-$tab}"
        params={{ space, name: app, tab }}
        className="block px-4 py-2 transition-colors duration-(--duration-fast) hover:bg-raised focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-accent-strong"
      >
        <span className="flex items-center gap-3">
          {icon}
          <span className="min-w-0 flex-1">
            <span className="block truncate font-mono text-sm font-medium text-ink" title={app}>
              {app}
            </span>
            <span className="block truncate text-xs text-muted">
              {space}
              {note && <> · {note}</>}
            </span>
          </span>
          {right ?? <ArrowUpRight className="size-3.5 shrink-0 text-muted" aria-hidden />}
        </span>
        {children && <div className="mt-2">{children}</div>}
      </Link>
    </li>
  )
}
