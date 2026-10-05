import { CircleAlert, Loader2, RefreshCw, Wrench } from 'lucide-react'
import { DiffView } from './DiffView'
import { useDiagnosis } from './queries'
import { Callout, ErrorNote, Eyebrow, IconButton, List, ListRow, Section, SectionHeader, wellClass } from './ui'

// The diagnosis of an app that isn't fully up, at the top of the Logs tab:
// the same evidence a failed rollout collects, read by the assistant as soon
// as the tab opens — no button to find first. The raw logs follow below it.
export function DiagnosisSection({ space, app, image }: { space: string; app: string; image: string }) {
  const { data: result, error, reload, fetching } = useDiagnosis(space, app, image)
  const proposals = result?.proposals ?? []
  const events = result?.events ?? []
  const reading = fetching && !result

  return (
    <Section className="space-y-3">
      <SectionHeader
        title="Diagnosis"
        actions={
          <IconButton icon={RefreshCw} label={fetching ? 'Reading the evidence…' : 'Check again'} onClick={reload}
            disabled={fetching} className={fetching ? '[&>svg]:animate-spin' : ''} />
        }
      />
      <Callout
        tone="danger"
        icon={reading ? Loader2 : CircleAlert}
        title="This app isn't fully up."
      >
        {reading
          ? 'Reading the logs and cluster events…'
          : result?.diagnosis || (result?.assistantError
            ? `The assistant couldn't be reached (${result.assistantError}). The raw evidence is below.`
            : undefined)}
      </Callout>
      <ErrorNote message={error} />
      {proposals.length > 0 && (
        <div className="space-y-1.5">
          <Eyebrow>Suggested fixes</Eyebrow>
          <List>
            {proposals.map((p, i) => (
              <ListRow key={i} className="items-start">
                <Wrench className="mt-0.5 size-4 shrink-0 text-muted" aria-hidden />
                <div className="min-w-0 flex-1 space-y-1">
                  {p.type === 'set_env' && (
                    <code className="block break-all font-mono text-xs text-ink">
                      donkey env set {p.key}={p.value}
                    </code>
                  )}
                  {p.type === 'edit_file' && <span className="block font-mono text-xs text-ink">{p.path}</span>}
                  <p className="text-xs text-muted">{p.explanation}</p>
                  {p.diff && <DiffView diff={p.diff} />}
                </div>
              </ListRow>
            ))}
          </List>
        </div>
      )}
      {events.length > 0 && (
        <div className="space-y-1.5">
          <Eyebrow>Cluster events</Eyebrow>
          <pre className={`${wellClass} whitespace-pre-wrap`}>{events.join('\n')}</pre>
        </div>
      )}
    </Section>
  )
}
