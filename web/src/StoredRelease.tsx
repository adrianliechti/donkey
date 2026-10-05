import { useState } from 'react'
import { FileCode2 } from 'lucide-react'
import type { BuildSummary } from './api'
import { ReleaseView } from './ReleaseView'
import { useAppBuild, useAppBuilds } from './queries'
import { CodeCard, CopyButton, ErrorNote, SectionHeader, Select, Spinner } from './ui'

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
        <p className="text-sm text-muted">No builds are recorded for this app yet — the next deploy starts its history.</p>
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
          update={{ ...(release.update ?? { stage: 'releasing' }), changes: release.changes }}
          result={release.result}
          layout="stacked"
        />
      )}
      {release.dockerfile ? <BuildFileCard content={release.dockerfile} filename=".donkey/Dockerfile" /> : null}
      {release.dockerignore !== undefined ? <BuildFileCard content={release.dockerignore} filename=".donkey/Dockerfile.dockerignore" /> : null}
    </>
  )
}

/** The recipe the image was built from. Keep any source repairs alongside it. */
function BuildFileCard({ content, filename }: { content: string; filename: string }) {
  return (
    <CodeCard
      icon={FileCode2}
      title={<span className="normal-case tracking-normal">{filename}</span>}
      className="max-h-[32rem]"
      meta={<CopyButton text={content} label={`Copy ${filename}`} />}
    >
      <pre className="whitespace-pre">{content}</pre>
    </CodeCard>
  )
}
