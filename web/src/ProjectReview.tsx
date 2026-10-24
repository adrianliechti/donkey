import { Fragment, useState } from 'react'
import { ArrowRight } from 'lucide-react'
import { planEdit, type PlanEdit, type ProjectPlan } from './api'
import { PlanReview } from './PlanReview'
import { FindingsList } from './FindingsList'
import { DiffView } from './DiffView'
import { Button, Disclosure, ErrorNote, Field, Input, List, ListRow, Section, SectionHeader } from './ui'

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
  const changed = edits.length !== reviewed.length || edits.some((edit, index) => !sameEdit(edit, reviewed[index]))
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
    <div className="flex flex-col gap-5">
      <fieldset disabled={busy || checking} className="flex min-w-0 flex-col gap-5 disabled:opacity-70">
        <Section className="space-y-4">
          <SectionHeader
            title={plan.apps.length === 1 ? 'One app will be deployed' : `${plan.apps.length} apps will be deployed`}
            description={
              plan.apps.length > 1
                ? 'Each runs separately in the same space.'
                : 'Review what donkey read from the project, then deploy.'
            }
          />
          {!space && (
            <Field label="Space for all apps">
              <Input
                value={edits[0].space}
                list="project-spaces"
                onChange={(e) => change(edits.map((edit) => ({ ...edit, space: e.target.value })))}
              />
              <datalist id="project-spaces">
                {spaces.map((value) => (
                  <option key={value} value={value} />
                ))}
              </datalist>
            </Field>
          )}
        </Section>
        {plan.apps.map((app, index) => (
          <Fragment key={app.id}>
            {/* With several apps, each gets a heading of its own. */}
            {plan.apps.length > 1 && (
              <Section>
                <h2 className="font-mono text-base font-bold">{edits[index].name || app.app.name}</h2>
                <p className="font-mono text-xs text-muted">{app.root || '.'}</p>
              </Section>
            )}
            <PlanReview
              plan={app}
              edit={edits[index]}
              onEdit={(edit) => change(edits.map((prev, i) => (i === index ? edit : prev)))}
            />
          </Fragment>
        ))}
      </fieldset>
      {plan.connections.length > 0 && (
        <Section className="space-y-3">
          <SectionHeader
            title="Connections"
            description="Service references are resolved when the app starts. Browser calls use the app’s own address."
          />
          <List>
            {plan.connections.map((c, i) => (
              <ListRow key={i} className="items-start">
                <ArrowRight className="mt-0.5 size-4 shrink-0 text-muted" aria-hidden />
                <div className="min-w-0 flex-1">
                  <p className="text-sm">
                    <span className="font-mono">{c.app}</span> → <span className="font-mono">{c.target}</span>
                  </p>
                  {c.env && (
                    <p className="break-all font-mono text-xs text-ink-soft">
                      {c.env} = {c.value}
                    </p>
                  )}
                  <p className="text-xs text-muted">{c.reason}</p>
                </div>
              </ListRow>
            ))}
          </List>
        </Section>
      )}
      {plan.changes.length > 0 && (
        <Section className="space-y-3">
          <SectionHeader
            title="Connection fixes"
            description="These changes are applied to the uploaded copy before building."
          />
          <div className="flex flex-col gap-2">
            {plan.changes.map((change) => (
              <Disclosure key={change.file} summary={<span className="font-mono">{change.file}</span>}>
                <DiffView diff={change.diff} />
              </Disclosure>
            ))}
          </div>
        </Section>
      )}
      {plan.findings.length > 0 && (
        <Section className="space-y-3">
          <SectionHeader title="Project checks" />
          <FindingsList findings={plan.findings} />
        </Section>
      )}
      <Section className="space-y-3">
        <ErrorNote message={error} />
        {changed && (
          <p className="text-xs text-muted">
            Deploy will check these changes first. Check them now to preview updated connections and deployment details.
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
            <Button onClick={() => onDeploy(edits)} disabled={busy || checking || (!changed && !plan.ready)}>
              {busy
                ? 'Preparing…'
                : plan.apps.length > 1
                  ? `Deploy all ${plan.apps.length} apps`
                  : `Deploy ${edits[0].name || plan.apps[0].app.name}`}
            </Button>
          </div>
        </div>
      </Section>
    </div>
  )
}
