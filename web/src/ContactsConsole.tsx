import { useEffect, useMemo, useRef, useState, type KeyboardEvent, type ReactNode } from 'react'
import { BookUser, Building2, ChevronLeft, Download, MapPin, RefreshCw, Trash2, Users } from 'lucide-react'
import type { ConsoleProps } from './consoles'
import {
  useAddressBooks,
  useContact,
  useContactList,
  useDeleteContact,
  type ContactDetail,
  type ContactKind,
  type ContactSummary,
  type Typed,
} from './contactsApi'
import { cardDay, contactMatches, contactName, initials, typeLabel, vcardFileName } from './contactCard'
import { Modal, ModalBody, ModalFooter, ModalHeader } from './dialog'
import { saveBlob } from './api'
import { mutationError } from './queries'
import { SourceCode } from './SourceCode'
import { useServiceSearch, type ServiceSearch } from './serviceSearch'
import { PaneEmpty, PaneHeader, Picker, Row, SearchRow, Segmented } from './Workbench'
import { Button, Chip, ErrorNote, Eyebrow, IconButton, Spinner, Toolbar } from './ui'
import { errText } from './format'

// The contacts console: the address book's cards down the left, searchable,
// the one picked on the right — as a card, or as the vCard it is stored as.
// Everything is read as the signed-in user, so what shows is what they may
// see. Where it stands — book, card, search — lives in the URL.
type Place = { book?: string; contact?: string; q?: string }

export function Console({ space, service }: ConsoleProps) {
  const [search, move] = useServiceSearch()
  const place = search as Place
  const go = (next: Place, how?: 'push' | 'replace') => move(next as ServiceSearch, how)
  const books = useAddressBooks(space, service)
  // The first book is where the console opens; its key may be the root ("").
  const bookKey = place.book ?? books.data?.[0]?.key
  const book = books.data?.find((b) => b.key === bookKey)
  // On a narrow screen the list and the card take turns.
  const showing = place.contact !== undefined

  return (
    <Row>
      <ContactList
        space={space}
        service={service}
        books={books}
        bookKey={bookKey}
        filter={place.q ?? ''}
        selected={place.contact}
        onBook={(key) => go({ book: key || undefined })}
        onOpen={(key) => go({ book: place.book, q: place.q, contact: key })}
        onFilter={(q) => go({ book: place.book, contact: place.contact, q: q || undefined }, 'replace')}
        className={showing ? 'hidden md:flex' : 'flex'}
      />
      <section className={`min-w-0 flex-1 flex-col overflow-hidden ${showing ? 'flex' : 'hidden md:flex'}`}>
        {place.contact !== undefined ? (
          <ContactCard
            key={place.contact}
            space={space}
            service={service}
            contactKey={place.contact}
            bookName={book?.name ?? service}
            onBack={() => go({ book: place.book, q: place.q })}
            onDeleted={() => go({ book: place.book, q: place.q }, 'replace')}
          />
        ) : (
          <PaneEmpty icon={BookUser} title="Pick a contact">
            Its addresses, numbers and notes show here — or the vCard itself.
          </PaneEmpty>
        )}
      </section>
    </Row>
  )
}

// ─── The list ──────────────────────────────────────────────────────────

function ContactList({
  space,
  service,
  books,
  bookKey,
  filter,
  selected,
  onBook,
  onOpen,
  onFilter,
  className,
}: {
  space: string
  service: string
  books: ReturnType<typeof useAddressBooks>
  bookKey?: string
  filter: string
  selected?: string
  onBook: (key: string) => void
  onOpen: (key: string) => void
  onFilter: (q: string) => void
  className: string
}) {
  const list = useContactList(space, service, bookKey)
  // The box answers at once; the URL catches up when typing pauses.
  const [text, setText] = useState(filter)
  const [synced, setSynced] = useState(filter)
  if (synced !== filter) {
    setSynced(filter)
    setText(filter)
  }
  const latest = useRef(onFilter)
  useEffect(() => {
    latest.current = onFilter
  })
  useEffect(() => {
    if (text === filter) return
    const timer = setTimeout(() => latest.current(text), 250)
    return () => clearTimeout(timer)
  }, [text, filter])

  const contacts = list.data?.contacts
  const shown = useMemo(() => (contacts ?? []).filter((c) => contactMatches(c, text)), [contacts, text])
  const nav = useRef<HTMLElement>(null)

  // The arrows walk the list; Enter or a click opens.
  const step = (e: KeyboardEvent, dir: 1 | -1) => {
    const buttons = [...(nav.current?.querySelectorAll<HTMLButtonElement>('[data-contact]') ?? [])]
    const at = buttons.indexOf(document.activeElement as HTMLButtonElement)
    const next = buttons[at < 0 ? 0 : Math.max(0, Math.min(buttons.length - 1, at + dir))]
    if (next) {
      e.preventDefault()
      next.focus()
    }
  }

  const refresh = () => {
    void books.refetch()
    void list.refetch()
  }

  return (
    <aside
      aria-label="Address book"
      className={`w-full shrink-0 flex-col overflow-hidden bg-surface/60 md:w-72 ${className}`}
    >
      <div className="flex min-h-11 shrink-0 items-center gap-1 border-b border-line pr-1.5 pl-1.5">
        <div className="flex min-w-0 flex-1 items-center">
          {books.data ? (
            books.data.length > 1 ? (
              <Picker
                plain
                label="Address book"
                value={bookKey ?? ''}
                onChange={onBook}
                options={books.data.map((b) => ({ value: b.key, label: b.name }))}
              />
            ) : books.data.length === 1 ? (
              <span className="truncate px-1.5 text-sm text-ink" title={books.data[0].description || undefined}>
                {books.data[0].name}
              </span>
            ) : (
              <span className="px-1.5 text-xs text-muted">No address books</span>
            )
          ) : books.error ? (
            <span className="px-1.5 text-xs text-danger">Not answering.</span>
          ) : (
            <span className="px-1.5">
              <Spinner inline label="Connecting…" />
            </span>
          )}
        </div>
        <Toolbar>
          <IconButton
            icon={RefreshCw}
            label="Read the contacts again"
            onClick={refresh}
            className={list.isFetching || books.isFetching ? '[&>svg]:animate-spin' : ''}
          />
        </Toolbar>
      </div>
      <SearchRow
        value={text}
        onChange={setText}
        placeholder="Find a contact"
        onKeyDown={(e) => {
          if (e.key === 'Enter' && shown[0]) onOpen(shown[0].key)
          if (e.key === 'ArrowDown') step(e, 1)
          if (e.key === 'Escape' && text) {
            e.preventDefault()
            setText('')
          }
        }}
      >
        {contacts && contacts.length > 0 && !text && (
          <span className="px-1 text-[11px] text-muted tabular-nums">{contacts.length.toLocaleString()}</span>
        )}
      </SearchRow>
      <nav
        ref={nav}
        aria-label="Contacts"
        className="min-h-0 flex-1 overflow-y-auto py-1"
        onKeyDown={(e) => {
          if (e.key === 'ArrowDown') step(e, 1)
          if (e.key === 'ArrowUp') step(e, -1)
        }}
      >
        {books.error ? (
          <div className="p-2">
            <ErrorNote message={errText(books.error, 'The address books did not answer.')} />
          </div>
        ) : books.data?.length === 0 ? (
          <p className="px-3 py-6 text-center text-xs leading-relaxed text-muted">
            The service shows you no address book.
          </p>
        ) : list.error ? (
          <div className="p-2">
            <ErrorNote message={errText(list.error, 'Reading the contacts did not work.')} />
          </div>
        ) : !contacts ? (
          <Spinner label="Reading the contacts…" />
        ) : contacts.length === 0 ? (
          <p className="px-3 py-6 text-center text-xs leading-relaxed text-muted">
            This address book holds no contacts.
          </p>
        ) : shown.length === 0 ? (
          <p className="px-3 py-6 text-center text-xs leading-relaxed text-muted">
            No contact matches “{text.trim()}”.
          </p>
        ) : (
          shown.map((c) => <ContactRow key={c.key} contact={c} current={c.key === selected} onOpen={onOpen} />)
        )}
      </nav>
      {list.data?.truncated && (
        <footer className="flex min-h-10 shrink-0 items-center border-t border-line px-3 text-xs text-muted">
          The first {contacts?.length.toLocaleString()} — the book holds more.
        </footer>
      )}
    </aside>
  )
}

function ContactRow({
  contact: c,
  current,
  onOpen,
}: {
  contact: ContactSummary
  current: boolean
  onOpen: (key: string) => void
}) {
  const second = c.email || c.org || c.phone
  return (
    <button
      type="button"
      data-contact={c.key}
      onClick={() => onOpen(c.key)}
      aria-current={current || undefined}
      className={`flex w-full items-center gap-2.5 px-2.5 py-1.5 text-left transition-colors focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-accent-strong ${
        current ? 'bg-accent-soft text-ink' : 'text-ink-soft hover:bg-line/50 hover:text-ink'
      }`}
    >
      <Avatar name={c.name} kind={c.kind} />
      <span className="min-w-0 flex-1">
        <span className={`block truncate text-sm ${c.name.trim() ? '' : 'text-muted'}`}>{contactName(c)}</span>
        {second && <span className="block truncate text-xs text-muted">{second}</span>}
      </span>
    </button>
  )
}

const kindIcons: Partial<Record<ContactKind, typeof Users>> = { group: Users, org: Building2, location: MapPin }

/** A card's face: its photo, else the initials of its name, else what it is. */
function Avatar({
  name,
  kind,
  photo,
  large = false,
}: {
  name: string
  kind?: ContactKind
  photo?: string
  large?: boolean
}) {
  const dims = large ? 'size-14 text-lg' : 'size-7 text-[11px]'
  if (photo) return <img src={photo} alt="" className={`${dims} shrink-0 rounded-full bg-raised object-cover`} />
  const letters = initials(name)
  const Icon = kindIcons[kind ?? 'individual'] ?? (letters ? undefined : Users)
  return (
    <span
      aria-hidden
      className={`grid ${dims} shrink-0 place-items-center rounded-full bg-raised font-medium text-ink-soft`}
    >
      {Icon ? <Icon className={large ? 'size-6' : 'size-3.5'} /> : letters}
    </span>
  )
}

// ─── The card ──────────────────────────────────────────────────────────

function ContactCard({
  space,
  service,
  contactKey,
  bookName,
  onBack,
  onDeleted,
}: {
  space: string
  service: string
  contactKey: string
  bookName: string
  onBack: () => void
  onDeleted: () => void
}) {
  const card = useContact(space, service, contactKey)
  const [view, setView] = useState<'card' | 'source'>('card')
  const [deleting, setDeleting] = useState(false)
  const gone = (card.error as { status?: number } | null)?.status === 404
  const c = card.data

  return (
    <>
      <PaneHeader
        className="pl-1.5"
        actions={
          <>
            <IconButton
              icon={Download}
              label="Download the vCard"
              disabled={!c}
              onClick={() => c && saveBlob(new Blob([c.data], { type: 'text/vcard' }), vcardFileName(c.name, c.key))}
            />
            <IconButton
              icon={Trash2}
              kind="danger"
              label="Delete the contact…"
              disabled={!c}
              onClick={() => setDeleting(true)}
            />
          </>
        }
      >
        <div className="flex min-w-0 items-center gap-1.5">
          <IconButton icon={ChevronLeft} label="Back to the contacts" onClick={onBack} className="md:hidden" />
          <Segmented
            label="Show as"
            value={view}
            onChange={setView}
            options={[
              { value: 'card', label: 'Card' },
              { value: 'source', label: 'vCard' },
            ]}
          />
        </div>
      </PaneHeader>
      {gone ? (
        <PaneEmpty icon={BookUser} title="This contact is gone">
          It was deleted or moved — perhaps from another app that syncs this address book.
        </PaneEmpty>
      ) : card.error ? (
        <div className="p-3">
          <ErrorNote message={errText(card.error, 'Reading the card did not work.')} />
        </div>
      ) : !c ? (
        <Spinner label="Reading the card…" />
      ) : view === 'source' ? (
        <div className="min-h-0 flex-1 overflow-auto bg-well">
          <SourceCode path="contact.vcf" text={c.data} className="" />
        </div>
      ) : (
        <CardView contact={c} />
      )}
      {deleting && c && (
        <DeleteContact
          space={space}
          service={service}
          contact={c}
          bookName={bookName}
          onClose={() => setDeleting(false)}
          onDeleted={() => {
            setDeleting(false)
            onDeleted()
          }}
        />
      )}
    </>
  )
}

function CardView({ contact: c }: { contact: ContactDetail }) {
  const subtitle = [c.title, c.department, c.kind === 'org' ? '' : c.org].filter(Boolean).join(' · ')
  const kindWord =
    c.kind === 'group' ? 'Group' : c.kind === 'org' ? 'Organisation' : c.kind === 'location' ? 'Place' : ''
  const revised = c.revision ? readable(c.revision) : ''
  const http = (v: string) => /^https?:\/\//i.test(v)

  return (
    <div className="min-h-0 flex-1 overflow-y-auto">
      <div className="mx-auto max-w-2xl space-y-6 px-5 py-6">
        <div className="flex items-center gap-4">
          <Avatar name={c.name} kind={c.kind} photo={c.photo} large />
          <div className="min-w-0">
            <h2 className={`text-lg font-semibold break-words ${c.name.trim() ? 'text-ink' : 'text-muted'}`}>
              {contactName(c)}
            </h2>
            {subtitle && <p className="text-sm break-words text-ink-soft">{subtitle}</p>}
            {kindWord && <p className="text-xs text-muted">{kindWord}</p>}
          </div>
        </div>
        <dl className="grid grid-cols-[minmax(0,7rem)_minmax(0,1fr)] gap-x-4 gap-y-4 border-t border-line pt-5">
          {c.emails.length > 0 && (
            <Field label="Email">
              {c.emails.map((e, i) => (
                <Value key={i} item={e}>
                  {/\s/.test(e.value) ? e.value : <Link href={`mailto:${e.value}`}>{e.value}</Link>}
                </Value>
              ))}
            </Field>
          )}
          {c.phones.length > 0 && (
            <Field label="Phone">
              {c.phones.map((p, i) => (
                <Value key={i} item={p}>
                  <Link href={`tel:${p.value.replace(/[^\d+*#,;]/g, '')}`}>{p.value}</Link>
                </Value>
              ))}
            </Field>
          )}
          {c.addresses.length > 0 && (
            <Field label="Address">
              {c.addresses.map((a, i) => (
                <Value key={i} item={a}>
                  <span className="whitespace-pre-line">{a.value}</span>
                </Value>
              ))}
            </Field>
          )}
          {c.urls.length > 0 && (
            <Field label="Website">
              {c.urls.map((u, i) => (
                <Value key={i} item={u}>
                  {http(u.value) ? (
                    <Link href={u.value} external>
                      {u.value.replace(/^https?:\/\//i, '').replace(/\/$/, '')}
                    </Link>
                  ) : (
                    u.value
                  )}
                </Value>
              ))}
            </Field>
          )}
          {c.nickname && <Field label="Nickname">{c.nickname}</Field>}
          {c.role && <Field label="Role">{c.role}</Field>}
          {c.birthday && <Field label="Birthday">{cardDay(c.birthday)}</Field>}
          {c.anniversary && <Field label="Anniversary">{cardDay(c.anniversary)}</Field>}
          {c.members && c.members.length > 0 && (
            <Field label="Members">
              <span title={c.members.join('\n')}>
                {c.members.length === 1 ? 'One member' : `${c.members.length.toLocaleString()} members`}
              </span>
            </Field>
          )}
          {c.categories.length > 0 && (
            <Field label="Groups">
              <span className="flex flex-wrap gap-1 pt-0.5">
                {c.categories.map((g) => (
                  <Chip key={g}>{g}</Chip>
                ))}
              </span>
            </Field>
          )}
          {c.note && (
            <Field label="Note">
              <span className="whitespace-pre-wrap text-ink-soft">{c.note}</span>
            </Field>
          )}
        </dl>
        {!hasFields(c) && <p className="text-sm text-muted">The card says nothing more than its name.</p>}
        {(c.version || revised) && (
          <p className="border-t border-line pt-3 text-xs text-muted">
            {[c.version && `vCard ${c.version}`, revised && `revised ${revised}`].filter(Boolean).join(' · ')}
          </p>
        )}
      </div>
    </div>
  )
}

const hasFields = (c: ContactDetail) =>
  c.emails.length + c.phones.length + c.addresses.length + c.urls.length + c.categories.length > 0 ||
  !!(c.nickname || c.role || c.birthday || c.anniversary || c.note || c.members?.length)

/** A REV to read: the server sends RFC 3339 when it could read it, else as written. */
function readable(raw: string): string {
  const d = new Date(raw)
  return Number.isNaN(d.getTime()) ? raw : new Intl.DateTimeFormat(undefined, { dateStyle: 'medium' }).format(d)
}

function Field({ label, children }: { label: string; children: ReactNode }) {
  return (
    <>
      <dt className="pt-1">
        <Eyebrow>{label}</Eyebrow>
      </dt>
      <dd className="min-w-0 space-y-1.5 text-sm break-words text-ink">{children}</dd>
    </>
  )
}

function Value({ item, children }: { item: Typed; children: ReactNode }) {
  const label = typeLabel(item)
  return (
    <div className="flex min-w-0 flex-wrap items-baseline gap-x-2">
      <span className="min-w-0 break-words">{children}</span>
      {(label || item.preferred) && (
        <span className="shrink-0 text-xs text-muted">
          {[label, item.preferred && 'preferred'].filter(Boolean).join(' · ')}
        </span>
      )}
    </div>
  )
}

function Link({ href, external = false, children }: { href: string; external?: boolean; children: ReactNode }) {
  return (
    <a
      href={href}
      {...(external ? { target: '_blank', rel: 'noreferrer' } : {})}
      className="text-ink underline decoration-line-strong underline-offset-2 transition-colors hover:text-accent-strong hover:decoration-accent-strong"
    >
      {children}
    </a>
  )
}

function DeleteContact({
  space,
  service,
  contact,
  bookName,
  onClose,
  onDeleted,
}: {
  space: string
  service: string
  contact: ContactDetail
  bookName: string
  onClose: () => void
  onDeleted: () => void
}) {
  const remove = useDeleteContact(space, service)
  const busy = remove.isPending
  return (
    <Modal onClose={() => !busy && onClose()} size="sm">
      <ModalHeader
        eyebrow={bookName}
        title={<>Delete {contactName(contact)}?</>}
        onClose={onClose}
        closeDisabled={busy}
      />
      <ModalBody>
        <p className="text-sm leading-relaxed text-ink-soft">
          The card leaves the address book — and every phone and app that syncs it. It does not come back.
        </p>
        {remove.error && (
          <div className="mt-3">
            <ErrorNote message={mutationError(remove, 'Deleting did not work.')} />
          </div>
        )}
      </ModalBody>
      <ModalFooter>
        <Button kind="quiet" onClick={onClose} disabled={busy}>
          Keep it
        </Button>
        <Button
          kind="danger"
          icon={Trash2}
          disabled={busy}
          onClick={() => remove.mutate({ key: contact.key, etag: contact.etag }, { onSuccess: onDeleted })}
        >
          {busy ? 'Deleting…' : 'Delete'}
        </Button>
      </ModalFooter>
    </Modal>
  )
}
