// Small shared pieces so every view speaks the same visual language.
import { Component, type ReactNode, useSyncExternalStore } from 'react'
import { Listbox, ListboxButton, ListboxOption, ListboxOptions } from '@headlessui/react'
import { Check, ChevronDown, CircleAlert, CircleCheck, Globe, Loader2, Lock, Network } from 'lucide-react'
import type { Exposure } from './api'

// useColorScheme follows the OS light/dark preference — the console has no
// theme toggle and stores nothing, so this reads `prefers-color-scheme` live.
// It exists for the rare place a value can't be a CSS token: the code
// highlighter's theme is a token→hex map chosen in JS.
const darkQuery = () => window.matchMedia('(prefers-color-scheme: dark)')
export function useColorScheme(): 'light' | 'dark' {
  return useSyncExternalStore(
    (cb) => {
      const mq = darkQuery()
      mq.addEventListener('change', cb)
      return () => mq.removeEventListener('change', cb)
    },
    () => (darkQuery().matches ? 'dark' : 'light'),
    () => 'dark',
  )
}

// The source SVG stays a plain monochrome shape. Rendering it as a mask lets
// each surface choose the color in CSS and keeps the brand mark identical
// across browsers (unlike the platform donkey emoji).
export function DonkeyLogo({ className = '' }: { className?: string }) {
  return <span className={`donkey-logo ${className}`} aria-hidden />
}

// errText gives every catch block the same voice: the server's own message
// when there is one, the caller's plain-language fallback otherwise.
export function errText(err: unknown, fallback: string): string {
  return err instanceof Error ? err.message : fallback
}

export type Health = 'ok' | 'bad' | 'unknown'

export function healthOf(healthy: boolean, ready: string): Health {
  if (ready === 'status unavailable') return 'unknown'
  return healthy ? 'ok' : 'bad'
}

// One wording for an app's state, wherever it is shown.
export const healthLine: Record<Health, (ready: string) => string> = {
  ok: (ready) => `running (${ready})`,
  bad: (ready) => `not fully up (${ready})`,
  unknown: () => 'status unknown — checking again',
}

// The dots glow: on a dark field, a lit lamp reads faster than a color chip.
const dotColor: Record<Health, string> = {
  ok: 'bg-success shadow-[0_0_8px_2px] shadow-success/40',
  bad: 'bg-danger shadow-[0_0_8px_2px] shadow-danger/40',
  unknown: 'bg-muted',
}

export function StatusDot({ health }: { health: Health }) {
  return <span className={`inline-block size-2.5 rounded-full ${dotColor[health]}`} aria-hidden />
}

// ExposureBadge says how the world reaches an app — the console's shorthand
// for the network settings: open globe, locked door, or stable-only.
export function ExposureBadge({ exposure }: { exposure?: Exposure }) {
  if (!exposure) return null
  const spec = {
    public: { icon: <Globe className="size-3" aria-hidden />, label: 'public', tone: 'text-ink-soft border-line' },
    protected: { icon: <Lock className="size-3" aria-hidden />, label: 'login', tone: 'text-accent border-accent/40' },
    internal: { icon: <Network className="size-3" aria-hidden />, label: 'internal', tone: 'text-muted border-line' },
  }[exposure]
  return (
    <span
      title={
        exposure === 'public'
          ? 'Public — anyone with the address can open it'
          : exposure === 'protected'
            ? 'Login-protected — visitors sign in through the identity provider'
            : 'Internal — only the apps of this space reach it, by name'
      }
      className={`inline-flex items-center gap-1 rounded border px-1.5 py-0.5 font-mono text-[10px] uppercase tracking-wide ${spec.tone}`}
    >
      {spec.icon}
      {spec.label}
    </span>
  )
}

export function CheckIcon({ ok }: { ok: boolean }) {
  return ok ? (
    <CircleCheck className="size-4 shrink-0 text-success" aria-label="ok" />
  ) : (
    <CircleAlert className="size-4 shrink-0 text-danger" aria-label="problem" />
  )
}

export function Spinner({ label }: { label: string }) {
  return (
    <div className="flex items-center gap-2 py-10 justify-center text-muted text-sm">
      <Loader2 className="size-4 animate-spin" aria-hidden /> {label}
    </div>
  )
}

export function Card({ children, className = '' }: { children: ReactNode; className?: string }) {
  return <div className={`rounded-lg border border-line bg-card ${className}`}>{children}</div>
}

export function SectionTitle({ children }: { children: ReactNode }) {
  return (
    <h2 className="font-mono text-[11px] font-semibold uppercase tracking-[0.14em] text-muted">
      {children}
    </h2>
  )
}

export function Button({
  children,
  onClick,
  kind = 'primary',
  disabled = false,
  type = 'button',
}: {
  children: ReactNode
  onClick?: () => void
  kind?: 'primary' | 'quiet' | 'danger'
  disabled?: boolean
  type?: 'button' | 'submit'
}) {
  const styles = {
    primary:
      'bg-accent text-on-accent hover:bg-accent-strong hover:text-white border border-transparent font-semibold',
    quiet: 'bg-transparent text-ink-soft hover:bg-line/50 border border-line',
    danger: 'bg-transparent text-danger hover:bg-danger hover:text-white border border-danger/40',
  }[kind]
  return (
    <button
      type={type}
      onClick={onClick}
      disabled={disabled}
      className={`rounded-md px-3 py-1.5 text-sm whitespace-nowrap transition-colors disabled:opacity-40 disabled:pointer-events-none focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-accent-strong ${styles}`}
    >
      {children}
    </button>
  )
}

// Select is the console's dropdown — a Headless UI Listbox wearing the same
// clothes as the text inputs, with keyboard navigation and focus handling
// the native element never styled well.
export function Select<T extends string | number>({
  value,
  onChange,
  options,
  ariaLabel,
  size = 'md',
}: {
  value: T
  onChange: (value: T) => void
  options: { value: T; label: string }[]
  ariaLabel: string
  size?: 'sm' | 'md'
}) {
  const pad = size === 'sm' ? 'px-2 py-1 text-xs' : 'px-2 py-1.5 text-sm'
  const current = options.find((o) => o.value === value)
  return (
    <Listbox value={value} onChange={onChange}>
      <ListboxButton
        aria-label={ariaLabel}
        className={`flex items-center gap-1.5 rounded-md border border-line bg-surface font-mono ${pad} focus:outline-2 focus:outline-accent-strong data-open:border-line-strong`}
      >
        {current?.label ?? String(value)}
        <ChevronDown className="size-3.5 text-muted" aria-hidden />
      </ListboxButton>
      <ListboxOptions
        anchor="bottom start"
        className="z-50 mt-1 min-w-(--button-width) overflow-hidden rounded-md border border-line bg-card py-1 shadow-lg focus:outline-none"
      >
        {options.map((o) => (
          <ListboxOption
            key={String(o.value)}
            value={o.value}
            className={`flex cursor-default items-center justify-between gap-2 ${pad} font-mono text-ink-soft data-focus:bg-accent-soft data-focus:text-ink`}
          >
            {({ selected }) => (
              <>
                {o.label}
                <Check className={`size-3.5 ${selected ? '' : 'invisible'}`} aria-hidden />
              </>
            )}
          </ListboxOption>
        ))}
      </ListboxOptions>
    </Listbox>
  )
}

// ErrorNote announces itself: async failures (polls, saves) must reach
// screen readers, not only eyes.
export function ErrorNote({ message }: { message: string }) {
  if (!message) return null
  return (
    <p role="alert" className="rounded-md border border-danger/30 bg-danger/5 px-3 py-2 text-sm text-danger">
      {message}
    </p>
  )
}

// StateChip renders a pod/container state at a glance: loud when it's the
// kind of word (CrashLoopBackOff) an admin came here to find.
export function StateChip({ state }: { state: string }) {
  const bad = /crashloop|error|backoff|failed|pending|terminat/i.test(state)
  const good = /running|ready/i.test(state)
  const tone = bad
    ? 'bg-danger/10 text-danger border-danger/30'
    : good
      ? 'bg-success/10 text-success border-success/30'
      : 'bg-line/50 text-ink-soft border-line'
  return (
    <span className={`inline-block rounded border px-1.5 py-0.5 font-mono text-[11px] leading-4 ${tone}`}>
      {state}
    </span>
  )
}

// Skeleton keeps the layout stable while first data is on its way.
export function Skeleton({ className = '' }: { className?: string }) {
  return <div className={`animate-pulse rounded-md bg-line/60 ${className}`} aria-hidden />
}

// ago turns an RFC 3339 timestamp into "3m", "2h", "5d" — fleet-triage units.
export function ago(iso: string): string {
  const secs = Math.max(0, Math.round((Date.now() - Date.parse(iso)) / 1000))
  if (secs < 90) return `${secs}s`
  if (secs < 5400) return `${Math.round(secs / 60)}m`
  if (secs < 129600) return `${Math.round(secs / 3600)}h`
  return `${Math.round(secs / 86400)}d`
}

export function UpdatedAgo({ at }: { at: number | null }) {
  if (at === null) return null
  const secs = Math.max(0, Math.round((Date.now() - at) / 1000))
  const text = secs < 5 ? 'just now' : secs < 90 ? `${secs}s ago` : `${Math.round(secs / 60)}m ago`
  return <span className="text-xs text-muted">updated {text}</span>
}

// ErrorBoundary keeps one broken view from blanking the whole console.
export class ErrorBoundary extends Component<{ children: ReactNode }, { error: Error | null }> {
  state = { error: null as Error | null }
  static getDerivedStateFromError(error: Error) {
    return { error }
  }
  render() {
    if (this.state.error) {
      return (
        <div className="mx-auto max-w-lg p-8">
          <ErrorNote message={`Something in this view broke: ${this.state.error.message}`} />
          <button
            onClick={() => location.reload()}
            className="mt-3 rounded-md border border-line px-3 py-1.5 text-sm hover:bg-line/50"
          >
            Reload the console
          </button>
        </div>
      )
    }
    return this.props.children
  }
}
