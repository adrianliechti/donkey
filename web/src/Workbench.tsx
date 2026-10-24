import { useState, type KeyboardEvent, type ReactNode } from 'react'
import { Listbox, ListboxButton, ListboxOption, ListboxOptions } from '@headlessui/react'
import { Check, ChevronsUpDown, Search, X, type LucideIcon } from 'lucide-react'
import { IconButton, Toolbar } from './ui'

// The pieces every service console is built from. A console fills its sheet
// edge to edge: a sidebar of places on the left — tables, keys, buckets —
// and panes beside it, split by single lines rather than boxed, each
// scrolling inside itself, never the page. Pickers and search sit in the
// bars as quiet controls, like the breadcrumb's switchers.

/**
 * A row of panes, split by single lines: side by side from md up, stacked
 * on phones. Put a Sidebar and Panes in it — or another Row.
 */
export function Row({ children, className = '' }: { children: ReactNode; className?: string }) {
  return (
    <div
      className={`flex min-h-0 min-w-0 flex-1 flex-col divide-y divide-line md:flex-row md:divide-x md:divide-y-0 ${className}`}
    >
      {children}
    </div>
  )
}

/** The places of a workbench, on a shade of the page behind the sheet. */
export function Sidebar({
  children,
  label,
  className = '',
}: {
  children: ReactNode
  label?: string
  className?: string
}) {
  return (
    <aside
      aria-label={label}
      className={`flex max-h-72 shrink-0 flex-col overflow-hidden bg-surface/60 md:max-h-none md:w-64 ${className}`}
    >
      {children}
    </aside>
  )
}

/** One region of a workbench: a table, a request, an answer. */
export function Pane({ children, label, className = '' }: { children: ReactNode; label?: string; className?: string }) {
  return (
    <section aria-label={label} className={`flex min-h-0 min-w-0 flex-col overflow-hidden ${className}`}>
      {children}
    </section>
  )
}

/**
 * A list's search, as a row of its own: no box, just the glass and the
 * words, with a line beneath. Whatever narrows the list further (a type)
 * sits at its end.
 */
export function SearchRow({
  value,
  onChange,
  placeholder,
  label,
  onKeyDown,
  autoFocus,
  line = 'bottom',
  children,
}: {
  value: string
  onChange: (value: string) => void
  placeholder: string
  label?: string
  onKeyDown?: (e: KeyboardEvent<HTMLInputElement>) => void
  autoFocus?: boolean
  /** Where its line is: under it (atop a list), or over it (under a header). */
  line?: 'bottom' | 'top' | 'none'
  children?: ReactNode
}) {
  const edge = line === 'bottom' ? 'border-b' : line === 'top' ? 'border-t' : ''
  return (
    <div className={`flex h-10 shrink-0 items-center gap-2 border-line pr-1.5 pl-3 ${edge}`}>
      <Search className="size-3.5 shrink-0 text-muted" aria-hidden />
      <input
        value={value}
        onChange={(e) => onChange(e.target.value)}
        onKeyDown={onKeyDown}
        placeholder={placeholder}
        aria-label={label ?? placeholder}
        autoFocus={autoFocus}
        spellCheck={false}
        className="h-full min-w-0 flex-1 bg-transparent text-sm text-ink placeholder:text-muted focus:outline-none"
      />
      {value && <IconButton icon={X} size="sm" label="Clear" onClick={() => onChange('')} />}
      {children}
    </div>
  )
}

/** A search inside a bar, beside other controls: no box, a shade on hover. */
export function BarSearch({
  value,
  onChange,
  placeholder,
  label,
  className = 'w-40',
}: {
  value: string
  onChange: (value: string) => void
  placeholder: string
  label?: string
  className?: string
}) {
  return (
    <label className="flex h-7 items-center gap-1.5 rounded-md px-2 transition-colors focus-within:bg-line/40 hover:bg-line/40">
      <Search className="size-3.5 shrink-0 text-muted" aria-hidden />
      <input
        value={value}
        onChange={(e) => onChange(e.target.value)}
        placeholder={placeholder}
        aria-label={label ?? placeholder}
        spellCheck={false}
        className={`min-w-0 bg-transparent text-sm text-ink placeholder:text-muted focus:outline-none ${className}`}
      />
    </label>
  )
}

export type PickerOption<T> = { value: T; label: string; detail?: string }

/**
 * A quiet choice in a bar — the database, the schema, the model — shaped like
 * the breadcrumb's switchers: the name and a chevron, details only in the
 * list. Identifiers are mono; `plain` is for words.
 */
export function Picker<T extends string | number>({
  value,
  onChange,
  options,
  label,
  placeholder,
  plain = false,
  up = false,
  className = '',
}: {
  value: T
  onChange: (value: T) => void
  options: PickerOption<T>[]
  label: string
  placeholder?: string
  plain?: boolean
  /** Opens upwards — for a picker at the bottom, in a composer. */
  up?: boolean
  className?: string
}) {
  const current = options.find((o) => o.value === value)
  return (
    <Listbox value={value} onChange={onChange}>
      <ListboxButton
        aria-label={label}
        title={label}
        className={`inline-flex h-7 min-w-0 items-center gap-1 rounded-md px-1.5 text-sm text-ink transition-colors hover:bg-line/60 focus-visible:outline-2 focus-visible:outline-accent-strong data-open:bg-line/60 ${plain ? '' : 'font-mono'} ${className}`}
      >
        <span className={`truncate ${current ? '' : 'text-muted'}`}>
          {current?.label ?? placeholder ?? String(value)}
        </span>
        <ChevronsUpDown className="size-3.5 shrink-0 text-muted" aria-hidden />
      </ListboxButton>
      <ListboxOptions
        anchor={up ? 'top start' : 'bottom start'}
        transition
        className="z-[70] min-w-56 overflow-hidden rounded-xl border border-line bg-card shadow-2xl [--anchor-gap:6px] transition duration-(--duration-fast) ease-emphasized focus:outline-none data-closed:scale-[.98] data-closed:opacity-0"
      >
        {/* The list scrolls on its own: the anchor sizes the panel to all the
            room there is, and a long list (a provider's models) would take it. */}
        <div className="max-h-80 overflow-y-auto p-1.5">
          {options.map((o) => (
            <ListboxOption
              key={String(o.value)}
              value={o.value}
              className="flex h-8 items-center gap-2 rounded-md px-2 text-sm text-ink-soft data-focus:bg-accent-soft data-focus:text-ink"
            >
              {({ selected }) => (
                <>
                  <span className={`min-w-0 truncate text-ink ${plain ? '' : 'font-mono'}`}>{o.label}</span>
                  {o.detail && <span className="ml-auto shrink-0 pl-3 text-xs text-muted">{o.detail}</span>}
                  <Check
                    className={`size-3.5 shrink-0 text-accent ${o.detail ? '' : 'ml-auto'} ${selected ? '' : 'invisible'}`}
                    aria-hidden
                  />
                </>
              )}
            </ListboxOption>
          ))}
        </div>
      </ListboxOptions>
    </Listbox>
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
      className={`flex min-h-11 shrink-0 items-center justify-between gap-2 border-b border-line pr-1.5 ${flush ? 'pl-1' : 'py-1 pl-3'} ${className}`}
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
