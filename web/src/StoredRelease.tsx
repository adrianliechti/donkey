import { useState } from 'react'
import { Link } from '@tanstack/react-router'
import { ChevronRight, FileCode2, GitCommitHorizontal } from 'lucide-react'
import type { BuildSummary } from './api'
import { focusRing } from './controls'
import { ReleaseView } from './ReleaseView'
import { useAppBuild, useAppBuilds, useCommits } from './queries'
import { ErrorNote, List, Section, SectionHeader, Select, Spinner } from './ui'

/** The image tag is the build's human name — the full reference repeats the
 * registry prefix on every entry. */
function buildLabel(build: BuildSummary): string {
  const tag = build.image?.split(':').pop()
  const when = new Date(build.created)
  const day = Number.isNaN(when.getTime()) ? build.created : when.toLocaleDateString()
  return `#${build.sequence} · ${tag ?? 'not released'} · ${day}`
}

export function StoredRelease({ space, app }: { space: string; app: string }) {
  const { data: history, error: listError } = useAppBuilds(space, app)
  const [selected, setSelected] = useState<number | null>(null)

  if (!history && !listError) return <Spinner label="Opening the build history…" />
  if (listError) return <ErrorNote message={listError} />
  const builds = history?.builds ?? []
  if (builds.length === 0) {
    return (
      <section className="space-y-3">
        <SectionHeader title="Deployment" />
        <p className="text-sm text-muted">
          No builds are recorded for this app yet — the next deploy starts its history.
        </p>
      </section>
    )
  }
  const sequence = selected ?? builds[0].sequence
  return (
    <section className="flex flex-col gap-5">
      <SectionHeader
        title={`Build #${sequence}`}
        actions={
          <Select
            variant="quiet"
            value={sequence}
            onChange={setSelected}
            options={builds.map((b) => ({ value: b.sequence, label: buildLabel(b) }))}
            ariaLabel="Stored build"
          />
        }
      />
      <StoredBuild space={space} app={app} sequence={sequence} />
    </section>
  )
}

function StoredBuild({ space, app, sequence }: { space: string; app: string; sequence: number }) {
  const { data: release, error } = useAppBuild(space, app, sequence)

  if (!release && !error) return <Spinner label="Opening the build…" />
  if (error) return <ErrorNote message={error} />
  if (!release) return null

  const finished = new Date(release.finishedAt)
  const when = Number.isNaN(finished.getTime()) ? release.finishedAt : finished.toLocaleString()

  return (
    <>
      <p className="-mt-4 text-xs text-muted">
        Finished {when}
        {release.truncated && ' · some logs or repair diffs were trimmed; the result and findings are complete'}
      </p>
      {!release.plan ? (
        <p className="text-sm text-muted">This older build has no structured plan to display.</p>
      ) : (
        <ReleaseView
          plan={release.plan}
          lines={release.lines ?? []}
          buildLog={release.buildLog ?? ''}
          update={release.update ?? { stage: 'releasing' }}
          result={release.result}
          layout="stacked"
        />
      )}
      <BuildFiles space={space} app={app} sequence={sequence} recipe={release.update?.recipe} />
    </>
  )
}

const fileRow = `flex min-h-11 items-center gap-3 px-3 py-2 text-sm transition-colors hover:bg-line/30 ${focusRing} focus-visible:-outline-offset-2`

/**
 * What the build used and wrote — its recipe, the assistant's repairs and
 * the settings it ran with — is its commit in the source history; the record
 * only says where the recipe sits. Builds from before the history have no
 * commit, and show nothing here.
 */
function BuildFiles({
  space,
  app,
  sequence,
  recipe,
}: {
  space: string
  app: string
  sequence: number
  recipe?: string
}) {
  const { data } = useCommits(space, app)
  const commit = data?.commits.find((c) => c.builds?.some((b) => b.sequence === sequence))
  if (!commit) return null
  const params = { space, name: app, tab: 'source' as const }
  return (
    <Section className="space-y-3">
      <SectionHeader
        title="Build files"
        description="The recipe, repairs and settings this build used, in its commit."
      />
      <List>
        <li>
          <Link to="/app/$space/$name/{-$tab}" params={params} search={{ commit: commit.id }} className={fileRow}>
            <GitCommitHorizontal className="size-4 shrink-0 text-muted" aria-hidden />
            <span className="shrink-0 font-mono text-xs text-muted">{commit.id.slice(0, 7)}</span>
            <span className="min-w-0 flex-1 truncate text-ink">{commit.message.split('\n')[0]}</span>
            <ChevronRight className="size-4 shrink-0 text-muted" aria-hidden />
          </Link>
        </li>
        {recipe && (
          <li>
            <Link
              to="/app/$space/$name/{-$tab}"
              params={params}
              search={{ commit: commit.id, files: 1, path: recipe }}
              className={fileRow}
            >
              <FileCode2 className="size-4 shrink-0 text-muted" aria-hidden />
              <span className="min-w-0 flex-1 truncate font-mono text-ink">{recipe}</span>
              <span className="shrink-0 text-xs text-muted">recipe</span>
              <ChevronRight className="size-4 shrink-0 text-muted" aria-hidden />
            </Link>
          </li>
        )}
      </List>
    </Section>
  )
}
