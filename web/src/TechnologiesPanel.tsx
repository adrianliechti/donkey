import { useMemo, useState } from 'react'
import { Link } from '@tanstack/react-router'
import { Layers, Search } from 'lucide-react'
import { api, type ReportApp, type Technology, type TechnologyCategoryId, type TechnologyReport } from './api'
import { AppIcon } from './AppIcon'
import { useTechnologies } from './queries'
import { appIndex, bySpace, filterTechnologies, percent } from './reports'
import { AppLine, ExportButton, Note, OpenRow, PanelBar, SpaceBlock } from './ReportParts'
import { Button, EmptyState, ErrorNote, SearchField, SectionTitle, Skeleton, panelClass } from './ui'

// What the running apps are built with, most used first: the languages, the
// frameworks, the base images and operating systems under them, and how
// they install their dependencies. Every row is a technology with a bar —
// the share of the running apps that use it — and opens to those apps, by
// space, with who answers for each.

const categoryLooks: Record<TechnologyCategoryId, { title: string; about: string; none: string }> = {
  language: {
    title: 'Languages',
    about: 'What an app is written in: read off the image it is built on and the manifests in its source.',
    none: 'No language was detected.',
  },
  framework: {
    title: 'Frameworks and libraries',
    about: 'The frameworks and notable libraries an app’s dependencies name.',
    none: 'No framework was detected.',
  },
  'base-image': {
    title: 'Base images',
    about: 'The images an app’s recipe builds on, as its last build resolved them.',
    none: 'No build on record names a base image.',
  },
  os: {
    title: 'Operating systems',
    about: 'The distribution inside the running image, as its scan recognized it.',
    none: 'No scan on record names an operating system.',
  },
  'package-manager': {
    title: 'Package managers',
    about: 'How an app installs its dependencies.',
    none: 'No package manager was detected.',
  },
}

// A category shows this many rows before it asks.
const SHORT = 8

const plural = (n: number, noun: string) => `${n} ${noun}${n === 1 ? '' : 's'}`

// The versions the apps of a technology are on, the most common first:
// "3.12 ×4 · 3.11".
function versionSummary(technology: Technology): string {
  const counts = new Map<string, number>()
  for (const app of technology.apps) if (app.version) counts.set(app.version, (counts.get(app.version) ?? 0) + 1)
  return [...counts.entries()]
    .sort(([a, x], [b, y]) => y - x || b.localeCompare(a, undefined, { numeric: true }))
    .slice(0, 4)
    .map(([version, count]) => (count > 1 ? `${version} ×${count}` : version))
    .join(' · ')
}

function TechnologyRow({
  category,
  technology,
  total,
  report,
  apps,
  open,
  onToggle,
}: {
  category: TechnologyCategoryId
  technology: Technology
  /** Every running app: what a full bar would be. */
  total: number
  report: TechnologyReport
  apps: Map<string, ReportApp>
  open: boolean
  onToggle: () => void
}) {
  const used = total > 0 ? technology.apps.length / total : 0
  // An operating system's "version" is its full name; the row already says it.
  const versions = category === 'os' ? '' : versionSummary(technology)
  return (
    <OpenRow
      open={open}
      onToggle={onToggle}
      summary={
        <>
          <span className="flex min-w-0 flex-[2] items-center gap-2">
            {category === 'language' && <AppIcon health="ok" stack={technology.key} />}
            <span
              className={`truncate text-sm text-ink ${category === 'base-image' || category === 'os' ? 'font-mono' : 'font-medium'}`}
              title={technology.name}
            >
              {technology.name}
            </span>
            {technology.endOfLife && (
              <span
                title="Its vendor publishes no more security updates"
                className="shrink-0 rounded bg-warning/15 px-1.5 py-0.5 font-mono text-[11px] leading-4 text-warning"
              >
                end of life
              </span>
            )}
            {versions && (
              <span className="min-w-0 truncate font-mono text-xs text-muted max-lg:hidden">{versions}</span>
            )}
          </span>
          {/* Ranked bars on one baseline: one hue, the length is the figure. */}
          <span
            className="flex min-w-0 flex-[3] items-center gap-3"
            title={`${plural(technology.apps.length, 'app')} of ${total}`}
          >
            <span className="h-1.5 min-w-0 flex-1 rounded-full bg-line/70 max-sm:hidden" aria-hidden>
              <span className="block h-full min-w-1 rounded-full bg-accent" style={{ width: percent(used) }} />
            </span>
            <span className="w-24 shrink-0 text-right text-xs text-ink-soft">
              <span className="font-mono text-ink">{technology.apps.length}</span>{' '}
              {technology.apps.length === 1 ? 'app' : 'apps'}
              <span className="ml-1.5 inline-block w-9 text-left font-mono text-muted">{percent(used)}</span>
            </span>
          </span>
        </>
      }
    >
      <div className="space-y-3">
        {bySpace(technology.apps).map(({ space, rows }) => (
          <SpaceBlock key={space} space={space} contacts={report.spaces[space]} platformAdmins={report.platformAdmins}>
            {rows.map((use) => (
              <AppLine
                key={use.app}
                app={use}
                info={apps.get(`${space}/${use.app}`)}
                tab="release"
                aside={use.version && <span className="font-mono text-xs text-ink-soft">{use.version}</span>}
              >
                {use.evidence && <p className="font-mono text-xs break-words text-muted">{use.evidence}</p>}
              </AppLine>
            ))}
          </SpaceBlock>
        ))}
      </div>
    </OpenRow>
  )
}

function CategorySection({
  id,
  technologies,
  total,
  report,
  apps,
  searching,
  open,
  onOpen,
}: {
  id: TechnologyCategoryId
  technologies: Technology[]
  total: number
  report: TechnologyReport
  apps: Map<string, ReportApp>
  searching: boolean
  open: string | null
  onOpen: (key: string | null) => void
}) {
  const [all, setAll] = useState(false)
  const look = categoryLooks[id]
  // A search shows everything it found; otherwise a long category is cut.
  const shown = all || searching ? technologies : technologies.slice(0, SHORT)
  return (
    <section aria-label={look.title} className={`overflow-hidden ${panelClass}`}>
      <div className="flex min-h-11 flex-wrap items-center justify-between gap-x-4 gap-y-1 border-b border-line px-4 py-2">
        <SectionTitle>
          {look.title}
          <span className="ml-2 font-mono text-xs font-normal text-muted">{technologies.length}</span>
        </SectionTitle>
        <p className="text-xs text-muted">{look.about}</p>
      </div>
      {technologies.length === 0 ? (
        <p className="px-4 py-4 text-sm text-muted">{searching ? 'Nothing here matches.' : look.none}</p>
      ) : (
        <ul className="divide-y divide-line">
          {shown.map((technology) => {
            const key = `${id}/${technology.name}`
            return (
              <TechnologyRow
                key={key}
                category={id}
                technology={technology}
                total={total}
                report={report}
                apps={apps}
                open={open === key}
                onToggle={() => onOpen(open === key ? null : key)}
              />
            )
          })}
        </ul>
      )}
      {technologies.length > shown.length && (
        <div className="flex justify-center border-t border-line/60 p-2">
          <Button kind="quiet" size="sm" onClick={() => setAll(true)}>
            Show all {technologies.length}
          </Button>
        </div>
      )}
    </section>
  )
}

export function TechnologiesPanel() {
  const { data: report, error, updatedAt, reload, fetching } = useTechnologies()
  const [query, setQuery] = useState('')
  const [open, setOpen] = useState<string | null>(null)
  const [exportError, setExportError] = useState('')

  const apps = useMemo(() => appIndex(report?.apps ?? []), [report])
  const categories = useMemo(() => filterTechnologies(report?.categories ?? [], query), [report, query])
  const searching = query.trim() !== ''
  const found = categories.reduce((sum, category) => sum + category.technologies.length, 0)
  const total = report?.apps.length ?? 0
  // Apps nothing is known about: deployed before builds recorded what they
  // detected, or from an image donkey did not build.
  const known = new Set(
    (report?.categories ?? []).flatMap((category) =>
      category.technologies.flatMap((technology) => technology.apps.map((app) => `${app.space}/${app.app}`)),
    ),
  )
  const unknown = (report?.apps ?? []).filter((app) => !known.has(`${app.space}/${app.app}`))

  return (
    <>
      <PanelBar
        updatedAt={updatedAt}
        onReload={reload}
        busy={fetching}
        actions={
          <ExportButton
            hint="One row per technology and app, with the contacts of the app’s space — what the search shows"
            disabled={!report || found === 0}
            onError={setExportError}
            download={() => api.exportTechnologies({ q: query.trim() })}
          />
        }
      >
        <SearchField
          value={query}
          onChange={setQuery}
          placeholder="Find a technology, space or app…"
          className="w-72"
        />
        {report && (
          <span className="text-xs text-muted">
            {plural(total, 'running app')}
            {searching && ` · ${plural(found, 'match')}${found === 1 ? '' : 'es'}`}
          </span>
        )}
      </PanelBar>

      <ErrorNote message={error || exportError} />
      {!report && !error && (
        <>
          <Skeleton className="h-48" />
          <Skeleton className="h-48" />
        </>
      )}

      {report && total === 0 && (
        <div className="rounded-lg border border-dashed border-line">
          <EmptyState icon={Layers} title="No apps are running">
            What apps are built with is listed here once one is deployed.
          </EmptyState>
        </div>
      )}
      {report && total > 0 && searching && found === 0 && (
        <div className="rounded-lg border border-dashed border-line">
          <EmptyState
            icon={Search}
            title={`Nothing matches “${query.trim()}”`}
            action={
              <Button kind="quiet" size="sm" onClick={() => setQuery('')}>
                Show everything
              </Button>
            }
          >
            Technologies are searched by name, and by the spaces and apps that use them.
          </EmptyState>
        </div>
      )}
      {report &&
        total > 0 &&
        !(searching && found === 0) &&
        categories
          // A search leaves out the categories it found nothing in.
          .filter((category) => !searching || category.technologies.length > 0)
          .map((category) => (
            <CategorySection
              key={category.id}
              id={category.id}
              technologies={category.technologies}
              total={total}
              report={report}
              apps={apps}
              searching={searching}
              open={open}
              onOpen={setOpen}
            />
          ))}
      {report && unknown.length > 0 && !searching && (
        <Note icon={Layers}>
          Nothing is on record for{' '}
          {unknown.map((app, index) => (
            <span key={`${app.space}/${app.app}`}>
              {index > 0 && ', '}
              <Link
                to="/app/$space/$name/{-$tab}"
                params={{ space: app.space, name: app.app, tab: undefined }}
                className="font-mono text-ink-soft underline-offset-2 hover:text-ink hover:underline"
              >
                {app.space}/{app.app}
              </Link>
            </span>
          ))}{' '}
          — deployed before builds kept what they detected. Deploying again records it.
        </Note>
      )}
    </>
  )
}
