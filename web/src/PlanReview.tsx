import { useId, useState } from 'react'
import { Check, Copy, Database, FileCode2, HardDrive } from 'lucide-react'
import { type AppMode, type Exposure, type Plan, type PlanEdit, type PortSource } from './api'
import { FindingsList } from './FindingsList'
import { HostEditor } from './HostEditor'
import { useFleet } from './queries'
import { APP_MODES, Button, Card, SectionTitle, loginHint } from './ui'

const PORT_SOURCE: Record<PortSource, string> = {
  config: 'from donkey.yaml',
  code: 'read out of the code',
  env: 'the app takes its port from PORT, so donkey sets this one',
  guess: "nothing in the code pins a port — donkey's guess for this stack",
}

// Where the app is reachable from. The login option reads differently for
// an API, so the hints take the mode.
function exposureOptions(mode: AppMode): { value: Exposure; label: string; hint: string }[] {
  return [
    { value: 'protected', label: 'Behind the login', hint: loginHint(mode) },
    {
      value: 'public',
      label: 'Public',
      hint: 'Anyone with the address can reach it. What webhook senders and open APIs need.',
    },
    {
      value: 'internal',
      label: 'Internal',
      hint: 'No address of its own; only apps in this space reach it, by name.',
    },
  ]
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
      <span className="mb-1 block text-xs text-muted">{label}</span>
      {children}
      {hint && <span className="mt-1 block text-xs text-muted">{hint}</span>}
    </label>
  )
}

const inputClass =
  'w-full rounded-md border border-line bg-surface px-2 py-1.5 font-mono text-sm focus:outline-2 focus:outline-accent-strong'

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
        className="mt-0.5 size-4 shrink-0 accent-accent-strong"
      />
      <span className="min-w-0">
        <span className="block font-mono text-sm">{title}</span>
        {reason && <span className="block text-xs text-muted">{reason}</span>}
        {note && <span className="mt-0.5 block text-xs text-accent/80">{note}</span>}
      </span>
    </label>
  )
}

// The project owns the edits and review actions; each app only edits its fields.
export function PlanReview({
  plan,
  edit,
  onEdit,
}: {
  plan: Plan
  edit: PlanEdit
  onEdit: (edit: PlanEdit) => void
}) {
  const [showConfig, setShowConfig] = useState(false)
  const { data: fleet } = useFleet()
  const domains = fleet?.domains[edit.space] ?? (edit.space === plan.app.space
    ? (plan.app.domains ?? []) : (fleet?.domain ? [`${edit.space}.${fleet.domain}`] : []))
  const [copied, setCopied] = useState(false)
  const exposureId = useId()
  const modeId = useId()
  const previewURL = edit.exposure !== 'internal' && edit.host.trim() ? `https://${edit.host.trim()}` : ''
  const set = <K extends keyof PlanEdit>(key: K, value: PlanEdit[K]) =>
    onEdit({ ...edit, [key]: value })

  const toggle = (key: 'storage' | 'services', value: string, on: boolean) =>
    onEdit({
      ...edit,
      [key]: on ? [...edit[key], value] : edit[key].filter((v) => v !== value),
    })

  const offered = plan.services.filter((s) => s.known)
  const blocked = plan.findings.filter((f) => f.level === 'block')
  const warnings = plan.findings.filter((f) => f.level === 'warn')
  const notes = plan.findings.filter((f) => f.level === 'info')

  return (
    <div className="space-y-4">
      {/* What it is. The assistant's reading, in its own words. */}
      <Card className="space-y-2 p-4">
        <SectionTitle>What donkey found</SectionTitle>
        <p className="flex items-center gap-2 text-sm">
          <Check className="size-4 shrink-0 text-success" aria-hidden />
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
          <p className="text-xs text-muted">
            App folder: <span className="font-mono">{plan.root}/</span>
          </p>
        )}
      </Card>

      {/* How it runs. Answers, not blanks. */}
      <Card className="space-y-4 p-4">
        <SectionTitle>How it will run</SectionTitle>
        <div className="grid gap-3 sm:grid-cols-2">
          <Field label="Name">
            <input
              value={edit.name}
              onChange={(e) => set('name', e.target.value)}
              className={inputClass}
              spellCheck={false}
            />
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
          <p className="text-xs text-muted">{plan.app.portReason}</p>
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
          <legend className="mb-1.5 text-xs text-muted">Kind of app</legend>
          <div className="grid gap-2 sm:grid-cols-2">
            {APP_MODES.map((option) => (
              <label
                key={option.value}
                className="flex cursor-pointer items-start gap-2.5 rounded-md border border-line p-3 text-sm"
              >
                <input
                  type="radio"
                  name={modeId}
                  checked={edit.mode === option.value}
                  onChange={() => onEdit({ ...edit, mode: option.value })}
                  className="mt-1 accent-accent-strong"
                />
                <option.icon className="mt-0.5 size-4 shrink-0" aria-hidden />
                <span>
                  {option.label}
                  <span className="block text-xs text-muted">{option.hint}</span>
                </span>
              </label>
            ))}
          </div>
        </fieldset>

        <fieldset>
          <legend className="mb-1.5 text-xs text-muted">Who can reach it</legend>
          <div className="space-y-1">
            {exposureOptions(edit.mode).map((o) => (
              <label
                key={o.value}
                className="flex cursor-pointer items-start gap-2.5 text-sm has-disabled:cursor-not-allowed has-disabled:opacity-45"
              >
                <input
                  type="radio"
                  name={exposureId}
                  checked={edit.exposure === o.value}
                  onChange={() => set('exposure', o.value)}
                  className="mt-1 accent-accent-strong"
                />
                <span>
                  {o.label}
                  <span className="block text-xs text-muted">{o.hint}</span>
                </span>
              </label>
            ))}
          </div>
        </fieldset>

        {edit.exposure !== 'internal' && (
          <HostEditor value={edit.host} domains={domains} onChange={(value) => set('host', value)} />
        )}

        {edit.exposure !== 'internal' && (
          <Choice
            checked={edit.cors === 'allowAll'}
            onChange={(on) => set('cors', on ? 'allowAll' : '')}
            title="Allow calls from any browser origin"
            reason="The gateway answers CORS preflights and makes responses readable with Access-Control-Allow-Origin: *."
          />
        )}

        {previewURL && (
          <p className="rounded-md border border-line bg-surface px-3 py-2 text-sm">
            <span className="text-muted">It will answer at </span>
            <span className="break-all font-mono text-accent">{previewURL}</span>
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
          <p className="text-xs text-muted">
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
      {plan.findings.length > 0 && (
        <Card className="space-y-3 p-4">
          <div className="flex items-center justify-between gap-2">
            <SectionTitle>Checks</SectionTitle>
            <span className="text-xs text-muted">
              {blocked.length + warnings.length > 0
                ? `${blocked.length + warnings.length} need attention`
                : 'nothing blocking'}
            </span>
          </div>
          <FindingsList findings={[...blocked, ...warnings, ...notes]} />
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
            <pre className="mt-2 max-h-72 overflow-auto rounded bg-well p-3 font-mono text-xs whitespace-pre text-emphasis">
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
              <span className="text-xs text-muted">
                Save this as <span className="font-mono">donkey.yaml</span> in the app folder to
                keep these settings for future uploads.
              </span>
            </div>
          </>
        )}
      </Card>
    </div>
  )
}
