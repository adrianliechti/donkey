import { useEffect, useRef, useState } from 'react'
import {
  Box,
  Check,
  ChevronRight,
  Circle,
  CircleAlert,
  Database,
  Loader2,
  Rocket,
  Terminal,
  Wrench,
  XCircle,
} from 'lucide-react'
import type { AppScan, DeployResult, DeployStage, DeployUpdate, Plan, PlanSignal } from './api'
import { DiffView } from './DiffView'
import { FindingsList } from './FindingsList'
import { ScanSection } from './ScanSection'
import { Button, Callout, Chip, CodeCard, Disclosure, Eyebrow, List, ListRow, Section, SectionHeader } from './ui'

// A release reads top to bottom like the Overview tab: a status callout,
// then plain titled sections — build, services, problems, repairs — and the
// log at the end. Nothing here is a card: sections are divided by rules,
// groups that need an outline are lists, and only the log is a frame.

const stages: { id: DeployStage; label: string; detail: string }[] = [
  { id: 'detecting', label: 'Inspect source', detail: 'Confirm the reviewed build plan' },
  { id: 'building', label: 'Build image', detail: 'Build, verify, and publish the container' },
  { id: 'releasing', label: 'Release', detail: 'Apply services and wait for a healthy app' },
]

function statusText(stage: DeployStage | undefined, result?: DeployResult): string {
  if (result && !result.ok) return 'Deployment failed'
  if (stage === 'building') return 'Building the image'
  if (stage === 'releasing') return 'Releasing and checking health'
  return 'Inspecting the source'
}

function StateIcon({ state }: { state: 'complete' | 'current' | 'failed' | 'waiting' }) {
  if (state === 'complete') return <Check className="size-4 shrink-0 text-success" aria-hidden />
  if (state === 'failed') return <XCircle className="size-4 shrink-0 text-danger" aria-hidden />
  if (state === 'current') return <Loader2 className="size-4 shrink-0 animate-spin text-accent" aria-hidden />
  return <Circle className="size-4 shrink-0 text-line-strong" aria-hidden />
}

// Status: where the release stands and the steps it went through. A
// release that went through needs no banner — the steps all show done, and
// the app's address is one click away in its header.
function ReleaseState({ update, result }: { update: DeployUpdate; result?: DeployResult }) {
  const current = Math.max(
    0,
    stages.findIndex((stage) => stage.id === (update.stage ?? 'detecting')),
  )
  const failed = Boolean(result && !result.ok)

  return (
    <div className="space-y-3">
      {!result?.ok && (
        <Callout
          tone={failed ? 'danger' : 'info'}
          icon={failed ? CircleAlert : Rocket}
          title={<span aria-live="polite">{statusText(update.stage, result)}</span>}
        >
          {failed ? (
            <span className="whitespace-pre-wrap break-words">{result?.error || 'The release did not complete.'}</span>
          ) : undefined}
        </Callout>
      )}
      <List>
        {stages.map((stage, index) => {
          let state: 'complete' | 'current' | 'failed' | 'waiting' = 'waiting'
          if (result?.ok || index < current) state = 'complete'
          else if (index === current) state = failed ? 'failed' : 'current'
          return (
            <ListRow key={stage.id}>
              <StateIcon state={state} />
              <span className={`text-sm ${state === 'waiting' ? 'text-muted' : 'text-ink'}`}>{stage.label}</span>
              <span className="min-w-0 flex-1 truncate text-xs text-muted">{stage.detail}</span>
            </ListRow>
          )
        })}
      </List>
    </div>
  )
}

export function SignalChips({ signals }: { signals: PlanSignal[] }) {
  return (
    <span className="flex flex-wrap gap-1.5">
      {signals.map((signal, index) => (
        <Chip key={`${signal.value}-${index}`} title={signal.evidence ? `Detected from ${signal.evidence}` : undefined}>
          {signal.value}
        </Chip>
      ))}
    </span>
  )
}

const exposureLabel: Record<Plan['app']['exposure'], string> = {
  protected: 'behind the login',
  public: 'public',
  internal: 'internal',
}

// One row of the build facts: the label in a fixed column, the value beside
// it — so the facts read down one edge instead of as scattered groups.
function Fact({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <>
      <dt className="text-xs text-muted">{label}</dt>
      <dd className="min-w-0 text-sm text-ink">{children}</dd>
    </>
  )
}

// Build: how the image was made and how it runs, as one list of facts.
// What the app *is* lives on the Overview tab.
function DetectionDetails({ plan, update, result }: { plan: Plan; update: DeployUpdate; result?: DeployResult }) {
  const stack = [...(plan.build.runtimes ?? []), ...(plan.build.frameworks ?? [])]
  const versions = plan.build.versions ?? []
  const images = update.baseImages?.length ? update.baseImages : (plan.build.baseImages ?? [])
  const packageManagers = plan.build.packageManagers ?? []
  const packages = plan.build.systemPackages ?? []

  return (
    <Section className="space-y-3">
      <SectionHeader title="Build" />
      <dl className="grid grid-cols-[6.5rem_minmax(0,1fr)] items-baseline gap-x-4 gap-y-3">
        <Fact label="Recipe">
          <span className="font-mono text-xs">{plan.build.summary}</span>
        </Fact>
        {stack.length > 0 && (
          <Fact label="Stack">
            <SignalChips signals={stack} />
          </Fact>
        )}
        {versions.length > 0 && (
          <Fact label="Versions">
            <SignalChips signals={versions} />
          </Fact>
        )}
        <Fact label="Base image">
          {images.length > 0 ? (
            images.map((image) => (
              <span key={image} className="block break-all font-mono text-xs">
                {image}
              </span>
            ))
          ) : (
            <span className="text-xs text-muted">
              {plan.build.strategy === 'ai'
                ? 'Chosen when donkey writes the Dockerfile; it appears here before the build starts.'
                : 'No FROM instruction was found.'}
            </span>
          )}
        </Fact>
        {(packageManagers.length > 0 || packages.length > 0) && (
          <Fact label="Packages">
            <span className="flex flex-col gap-1.5">
              {packageManagers.length > 0 && <SignalChips signals={packageManagers} />}
              {packages.map((item) => (
                <span key={item.dependency} className="font-mono text-xs">
                  {item.dependency}
                  {(item.build ?? []).length > 0 && <span className="text-muted"> · build </span>}
                  {item.build?.join(' ')}
                  {(item.runtime ?? []).length > 0 && <span className="text-muted"> · runtime </span>}
                  {item.runtime?.join(' ')}
                </span>
              ))}
            </span>
          </Fact>
        )}
        <Fact label="Runs">
          <span className="text-xs">
            port <span className="font-mono">{plan.app.port}</span> · {exposureLabel[plan.app.exposure]}
          </span>
        </Fact>
        {result?.imageRef && (
          <Fact label="Image">
            <span className="break-all font-mono text-xs">{result.imageRef}</span>
          </Fact>
        )}
      </dl>
    </Section>
  )
}

function ServicesNeeded({ plan }: { plan: Plan }) {
  const services = plan.services ?? []
  const storage = plan.app.storage ?? []
  return (
    <Section className="space-y-3">
      <SectionHeader title="Services" />
      {services.length === 0 ? (
        <p className="text-sm text-muted">No backing service dependencies were detected.</p>
      ) : (
        <List>
          {services.map((service) => {
            const state = !service.known
              ? 'unsupported'
              : !service.selected
                ? 'not included'
                : service.exists
                  ? 'connects to the existing one'
                  : 'created with the release'
            const on = service.selected && service.known
            return (
              <ListRow key={service.name}>
                <Database className={`size-4 shrink-0 ${on ? 'text-ink-soft' : 'text-muted/60'}`} aria-hidden />
                <div className="min-w-0 flex-1">
                  <span className={`font-mono text-sm ${on ? 'text-ink' : 'text-muted'}`}>{service.name}</span>
                  {service.type !== service.name && (
                    <span className="ml-2 font-mono text-[11px] text-muted">{service.type}</span>
                  )}
                  {service.reason && <p className="truncate text-xs text-muted">{service.reason}</p>}
                </div>
                <span className={`shrink-0 text-xs ${on ? 'text-success' : 'text-muted'}`}>{state}</span>
              </ListRow>
            )
          })}
        </List>
      )}
      {storage.length > 0 && (
        <p className="text-xs text-muted">
          <Box className="mr-1.5 inline size-3.5 align-[-3px]" aria-hidden />
          One {plan.app.storageSize || '1Gi'} disk keeps{' '}
          <span className="font-mono text-ink-soft">{storage.join(', ')}</span>.
        </p>
      )}
    </Section>
  )
}

// Problems only appear when there is one: the status callout already says
// when everything went well.
function Problems({ plan, result }: { plan: Plan; result?: DeployResult }) {
  const findings = (plan.findings ?? []).filter((finding) => finding.level !== 'info')
  const proposals = result?.proposals ?? []
  const failed = Boolean(result && !result.ok)
  if (findings.length === 0 && !(failed && (result?.diagnosis || proposals.length > 0))) return null
  return (
    <Section className="space-y-3">
      <SectionHeader title="Problems" />
      {failed && result?.diagnosis && (
        <div className="space-y-1">
          <Eyebrow>Diagnosis</Eyebrow>
          <p className="text-sm leading-relaxed text-ink">{result.diagnosis}</p>
        </div>
      )}
      <FindingsList findings={findings} />
      {proposals.length > 0 && (
        <div className="space-y-1.5">
          <Eyebrow>Suggested fixes</Eyebrow>
          <List>
            {proposals.map((proposal, index) => (
              <ListRow key={index} className="items-start">
                <Wrench className="mt-0.5 size-4 shrink-0 text-muted" aria-hidden />
                <div className="min-w-0 flex-1 space-y-1">
                  {proposal.type === 'set_env' && (
                    <code className="block break-all font-mono text-xs text-ink">
                      donkey env set {proposal.key}={proposal.value}
                    </code>
                  )}
                  {proposal.type === 'edit_file' && (
                    <span className="block font-mono text-xs text-ink">{proposal.path}</span>
                  )}
                  <p className="text-xs leading-relaxed text-muted">{proposal.explanation}</p>
                  {proposal.diff && <DiffView diff={proposal.diff} />}
                </div>
              </ListRow>
            ))}
          </List>
        </div>
      )}
    </Section>
  )
}

function ReleaseLog({
  lines,
  buildLog,
  running,
  sticky = true,
}: {
  lines: string[]
  buildLog: string
  running: boolean
  sticky?: boolean
}) {
  const pane = useRef<HTMLDivElement>(null)
  const pinned = useRef(true)
  // Beside a live release the log is a pane of its own, build output and
  // all. In the stored build's page it is part of the document — no box
  // that scrolls inside the page — so the build output, which can run to
  // thousands of lines, waits to be asked for.
  const [showBuild, setShowBuild] = useState(sticky)
  const buildLines = buildLog ? buildLog.trimEnd().split('\n').length : 0

  useEffect(() => {
    if (pane.current && pinned.current) pane.current.scrollTop = pane.current.scrollHeight
  }, [lines, buildLog])

  return (
    <CodeCard
      icon={Terminal}
      title="Deployment log"
      className={sticky ? 'min-h-[34rem] lg:sticky lg:top-20 lg:h-[calc(100vh-7rem)] lg:max-h-[52rem]' : ''}
      meta={
        <>
          <span className={`size-1.5 rounded-full ${running ? 'animate-pulse bg-accent' : 'bg-muted'}`} aria-hidden />
          {running ? 'live' : 'finished'}
        </>
      }
    >
      <div
        ref={pane}
        role="log"
        aria-live="off"
        tabIndex={0}
        onScroll={() => {
          const el = pane.current
          if (el) pinned.current = el.scrollHeight - el.scrollTop - el.clientHeight < 48
        }}
        className="h-full focus-visible:outline-2 focus-visible:outline-inset focus-visible:outline-accent-strong"
      >
        {lines.length === 0 && !buildLog && (
          <p className="flex items-center gap-2 text-muted">
            <Loader2 className="size-3.5 animate-spin" aria-hidden /> Waiting for the first release event…
          </p>
        )}
        <div className="space-y-1">
          {lines.map((line, index) => (
            <p
              key={index}
              className={`whitespace-pre-wrap break-words ${line.startsWith('✓') ? 'text-success' : line.startsWith('·') ? 'text-ink-soft' : 'text-emphasis'}`}
            >
              {line}
            </p>
          ))}
        </div>
        {buildLog && (
          <div className={`${lines.length > 0 ? 'mt-4 border-t border-line/70 pt-3' : ''}`}>
            <button
              type="button"
              onClick={() => setShowBuild(!showBuild)}
              aria-expanded={showBuild}
              className="mb-2 flex items-center gap-1 rounded text-[10px] tracking-wider text-muted uppercase transition-colors hover:text-ink focus-visible:outline-2 focus-visible:outline-accent-strong"
            >
              <ChevronRight className={`size-3 transition-transform ${showBuild ? 'rotate-90' : ''}`} aria-hidden />
              Build output · {buildLines} {buildLines === 1 ? 'line' : 'lines'}
            </button>
            {showBuild && <pre className="whitespace-pre-wrap break-words text-ink-soft">{buildLog}</pre>}
          </div>
        )}
      </div>
    </CodeCard>
  )
}

export function ReleaseView({
  plan,
  lines,
  buildLog,
  update,
  result,
  onOpen,
  onBackToReview,
  onDeployAnother,
  scan,
  layout = 'split',
}: {
  plan: Plan
  lines: string[]
  buildLog: string
  update: DeployUpdate
  result?: DeployResult
  onOpen?: () => void
  onBackToReview?: () => void
  onDeployAnother?: () => void
  /** The stored scan; a live release carries its own on the update. */
  scan?: AppScan
  layout?: 'split' | 'stacked'
}) {
  const scanned = scan ?? update.scan
  const running = result === undefined
  const actions = result && (onOpen || onBackToReview || onDeployAnother) && (
    <div className="flex flex-wrap items-center justify-end gap-2">
      {result.ok ? (
        <>
          {onDeployAnother && (
            <Button kind="quiet" onClick={onDeployAnother}>
              Deploy another
            </Button>
          )}
          {onOpen && <Button onClick={onOpen}>Open the app page</Button>}
        </>
      ) : (
        <>
          {onDeployAnother && (
            <Button kind="quiet" onClick={onDeployAnother}>
              Pick something else
            </Button>
          )}
          {onBackToReview && <Button onClick={onBackToReview}>Back to review</Button>}
        </>
      )}
    </div>
  )
  const details = (
    <div className="flex flex-col gap-5">
      <ReleaseState update={update} result={result} />
      <DetectionDetails plan={plan} update={update} result={result} />
      {scanned && <ScanSection scan={scanned} />}
      <ServicesNeeded plan={plan} />
      <Problems plan={plan} result={result} />
      {Boolean(update.changes?.length) && (
        <Section className="space-y-3">
          <SectionHeader
            title="Automatic repairs"
            description="Applied to this build. Keep these changes with the Dockerfile to reproduce it."
          />
          <div className="flex flex-col gap-2">
            {update.changes?.map((change, index) => (
              <Disclosure key={`${change.path}-${index}`} summary={<span className="font-mono">{change.path}</span>}>
                <DiffView diff={change.diff} />
              </Disclosure>
            ))}
          </div>
        </Section>
      )}
      {actions}
    </div>
  )

  if (layout === 'stacked') {
    return (
      <div className="flex flex-col gap-5">
        {details}
        <ReleaseLog lines={lines} buildLog={buildLog} running={running} sticky={false} />
      </div>
    )
  }

  return (
    <div className="grid items-start gap-6 lg:grid-cols-2">
      <ReleaseLog lines={lines} buildLog={buildLog} running={running} />
      {details}
    </div>
  )
}
