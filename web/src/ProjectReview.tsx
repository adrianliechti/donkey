import { useState } from 'react'
import { planEdit, type PlanEdit, type ProjectPlan } from './api'
import { PlanReview } from './PlanReview'
import { FindingsList } from './FindingsList'
import { DiffView } from './DiffView'
import { Button, Card, ErrorNote, SectionTitle } from './ui'

// Dirty state is a fact about the current answers, not a one-way event. Keeping
// it as a boolean meant that changing a choice and then changing it back still
// left the deploy button disabled. Checkbox arrays are selections rather than
// ordered values: unticking and re-ticking an item may append it in a different
// position, but that is still the same answer.
function sameSelection(left: string[], right: string[]): boolean {
  if (left.length !== right.length) return false
  const sortedLeft = [...left].sort()
  const sortedRight = [...right].sort()
  return sortedLeft.every((value, index) => value === sortedRight[index])
}

function sameEdit(left: PlanEdit, right: PlanEdit): boolean {
  return (
    left.name === right.name &&
    left.space === right.space &&
    left.host === right.host &&
    left.port === right.port &&
    left.healthcheck === right.healthcheck &&
    left.exposure === right.exposure &&
    left.mode === right.mode &&
    left.cors === right.cors &&
    sameSelection(left.storage, right.storage) &&
    sameSelection(left.services, right.services)
  )
}

export function ProjectReview({
  plan,
  spaces,
  space,
  busy,
  error,
  onCheck,
  onDeploy,
  onStartOver,
}: {
  plan: ProjectPlan
  spaces: string[]
  space?: string
  busy: boolean
  error: string
  onCheck: (edits: PlanEdit[]) => Promise<ProjectPlan>
  onDeploy: (edits: PlanEdit[]) => void
  onStartOver: () => void
}) {
  const [edits, setEdits] = useState(() => plan.apps.map(planEdit))
  const [checking, setChecking] = useState(false)
  const reviewed = plan.apps.map(planEdit)
  const changed =
    edits.length !== reviewed.length || edits.some((edit, index) => !sameEdit(edit, reviewed[index]))
  const change = (next: PlanEdit[]) => setEdits(next)

  async function check() {
    setChecking(true)
    try {
      const checked = await onCheck(edits)
      setEdits(checked.apps.map(planEdit))
    } catch {
      // The parent owns the visible error. Keep the draft intact so it can be
      // corrected or retried without producing an unhandled click promise.
    } finally {
      setChecking(false)
    }
  }
  return (
    <div className="space-y-4">
      <p className="text-sm text-ink-soft">
        {plan.apps.length === 1 ? 'One app' : `${plan.apps.length} apps`} will be deployed.
        {plan.apps.length > 1 && ' Each runs separately in the same space.'}
      </p>
      <fieldset disabled={busy || checking} className="space-y-4 disabled:opacity-70">
        {!space && (
          <label className="block text-sm">
            <span className="mb-1 block text-xs text-muted">Space for all apps</span>
            <input
              value={edits[0].space}
              list="project-spaces"
              className="w-full rounded-md border border-line bg-surface px-2 py-1.5 font-mono text-sm"
              onChange={(e) => change(edits.map((edit) => ({ ...edit, space: e.target.value })))}
            />
            <datalist id="project-spaces">
              {spaces.map((value) => (
                <option key={value} value={value} />
              ))}
            </datalist>
          </label>
        )}
        {plan.apps.map((app, index) => (
          <details
            key={app.id}
            open={plan.apps.length === 1 || !app.ready || undefined}
            className="rounded-lg border border-line p-4"
          >
            <summary className="cursor-pointer text-sm font-medium">
              {edits[index].name || app.app.name}{' '}
              <span className="font-mono text-xs text-muted">{app.root || '.'}</span>
            </summary>
            <div className="mt-4">
              <PlanReview
                plan={app}
                edit={edits[index]}
                onEdit={(edit) => change(edits.map((prev, i) => (i === index ? edit : prev)))}
              />
            </div>
          </details>
        ))}
      </fieldset>
      {plan.connections.length > 0 && (
        <Card className="space-y-3 p-4">
          <SectionTitle>Connections</SectionTitle>
          {plan.connections.map((c, i) => (
            <div key={i} className="text-sm">
              <p>
                {c.app} → {c.target}
              </p>
              {c.env && (
                <p className="break-all font-mono text-xs text-ink-soft">
                  {c.env} = {c.value}
                </p>
              )}
              <p className="text-xs text-muted">{c.reason}</p>
            </div>
          ))}
          <p className="text-xs text-muted">
            Service references are resolved when the app starts. Browser calls use the app’s own
            address.
          </p>
        </Card>
      )}
      {plan.changes.length > 0 && (
        <Card className="space-y-3 p-4">
          <SectionTitle>Connection fixes</SectionTitle>
          <p className="text-xs text-muted">
            These changes are applied to the uploaded copy before building.
          </p>
          {plan.changes.map((change) => (
            <details key={change.file}>
              <summary className="cursor-pointer font-mono text-sm">{change.file}</summary>
              <DiffView diff={change.diff} />
            </details>
          ))}
        </Card>
      )}
      <FindingsList findings={plan.findings} />
      <ErrorNote message={error} />
      {changed && (
        <p className="text-xs text-muted">
          Deploy will check these changes first. Check them now to preview updated connections and
          deployment details.
        </p>
      )}
      <div className="flex flex-wrap items-center justify-between gap-3">
        <Button kind="quiet" onClick={onStartOver} disabled={busy || checking}>
          Pick something else
        </Button>
        <div className="flex gap-2">
          <Button kind="quiet" onClick={check} disabled={busy || checking}>
            {checking ? 'Checking…' : 'Check changes'}
          </Button>
          <Button
            onClick={() => onDeploy(edits)}
            disabled={busy || checking || (!changed && !plan.ready)}
          >
            {busy
              ? 'Preparing…'
              : plan.apps.length > 1
                ? `Deploy all ${plan.apps.length} apps`
                : `Deploy ${edits[0].name || plan.apps[0].app.name}`}
          </Button>
        </div>
      </div>
    </div>
  )
}
