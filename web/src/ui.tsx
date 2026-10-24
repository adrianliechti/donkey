// Small shared pieces so every view speaks the same visual language.
//
// Sizing is one scale everywhere (controls.ts), so anything placed in a row
// lines up without per-call padding fiddling. Dialogs and drawers share the
// header/body/footer frame in dialog.tsx; sections inside a panel share
// SectionHeader (title left, action portlet right).
import {
  Component,
  type InputHTMLAttributes,
  type ReactNode,
  type TextareaHTMLAttributes,
  useState,
} from 'react'
import { Listbox, ListboxButton, ListboxOption, ListboxOptions, Switch } from '@headlessui/react'
import {
  Check,
  ChevronDown,
  ChevronRight,
  CircleAlert,
  Copy,
  CircleCheck,
  Globe,
  Loader2,
  type LucideIcon,
} from 'lucide-react'
import type { AppMode, Exposure } from './api'
import { type ControlSize, controlSize, fieldBase, focusRing, iconSize, inputClass } from './controls'
import type { Health } from './vocabulary'




// The source SVG stays a plain monochrome shape. Rendering it as a mask lets
// each surface choose the color in CSS and keeps the brand mark identical
// across browsers (unlike the platform donkey emoji).
export function DonkeyLogo({ className = '' }: { className?: string }) {
  return <span className={`donkey-logo ${className}`} aria-hidden />
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


// ExposureBadge marks the exception: an app anyone can reach. Behind the
// login is the default and says nothing; an internal app shows it by having
// no address at all.
export function ExposureBadge({ exposure, mode }: { exposure?: Exposure; mode?: AppMode }) {
  if (exposure !== 'public') return null
  return (
    <span
      title={mode === 'api' ? 'Public API — anyone with the address can call it' : 'Public — anyone with the address can open it'}
      className="inline-flex items-center gap-1 rounded border border-warning/40 px-1.5 py-0.5 font-mono text-[10px] uppercase tracking-wide text-warning"
    >
      <Globe className="size-3" aria-hidden />
      public
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

// Spinner fills a loading area by default; `inline` makes it a quiet
// in-line status (next to a heading, inside a row).
export function Spinner({ label, inline = false }: { label: string; inline?: boolean }) {
  return (
    <div
      className={`flex items-center gap-2 text-sm text-muted ${inline ? '' : 'justify-center py-10'}`}
      role="status"
    >
      <Loader2 className="size-4 shrink-0 animate-spin" aria-hidden /> {label}
    </div>
  )
}

export function Card({ children, className = '' }: { children: ReactNode; className?: string }) {
  return <div className={`rounded-lg border border-line bg-card ${className}`}>{children}</div>
}

// The console has two text levels for structure, and only two:
//
//   SectionTitle — a block of a panel: "Services", "Domains", "Build #3".
//                  Plain semibold text.
//   Eyebrow      — a label inside a block: a field ("Hostname"), a group of
//                  values ("Detected stack"), a stat ("Requests"). Small caps.
//
// Mixing them up (small caps for both) is what made sections and their
// fields read as one flat list.
export function Eyebrow({ children, className = '' }: { children: ReactNode; className?: string }) {
  return (
    <span className={`block font-mono text-[10px] font-semibold uppercase tracking-[0.14em] text-muted ${className}`}>
      {children}
    </span>
  )
}

export function SectionTitle({ children }: { children: ReactNode }) {
  return <h2 className="text-sm font-semibold text-ink">{children}</h2>
}

// SectionHeader opens every block the same way: the title (and a one-line
// description) on the left, its actions in a portlet on the right. The row
// is as tall as a control, so headers with and without actions line up.
export function SectionHeader({
  title,
  actions,
  description,
}: {
  title: ReactNode
  actions?: ReactNode
  description?: ReactNode
}) {
  return (
    <div>
      <div className="flex min-h-8 items-center justify-between gap-3">
        <SectionTitle>{title}</SectionTitle>
        {actions && <Toolbar>{actions}</Toolbar>}
      </div>
      {description && <p className="mt-0.5 text-xs leading-relaxed text-muted">{description}</p>}
    </div>
  )
}

// Section is one block of a panel body. Blocks are separated by a rule, so
// a tab with three of them reads as three things — the first one needs no
// rule above it. `tone="danger"` is for the remove block at the very end.
export function Section({
  children,
  tone = 'default',
  className = '',
}: {
  children: ReactNode
  tone?: 'default' | 'danger'
  className?: string
}) {
  const rule = tone === 'danger' ? 'border-danger/30' : 'border-line'
  return (
    <section className={`border-t pt-5 first:border-t-0 first:pt-0 ${rule} ${className}`}>{children}</section>
  )
}

// ChoiceCards is the console's radio group: one card per option, the chosen
// one tinted with the accent. The same picker serves the deploy review and
// the live network settings, so a choice looks the same wherever it is made.
export function ChoiceCards<T extends string>({
  value,
  onChange,
  options,
  label,
  columns = 'sm:grid-cols-2',
  disabled = false,
}: {
  value: T
  onChange: (value: T) => void
  options: { value: T; icon?: LucideIcon; label: string; hint?: string }[]
  label: string
  columns?: string
  disabled?: boolean
}) {
  return (
    <div className={`grid gap-2 ${columns}`} role="radiogroup" aria-label={label}>
      {options.map((o) => {
        const selected = value === o.value
        return (
          <button
            key={o.value}
            type="button"
            role="radio"
            aria-checked={selected}
            disabled={disabled}
            onClick={() => onChange(o.value)}
            className={`rounded-lg border p-3 text-left transition-colors disabled:opacity-50 ${focusRing} ${
              selected ? 'border-accent bg-accent-soft/40' : 'border-line hover:border-line-strong'
            }`}
          >
            <span className="flex items-center gap-1.5 text-sm font-medium text-ink">
              {o.icon && <o.icon className={`size-4 shrink-0 ${selected ? 'text-accent' : 'text-muted'}`} aria-hidden />}
              {o.label}
            </span>
            {o.hint && <span className="mt-1 block text-xs leading-snug text-muted">{o.hint}</span>}
          </button>
        )
      })}
    </div>
  )
}

// ─── Building blocks ────────────────────────────────────────────────────
//
// One frame per level. A drawer or dialog is the frame; inside it, blocks
// are Sections divided by hairlines. A group that needs an outline — a
// list, a strip of stats, a piece of code — gets exactly one, and nothing
// inside it carries a border of its own: rows are divided, chips are tinted,
// code sits on the well colour.

// List is the one outlined group: rows divided by hairlines.
export function List({ children, className = '' }: { children: ReactNode; className?: string }) {
  return <ul className={`divide-y divide-line/60 overflow-hidden rounded-lg border border-line ${className}`}>{children}</ul>
}

// ListRow is a row of a List. `actions` sit at the right edge, pulled out so
// an icon button's glyph — not its hit area — lines up with the row's text.
export function ListRow({
  children,
  actions,
  className = '',
}: {
  children: ReactNode
  actions?: ReactNode
  className?: string
}) {
  return (
    <li className={`flex min-h-11 items-center gap-3 py-2 pl-3 pr-3 ${className}`}>
      {children}
      {actions && <div className="-mr-1.5 flex shrink-0 items-center gap-0.5">{actions}</div>}
    </li>
  )
}

// Callout is a tinted note — an error, a warning, a diagnosis, a finding.
// Tinted, never outlined: it sits inside sections and lists without adding a
// frame. Title in the tone, body in the quiet ink.
const calloutTones = {
  danger: { box: 'bg-danger/10', icon: 'text-danger', title: 'text-danger' },
  warning: { box: 'bg-warning/10', icon: 'text-warning', title: 'text-ink' },
  info: { box: 'bg-accent-soft/50', icon: 'text-accent', title: 'text-ink' },
  success: { box: 'bg-success/10', icon: 'text-success', title: 'text-ink' },
  neutral: { box: 'bg-raised/70', icon: 'text-muted', title: 'text-ink' },
}
export type CalloutTone = keyof typeof calloutTones

export function Callout({
  tone = 'neutral',
  icon: Icon,
  title,
  children,
  action,
  role,
  className = '',
}: {
  tone?: CalloutTone
  icon?: LucideIcon
  title?: ReactNode
  children?: ReactNode
  /** One action, at the right. */
  action?: ReactNode
  role?: 'alert' | 'status'
  className?: string
}) {
  const t = calloutTones[tone]
  return (
    <div role={role} className={`flex items-start gap-3 rounded-lg px-3 py-2.5 text-sm ${t.box} ${className}`}>
      {Icon && <Icon className={`mt-0.5 size-4 shrink-0 ${t.icon} ${Icon === Loader2 ? 'animate-spin' : ''}`} aria-hidden />}
      <div className="min-w-0 flex-1">
        {title && <p className={`font-medium ${t.title}`}>{title}</p>}
        {children && (
          <div className={`${title ? 'mt-0.5 text-ink-soft' : t.title} leading-relaxed`}>{children}</div>
        )}
      </div>
      {action && <div className="-my-1 shrink-0 self-center">{action}</div>}
    </div>
  )
}

// StatGrid is a strip of numbers in one outline, cells divided by hairlines
// however the grid wraps.
export function StatGrid({
  stats,
  columns = 'grid-cols-2 sm:grid-cols-4',
}: {
  stats: { label: string; value: ReactNode; note?: ReactNode; tone?: 'danger' }[]
  columns?: string
}) {
  return (
    <dl className={`grid gap-px overflow-hidden rounded-lg border border-line bg-line ${columns}`}>
      {stats.map((stat) => (
        <div key={stat.label} className="min-w-0 bg-card px-4 py-3">
          <dt><Eyebrow>{stat.label}</Eyebrow></dt>
          <dd className={`mt-1 truncate font-mono text-xl font-bold ${stat.tone === 'danger' ? 'text-danger' : 'text-ink'}`}>
            {stat.value}
          </dd>
          {stat.note && <dd className="mt-0.5 text-[11px] leading-snug text-muted">{stat.note}</dd>}
        </div>
      ))}
    </dl>
  )
}

// ToggleRow is an on/off setting: what it does on the left, the switch on
// the right. No outline — it is one line of a section.
export function ToggleRow({
  label,
  description,
  checked,
  onChange,
  disabled = false,
}: {
  label: string
  description?: ReactNode
  checked: boolean
  onChange: (checked: boolean) => void
  disabled?: boolean
}) {
  return (
    <div className="flex items-center justify-between gap-4">
      <div className="min-w-0">
        <p className="text-sm text-ink">{label}</p>
        {description && <p className="mt-0.5 text-xs leading-relaxed text-muted">{description}</p>}
      </div>
      <Toggle label={label} checked={checked} onChange={onChange} disabled={disabled} />
    </div>
  )
}

// Disclosure tucks away detail a reader may want: a quiet line with a
// chevron, the content below it when open.
export function Disclosure({
  summary,
  children,
  defaultOpen = false,
}: {
  summary: ReactNode
  children: ReactNode
  defaultOpen?: boolean
}) {
  return (
    <details className="group" open={defaultOpen || undefined}>
      <summary className="flex w-fit cursor-pointer list-none items-center gap-1 text-xs text-muted transition-colors hover:text-ink [&::-webkit-details-marker]:hidden">
        <ChevronRight className="size-3.5 transition-transform group-open:rotate-90" aria-hidden />
        {summary}
      </summary>
      <div className="mt-2">{children}</div>
    </details>
  )
}

// Chip is a small mono token — a variable name, a detected runtime. Tinted,
// no outline; `dashed` marks one that would apply but does not yet.
export function Chip({ children, dashed = false, title }: { children: ReactNode; dashed?: boolean; title?: string }) {
  return (
    <span
      title={title}
      className={`inline-flex items-center gap-1 rounded px-1.5 py-0.5 font-mono text-[10px] ${
        dashed ? 'border border-dashed border-line text-muted' : 'bg-raised text-ink-soft'
      }`}
    >
      {children}
    </span>
  )
}

// Well is the surface code and raw text sit on — the darkest one.
export const wellClass = 'overflow-auto rounded-md bg-well p-3 font-mono text-xs leading-relaxed text-emphasis'

// CodeCard frames monospace content — a log, a Dockerfile, a config — the
// same way everywhere: an eyebrow title with its icon on the left, status
// or actions on the right, and the text on the console's darkest surface.
export function CodeCard({
  icon: Icon,
  title,
  meta,
  children,
  className = '',
  bodyClassName = '',
}: {
  icon?: LucideIcon
  title: ReactNode
  /** Status or actions, right-aligned in the header. */
  meta?: ReactNode
  children: ReactNode
  className?: string
  bodyClassName?: string
}) {
  return (
    <Card className={`flex flex-col overflow-hidden ${className}`}>
      <div className="flex h-11 shrink-0 items-center justify-between gap-3 border-b border-line px-4">
        <SectionTitle>
          <span className="flex items-center gap-2">
            {Icon && <Icon className="size-4 text-muted" aria-hidden />}
            {title}
          </span>
        </SectionTitle>
        {meta && <div className="flex items-center gap-2 text-xs text-muted">{meta}</div>}
      </div>
      <div className={`min-h-0 flex-1 overflow-auto bg-well p-4 font-mono text-xs leading-relaxed text-emphasis ${bodyClassName}`}>
        {children}
      </div>
    </Card>
  )
}

// TabLabel keeps a tab as wide as it is when selected: an invisible bold
// copy shares the cell, so turning bold never nudges the tabs beside it.
export function TabLabel({ children }: { children: string }) {
  return (
    <span className="inline-grid justify-items-center">
      <span className="col-start-1 row-start-1">{children}</span>
      <span className="invisible col-start-1 row-start-1 font-medium" aria-hidden>
        {children}
      </span>
    </span>
  )
}

// Toolbar is the action portlet: a tight row of icon buttons and small
// controls, right-aligned by whoever places it.
export function Toolbar({ children, className = '' }: { children: ReactNode; className?: string }) {
  return <div className={`flex shrink-0 items-center gap-1 ${className}`}>{children}</div>
}

export function Button({
  children,
  onClick,
  kind = 'primary',
  size = 'md',
  icon: Icon,
  disabled = false,
  type = 'button',
  title,
  className = '',
}: {
  children?: ReactNode
  onClick?: () => void
  kind?: 'primary' | 'quiet' | 'danger'
  size?: ControlSize
  icon?: LucideIcon
  disabled?: boolean
  type?: 'button' | 'submit'
  title?: string
  className?: string
}) {
  // The primary action is ink on the surface — the console's one solid
  // button, and it carries no colour: blue is for what is selected or
  // focused, red for what destroys.
  const styles = {
    primary: 'bg-ink text-surface hover:bg-emphasis border border-transparent font-medium',
    quiet: 'bg-transparent text-ink-soft hover:bg-line/50 hover:text-ink border border-line',
    danger: 'bg-transparent text-danger hover:bg-danger hover:text-white border border-danger/40',
  }[kind]
  return (
    <button
      type={type}
      onClick={onClick}
      disabled={disabled}
      title={title}
      className={`inline-flex items-center justify-center gap-1.5 whitespace-nowrap rounded-md transition-[color,background-color,border-color,scale] duration-(--duration-fast) ease-emphasized motion-safe:active:scale-[.97] disabled:pointer-events-none disabled:opacity-40 ${focusRing} ${controlSize[size]} ${styles} ${className}`}
    >
      {Icon && <Icon className={`${iconSize[size]} shrink-0`} aria-hidden />}
      {children}
    </button>
  )
}

// A Button that is a link out — the same look, opened in a new tab, so
// modifier clicks and copy-link work as on any anchor.
export function LinkButton({
  children,
  href,
  kind = 'primary',
  size = 'md',
  icon: Icon,
  className = '',
}: {
  children?: ReactNode
  href: string
  kind?: 'primary' | 'quiet'
  size?: ControlSize
  icon?: LucideIcon
  className?: string
}) {
  const styles = {
    primary: 'bg-ink text-surface hover:bg-emphasis border border-transparent font-medium',
    quiet: 'bg-transparent text-ink-soft hover:bg-line/50 hover:text-ink border border-line',
  }[kind]
  return (
    <a
      href={href}
      target="_blank"
      rel="noreferrer"
      className={`inline-flex items-center justify-center gap-1.5 whitespace-nowrap rounded-md transition-[color,background-color,border-color,scale] duration-(--duration-fast) ease-emphasized motion-safe:active:scale-[.97] ${focusRing} ${controlSize[size]} ${styles} ${className}`}
    >
      {children}
      {Icon && <Icon className={`${iconSize[size]} shrink-0`} aria-hidden />}
    </a>
  )
}

// The icon-only counterpart, for toolbars: the app header and every dialog's
// action row. The label is the tooltip and the accessible name. `md` is the
// toolbar size (32px); `sm` is for dense lists.
export function IconButton({
  icon: Icon,
  label,
  onClick,
  kind = 'quiet',
  size = 'md',
  disabled = false,
  type = 'button',
  active = false,
  className = '',
}: {
  icon: LucideIcon
  label: string
  onClick?: () => void
  kind?: 'quiet' | 'danger'
  size?: ControlSize
  disabled?: boolean
  type?: 'button' | 'submit'
  active?: boolean
  className?: string
}) {
  const tone =
    kind === 'danger' ? 'hover:bg-danger/10 hover:text-danger' : 'hover:bg-line/60 hover:text-ink'
  const dims = size === 'sm' ? 'size-7' : 'size-8'
  return (
    <button
      type={type}
      title={label}
      aria-label={label}
      aria-pressed={active || undefined}
      onClick={onClick}
      disabled={disabled}
      className={`flex shrink-0 items-center justify-center rounded-md text-muted transition-[color,background-color,scale] duration-(--duration-fast) ease-emphasized motion-safe:active:scale-[.97] disabled:pointer-events-none disabled:opacity-40 ${focusRing} ${dims} ${tone} ${active ? 'bg-line/60 text-ink' : ''} ${className}`}
    >
      <Icon className={size === 'sm' ? 'size-3.5' : 'size-4'} aria-hidden />
    </button>
  )
}

// Toggle is the console's switch: an on/off setting that applies as part of
// the form around it. Monochrome like the primary button — on is ink.
export function Toggle({
  checked,
  onChange,
  label,
  disabled = false,
  size = 'md',
}: {
  checked: boolean
  onChange: (checked: boolean) => void
  label: string
  disabled?: boolean
  size?: ControlSize
}) {
  const dims = size === 'sm' ? 'h-4 w-7' : 'h-5 w-9'
  const knob = size === 'sm' ? 'size-3 group-data-checked:translate-x-3' : 'size-4 group-data-checked:translate-x-4'
  return (
    <Switch
      checked={checked}
      onChange={onChange}
      disabled={disabled}
      aria-label={label}
      className={`group inline-flex shrink-0 items-center rounded-full border border-transparent bg-line-strong p-0.5 transition-colors duration-(--duration-fast) data-checked:bg-ink data-disabled:opacity-40 ${focusRing} ${dims}`}
    >
      <span
        aria-hidden
        className={`block rounded-full bg-card shadow-sm transition-transform duration-(--duration-fast) ease-emphasized group-data-checked:bg-surface ${knob}`}
      />
    </Switch>
  )
}

// CopyButton copies text and says so for a moment: the copy icon turns into
// a check. Icon-only, like every other portlet action.
export function CopyButton({ text, label = 'Copy' }: { text: string; label?: string }) {
  const [copied, setCopied] = useState(false)
  return (
    <IconButton
      icon={copied ? Check : Copy}
      label={copied ? 'Copied' : label}
      className={copied ? 'text-success hover:text-success' : ''}
      onClick={() => {
        void navigator.clipboard?.writeText(text).then(() => {
          setCopied(true)
          setTimeout(() => setCopied(false), 2000)
        })
      }}
    />
  )
}


// `bare` drops the outline for an input that sits inside a list row: the
// row is already the frame.
export function Input({
  size = 'md',
  bare = false,
  className = '',
  ...props
}: Omit<InputHTMLAttributes<HTMLInputElement>, 'size'> & { size?: ControlSize; bare?: boolean }) {
  const cls = bare
    ? `min-w-0 bg-transparent font-mono text-ink placeholder:text-muted/60 outline-none disabled:opacity-50 ${size === 'sm' ? 'h-7 text-xs' : 'h-8 text-sm'} ${className}`
    : inputClass(size, className)
  return <input className={cls} {...props} />
}

export function TextArea({
  size = 'md',
  className = '',
  ...props
}: TextareaHTMLAttributes<HTMLTextAreaElement> & { size?: ControlSize }) {
  const pad = size === 'sm' ? 'px-2.5 py-1.5 text-xs' : 'px-3 py-2 text-sm'
  return <textarea className={`${fieldBase} ${pad} leading-relaxed ${className}`} {...props} />
}

// Field frames one control: label above, hint below, both in the console's
// quiet voice. It is a <label>, so clicking the text focuses the control.
export function Field({
  label,
  hint,
  className = '',
  children,
}: {
  label: ReactNode
  hint?: ReactNode
  className?: string
  children: ReactNode
}) {
  return (
    <label className={`flex min-w-0 flex-col gap-1.5 ${className}`}>
      <Eyebrow>{label}</Eyebrow>
      {children}
      {hint && <span className="text-xs leading-relaxed text-muted">{hint}</span>}
    </label>
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
  variant = 'field',
  className = '',
  disabled = false,
}: {
  value: T
  onChange: (value: T) => void
  options: { value: T; label: string }[]
  ariaLabel: string
  size?: ControlSize
  /** `field` looks like an input; `quiet` is a text link with a chevron, for
   *  view choices in section headers (which build, which range). */
  variant?: 'field' | 'quiet'
  className?: string
  disabled?: boolean
}) {
  const current = options.find((o) => o.value === value)
  const optionPad = size === 'sm' || variant === 'quiet' ? 'px-2.5 py-1.5 text-xs' : 'px-3 py-1.5 text-sm'
  const trigger =
    variant === 'quiet'
      ? 'h-8 rounded px-1 text-xs text-muted hover:text-ink focus-visible:outline-2 focus-visible:outline-accent-strong data-open:text-ink'
      : `rounded-md border border-line bg-surface font-mono text-ink focus:outline-2 focus:outline-accent-strong data-open:border-line-strong ${controlSize[size]}`
  return (
    <Listbox value={value} onChange={onChange} disabled={disabled}>
      <ListboxButton
        aria-label={ariaLabel}
        className={`inline-flex items-center justify-between gap-1.5 disabled:opacity-50 ${trigger} ${className}`}
      >
        <span className="truncate">{current?.label ?? String(value)}</span>
        <ChevronDown className="size-3.5 shrink-0 text-muted" aria-hidden />
      </ListboxButton>
      <ListboxOptions
        anchor="bottom start"
        className="z-[70] mt-1 max-h-72 min-w-(--button-width) overflow-auto rounded-md border border-line bg-card py-1 shadow-lg focus:outline-none"
      >
        {options.map((o) => (
          <ListboxOption
            key={String(o.value)}
            value={o.value}
            className={`flex items-center justify-between gap-2 ${optionPad} font-mono text-ink-soft data-focus:bg-accent-soft data-focus:text-ink`}
          >
            {({ selected }) => (
              <>
                <span className="truncate">{o.label}</span>
                <Check className={`size-3.5 shrink-0 ${selected ? '' : 'invisible'}`} aria-hidden />
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
    <Callout tone="danger" icon={CircleAlert} role="alert">
      {message}
    </Callout>
  )
}

// SuccessNote is the quiet confirmation after a save.
export function SuccessNote({ message }: { message: string }) {
  if (!message) return null
  return (
    <p role="status" className="text-sm text-success">
      {message}
    </p>
  )
}

// StateChip renders a pod/container state at a glance: loud when it's the
// kind of word (CrashLoopBackOff) an admin came here to find.
export function StateChip({ state }: { state: string }) {
  const bad = /crashloop|error|backoff|failed|pending|terminat/i.test(state)
  const good = /running|ready/i.test(state)
  const tone = bad ? 'bg-danger/10 text-danger' : good ? 'bg-success/10 text-success' : 'bg-raised text-ink-soft'
  return (
    <span className={`inline-block rounded px-1.5 py-0.5 font-mono text-[11px] leading-4 ${tone}`}>
      {state}
    </span>
  )
}

// Skeleton keeps the layout stable while first data is on its way.
export function Skeleton({ className = '' }: { className?: string }) {
  return <div className={`animate-pulse rounded-md bg-line/60 ${className}`} aria-hidden />
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
