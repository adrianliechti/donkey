import { Clock3 } from 'lucide-react'
import { ReleaseView } from './ReleaseView'
import { useAppRelease } from './queries'
import { Card, ErrorNote, Spinner } from './ui'

export function StoredRelease({ space, app }: { space: string; app: string }) {
  const { data: release, error } = useAppRelease(space, app)

  if (!release && !error) return <Spinner label="Opening the last release…" />
  if (error) return <ErrorNote message={error} />
  if (!release) return null

  const finished = new Date(release.finishedAt)
  const when = Number.isNaN(finished.getTime()) ? release.finishedAt : finished.toLocaleString()

  return (
    <div className="mx-auto w-full max-w-3xl space-y-4">
      <div className="flex flex-wrap items-center justify-between gap-2 text-xs text-stone">
        <span className="flex items-center gap-1.5">
          <Clock3 className="size-3.5" aria-hidden /> Last deployment finished {when}
        </span>
        {release.truncated && <span>Older output was trimmed; the result and findings are complete.</span>}
      </div>
      {!release.plan ? (
        <Card className="p-4 text-sm text-stone">This older release has no structured plan to display.</Card>
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
    </div>
  )
}
