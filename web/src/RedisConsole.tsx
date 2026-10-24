import { useEffect, useRef, useState } from 'react'
import {
  Braces,
  Check,
  ChevronLeft,
  Clock,
  Hash,
  KeyRound,
  List,
  ListOrdered,
  Pencil,
  Plus,
  RefreshCw,
  Rows3,
  Trash2,
  Type,
  X,
} from 'lucide-react'
import type { ConsoleProps } from './consoles'
import { Modal, ModalBody, ModalFooter, ModalHeader } from './dialog'
import { useNow } from './hooks'
import { mutationError } from './queries'
import {
  redisDuration,
  redisId,
  redisLabel,
  redisNameOf,
  redisSummary,
  redisTypes,
  typeLabel,
  useRedisDatabases,
  useRedisKeys,
  useRedisValue,
  useRedisWrites,
  type RedisKeyEntry,
  type RedisType,
} from './redisApi'
import { RedisValueView, RowsEditor, TypeBadge } from './RedisValue'
import { Picker, Row, SearchRow } from './Workbench'
import { useServiceSearch, type ServiceSearch } from './serviceSearch'
import {
  Button,
  ChoiceCards,
  CopyButton,
  ErrorNote,
  Eyebrow,
  Field,
  IconButton,
  Input,
  Select,
  Spinner,
  TextArea,
  Toolbar,
} from './ui'
import { errText } from './format'

// The key browser: the keys of one database on the left, found with SCAN a
// page at a time, and the open key on the right with an editor for its type.
// Where it stands lives in the URL (see Place), so back and forward walk the
// keys a person opened and any of them can be linked.

// The database (absent: the binding's own), the pattern and type the list
// shows, and the open key — by name when its bytes are text, by ID when they
// are not — or the new-key form.
type Place = { db?: string; match?: string; type?: string; key?: string; keyid?: string; new?: string }

const keyPlace = (entry: { id: string }): Place => {
  const name = redisNameOf(entry.id)
  return name !== null && name !== '' ? { key: name } : { keyid: entry.id }
}

const secondsLeft = (expiresAt: number | null, now: number) =>
  expiresAt === null ? null : Math.max(0, Math.ceil((expiresAt - now) / 1000))

// A pattern without glob characters is a search for keys containing it —
// what a person typing "session" means.
const globOf = (text: string) => (text === '' ? '' : /[*?[]/.test(text) ? text : `*${text}*`)

export function Console({ space, service }: ConsoleProps) {
  const [search, move] = useServiceSearch()
  const place = search as Place
  const go = (next: Place, how?: 'push' | 'replace') => move(next as ServiceSearch, how)
  const list: Place = { db: place.db, match: place.match, type: place.type }
  const id = place.keyid ?? (place.key !== undefined ? redisId(place.key) : undefined)
  const creating = place.new === '1'
  const open = (entry: { id: string }, how?: 'push' | 'replace') => go({ ...list, ...keyPlace(entry) }, how)
  const closeKey = (how?: 'push' | 'replace') => go(list, how)
  // On a narrow screen the list and the key take turns.
  const showing = creating || id !== undefined

  return (
    <Row>
      <KeyList
        space={space}
        service={service}
        place={place}
        selected={id}
        onOpen={open}
        // A new pattern or type keeps the open key open; another database
        // is another set of keys.
        onFilter={(next) => go({ ...place, ...next }, 'replace')}
        onDatabase={(db) => go({ db, match: place.match, type: place.type })}
        onNew={() => go({ ...list, new: '1' })}
        className={showing ? 'hidden md:flex' : 'flex'}
      />
      <section className={`min-w-0 flex-1 flex-col overflow-hidden ${showing ? 'flex' : 'hidden md:flex'}`}>
        {creating ? (
          <NewKey
            space={space}
            service={service}
            db={place.db}
            onCreated={(entry) => open(entry, 'replace')}
            onCancel={() => closeKey()}
          />
        ) : id !== undefined ? (
          <KeyView
            key={`${place.db ?? ''}:${id}`}
            space={space}
            service={service}
            db={place.db}
            id={id}
            onRenamed={(entry) => open(entry, 'replace')}
            onClosed={() => closeKey('replace')}
            onBack={() => closeKey()}
          />
        ) : (
          <div className="flex flex-1 flex-col items-center justify-center gap-3 p-8 text-center">
            <KeyRound className="size-6 text-muted" aria-hidden />
            <p className="max-w-sm text-sm text-muted">
              Pick a key to see its value, its expiry and what it holds — or start a new one.
            </p>
            <Button kind="quiet" size="sm" icon={Plus} onClick={() => go({ ...list, new: '1' })}>
              New key
            </Button>
          </div>
        )}
      </section>
    </Row>
  )
}

// ─── The key list ──────────────────────────────────────────────────────

function KeyList({
  space,
  service,
  place,
  selected,
  onOpen,
  onFilter,
  onDatabase,
  onNew,
  className,
}: {
  space: string
  service: string
  place: Place
  selected?: string
  onOpen: (entry: RedisKeyEntry, how?: 'push' | 'replace') => void
  onFilter: (next: Place) => void
  onDatabase: (db: string | undefined) => void
  onNew: () => void
  className: string
}) {
  const typed = place.match ?? ''
  const type = place.type ?? ''
  // The box follows the URL (back and forward), the URL follows the box once
  // typing pauses — a refinement of this place, not a new one.
  const [text, setText] = useState(typed)
  const [synced, setSynced] = useState(typed)
  if (synced !== typed) {
    setSynced(typed)
    setText(typed)
  }
  // The latest onFilter, so a pause that ends after another change (a new
  // database) does not move back to the place it was typed in.
  const latest = useRef(onFilter)
  useEffect(() => {
    latest.current = onFilter
  })
  useEffect(() => {
    if (text === typed) return
    const timer = setTimeout(() => latest.current({ match: text || undefined }), 300)
    return () => clearTimeout(timer)
  }, [text, typed])

  const pattern = globOf(typed)
  const keysQuery = useRedisKeys(space, service, place.db, pattern, type)
  const { data, error, isPending, isFetching, hasNextPage, isFetchingNextPage, fetchNextPage, refetch } = keysQuery
  // SCAN may return a key in two pages; the list shows it once.
  const keys: RedisKeyEntry[] = []
  const seen = new Set<string>()
  for (const page of data?.pages ?? []) {
    for (const k of page.keys) {
      if (!seen.has(k.id)) {
        seen.add(k.id)
        keys.push(k)
      }
    }
  }
  const now = useNow(15_000)
  const nav = useRef<HTMLElement>(null)

  // Up and down walk the list without leaving a history step per key.
  const step = (delta: number) => {
    const at = keys.findIndex((k) => k.id === selected)
    const next = keys[at === -1 ? 0 : Math.max(0, Math.min(keys.length - 1, at + delta))]
    if (!next) return
    onOpen(next, 'replace')
    nav.current?.querySelector<HTMLElement>(`[data-key="${CSS.escape(next.id)}"]`)?.focus()
  }

  return (
    <aside className={`w-full shrink-0 flex-col overflow-hidden bg-surface/60 md:w-64 ${className}`}>
      <div className="flex min-h-11 shrink-0 items-center justify-between gap-2 border-b border-line pr-1.5 pl-1.5">
        <DatabasePicker space={space} service={service} db={place.db} onPick={onDatabase} />
        <Toolbar>
          <IconButton
            icon={RefreshCw}
            label="Scan again"
            onClick={() => void refetch()}
            className={isFetching && !isFetchingNextPage ? '[&>svg]:animate-spin' : ''}
          />
          <IconButton icon={Plus} label="New key" onClick={onNew} />
        </Toolbar>
      </div>
      <SearchRow
        value={text}
        onChange={setText}
        onKeyDown={(e) => {
          // Enter scans now (or again); down moves into the list.
          if (e.key === 'Enter') {
            if (text === typed) void refetch()
            else onFilter({ match: text || undefined })
          }
          if (e.key === 'ArrowDown') {
            e.preventDefault()
            nav.current?.querySelector<HTMLElement>('[data-key]')?.focus()
          }
        }}
        placeholder="Find keys"
        label="Key pattern"
      >
        <Picker
          plain
          label="Key type"
          value={type}
          onChange={(next) => onFilter({ type: next || undefined })}
          options={[
            { value: '', label: 'all types' },
            ...redisTypes.map((t) => ({ value: t, label: typeLabel[t].toLowerCase() })),
          ]}
          className="text-xs text-muted"
        />
      </SearchRow>
      <nav
        ref={nav}
        aria-label="Keys"
        className="min-h-0 flex-1 overflow-y-auto py-1"
        onKeyDown={(e) => {
          if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
            e.preventDefault()
            step(e.key === 'ArrowDown' ? 1 : -1)
          }
        }}
      >
        {error ? (
          <div className="p-2">
            <ErrorNote message={errText(error, 'Redis did not answer.')} />
          </div>
        ) : isPending ? (
          <Spinner label="Scanning keys…" />
        ) : keys.length === 0 ? (
          <p className="px-3 py-6 text-center text-xs leading-relaxed text-muted">
            {hasNextPage
              ? 'No match in this stretch — Redis scans a little at a time. Load more to keep looking.'
              : pattern || type
                ? 'No key matches.'
                : 'This database holds no keys yet.'}
          </p>
        ) : (
          keys.map((k) => {
            const left = secondsLeft(k.expiresAt, now)
            const current = k.id === selected
            return (
              <button
                key={k.id}
                type="button"
                data-key={k.id}
                onClick={() => onOpen(k)}
                aria-current={current || undefined}
                title={k.name}
                className={`flex h-8 w-full items-center gap-2 px-2 text-left font-mono text-xs transition-colors focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-accent-strong ${
                  current ? 'bg-accent-soft text-ink' : 'text-ink-soft hover:bg-line/50 hover:text-ink'
                }`}
              >
                <TypeBadge type={k.type} />
                <span className="min-w-0 flex-1 truncate">{k.name || '(empty name)'}</span>
                {left !== null && (
                  <span
                    className="shrink-0 text-[10px] text-muted"
                    title={left === 0 ? 'Expired' : `Expires in ${redisDuration(left)}`}
                  >
                    {left === 0 ? 'expired' : redisDuration(left).split(' ')[0]}
                  </span>
                )}
              </button>
            )
          })
        )}
      </nav>
      <footer className="flex min-h-10 shrink-0 items-center justify-between gap-2 border-t border-line py-1 pr-1.5 pl-3 text-xs text-muted">
        <span className="min-w-0 truncate" title={pattern ? `Matching ${pattern}` : undefined}>
          {data && (
            <>
              {keys.length.toLocaleString()} {keys.length === 1 ? 'key' : 'keys'}
              {hasNextPage ? ' so far' : ''}
            </>
          )}
          {pattern && (
            <span className="font-mono">
              {data ? ' · ' : ''}
              {pattern}
            </span>
          )}
        </span>
        {hasNextPage && (
          <Button kind="quiet" size="sm" disabled={isFetchingNextPage} onClick={() => void fetchNextPage()}>
            {isFetchingNextPage ? 'Scanning…' : 'Load more'}
          </Button>
        )}
      </footer>
    </aside>
  )
}

// The logical databases, with their key counts where the server tells.
// Apps only ever see the binding's own; the others are a console's reach.
function DatabasePicker({
  space,
  service,
  db,
  onPick,
}: {
  space: string
  service: string
  db?: string
  onPick: (db: string | undefined) => void
}) {
  const { data } = useRedisDatabases(space, service)
  const bound = data?.default ?? 0
  const current = db !== undefined ? Number(db) : bound
  const keys = new Map((data?.databases ?? []).map((d) => [d.db, d.keys]))
  // Every database up to a sensible number, plus any that hold keys beyond it.
  const listed = new Set<number>([...Array(Math.min(data?.count ?? 0, 64)).keys(), ...keys.keys(), bound, current])
  const options = [...listed]
    .sort((a, b) => a - b)
    .map((n) => ({
      value: String(n),
      label: `db ${n}`,
      detail: [
        keys.has(n) ? `${keys.get(n)!.toLocaleString()} keys` : data?.count ? 'empty' : '',
        n === bound ? 'default' : '',
      ]
        .filter(Boolean)
        .join(' · '),
    }))
  return (
    <Picker
      label="Database"
      value={String(current)}
      onChange={(next) => onPick(Number(next) === bound ? undefined : next)}
      options={options}
    />
  )
}

// ─── One key ───────────────────────────────────────────────────────────

function KeyView({
  space,
  service,
  db,
  id,
  onRenamed,
  onClosed,
  onBack,
}: {
  space: string
  service: string
  db?: string
  id: string
  onRenamed: (entry: { id: string }) => void
  onClosed: () => void
  onBack: () => void
}) {
  const value = useRedisValue(space, service, db, id)
  const writes = useRedisWrites(space, service, db)
  const label = redisLabel(id)
  const page = value.data?.pages[0]
  const [renaming, setRenaming] = useState<string | null>(null)
  const [deleting, setDeleting] = useState(false)
  const gone = (value.error as { status?: number } | null)?.status === 404

  const rename = () => {
    if (renaming === null) return
    const name = renaming.trim() === '' ? '' : renaming
    if (name === '' || name === label) return setRenaming(null)
    writes.change.mutate(
      { id, change: { op: 'rename', name } },
      {
        onSuccess: (result) => {
          setRenaming(null)
          if (result.id) onRenamed({ id: result.id })
        },
      },
    )
  }

  return (
    <>
      <header className="flex min-h-12 shrink-0 flex-wrap items-center gap-2 border-b border-line py-1.5 pr-1.5 pl-2">
        <IconButton icon={ChevronLeft} label="Back to the keys" onClick={onBack} className="md:hidden" />
        {page && <TypeBadge type={page.type} className="ml-1" />}
        {renaming !== null ? (
          <form
            className="flex min-w-48 flex-1 items-center gap-1"
            onSubmit={(e) => {
              e.preventDefault()
              rename()
            }}
          >
            <Input
              size="sm"
              autoFocus
              value={renaming}
              aria-label="New name"
              onChange={(e) => setRenaming(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === 'Escape') {
                  e.preventDefault()
                  setRenaming(null)
                  writes.change.reset()
                }
              }}
              className="min-w-0 flex-1"
            />
            <IconButton icon={Check} size="sm" type="submit" label="Rename" disabled={writes.change.isPending} />
            <IconButton
              icon={X}
              size="sm"
              label="Keep the name"
              onClick={() => {
                setRenaming(null)
                writes.change.reset()
              }}
            />
          </form>
        ) : (
          <h2 className="min-w-32 flex-1 truncate pl-1 font-mono text-sm font-semibold text-ink" title={label}>
            {label || '(empty name)'}
          </h2>
        )}
        <Toolbar>
          {renaming === null && (
            <>
              <CopyButton text={label} label="Copy the key name" />
              {redisNameOf(id) !== null && (
                <IconButton icon={Pencil} label="Rename the key" disabled={!page} onClick={() => setRenaming(label)} />
              )}
              <span className="mx-1 h-4 w-px bg-line" aria-hidden />
            </>
          )}
          {page && (
            <Expiry
              expiresAt={page.expiresAt}
              busy={writes.change.isPending}
              onSet={(ttl) => writes.change.mutateAsync({ id, change: { op: 'expire', ttl } })}
              onPersist={() => writes.change.mutateAsync({ id, change: { op: 'persist' } })}
              onExpired={() => void value.refetch()}
            />
          )}
          <IconButton
            icon={RefreshCw}
            label="Read the key again"
            onClick={() => void value.refetch()}
            className={value.isFetching && !value.isFetchingNextPage ? '[&>svg]:animate-spin' : ''}
          />
          <IconButton
            icon={Trash2}
            kind="danger"
            label="Delete the key…"
            disabled={!page}
            onClick={() => setDeleting(true)}
          />
        </Toolbar>
      </header>
      {writes.change.error && !deleting && (
        <div className="shrink-0 border-b border-line p-3">
          <ErrorNote message={mutationError(writes.change, 'That did not work.')} />
        </div>
      )}
      {gone ? (
        <div className="flex flex-1 flex-col items-center justify-center gap-3 p-8 text-center">
          <p className="max-w-sm text-sm text-muted">
            <span className="font-mono text-ink">{label}</span> is gone — it expired, was deleted, or went with its last
            element.
          </p>
          <Button kind="quiet" size="sm" onClick={onClosed}>
            Close
          </Button>
        </div>
      ) : value.error ? (
        <div className="space-y-3 p-4">
          <ErrorNote message={errText(value.error, 'Reading the key did not work.')} />
          <Button kind="quiet" size="sm" icon={RefreshCw} onClick={() => void value.refetch()}>
            Try again
          </Button>
        </div>
      ) : !value.data ? (
        <Spinner label="Reading the key…" />
      ) : (
        <RedisValueView
          space={space}
          service={service}
          db={db}
          id={id}
          pages={value.data.pages}
          hasMore={value.hasNextPage}
          loadingMore={value.isFetchingNextPage}
          onMore={() => void value.fetchNextPage()}
        />
      )}
      {deleting && page && (
        <Modal onClose={() => !writes.remove.isPending && setDeleting(false)} size="sm">
          <ModalHeader
            eyebrow={typeLabel[page.type] ?? page.type}
            title={
              <>
                Delete <span className="font-mono">{label}</span>?
              </>
            }
            onClose={() => setDeleting(false)}
            closeDisabled={writes.remove.isPending}
          />
          <ModalBody>
            <p className="text-sm leading-relaxed text-ink-soft">
              The key and all it holds ({redisSummary(page.type, page.size)}) go for good. An app that reads it next
              finds nothing.
            </p>
            {writes.remove.error && (
              <div className="mt-3">
                <ErrorNote message={mutationError(writes.remove, 'Deleting did not work.')} />
              </div>
            )}
          </ModalBody>
          <ModalFooter>
            <Button kind="quiet" onClick={() => setDeleting(false)} disabled={writes.remove.isPending}>
              Keep it
            </Button>
            <Button
              kind="danger"
              icon={Trash2}
              disabled={writes.remove.isPending}
              onClick={() => writes.remove.mutate(id, { onSuccess: onClosed })}
            >
              {writes.remove.isPending ? 'Deleting…' : 'Delete key'}
            </Button>
          </ModalFooter>
        </Modal>
      )}
    </>
  )
}

const units = [
  { value: 's', label: 'seconds', seconds: 1 },
  { value: 'm', label: 'minutes', seconds: 60 },
  { value: 'h', label: 'hours', seconds: 3600 },
  { value: 'd', label: 'days', seconds: 86400 },
] as const

// The key's expiry, counted down live; opened, a way to set or remove it.
function Expiry({
  expiresAt,
  busy,
  onSet,
  onPersist,
  onExpired,
}: {
  expiresAt: number | null
  busy: boolean
  onSet: (seconds: number) => Promise<unknown>
  onPersist: () => Promise<unknown>
  onExpired: () => void
}) {
  const now = useNow(1000)
  const [open, setOpen] = useState(false)
  const [amount, setAmount] = useState('')
  const [unit, setUnit] = useState<(typeof units)[number]['value']>('m')
  const left = secondsLeft(expiresAt, now)
  const expired = left === 0
  const expire = useRef(onExpired)
  useEffect(() => {
    expire.current = onExpired
  })
  useEffect(() => {
    // The countdown hit zero: read the key again to show it is gone.
    if (expired) expire.current()
  }, [expired])

  if (!open) {
    return (
      <Button
        kind="quiet"
        size="sm"
        icon={Clock}
        onClick={() => setOpen(true)}
        title="Set or remove the expiry"
        className="tabular-nums"
      >
        {left === null ? 'No expiry' : expired ? 'Expiring' : `Expires in ${redisDuration(left)}`}
      </Button>
    )
  }
  const seconds = Math.round(Number(amount) * (units.find((u) => u.value === unit)?.seconds ?? 1))
  const valid = amount !== '' && Number.isFinite(seconds) && seconds >= 1 && seconds <= 2147483647
  const done = () => {
    setOpen(false)
    setAmount('')
  }
  return (
    <form
      className="flex items-center gap-1"
      onSubmit={(e) => {
        e.preventDefault()
        if (valid) void onSet(seconds).then(done, () => {})
      }}
    >
      <Input
        size="sm"
        type="number"
        min="1"
        step="any"
        autoFocus
        value={amount}
        onChange={(e) => setAmount(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === 'Escape') {
            e.preventDefault()
            done()
          }
        }}
        aria-label="Expire after"
        placeholder="in…"
        className="w-20"
      />
      <Select
        size="sm"
        ariaLabel="Unit"
        value={unit}
        onChange={setUnit}
        options={units.map((u) => ({ value: u.value, label: u.label }))}
      />
      <Button size="sm" type="submit" disabled={!valid || busy}>
        Set
      </Button>
      {expiresAt !== null && (
        <Button kind="quiet" size="sm" disabled={busy} onClick={() => void onPersist().then(done, () => {})}>
          Never expire
        </Button>
      )}
      <IconButton icon={X} size="sm" label="Cancel" onClick={done} />
    </form>
  )
}

// ─── A new key ─────────────────────────────────────────────────────────

const typeChoices: { value: RedisType; icon: typeof Type; label: string; hint: string }[] = [
  { value: 'string', icon: Type, label: 'String', hint: 'Text, a number, JSON' },
  { value: 'hash', icon: Braces, label: 'Hash', hint: 'Fields and their values' },
  { value: 'list', icon: List, label: 'List', hint: 'Items in order, ends to push to' },
  { value: 'set', icon: Hash, label: 'Set', hint: 'Unique members, no order' },
  { value: 'zset', icon: ListOrdered, label: 'Sorted set', hint: 'Unique members ranked by score' },
  { value: 'stream', icon: Rows3, label: 'Stream', hint: 'An append-only log of entries' },
]

// What each type starts with — Redis keeps no empty collection.
const firstElements: Record<
  Exclude<RedisType, 'string'>,
  { title: string; noun: string; columns: { label: string; placeholder?: string; className?: string }[] }
> = {
  hash: { title: 'Fields', noun: 'field', columns: [{ label: 'Field', className: 'w-48' }, { label: 'Value' }] },
  list: { title: 'Items, head first', noun: 'item', columns: [{ label: 'Item' }] },
  set: { title: 'Members', noun: 'member', columns: [{ label: 'Member' }] },
  zset: {
    title: 'Members',
    noun: 'member',
    columns: [{ label: 'Member' }, { label: 'Score', placeholder: 'Score', className: 'w-32' }],
  },
  stream: {
    title: 'The first entry',
    noun: 'field',
    columns: [{ label: 'Field', className: 'w-48' }, { label: 'Value' }],
  },
}

function NewKey({
  space,
  service,
  db,
  onCreated,
  onCancel,
}: {
  space: string
  service: string
  db?: string
  onCreated: (entry: { id: string }) => void
  onCancel: () => void
}) {
  const { create } = useRedisWrites(space, service, db)
  const [name, setName] = useState('')
  const [type, setTypeState] = useState<RedisType>('string')
  const [value, setValue] = useState('')
  const [rows, setRows] = useState<string[][]>([['']])
  const [ttl, setTTL] = useState('')
  const setType = (next: RedisType) => {
    setTypeState(next)
    if (next !== 'string') setRows([firstElements[next].columns.map(() => '')])
  }
  const items = rows.filter((r) => r.some((cell) => cell !== ''))
  const badScore =
    type === 'zset' && items.some((r) => !/^\s*([+-]?inf|[+-]?(\d+\.?\d*|\.\d+)(e[+-]?\d+)?)\s*$/i.test(r[1] ?? ''))
  const expiry = ttl === '' ? undefined : Number(ttl)
  const badTTL = expiry !== undefined && (!Number.isInteger(expiry) || expiry < 0 || expiry > 2147483647)
  const ready = name !== '' && !badTTL && (type === 'string' || (items.length > 0 && !badScore))

  return (
    <form
      className="flex min-h-0 flex-1 flex-col"
      onSubmit={(e) => {
        e.preventDefault()
        if (!ready) return
        create.mutate(
          {
            name,
            type,
            value: type === 'string' ? value : undefined,
            items: type === 'string' ? undefined : items,
            ttl: expiry,
          },
          { onSuccess: (result) => onCreated({ id: result.id }) },
        )
      }}
    >
      <header className="flex min-h-12 shrink-0 items-center justify-between gap-2 border-b border-line py-1.5 pr-1.5 pl-2">
        <div className="flex min-w-0 items-center gap-1">
          <IconButton icon={ChevronLeft} label="Back to the keys" onClick={onCancel} className="md:hidden" />
          <h2 className="pl-1 text-sm font-semibold text-ink">New key{db !== undefined ? ` in db ${db}` : ''}</h2>
        </div>
        <IconButton icon={X} label="Cancel" onClick={onCancel} />
      </header>
      <div className="min-h-0 flex-1 overflow-y-auto px-4 py-5">
        <div className="max-w-3xl space-y-5">
          <Field label="Name">
            <Input
              autoFocus
              value={name}
              maxLength={4096}
              onChange={(e) => setName(e.target.value)}
              placeholder="user:42:profile"
            />
          </Field>
          <div className="space-y-1.5">
            <Eyebrow>Type</Eyebrow>
            <ChoiceCards
              value={type}
              onChange={setType}
              options={typeChoices}
              label="Type"
              columns="grid-cols-2 lg:grid-cols-3"
            />
          </div>
          {type === 'string' ? (
            <Field label="Value">
              <TextArea
                spellCheck={false}
                value={value}
                onChange={(e) => setValue(e.target.value)}
                className="min-h-40"
              />
            </Field>
          ) : (
            <div className="space-y-1.5">
              <Eyebrow>{firstElements[type].title}</Eyebrow>
              <RowsEditor
                columns={firstElements[type].columns}
                rows={rows}
                onChange={setRows}
                noun={firstElements[type].noun}
              />
              {badScore && <p className="text-xs text-danger">A score is a number, inf or -inf.</p>}
            </div>
          )}
          <Field label="Expiry in seconds" hint="Empty or 0: the key stays until it is deleted.">
            <Input
              type="number"
              min="0"
              step="1"
              value={ttl}
              onChange={(e) => setTTL(e.target.value)}
              placeholder="never"
              className="w-40"
            />
          </Field>
          <ErrorNote message={mutationError(create, 'Creating the key did not work.')} />
        </div>
      </div>
      <footer className="flex shrink-0 items-center justify-end gap-2 border-t border-line px-4 py-3">
        <Button kind="quiet" onClick={onCancel}>
          Cancel
        </Button>
        <Button type="submit" disabled={!ready || create.isPending}>
          {create.isPending ? 'Creating…' : 'Create key'}
        </Button>
      </footer>
    </form>
  )
}
