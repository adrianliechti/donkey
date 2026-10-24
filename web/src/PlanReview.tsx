import { Check, FileCode2, Globe, Lock, Network, type LucideIcon } from 'lucide-react'
import { type AppMode, type Exposure, type Plan, type PlanEdit, type PortSource } from './api'
import { FindingsList } from './FindingsList'
import { HostEditor } from './HostEditor'
import { useFleet } from './queries'
import {
  ChoiceCards,
  CodeCard,
  CopyButton,
  Disclosure,
  Eyebrow,
  Field,
  Input,
  List,
  ListRow,
  Section,
  SectionHeader,
  ToggleRow,
} from './ui'
import { APP_MODES, loginHint } from './vocabulary'

const PORT_SOURCE: Record<PortSource, string> = {
  config: 'from donkey.yaml',
  code: 'read out of the code',
  env: 'the app takes its port from PORT, so donkey sets this one',
  guess: "nothing in the code pins a port — donkey's guess for this stack",
}

// Where the app is reachable from. The login option reads differently for
// an API, so the hints take the mode.
function exposureOptions(mode: AppMode): { value: Exposure; icon: LucideIcon; label: string; hint: string }[] {
  return [
    { value: 'protected', icon: Lock, label: 'Behind the login', hint: loginHint(mode) },
    {
      value: 'public',
      icon: Globe,
      label: 'Public',
      hint: 'Anyone with the address can reach it. What webhook senders and open APIs need.',
    },
    {
      value: 'internal',
      icon: Network,
      label: 'Internal',
      hint: 'No address of its own; only apps in this space reach it, by name.',
    },
  ]
}

// Choice is a checkbox row that reads as a decision already made, with the
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
    <ListRow className="items-start">
      <label className="flex min-w-0 flex-1 cursor-pointer items-start gap-3">
        <input
          type="checkbox"
          checked={checked}
          onChange={(e) => onChange(e.target.checked)}
          className="mt-0.5 size-4 shrink-0"
        />
        <span className="min-w-0">
          <span className={`block font-mono text-sm ${checked ? 'text-ink' : 'text-muted'}`}>{title}</span>
          {reason && <span className="block text-xs text-muted">{reason}</span>}
          {note && <span className="block text-xs text-ink-soft">{note}</span>}
        </span>
      </label>
    </ListRow>
  )
}

// The project owns the edits and review actions; each app only edits its
// fields. Its blocks are the drawer's plain sections, like a release.
export function PlanReview({ plan, edit, onEdit }: { plan: Plan; edit: PlanEdit; onEdit: (edit: PlanEdit) => void }) {
  const { data: fleet } = useFleet()
  const domains =
    fleet?.domains[edit.space] ??
    (edit.space === plan.app.space ? (plan.app.domains ?? []) : fleet?.domain ? [`${edit.space}.${fleet.domain}`] : [])
  const previewURL = edit.exposure !== 'internal' && edit.host.trim() ? `https://${edit.host.trim()}` : ''
  const set = <K extends keyof PlanEdit>(key: K, value: PlanEdit[K]) => onEdit({ ...edit, [key]: value })

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
    <>
      {/* What it is. The assistant's reading, in its own words. */}
      <Section className="space-y-2">
        <SectionHeader title="What donkey found" />
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
      </Section>

      {/* How it runs. Answers, not blanks. */}
      <Section className="space-y-5">
        <SectionHeader title="How it will run" />
        <div className="grid gap-4 sm:grid-cols-2">
          <Field label="Name">
            <Input value={edit.name} onChange={(e) => set('name', e.target.value)} spellCheck={false} />
          </Field>
          <Field
            label="Port"
            hint={
              plan.app.portReason && plan.app.portSource === 'code'
                ? `${PORT_SOURCE[plan.app.portSource]} — ${plan.app.portReason}`
                : PORT_SOURCE[plan.app.portSource]
            }
          >
            <Input
              value={edit.port || ''}
              onChange={(e) => set('port', Number(e.target.value.replace(/\D/g, '')) || 0)}
              inputMode="numeric"
            />
          </Field>
        </div>

        <Field
          label="Health check path"
          hint="A path that answers OK when the app is fine. Empty just checks that the port answers."
        >
          <Input
            value={edit.healthcheck}
            onChange={(e) => set('healthcheck', e.target.value)}
            placeholder="/healthz"
            spellCheck={false}
          />
        </Field>

        <div className="space-y-1.5">
          <Eyebrow>Kind of app</Eyebrow>
          <ChoiceCards
            label="Kind of app"
            value={edit.mode}
            onChange={(mode) => onEdit({ ...edit, mode })}
            options={APP_MODES}
          />
        </div>

        <div className="space-y-1.5">
          <Eyebrow>Who can reach it</Eyebrow>
          <ChoiceCards
            label="Who can reach it"
            value={edit.exposure}
            onChange={(v) => set('exposure', v)}
            options={exposureOptions(edit.mode).filter(
              (option) => option.value !== 'public' || fleet?.allowPublic[edit.space],
            )}
            columns="sm:grid-cols-3"
          />
        </div>

        {edit.exposure !== 'internal' && (
          <HostEditor value={edit.host} domains={domains} onChange={(value) => set('host', value)} />
        )}
        {previewURL && (
          <p className="-mt-3 text-xs text-muted">
            It will answer at <span className="break-all font-mono text-accent-strong">{previewURL}</span>
          </p>
        )}

        {edit.exposure !== 'internal' && (
          <ToggleRow
            label="Allow calls from any browser origin"
            description="The gateway answers CORS preflights and adds Access-Control-Allow-Origin: *."
            checked={edit.cors === 'allowAll'}
            onChange={(on) => set('cors', on ? 'allowAll' : '')}
          />
        )}
      </Section>

      {/* The folders that hold data. Getting this wrong loses it. */}
      {plan.volumes.length > 0 && (
        <Section className="space-y-3">
          <SectionHeader
            title="Data that must survive a restart"
            description={
              edit.storage.length > 0
                ? `They share one ${plan.app.storageSize || '1Gi'} disk (a folder each), and the app then runs as a single copy.`
                : 'Nothing is kept: everything the app writes disappears when it restarts.'
            }
          />
          <List>
            {plan.volumes.map((v) => (
              <Choice
                key={v.path}
                checked={edit.storage.includes(v.path)}
                onChange={(on) => toggle('storage', v.path, on)}
                title={v.path}
                reason={v.reason}
              />
            ))}
          </List>
        </Section>
      )}

      {/* Services. Creating one is real, so it says which it is. */}
      {offered.length > 0 && (
        <Section className="space-y-3">
          <SectionHeader title="Services it needs" />
          <List>
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
          </List>
        </Section>
      )}

      {/* Everything else that is true about this app. */}
      {plan.findings.length > 0 && (
        <Section className="space-y-3">
          <SectionHeader
            title="Checks"
            actions={
              <span className="text-xs text-muted">
                {blocked.length + warnings.length > 0
                  ? `${blocked.length + warnings.length} need attention`
                  : 'nothing blocking'}
              </span>
            }
          />
          <FindingsList findings={[...blocked, ...warnings, ...notes]} />
        </Section>
      )}

      {/* The file this amounts to. The browser has nowhere to write it. */}
      <Section>
        <Disclosure summary="Show the donkey.yaml this amounts to">
          <CodeCard
            icon={FileCode2}
            title={<span className="font-mono">donkey.yaml</span>}
            className="max-h-80"
            meta={<CopyButton text={plan.config} label="Copy donkey.yaml" />}
          >
            <pre className="whitespace-pre">{plan.config}</pre>
          </CodeCard>
          <p className="mt-2 text-xs text-muted">
            Save this as <span className="font-mono">donkey.yaml</span> in the app folder to keep these settings for
            future uploads.
          </p>
        </Disclosure>
      </Section>
    </>
  )
}
