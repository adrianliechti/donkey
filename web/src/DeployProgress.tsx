import { Check, Loader2, Rocket } from 'lucide-react'
import type { ActiveDeploy } from './api'
import { useDeploy } from './deploy'
import { ago } from './format'
import { useNow } from './hooks'
import { Button } from './ui'

// A deploy as it goes, said the same way wherever it shows: in a space's
// tile, in the fleet's activity rail, on the page of an app that does not
// run yet, and above a running app while its next version is on its way.

const STAGES: { id: ActiveDeploy['stage']; label: string }[] = [
  { id: 'detecting', label: 'Inspect' },
  { id: 'building', label: 'Build' },
  { id: 'releasing', label: 'Release' },
]

const stageText: Record<ActiveDeploy['stage'], string> = {
  queued: 'Waiting for the deploy before it',
  detecting: 'Inspecting the source',
  building: 'Building the image',
  releasing: 'Releasing and checking health',
  done: 'Deployed',
  failed: 'Deploy failed',
}

/**
 * A deploy in the two lines a tile gives an app's address and health, so the
 * tile keeps its height — and the grid its rows — while the app deploys.
 */
export function DeployState({ deploy }: { deploy: ActiveDeploy }) {
  useNow(deploy.finishedAt ? 30_000 : 5_000)
  const failed = deploy.stage === 'failed'
  const who = [deploy.by, deploy.via === 'cli' ? 'CLI' : 'console'].filter(Boolean).join(' · ')
  return (
    <div className="mt-2 space-y-1 text-xs">
      <div className="flex items-center justify-between gap-3">
        <span className={`truncate ${failed ? 'text-danger' : 'text-accent'}`}>{stageText[deploy.stage]}</span>
        <span className="shrink-0 text-muted" title={who}>
          {ago(deploy.startedAt)} ago
        </span>
      </div>
      {failed ? (
        <div className="truncate font-mono text-danger" title={deploy.error}>
          {deploy.error}
        </div>
      ) : (
        // A line's height, the bar centred in it.
        <div className="grid h-4 content-center">
          <StageBar deploy={deploy} />
        </div>
      )}
    </div>
  )
}

/**
 * Where a deploy stands: its three steps, the latest line of its log, who
 * started it and from where. Compact for a card; full for an app's page.
 */
export function DeployProgress({ deploy, compact = false }: { deploy: ActiveDeploy; compact?: boolean }) {
  useNow(deploy.finishedAt ? 30_000 : 5_000) // keeps "started 2m ago" counting
  const failed = deploy.stage === 'failed'
  const who = [deploy.by, deploy.via === 'cli' ? 'CLI' : 'console'].filter(Boolean).join(' · ')
  return (
    <div className={compact ? 'space-y-2' : 'space-y-3'}>
      <div className="flex items-center justify-between gap-3 text-xs">
        <span className={`truncate ${failed ? 'text-danger' : 'text-ink-soft'}`}>{stageText[deploy.stage]}</span>
        <span className="shrink-0 text-muted">
          {ago(deploy.startedAt)} ago{who && ` · ${who}`}
        </span>
      </div>
      <StageBar deploy={deploy} />
      {!compact && (
        <div className="flex justify-between text-xs text-muted">
          {STAGES.map((stage) => (
            <span key={stage.id}>{stage.label}</span>
          ))}
        </div>
      )}
      {(failed ? deploy.error : deploy.line) && (
        // A live log line stays on one line so the block keeps its height
        // while it changes; a failure is final, and its reason — which
        // names the finding that refused an image — is shown in full.
        <p
          className={`font-mono text-xs ${failed ? 'text-danger' : 'text-muted'} ${failed && !compact ? 'break-words' : 'truncate'}`}
          title={failed ? deploy.error : deploy.line}
        >
          {failed ? deploy.error : deploy.line}
        </p>
      )}
    </div>
  )
}

// Three segments: inspect, build, release. The current one breathes.
function StageBar({ deploy }: { deploy: ActiveDeploy }) {
  const failed = deploy.stage === 'failed'
  const at = deploy.stage === 'done' ? STAGES.length : STAGES.findIndex((s) => s.id === deploy.stage)
  return (
    <div className="grid grid-cols-3 gap-1" aria-hidden>
      {STAGES.map((stage, i) => (
        <span
          key={stage.id}
          className={`h-1 rounded-full ${
            failed && i === Math.max(at, 0)
              ? 'bg-danger'
              : i < at
                ? 'bg-accent'
                : i === at && !failed
                  ? 'bg-accent/60 motion-safe:animate-pulse'
                  : 'bg-line'
          }`}
        />
      ))}
    </div>
  )
}

/**
 * An app that does not run yet, on its own page: its first deploy as it
 * goes, or how it failed. The page turns into the app once it runs.
 */
export function DeployPlaceholder({ deploy }: { deploy: ActiveDeploy }) {
  const openDeploy = useDeploy()
  const failed = deploy.stage === 'failed'
  const done = deploy.stage === 'done'
  return (
    <div
      className={`rounded-lg border p-6 ${failed ? 'border-dashed border-danger/50 bg-danger/[.03]' : done ? 'border-line bg-card' : 'border-dashed border-accent/50 bg-accent-soft/30'}`}
    >
      <div className="flex min-h-8 flex-wrap items-center justify-between gap-3">
        <div className="flex items-center gap-2.5">
          {failed ? (
            <span className="size-3 rounded-full border-2 border-danger" aria-hidden />
          ) : done ? (
            <Check className="size-4 text-success" aria-hidden />
          ) : (
            <Loader2 className="size-4 animate-spin text-accent" aria-hidden />
          )}
          <h2 className="text-sm font-semibold text-ink">
            {failed ? 'The first deploy failed' : done ? 'Deployment finished' : 'Deploying the first version'}
          </h2>
        </div>
        {failed && (
          <Button kind="quiet" icon={Rocket} onClick={() => openDeploy({ space: deploy.space, name: deploy.app })}>
            Deploy again
          </Button>
        )}
      </div>
      <div className="mt-4">
        <DeployProgress deploy={deploy} />
      </div>
      <p className="mt-4 text-xs text-muted">
        {failed
          ? 'Nothing runs yet. Resolve the error and deploy again.'
          : done
            ? 'The deployment finished. Waiting for the latest app status.'
            : 'The rest of this page appears as soon as the app runs — you can close the deploy window or the browser meanwhile.'}
      </p>
    </div>
  )
}
