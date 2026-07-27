// The review screen: what donkey read out of the upload, as something to
// check rather than a form to fill in.
//
// Everything here is already answered — the port comes from the code, the
// disks from the folders the app writes to, the services from its imports.
// What the screen adds is the chance to disagree, and the checks that only
// the cluster can answer (is this name free, does the space exist, what is
// running there now). The only question it asks outright is "yes?".
import { useEffect, useRef, useState } from 'react'
import {
  ArrowRight,
  Check,
  CircleAlert,
  Copy,
  Database,
  FileCode2,
  HardDrive,
  Info,
  Loader2,
  TriangleAlert,
} from 'lucide-react'
import type { Exposure, Finding, Plan, PlanEdit, PortSource } from './api'
import { Button, Card, ErrorNote, SectionTitle } from './ui'

// editFrom is the plan as an answer sheet: the server already decided every
// value, so the screen starts from its decisions rather than from blanks.
function editFrom(plan: Plan): PlanEdit {
  return {
    name: plan.app.name,
    space: plan.app.space,
    port: plan.app.port,
    healthcheck: plan.app.healthcheck ?? '',
    storage: [...plan.app.storage],
    services: plan.services.filter((s) => s.selected).map((s) => s.name),
    exposure: plan.app.exposure,
  }
}

const PORT_SOURCE: Record<PortSource, string> = {
  config: 'from donkey.yaml',
  code: 'read out of the code',
  env: 'the app takes its port from PORT, so donkey sets this one',
  guess: "nothing in the code pins a port — donkey's guess for this stack",
}

const EXPOSURE: { value: Exposure; label: string; hint: string }[] = [
  { value: 'protected', label: 'Behind the login', hint: 'Visitors sign in through your identity provider.' },
  { value: 'public', label: 'Public', hint: 'Anyone with the address can open it.' },
  { value: 'internal', label: 'Internal', hint: 'No address of its own; only apps in this space reach it, by name.' },
]

function FindingIcon({ level }: { level: Finding['level'] }) {
  if (level === 'block') return <CircleAlert className="mt-0.5 size-4 shrink-0 text-brick" aria-label="blocked" />
  if (level === 'warn')
    return <TriangleAlert className="mt-0.5 size-4 shrink-0 text-hay" aria-label="warning" />
  return <Info className="mt-0.5 size-4 shrink-0 text-stone" aria-label="note" />
}

// FindingList is the part of the plan that has no control to live in: the
// things that are simply true about this app.
function FindingList({ findings }: { findings: Finding[] }) {
  if (findings.length === 0) return null
  return (
    <ul className="space-y-2.5">
      {findings.map((f, i) => (
        <li key={`${f.code}-${i}`} className="flex gap-2 text-sm">
          <FindingIcon level={f.level} />
          <div className="min-w-0">
            <p className={f.level === 'block' ? 'text-brick' : 'text-ink-soft'}>{f.title}</p>
            {(f.detail || f.fix) && (
              <p className="mt-0.5 text-xs text-stone">{[f.detail, f.fix].filter(Boolean).join(' ')}</p>
            )}
          </div>
        </li>
      ))}
    </ul>
  )
}

function Field({
  label,
  hint,
  children,
}: {
  label: string
  hint?: string
  children: React.ReactNode
}) {
  return (
    <label className="block text-sm">
      <span className="mb-1 block text-xs text-stone">{label}</span>
      {children}
      {hint && <span className="mt-1 block text-xs text-stone">{hint}</span>}
    </label>
  )
}

const inputClass =
  'w-full rounded-md border border-line bg-surface px-2 py-1.5 font-mono text-sm focus:outline-2 focus:outline-hay-deep'

// Choice is a checkbox that reads as a decision already made, with the
// assistant's reason for it right there — the reason is the whole point.
function Choice({
  checked,
  onChange,
  title,
  reason,
  note,
}: {
  checked: boolean
  onChange: (v: boolean) => void
  title: string
  reason?: string
  note?: string
}) {
  return (
    <label className="flex cursor-pointer items-start gap-2.5 rounded-md px-1 py-1 hover:bg-raised/60">
      <input
        type="checkbox"
        checked={checked}
        onChange={(e) => onChange(e.target.checked)}
        className="mt-0.5 size-4 shrink-0 accent-hay-deep"
      />
      <span className="min-w-0">
        <span className="block font-mono text-sm">{title}</span>
        {reason && <span className="block text-xs text-stone">{reason}</span>}
        {note && <span className="mt-0.5 block text-xs text-hay/80">{note}</span>}
      </span>
    </label>
  )
}

export function PlanReview({
  plan,
  spaces,
  onRecheck,
  onDeploy,
  busy,
  error,
  onStartOver,
}: {
  plan: Plan
  /** Existing spaces, offered as completions — a new name is still allowed. */
  spaces: string[]
  /** Sends the answers to the server, which re-checks them and returns the
   *  plan as it now stands. Settles either way — a check that failed must not
   *  leave the screen waiting for an answer that is never coming. */
  onRecheck: (edit: PlanEdit) => Promise<void>
  onDeploy: (edit: PlanEdit) => void
  busy: boolean
  error: string
  onStartOver: () => void
}) {
  const [edit, setEdit] = useState<PlanEdit>(() => editFrom(plan))
  const [checking, setChecking] = useState(false)
  const [showConfig, setShowConfig] = useState(false)
  const [copied, setCopied] = useState(false)
  const first = useRef(true)

  // Every change is re-checked, because half of what matters here is only
  // true at this instant: a free name, a space that exists, a service already
  // running. Debounced, so typing a name doesn't ask ten times.
  useEffect(() => {
    if (first.current) {
      first.current = false
      return
    }
    setChecking(true)
    const timer = setTimeout(() => {
      onRecheck(edit).finally(() => setChecking(false))
    }, 400)
    return () => clearTimeout(timer)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [edit])

  const set = <K extends keyof PlanEdit>(key: K, value: PlanEdit[K]) =>
    setEdit((prev) => ({ ...prev, [key]: value }))

  const toggle = (key: 'storage' | 'services', value: string, on: boolean) =>
    setEdit((prev) => ({
      ...prev,
      [key]: on ? [...prev[key], value] : prev[key].filter((v) => v !== value),
    }))

  const offered = plan.services.filter((s) => s.known)
  const blocked = plan.findings.filter((f) => f.level === 'block')
  const rest = plan.findings.filter((f) => f.level !== 'block')

  return (
    <div className="space-y-4">
      {/* What it is. The assistant's reading, in its own words. */}
      <Card className="space-y-2 p-4">
        <SectionTitle>What donkey found</SectionTitle>
        <p className="flex items-center gap-2 text-sm">
          <Check className="size-4 shrink-0 text-moss" aria-hidden />
          <span>
            Builds with <span className="font-mono">{plan.build.summary}</span>
          </span>
        </p>
        {plan.appType && (
          <p className="text-sm text-ink-soft">
            {plan.appType}
            {plan.notes ? ` — ${plan.notes}` : ''}
          </p>
        )}
        {plan.root && (
          <p className="text-xs text-stone">
            The app is in <span className="font-mono">{plan.root}/</span> — only that folder was read.
          </p>
        )}
      </Card>

      {/* How it runs. Answers, not blanks. */}
      <Card className="space-y-4 p-4">
        <div className="flex items-center justify-between gap-2">
          <SectionTitle>How it will run</SectionTitle>
          {checking && (
            <span className="flex items-center gap-1.5 text-xs text-stone">
              <Loader2 className="size-3 animate-spin" aria-hidden /> checking
            </span>
          )}
        </div>
        <div className="grid gap-3 sm:grid-cols-3">
          <Field label="Name">
            <input
              value={edit.name}
              onChange={(e) => set('name', e.target.value)}
              className={inputClass}
              spellCheck={false}
            />
          </Field>
          <Field label="Space" hint={plan.hasConfig ? 'from donkey.yaml' : undefined}>
            <input
              value={edit.space}
              onChange={(e) => set('space', e.target.value)}
              list="donkey-spaces"
              className={inputClass}
              spellCheck={false}
            />
            <datalist id="donkey-spaces">
              {spaces.map((s) => (
                <option key={s} value={s} />
              ))}
            </datalist>
          </Field>
          <Field label="Port" hint={PORT_SOURCE[plan.app.portSource]}>
            <input
              value={edit.port || ''}
              onChange={(e) => set('port', Number(e.target.value.replace(/\D/g, '')) || 0)}
              inputMode="numeric"
              className={inputClass}
            />
          </Field>
        </div>
        {plan.app.portReason && plan.app.portSource === 'code' && (
          <p className="text-xs text-stone">{plan.app.portReason}</p>
        )}

        <Field
          label="Health check path"
          hint="A path that answers OK when the app is fine. Empty just checks that the port answers."
        >
          <input
            value={edit.healthcheck}
            onChange={(e) => set('healthcheck', e.target.value)}
            placeholder="/healthz"
            className={inputClass}
            spellCheck={false}
          />
        </Field>

        <fieldset>
          <legend className="mb-1.5 text-xs text-stone">Who can reach it</legend>
          <div className="space-y-1">
            {EXPOSURE.map((o) => (
              <label key={o.value} className="flex cursor-pointer items-start gap-2.5 text-sm">
                <input
                  type="radio"
                  name="exposure"
                  checked={edit.exposure === o.value}
                  onChange={() => set('exposure', o.value)}
                  className="mt-1 accent-hay-deep"
                />
                <span>
                  {o.label}
                  <span className="block text-xs text-stone">{o.hint}</span>
                </span>
              </label>
            ))}
          </div>
        </fieldset>

        {plan.app.url && edit.exposure !== 'internal' && (
          <p className="rounded-md border border-line bg-surface px-3 py-2 text-sm">
            <span className="text-stone">It will answer at </span>
            <span className="font-mono text-hay">{plan.app.url}</span>
          </p>
        )}
      </Card>

      {/* The folders that hold data. Getting this wrong loses it. */}
      {plan.volumes.length > 0 && (
        <Card className="space-y-2 p-4">
          <SectionTitle>
            <span className="flex items-center gap-1.5">
              <HardDrive className="size-3.5" aria-hidden /> Data that must survive a restart
            </span>
          </SectionTitle>
          <div className="space-y-0.5">
            {plan.volumes.map((v) => (
              <Choice
                key={v.path}
                checked={edit.storage.includes(v.path)}
                onChange={(on) => toggle('storage', v.path, on)}
                title={v.path}
                reason={v.reason}
              />
            ))}
          </div>
          <p className="text-xs text-stone">
            {edit.storage.length > 0
              ? `They share one ${plan.app.storageSize || '1Gi'} disk (a folder each), and the app then runs as a single copy.`
              : 'Nothing is kept: everything the app writes disappears when it restarts.'}
          </p>
        </Card>
      )}

      {/* Services. Creating one is real, so it says which it is. */}
      {offered.length > 0 && (
        <Card className="space-y-2 p-4">
          <SectionTitle>
            <span className="flex items-center gap-1.5">
              <Database className="size-3.5" aria-hidden /> Services it needs
            </span>
          </SectionTitle>
          <div className="space-y-0.5">
            {offered.map((s) => (
              <Choice
                key={s.name}
                checked={edit.services.includes(s.name)}
                onChange={(on) => toggle('services', s.name, on)}
                title={s.name}
                reason={s.reason}
                note={
                  s.exists
                    ? 'This space already runs one — the app connects to it.'
                    : 'Created in this space when you deploy.'
                }
              />
            ))}
          </div>
        </Card>
      )}

      {/* Everything else that is true about this app. */}
      {(blocked.length > 0 || rest.length > 0) && (
        <Card className="space-y-3 p-4">
          <SectionTitle>Checks</SectionTitle>
          {blocked.length > 0 && <FindingList findings={blocked} />}
          {rest.length > 0 && <FindingList findings={rest} />}
        </Card>
      )}

      {/* The file this amounts to. The browser has nowhere to write it. */}
      <Card className="p-4">
        <button
          onClick={() => setShowConfig((v) => !v)}
          className="flex items-center gap-1.5 text-sm text-ink-soft hover:text-ink"
        >
          <FileCode2 className="size-4" aria-hidden />
          {showConfig ? 'Hide' : 'Show'} the donkey.yaml this amounts to
        </button>
        {showConfig && (
          <>
            <pre className="mt-2 max-h-72 overflow-auto rounded bg-ink-well p-3 font-mono text-xs whitespace-pre text-parchment">
              {plan.config}
            </pre>
            <div className="mt-2 flex items-center gap-2">
              <Button
                kind="quiet"
                onClick={() => {
                  navigator.clipboard?.writeText(plan.config).then(
                    () => setCopied(true),
                    () => setCopied(false),
                  )
                }}
              >
                <span className="flex items-center gap-1.5">
                  <Copy className="size-3.5" aria-hidden /> {copied ? 'Copied' : 'Copy'}
                </span>
              </Button>
              <span className="text-xs text-stone">
                Put this in your project so <span className="font-mono">donkey deploy</span> does the
                same thing from your machine.
              </span>
            </div>
          </>
        )}
      </Card>

      <ErrorNote message={error} />
      <div className="flex items-center justify-between gap-3">
        <Button kind="quiet" onClick={onStartOver}>
          Pick something else
        </Button>
        <div className="flex items-center gap-3">
          {!plan.ready && (
            <span className="text-xs text-brick">Fix what's blocked above to deploy.</span>
          )}
          <Button onClick={() => onDeploy(edit)} disabled={busy || checking || !plan.ready}>
            <span className="flex items-center gap-1.5">
              {busy ? (
                <Loader2 className="size-3.5 animate-spin" aria-hidden />
              ) : (
                <ArrowRight className="size-3.5" aria-hidden />
              )}
              Deploy {edit.name || 'this app'}
            </span>
          </Button>
        </div>
      </div>
    </div>
  )
}
