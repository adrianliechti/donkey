// The pieces the platform's reports share: the bar on top of a tab, a row
// that opens, the apps of one space with who answers for it.
import { useState, type ReactNode } from 'react'
import { Link } from '@tanstack/react-router'
import { ChevronRight, Download, Globe, Loader2, RefreshCw, Users, type LucideIcon } from 'lucide-react'
import { saveBlob, type AccessConfig, type ReportApp, type SpaceContacts } from './api'
import { errText } from './format'
import { contactAddresses, unassigned } from './reports'
import { Button, CopyButton, Eyebrow, IconButton, PageBar, UpdatedAgo } from './ui'

// PanelBar is the top row of every tab on the platform page: what narrows
// the tab on the left; when it was last read, reading it again and what
// else can be done with it on the right. A control's height in every state.
export function PanelBar({
  children,
  updatedAt,
  onReload,
  reloadLabel = 'Refresh',
  busy = false,
  actions,
}: {
  children?: ReactNode
  updatedAt: number | null
  onReload: () => void
  reloadLabel?: string
  /** A read is under way: the refresh icon turns. */
  busy?: boolean
  actions?: ReactNode
}) {
  return (
    <PageBar
      actions={
        <>
          <UpdatedAgo at={updatedAt} />
          <IconButton
            icon={RefreshCw}
            label={reloadLabel}
            onClick={onReload}
            className={busy ? '[&>svg]:animate-spin' : ''}
          />
          {actions}
        </>
      }
    >
      {children}
    </PageBar>
  )
}

// ExportButton downloads a report as the server makes it. A failure is
// handed to the tab, which says it in the page: the bar keeps its shape.
export function ExportButton({
  download,
  onError,
  hint,
  disabled = false,
}: {
  download: () => Promise<{ blob: Blob; name: string }>
  onError: (message: string) => void
  /** What a row of the file is. */
  hint: string
  disabled?: boolean
}) {
  const [busy, setBusy] = useState(false)
  return (
    <Button
      kind="quiet"
      icon={busy ? Loader2 : Download}
      disabled={busy || disabled}
      title={hint}
      className={busy ? '[&>svg]:animate-spin' : ''}
      onClick={async () => {
        setBusy(true)
        onError('')
        try {
          const { blob, name } = await download()
          saveBlob(blob, name)
        } catch (err) {
          onError(errText(err, 'The export did not work.'))
        } finally {
          setBusy(false)
        }
      }}
    >
      Export CSV
    </Button>
  )
}

// OpenRow is a row of a report that opens: the summary is one button as wide
// as the list, tinted on hover like every other row, and what it holds
// shows under it on the raised tint.
export function OpenRow({
  open,
  onToggle,
  summary,
  children,
}: {
  open: boolean
  onToggle: () => void
  summary: ReactNode
  children: ReactNode
}) {
  return (
    <li>
      <button
        type="button"
        aria-expanded={open}
        onClick={onToggle}
        className={`flex w-full items-center gap-3 px-4 py-2.5 text-left transition-colors duration-(--duration-fast) hover:bg-raised focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-accent-strong ${open ? 'bg-raised/60' : ''}`}
      >
        {summary}
        <ChevronRight
          className={`size-4 shrink-0 text-muted transition-transform duration-(--duration-fast) ${open ? 'rotate-90' : ''}`}
          aria-hidden
        />
      </button>
      {open && <div className="border-t border-line/60 bg-raised/40 px-4 py-3">{children}</div>}
    </li>
  )
}

// One holder of a role: a person by address (a link to write to), a person
// by subject, or a group.
function Holder({ name, group = false }: { name: string; group?: boolean }) {
  if (group) {
    return (
      <span
        className="inline-flex items-center gap-1 font-mono text-xs text-ink-soft"
        title="A group of the identity provider"
      >
        <Users className="size-3 text-muted" aria-hidden />
        {name}
      </span>
    )
  }
  if (name.includes('@') && !name.startsWith('sub:')) {
    return (
      <a
        href={`mailto:${name}`}
        className="font-mono text-xs text-ink-soft underline-offset-2 hover:text-ink hover:underline"
      >
        {name}
      </a>
    )
  }
  return <span className="font-mono text-xs text-ink-soft">{name}</span>
}

function Holders({ label, access }: { label: string; access?: AccessConfig }) {
  const users = access?.users ?? []
  const groups = access?.groups ?? []
  if (users.length + groups.length === 0) return null
  return (
    <span className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-0.5">
      <span className="text-xs text-muted">{label}</span>
      {users.map((user) => (
        <Holder key={user} name={user} />
      ))}
      {groups.map((group) => (
        <Holder key={`group:${group}`} name={group} group />
      ))}
    </span>
  )
}

// SpaceBlock is one space inside an opened row: its name, who answers for
// it — its admins and developers, or the platform's admins where nobody was
// given the space — and its apps under that.
export function SpaceBlock({
  space,
  contacts,
  platformAdmins,
  children,
}: {
  space: string
  contacts?: SpaceContacts
  platformAdmins?: AccessConfig
  children: ReactNode
}) {
  const nobody = unassigned(contacts)
  const addresses = contactAddresses([contacts], platformAdmins)
  return (
    <section className="space-y-1.5" aria-label={`Space ${space}`}>
      <div className="flex flex-wrap items-center gap-x-4 gap-y-1">
        <Link
          to="/space/$space"
          params={{ space }}
          className="font-mono text-sm font-semibold text-ink underline-offset-2 hover:underline focus-visible:outline-2 focus-visible:outline-accent-strong"
        >
          {space}
        </Link>
        {nobody ? (
          <>
            <span className="text-xs text-muted">Nobody was given this space —</span>
            <Holders label="platform admins" access={platformAdmins} />
          </>
        ) : (
          <>
            <Holders label="admins" access={contacts?.admins} />
            <Holders label="developers" access={contacts?.developers} />
          </>
        )}
        {addresses.length > 0 && (
          <span className="-my-1.5 ml-auto">
            <CopyButton
              text={addresses.join(', ')}
              label={`Copy ${addresses.length === 1 ? 'the address' : 'the addresses'} to write to`}
            />
          </span>
        )}
      </div>
      <ul className="divide-y divide-line/60 overflow-hidden rounded-lg border border-line bg-card">{children}</ul>
    </section>
  )
}

// AppLine is one app inside a SpaceBlock: its name (the way to its page),
// how it is reached when that is worth saying, and what the report has to
// say about it.
export function AppLine({
  app,
  info,
  tab,
  children,
  aside,
}: {
  app: { space: string; app: string }
  info?: ReportApp
  /** The tab of the app's page the name leads to. */
  tab?: string
  children?: ReactNode
  aside?: ReactNode
}) {
  return (
    <li className="flex flex-wrap items-start gap-x-4 gap-y-1 px-3 py-2">
      <span className="flex w-44 shrink-0 items-center gap-2">
        <Link
          to="/app/$space/$name/{-$tab}"
          params={{ space: app.space, name: app.app, tab }}
          className="truncate font-mono text-sm text-ink underline-offset-2 hover:underline focus-visible:outline-2 focus-visible:outline-accent-strong"
          title={app.app}
        >
          {app.app}
        </Link>
        {info?.exposure === 'public' && (
          <span
            title="Public — anyone with the address can reach it"
            className="inline-flex shrink-0 items-center text-warning"
          >
            <Globe className="size-3.5" aria-hidden />
            <span className="sr-only">public</span>
          </span>
        )}
      </span>
      <div className="min-w-0 flex-1">{children}</div>
      {aside && <div className="flex shrink-0 items-center gap-2">{aside}</div>}
    </li>
  )
}

// ListHead is the column header of a report's list: the same quiet row as
// the fleet's, so a list reads as a table without becoming one. The caller
// gives it the columns its rows have.
export function ListHead({ children, className }: { children: ReactNode; className: string }) {
  return (
    <div aria-hidden className={`h-9 shrink-0 border-b border-line px-4 ${className}`}>
      {children}
    </div>
  )
}

export function HeadCell({ children, className = '' }: { children: ReactNode; className?: string }) {
  return (
    <span className={className}>
      <Eyebrow>{children}</Eyebrow>
    </span>
  )
}

// Note is a quiet line under a section: what a report leaves out, and why.
export function Note({ icon: Icon, children }: { icon?: LucideIcon; children: ReactNode }) {
  return (
    <p className="flex items-start gap-2 text-xs leading-relaxed text-muted">
      {Icon && <Icon className="mt-0.5 size-3.5 shrink-0" aria-hidden />}
      <span className="min-w-0">{children}</span>
    </p>
  )
}

// MoreButton shows the rest of a long list a step at a time.
export function MoreButton({ hidden, onClick, noun }: { hidden: number; onClick: () => void; noun: string }) {
  if (hidden <= 0) return null
  return (
    <div className="flex justify-center border-t border-line/60 p-2">
      <Button kind="quiet" size="sm" onClick={onClick}>
        Show {Math.min(hidden, 100)} more {noun}
        {hidden > 100 ? ` of ${hidden}` : ''}
      </Button>
    </div>
  )
}
