import { useState } from 'react'
import { Stethoscope } from 'lucide-react'
import { api, type Diagnosis } from './api'
import { Button, Card, ErrorNote, errText } from './ui'
import { DiffView } from './DiffView'

// DiagnoseCard is the "explain what's wrong" moment: the same evidence a
// failed rollout collects, read by the assistant on demand.
export function DiagnoseCard({ space, app }: { space: string; app: string }) {
  const [result, setResult] = useState<Diagnosis | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  async function run() {
    setBusy(true)
    setError('')
    try {
      setResult(await api.diagnose(space, app))
    } catch (err) {
      setError(errText(err, 'The diagnosis did not work.'))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Card className="border-hay/60 bg-hay-wash/60 p-4">
      <div className="flex items-center justify-between gap-3">
        <div className="flex items-center gap-2 text-sm font-medium">
          <Stethoscope className="size-4 text-hay-deep" aria-hidden />
          This app isn't fully up.
        </div>
        <Button kind="quiet" onClick={run} disabled={busy}>
          {busy ? 'Reading the evidence…' : result ? 'Check again' : 'Explain what’s wrong'}
        </Button>
      </div>
      <ErrorNote message={error} />
      {result && (
        <div className="mt-3 space-y-3">
          {result.diagnosis && <p className="text-sm">{result.diagnosis}</p>}
          {result.assistantError && (
            <p className="text-xs text-stone">
              The assistant couldn't be reached ({result.assistantError}) — the raw evidence is below.
            </p>
          )}
          {(result.proposals ?? []).map((p, i) => (
            <div key={i} className="rounded-md border border-line bg-card px-3 py-2 text-sm">
              {p.type === 'set_env' && (
                <code className="font-mono text-xs">
                  donkey env set {p.key}={p.value}
                </code>
              )}
              {p.type === 'edit_file' && (
                <span className="font-mono text-xs">{p.path}</span>
              )}
              <p className="mt-0.5 text-xs text-ink-soft">{p.explanation}</p>
              {p.diff && <DiffView diff={p.diff} />}
            </div>
          ))}
          {(result.events ?? []).length > 0 && (
            <details className="text-xs text-ink-soft">
              <summary className="cursor-pointer text-stone">Cluster events</summary>
              <ul className="mt-1 space-y-0.5 font-mono">
                {(result.events ?? []).map((e) => (
                  <li key={e}>{e}</li>
                ))}
              </ul>
            </details>
          )}
          {result.logs && (
            <details className="text-xs">
              <summary className="cursor-pointer text-stone">Last lines from the app</summary>
              <pre className="mt-1 max-h-48 overflow-auto rounded bg-ink-well p-2 font-mono text-parchment whitespace-pre-wrap">
                {result.logs}
              </pre>
            </details>
          )}
        </div>
      )}
    </Card>
  )
}
