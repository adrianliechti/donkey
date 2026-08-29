import { useState } from 'react'
import { Clock3, FileCode2 } from 'lucide-react'
import type { BuildSummary } from './api'
import { ReleaseView } from './ReleaseView'
import { useAppBuild, useAppBuilds } from './queries'
import { Card, CheckIcon, ErrorNote, Select, Spinner } from './ui'

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
      <Card className="mx-auto w-full max-w-3xl p-4 text-sm text-muted">
        No builds are recorded for this app yet — the next deploy starts its history.
      </Card>
    )
  }
  const sequence = selected ?? builds[0].sequence
  const current = builds.find((b) => b.sequence === sequence) ?? builds[0]
  return (
    <div className="mx-auto w-full max-w-3xl space-y-4">
      <div className="flex flex-wrap items-center justify-between gap-2 text-xs text-muted">
        <span className="flex items-center gap-1.5">
          <CheckIcon ok={current.ok} /> Build #{current.sequence}
          {current.image ? <span className="font-mono">{current.image}</span> : null}
        </span>
        <Select
          value={sequence}
          onChange={setSelected}
          options={builds.map((b) => ({ value: b.sequence, label: buildLabel(b) }))}
          ariaLabel="Stored build"
          size="sm"
        />
      </div>
      <StoredBuild space={space} app={app} sequence={sequence} />
    </div>
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
    <div className="space-y-4">
      <div className="flex flex-wrap items-center justify-between gap-2 text-xs text-muted">
        <span className="flex items-center gap-1.5">
          <Clock3 className="size-3.5" aria-hidden /> Finished {when}
        </span>
        {release.truncated && <span>Older output was trimmed; the result and findings are complete.</span>}
      </div>
      {!release.plan ? (
        <Card className="p-4 text-sm text-muted">This older build has no structured plan to display.</Card>
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
      {release.dockerfile ? <DockerfileCard content={release.dockerfile} /> : null}
    </div>
  )
}

/** The recipe the image was built from — the durable half of an AI build.
 * Saved as .donkey/Dockerfile, every later deploy is a plain build. */
function DockerfileCard({ content }: { content: string }) {
  const [copied, setCopied] = useState(false)
  return (
    <Card className="p-4">
      <div className="mb-2 flex items-center justify-between gap-2">
        <span className="flex items-center gap-1.5 text-sm font-medium text-ink-soft">
          <FileCode2 className="size-4" aria-hidden /> Dockerfile
        </span>
        <button
          type="button"
          className="rounded-md border border-line px-2 py-1 text-xs text-muted hover:border-line-strong"
          onClick={() => {
            void navigator.clipboard.writeText(content).then(() => {
              setCopied(true)
              setTimeout(() => setCopied(false), 2000)
            })
          }}
        >
          {copied ? 'Copied' : 'Copy — save as .donkey/Dockerfile'}
        </button>
      </div>
      <pre className="max-h-96 overflow-auto rounded-md bg-surface p-3 font-mono text-xs leading-relaxed whitespace-pre">
        {content}
      </pre>
    </Card>
  )
}
