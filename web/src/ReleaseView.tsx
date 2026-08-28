import { useEffect, useRef } from 'react'
import {
  Box,
  Check,
  CheckCircle2,
  Circle,
  CircleAlert,
  Database,
  ExternalLink,
  Layers3,
  Loader2,
  Package,
  Rocket,
  Terminal,
  XCircle,
} from 'lucide-react'
import type { DeployResult, DeployStage, DeployUpdate, Plan, PlanSignal } from './api'
import { DiffView } from './DiffView'
import { FindingsList } from './FindingsList'
import { Button, Card, SectionTitle } from './ui'

const stages: { id: DeployStage; label: string; detail: string }[] = [
  { id: 'detecting', label: 'Inspect source', detail: 'Confirm the reviewed build plan' },
  { id: 'building', label: 'Build image', detail: 'Build, verify, and publish the container' },
  { id: 'releasing', label: 'Release', detail: 'Apply services and wait for a healthy app' },
]

function statusText(stage: DeployStage | undefined, result?: DeployResult): string {
  if (result?.ok) return 'Deployment complete'
  if (result && !result.ok) return 'Deployment failed'
  if (stage === 'building') return 'Building the image'
  if (stage === 'releasing') return 'Releasing and checking health'
  return 'Inspecting the source'
}

function StateIcon({ state }: { state: 'complete' | 'current' | 'failed' | 'waiting' }) {
  if (state === 'complete') return <Check className="size-3.5 text-moss" aria-hidden />
  if (state === 'failed') return <XCircle className="size-3.5 text-brick" aria-hidden />
  if (state === 'current') return <Loader2 className="size-3.5 animate-spin text-hay" aria-hidden />
  return <Circle className="size-3.5 text-line-bright" aria-hidden />
}

function ReleaseState({ plan, update, result }: { plan: Plan; update: DeployUpdate; result?: DeployResult }) {
  const current = Math.max(
    0,
    stages.findIndex((stage) => stage.id === (update.stage ?? 'detecting')),
  )
  const failed = Boolean(result && !result.ok)

  return (
    <Card className={`overflow-hidden ${failed ? 'border-brick/40' : result?.ok ? 'border-moss/40' : ''}`}>
      <div className="flex items-start justify-between gap-3 border-b border-line px-4 py-3">
        <div>
          <SectionTitle>Deployment state</SectionTitle>
          <p
            aria-live="polite"
            className={`mt-1 text-sm font-medium ${failed ? 'text-brick' : result?.ok ? 'text-moss' : 'text-ink'}`}
          >
            {statusText(update.stage, result)}
          </p>
        </div>
        {result?.ok ? (
          <CheckCircle2 className="size-5 text-moss" aria-hidden />
        ) : failed ? (
          <CircleAlert className="size-5 text-brick" aria-hidden />
        ) : (
          <Rocket className="size-5 text-hay" aria-hidden />
        )}
      </div>
      <div className="px-4 py-3">
        <dl className="mb-4 grid grid-cols-2 gap-x-4 gap-y-2 text-xs">
          <div>
            <dt className="text-stone">App</dt>
            <dd className="mt-0.5 truncate font-mono text-ink">{plan.app.name}</dd>
          </div>
          <div>
            <dt className="text-stone">Space</dt>
            <dd className="mt-0.5 truncate font-mono text-ink">{plan.app.space}</dd>
          </div>
          <div>
            <dt className="text-stone">Port</dt>
            <dd className="mt-0.5 font-mono text-ink">{plan.app.port}</dd>
          </div>
          <div>
            <dt className="text-stone">Access</dt>
            <dd className="mt-0.5 font-mono text-ink">{plan.app.exposure}</dd>
          </div>
        </dl>
        <ol className="space-y-2.5">
          {stages.map((stage, index) => {
            let state: 'complete' | 'current' | 'failed' | 'waiting' = 'waiting'
            if (result?.ok || index < current) state = 'complete'
            else if (index === current) state = failed ? 'failed' : 'current'
            return (
              <li key={stage.id} className="flex items-start gap-2.5">
                <span className="mt-0.5 flex size-5 shrink-0 items-center justify-center rounded-full border border-line bg-surface">
                  <StateIcon state={state} />
                </span>
                <span className="min-w-0">
                  <span className={`block text-sm ${state === 'waiting' ? 'text-stone' : 'text-ink'}`}>
                    {stage.label}
                  </span>
                  <span className="block text-xs text-stone">{stage.detail}</span>
                </span>
              </li>
            )
          })}
        </ol>
        {result?.imageRef && (
          <p className="mt-3 break-all rounded-md bg-surface px-2.5 py-2 font-mono text-[10px] text-stone">
            {result.imageRef}
          </p>
        )}
      </div>
    </Card>
  )
}

function SignalChips({ signals }: { signals: PlanSignal[] }) {
  return (
    <div className="flex flex-wrap gap-1.5">
      {signals.map((signal, index) => (
        <span
          key={`${signal.value}-${index}`}
          title={signal.evidence ? `Detected from ${signal.evidence}` : undefined}
          className="rounded border border-line bg-raised px-2 py-1 font-mono text-[11px] text-ink-soft"
        >
          {signal.value}
        </span>
      ))}
    </div>
  )
}

function DetailGroup({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div>
      <p className="mb-1.5 text-[10px] font-semibold uppercase tracking-wider text-stone">{label}</p>
      {children}
    </div>
  )
}

function DetectionDetails({ plan, update }: { plan: Plan; update: DeployUpdate }) {
  const stack = [...(plan.build.runtimes ?? []), ...(plan.build.frameworks ?? [])]
  const images = update.baseImages?.length ? update.baseImages : (plan.build.baseImages ?? [])
  const packageManagers = plan.build.packageManagers ?? []
  const packages = plan.build.systemPackages ?? []

  return (
    <Card className="space-y-4 p-4">
      <SectionTitle>
        <span className="flex items-center gap-1.5">
          <Layers3 className="size-3.5" aria-hidden /> Detection & build
        </span>
      </SectionTitle>

      {plan.appType && (
        <DetailGroup label="Application">
          <p className="text-sm text-ink">{plan.appType}</p>
          {plan.notes && <p className="mt-1 text-xs leading-relaxed text-stone">{plan.notes}</p>}
        </DetailGroup>
      )}

      <DetailGroup label="Build plan">
        <p className="text-sm text-ink-soft">{plan.build.summary}</p>
      </DetailGroup>

      {stack.length > 0 && (
        <DetailGroup label="Detected stack">
          <SignalChips signals={stack} />
        </DetailGroup>
      )}

      {(plan.build.versions ?? []).length > 0 && (
        <DetailGroup label="Pinned versions">
          <SignalChips signals={plan.build.versions ?? []} />
        </DetailGroup>
      )}

      <DetailGroup label="Base image">
        {images.length > 0 ? (
          <div className="space-y-1">
            {images.map((image) => (
              <p key={image} className="break-all rounded bg-surface px-2 py-1.5 font-mono text-xs text-parchment">
                {image}
              </p>
            ))}
          </div>
        ) : (
          <p className="text-xs text-stone">
            {plan.build.strategy === 'ai'
              ? 'Chosen when donkey writes the Dockerfile; it will appear here before the build starts.'
              : 'No FROM instruction was found.'}
          </p>
        )}
      </DetailGroup>

      {(packageManagers.length > 0 || packages.length > 0) && (
        <DetailGroup label="Packages">
          {packageManagers.length > 0 && <SignalChips signals={packageManagers} />}
          {packages.length > 0 && (
            <ul className={`${packageManagers.length > 0 ? 'mt-2' : ''} space-y-2`}>
              {packages.map((item) => (
                <li key={item.dependency} className="rounded-md border border-line bg-surface/60 px-2.5 py-2">
                  <p className="flex items-center gap-1.5 font-mono text-xs text-ink">
                    <Package className="size-3.5 text-hay" aria-hidden /> {item.dependency}
                  </p>
                  {(item.build ?? []).length > 0 && (
                    <p className="mt-1 text-[11px] text-stone">
                      build: <span className="font-mono text-ink-soft">{item.build?.join(' ')}</span>
                    </p>
                  )}
                  {(item.runtime ?? []).length > 0 && (
                    <p className="mt-0.5 text-[11px] text-stone">
                      runtime: <span className="font-mono text-ink-soft">{item.runtime?.join(' ')}</span>
                    </p>
                  )}
                </li>
              ))}
            </ul>
          )}
        </DetailGroup>
      )}
    </Card>
  )
}

function ServicesNeeded({ plan }: { plan: Plan }) {
  const services = plan.services ?? []
  const storage = plan.app.storage ?? []
  return (
    <Card className="space-y-3 p-4">
      <SectionTitle>
        <span className="flex items-center gap-1.5">
          <Database className="size-3.5" aria-hidden /> Backing services
        </span>
      </SectionTitle>
      {services.length === 0 ? (
        <p className="text-xs text-stone">No backing service dependencies were detected.</p>
      ) : (
        <ul className="space-y-2">
          {services.map((service) => {
            const state = !service.known
              ? 'unsupported'
              : !service.selected
                ? 'not included'
                : service.exists
                  ? 'connect existing'
                  : 'create with release'
            const tone = service.selected && service.known ? 'text-moss' : 'text-stone'
            return (
              <li key={service.name} className="rounded-md border border-line bg-surface/50 px-2.5 py-2">
                <div className="flex items-center justify-between gap-2">
                  <span className="flex min-w-0 items-center gap-1.5 font-mono text-xs text-ink">
                    <Database className={`size-3.5 shrink-0 ${tone}`} aria-hidden />
                    <span className="truncate">{service.name}</span>
                    {service.type !== service.name && <span className="text-stone">({service.type})</span>}
                  </span>
                  <span className={`shrink-0 text-[10px] ${tone}`}>{state}</span>
                </div>
                {service.reason && <p className="mt-1 text-[11px] leading-relaxed text-stone">{service.reason}</p>}
              </li>
            )
          })}
        </ul>
      )}
      {storage.length > 0 && (
        <p className="flex items-start gap-1.5 border-t border-line pt-2 text-xs text-stone">
          <Box className="mt-0.5 size-3.5 shrink-0" aria-hidden />
          <span>
            One {plan.app.storageSize || '1Gi'} disk keeps{' '}
            <span className="font-mono text-ink-soft">{storage.join(', ')}</span>.
          </span>
        </p>
      )}
    </Card>
  )
}

function Problems({ plan, result }: { plan: Plan; result?: DeployResult }) {
  const findings = (plan.findings ?? []).filter((finding) => finding.level !== 'info')
  const failed = Boolean(result && !result.ok)
  if (!failed && findings.length === 0) {
    return (
      <Card className="flex items-center gap-2 border-moss/25 p-4 text-sm text-moss">
        <CheckCircle2 className="size-4 shrink-0" aria-hidden /> No problems detected before release.
      </Card>
    )
  }
  return (
    <Card className="space-y-3 p-4">
      <div className="flex items-center justify-between gap-2">
        <SectionTitle>Problems</SectionTitle>
        <span className={`text-xs ${failed ? 'text-brick' : 'text-hay'}`}>
          {failed ? 'release stopped' : `${findings.length} need attention`}
        </span>
      </div>
      {failed && (
        <div className="rounded-md border border-brick/35 bg-brick/5 px-3 py-2.5">
          <p className="flex items-start gap-2 text-sm text-brick">
            <CircleAlert className="mt-0.5 size-4 shrink-0" aria-hidden />
            <span className="whitespace-pre-wrap break-words">
              {result?.error || 'The release did not complete.'}
            </span>
          </p>
          {result?.diagnosis && (
            <div className="mt-2 border-t border-brick/20 pt-2">
              <p className="text-[10px] font-semibold uppercase tracking-wider text-stone">Diagnosis</p>
              <p className="mt-1 text-sm leading-relaxed text-ink-soft">{result.diagnosis}</p>
            </div>
          )}
        </div>
      )}
      <FindingsList findings={findings} />
      {(result?.proposals ?? []).length > 0 && (
        <div className="space-y-2 border-t border-line pt-3">
          <p className="text-[10px] font-semibold uppercase tracking-wider text-stone">Suggested fixes</p>
          {(result?.proposals ?? []).map((proposal, index) => (
            <div key={index} className="rounded-md border border-line bg-surface px-3 py-2 text-sm">
              {proposal.type === 'set_env' && (
                <code className="break-all font-mono text-xs text-hay">
                  donkey env set {proposal.key}={proposal.value}
                </code>
              )}
              {proposal.type === 'edit_file' && (
                <span className="font-mono text-xs text-hay">{proposal.path}</span>
              )}
              <p className="mt-1 text-xs leading-relaxed text-ink-soft">{proposal.explanation}</p>
              {proposal.diff && <DiffView diff={proposal.diff} />}
            </div>
          ))}
        </div>
      )}
    </Card>
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

  useEffect(() => {
    if (pane.current && pinned.current) pane.current.scrollTop = pane.current.scrollHeight
  }, [lines, buildLog])

  return (
    <Card
      className={`flex min-h-[34rem] flex-col overflow-hidden ${
        sticky ? 'lg:sticky lg:top-20 lg:h-[calc(100vh-7rem)] lg:max-h-[52rem]' : 'h-[min(44rem,70vh)]'
      }`}
    >
      <div className="flex items-center justify-between gap-3 border-b border-line px-4 py-3">
        <SectionTitle>
          <span className="flex items-center gap-1.5">
            <Terminal className="size-3.5" aria-hidden /> Deployment log
          </span>
        </SectionTitle>
        <span className="flex items-center gap-1.5 text-xs text-stone">
          <span className={`size-1.5 rounded-full ${running ? 'animate-pulse bg-hay' : 'bg-stone'}`} aria-hidden />
          {running ? 'live' : 'finished'}
        </span>
      </div>
      <div
        ref={pane}
        role="log"
        aria-live="off"
        tabIndex={0}
        onScroll={() => {
          const el = pane.current
          if (el) pinned.current = el.scrollHeight - el.scrollTop - el.clientHeight < 48
        }}
        className="min-h-0 flex-1 overflow-auto bg-ink-well p-4 font-mono text-xs leading-relaxed focus-visible:outline-2 focus-visible:outline-inset focus-visible:outline-hay-deep"
      >
        {lines.length === 0 && !buildLog && (
          <p className="flex items-center gap-2 text-stone">
            <Loader2 className="size-3.5 animate-spin" aria-hidden /> Waiting for the first release event…
          </p>
        )}
        <div className="space-y-1">
          {lines.map((line, index) => (
            <p
              key={index}
              className={`whitespace-pre-wrap break-words ${line.startsWith('✓') ? 'text-moss' : line.startsWith('·') ? 'text-ink-soft' : 'text-parchment'}`}
            >
              {line}
            </p>
          ))}
        </div>
        {buildLog && (
          <div className={`${lines.length > 0 ? 'mt-4 border-t border-line/70 pt-3' : ''}`}>
            <p className="mb-2 text-[10px] uppercase tracking-wider text-stone">Build output</p>
            <pre className="whitespace-pre-wrap break-words text-[11px] leading-relaxed text-ink-soft">
              {buildLog}
            </pre>
          </div>
        )}
      </div>
    </Card>
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
  layout?: 'split' | 'stacked'
}) {
  const running = result === undefined
  const details = (
    <div className="space-y-4">
      <ReleaseState plan={plan} update={update} result={result} />
      <DetectionDetails plan={plan} update={update} />
      <ServicesNeeded plan={plan} />
      <Problems plan={plan} result={result} />

      {result && (result.url || onOpen || onBackToReview || onDeployAnother) && (
        <div className="flex flex-wrap items-center justify-end gap-2">
          {result.ok ? (
            <>
              {result.url && (
                <a
                  href={result.url}
                  target="_blank"
                  rel="noreferrer"
                  className="mr-auto flex min-w-0 items-center gap-1.5 truncate font-mono text-xs text-hay-deep hover:underline"
                >
                  <span className="truncate">{result.url}</span>
                  <ExternalLink className="size-3.5 shrink-0" aria-hidden />
                </a>
              )}
              {onDeployAnother && <Button kind="quiet" onClick={onDeployAnother}>Deploy another</Button>}
              {onOpen && <Button onClick={onOpen}>Open the app page</Button>}
            </>
          ) : (
            <>
              {onDeployAnother && <Button kind="quiet" onClick={onDeployAnother}>Pick something else</Button>}
              {onBackToReview && <Button onClick={onBackToReview}>Back to review</Button>}
            </>
          )}
        </div>
      )}
    </div>
  )

  if (layout === 'stacked') {
    return (
      <div className="space-y-4">
        {details}
        <ReleaseLog lines={lines} buildLog={buildLog} running={running} sticky={false} />
      </div>
    )
  }

  return (
    <div className="grid items-start gap-4 lg:grid-cols-2">
      <ReleaseLog lines={lines} buildLog={buildLog} running={running} />
      {details}
    </div>
  )
}
