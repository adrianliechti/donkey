import { useEffect, useRef, useState, type ReactNode } from 'react'
import { Check, Pencil, Trash2, type LucideIcon } from 'lucide-react'
import { Eyebrow, IconButton, Toolbar } from './ui'

// The pieces the API consoles (HTTP, AI, Exa) are built from. Each is a
// workbench: a sidebar of places on the left — saved requests, conversations,
// recent searches — and panes beside it, every one outlined once, scrolling
// inside itself, never the page.

/** One outlined region of a workbench: a sidebar, a request, an answer. */
export function Pane({
  children,
  label,
  className = '',
}: {
  children: ReactNode
  label?: string
  className?: string
}) {
  return (
    <section
      aria-label={label}
      className={`flex min-h-0 min-w-0 flex-col overflow-hidden rounded-lg border border-line bg-card ${className}`}
    >
      {children}
    </section>
  )
}

/**
 * A pane's top row: what it shows on the left, its actions on the right.
 * `flush` drops the vertical padding, for a row of PaneTabs whose underline
 * sits on the header's own border.
 */
export function PaneHeader({
  children,
  actions,
  flush = false,
  className = '',
}: {
  children?: ReactNode
  actions?: ReactNode
  flush?: boolean
  className?: string
}) {
  return (
    <div
      className={`flex min-h-10 shrink-0 items-center justify-between gap-2 border-b border-line pr-1.5 ${flush ? 'pl-1' : 'py-1 pl-3'} ${className}`}
    >
      {children}
      {actions && <Toolbar className="ml-auto">{actions}</Toolbar>}
    </div>
  )
}

/** A pane's own views — the page's tabs, one size down. */
export function PaneTabs<T extends string>({
  tabs,
  value,
  onChange,
  label,
}: {
  tabs: { id: T; label: string; count?: number; mark?: boolean }[]
  value: T
  onChange: (id: T) => void
  label: string
}) {
  return (
    <div role="tablist" aria-label={label} className="-mb-px flex min-w-0 self-stretch overflow-x-auto scrollbar-none">
      {tabs.map((t) => {
        const on = t.id === value
        return (
          <button
            key={t.id}
            type="button"
            role="tab"
            aria-selected={on}
            onClick={() => onChange(t.id)}
            className={`flex shrink-0 items-center gap-1.5 border-b-2 px-2.5 text-xs transition-colors focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-accent-strong ${
              on ? 'border-accent font-medium text-ink' : 'border-transparent text-muted hover:text-ink'
            }`}
          >
            {t.label}
            {t.count ? (
              <span className="rounded bg-raised px-1 font-mono text-[10px] leading-4 text-ink-soft">{t.count}</span>
            ) : null}
            {t.mark && <span className="size-1.5 rounded-full bg-accent" aria-hidden />}
          </button>
        )
      })}
    </div>
  )
}

/** A small either-or: a body's type, pretty or raw. */
export function Segmented<T extends string>({
  options,
  value,
  onChange,
  label,
}: {
  options: { value: T; label: string }[]
  value: T
  onChange: (value: T) => void
  label: string
}) {
  return (
    <div role="radiogroup" aria-label={label} className="inline-flex shrink-0 rounded-md border border-line p-0.5">
      {options.map((o) => (
        <button
          key={o.value}
          type="button"
          role="radio"
          aria-checked={o.value === value}
          onClick={() => onChange(o.value)}
          className={`h-6 rounded px-2 text-xs transition-colors focus-visible:outline-2 focus-visible:outline-accent-strong ${
            o.value === value ? 'bg-raised font-medium text-ink' : 'text-muted hover:text-ink'
          }`}
        >
          {o.label}
        </button>
      ))}
    </div>
  )
}

/** What an empty pane says: what will appear here, and how to get it. */
export function PaneEmpty({ icon: Icon, title, children }: { icon: LucideIcon; title: string; children?: ReactNode }) {
  return (
    <div className="flex flex-1 flex-col items-center justify-center gap-2 p-8 text-center">
      <Icon className="size-6 text-muted/70" aria-hidden />
      <p className="text-sm font-medium text-ink">{title}</p>
      {children && <div className="max-w-sm text-xs leading-relaxed text-muted">{children}</div>}
    </div>
  )
}

const mac = typeof navigator !== 'undefined' && /Mac|iPhone|iPad/.test(navigator.userAgent)

/** A key chord, written the way this platform writes it. */
export function Keys({ chord }: { chord: 'send' | 'save' }) {
  const mod = mac ? '⌘' : 'Ctrl+'
  return (
    <kbd className="rounded border border-line bg-raised px-1 font-mono text-[10px] leading-4 text-ink-soft">
      {mod}
      {chord === 'send' ? '↵' : 'S'}
    </kbd>
  )
}

/** A sidebar section's header: its name, a count, its actions. */
export function SideHeader({ title, count, actions }: { title: string; count?: number; actions?: ReactNode }) {
  return (
    <div className="flex h-9 shrink-0 items-center justify-between gap-2 pr-1.5 pl-3">
      <Eyebrow>
        {title}
        {count ? <span className="ml-1.5 text-muted/70">{count}</span> : null}
      </Eyebrow>
      {actions && <Toolbar>{actions}</Toolbar>}
    </div>
  )
}

/**
 * One place in a sidebar. Clicking opens it; rename and delete show on
 * hover or focus. Renaming happens in the row itself, and deleting asks a
 * second click — the entries live only in this browser, so a slip would be
 * for good.
 */
export function SideItem({
  title,
  meta,
  lead,
  selected = false,
  onOpen,
  onRename,
  onDelete,
  renameNow = false,
  onRenameDone,
  deleteLabel = 'Delete',
}: {
  title: ReactNode
  meta?: ReactNode
  lead?: ReactNode
  selected?: boolean
  onOpen: () => void
  /** The current name and a way to set it; leave out for rows that have none. */
  onRename?: { name: string; set: (name: string) => void }
  onDelete?: () => void
  /** Opens the row in rename mode — right after it was made. */
  renameNow?: boolean
  onRenameDone?: () => void
  deleteLabel?: string
}) {
  const [editing, setEditing] = useState(renameNow)
  useEffect(() => {
    if (renameNow) setEditing(true)
  }, [renameNow])

  const finish = (name?: string) => {
    if (name !== undefined && onRename && name.trim() && name.trim() !== onRename.name) onRename.set(name.trim())
    setEditing(false)
    onRenameDone?.()
  }

  if (editing && onRename) return <RenameRow name={onRename.name} onDone={finish} />

  return (
    <li
      className={`group relative flex items-center transition-colors ${selected ? 'bg-accent-soft' : 'hover:bg-line/50'}`}
    >
      <button
        type="button"
        onClick={onOpen}
        aria-current={selected || undefined}
        className={`flex min-w-0 flex-1 items-start gap-2 py-1.5 pl-3 text-left focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-accent-strong ${
          onRename || onDelete ? 'pr-3 group-focus-within:pr-16 group-hover:pr-16' : 'pr-3'
        }`}
      >
        {lead}
        <span className="min-w-0 flex-1">
          <span className={`block truncate text-sm ${selected ? 'text-ink' : 'text-ink-soft group-hover:text-ink'}`}>{title}</span>
          {meta && <span className="mt-0.5 block truncate text-[11px] text-muted">{meta}</span>}
        </span>
      </button>
      {(onRename || onDelete) && (
        <div className="absolute top-1/2 right-1 flex -translate-y-1/2 items-center opacity-0 group-focus-within:opacity-100 group-hover:opacity-100 has-[[aria-pressed=true]]:opacity-100">
          {onRename && <IconButton icon={Pencil} size="sm" label="Rename" onClick={() => setEditing(true)} />}
          {onDelete && <ConfirmIconButton icon={Trash2} label={deleteLabel} onConfirm={onDelete} />}
        </div>
      )}
    </li>
  )
}

/** CopyButton with an icon of its own — a link, a command — beside a plain copy. */
export function CopyAs({ text, label, icon }: { text: string; label: string; icon: LucideIcon }) {
  const [copied, setCopied] = useState(false)
  return (
    <IconButton
      icon={copied ? Check : icon}
      label={copied ? 'Copied' : label}
      className={copied ? 'text-success hover:text-success' : ''}
      onClick={() =>
        void navigator.clipboard?.writeText(text).then(() => {
          setCopied(true)
          window.setTimeout(() => setCopied(false), 2000)
        })
      }
    />
  )
}

/**
 * An icon button for something that cannot be undone: the first click arms
 * it, the second does it, and it disarms itself after a moment.
 */
export function ConfirmIconButton({
  icon,
  label,
  onConfirm,
  disabled = false,
}: {
  icon: LucideIcon
  label: string
  onConfirm: () => void
  disabled?: boolean
}) {
  const [armed, setArmed] = useState(false)
  useEffect(() => {
    if (!armed) return
    const timer = window.setTimeout(() => setArmed(false), 3000)
    return () => window.clearTimeout(timer)
  }, [armed])
  return (
    <IconButton
      icon={icon}
      size="sm"
      kind="danger"
      active={armed}
      disabled={disabled}
      className={armed ? 'bg-danger/10 text-danger hover:text-danger' : ''}
      label={armed ? `${label} — click again` : label}
      onClick={() => {
        if (!armed) return setArmed(true)
        setArmed(false)
        onConfirm()
      }}
    />
  )
}

function RenameRow({ name, onDone }: { name: string; onDone: (name?: string) => void }) {
  const [value, setValue] = useState(name)
  const input = useRef<HTMLInputElement>(null)
  // Enter ends the edit, and the input leaving the page may blur it once
  // more: the first ending is the one that counts.
  const ended = useRef(false)
  const end = (name?: string) => {
    if (ended.current) return
    ended.current = true
    onDone(name)
  }
  useEffect(() => {
    input.current?.focus()
    input.current?.select()
  }, [])
  return (
    <li className="px-1.5 py-1">
      <input
        ref={input}
        value={value}
        aria-label="Name"
        onChange={(e) => setValue(e.target.value)}
        onBlur={() => end(value)}
        onKeyDown={(e) => {
          if (e.key === 'Enter') end(value)
          if (e.key === 'Escape') {
            // Escape here only ends the rename — not a request it would cancel.
            e.stopPropagation()
            end()
          }
        }}
        maxLength={120}
        className="h-7 w-full rounded-md border border-line-strong bg-surface px-1.5 text-sm text-ink focus:outline-2 focus:outline-accent-strong"
      />
    </li>
  )
}
