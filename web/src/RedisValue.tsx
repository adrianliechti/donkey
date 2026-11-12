// The open key's value, one view per Redis type. Collections are tables of
// their elements, each editable in its row; a string is text (or JSON) with
// an editor that saves only over the text it was opened from. Elements that
// arrived cut short, or as bytes rather than text, are shown and never
// edited — saving a cut value would write the cut.
import { useMemo, useState, type ReactNode } from 'react'
import { Pencil, Plus, Trash2, X } from 'lucide-react'
import { SourceCode } from './SourceCode'
import { mutationError } from './queries'
import {
  blobCut,
  blobRef,
  blobShown,
  blobSize,
  blobText,
  compactJson,
  looksLikeJson,
  prettyJson,
  redisSummary,
  type RedisBlob,
  type RedisChange,
  type RedisHashItem,
  type RedisListItem,
  type RedisSetItem,
  type RedisStreamItem,
  type RedisValuePage,
  type RedisZsetItem,
  typeShort,
  useRedisWrites,
} from './redisApi'
import { Button, Callout, ErrorNote, IconButton, Input, Select, TextArea, Toolbar } from './ui'
import { BarSearch } from './Workbench'
import { errText, humanSize } from './format'

type ViewProps = {
  space: string
  service: string
  db: string | undefined
  id: string
  pages: RedisValuePage[]
  hasMore: boolean
  loadingMore: boolean
  onMore: () => void
}

// The type, small and mono, the same in the sidebar and the key's header.
export function TypeBadge({ type, className = '' }: { type: string; className?: string }) {
  return (
    <span
      title={type}
      className={`inline-flex w-9 shrink-0 justify-center rounded bg-raised py-px font-mono text-[10px] leading-4 text-ink-soft ${className}`}
    >
      {typeShort(type)}
    </span>
  )
}

export function RedisValueView(props: ViewProps) {
  const type = props.pages[0]?.type
  switch (type) {
    case 'string':
      return <StringView {...props} />
    case 'hash':
      return <HashView {...props} />
    case 'list':
      return <ListView {...props} />
    case 'set':
      return <SetView {...props} />
    case 'zset':
      return <ZsetView {...props} />
    case 'stream':
      return <StreamView {...props} />
  }
  return (
    <div className="p-4">
      <Callout title={`A ${type} key`}>
        The console shows strings, hashes, lists, sets, sorted sets and streams. This one can still be renamed, given an
        expiry or deleted.
      </Callout>
    </div>
  )
}

// The bar over a value: what it holds on the left, its tools on the right.
function ViewBar({ summary, children }: { summary: ReactNode; children?: ReactNode }) {
  return (
    <div className="flex min-h-10 shrink-0 flex-wrap items-center justify-between gap-x-3 gap-y-1 border-b border-line py-1 pl-3 pr-1.5 text-xs text-muted">
      <span className="min-w-0 truncate">{summary}</span>
      {children && <Toolbar>{children}</Toolbar>}
    </div>
  )
}

function loadedSummary(page: RedisValuePage, loaded: number) {
  const total = redisSummary(page.type, page.size)
  return loaded < page.size ? `${total} · ${loaded} loaded` : total
}

// ─── Strings ───────────────────────────────────────────────────────────

// Text or JSON is a viewer's habit, kept between keys and visits.
const formatKey = 'donkey.redis.format'
function storedFormat(): 'json' | 'text' {
  try {
    return localStorage.getItem(formatKey) === 'text' ? 'text' : 'json'
  } catch {
    return 'json'
  }
}

function StringView({ space, service, db, id, pages }: ViewProps) {
  const page = pages[0]
  const blob = page.value ?? ''
  const text = blobText(blob)
  const isJson = useMemo(() => text !== null && looksLikeJson(text), [text])
  const [format, setFormatState] = useState(storedFormat)
  const setFormat = (next: 'json' | 'text') => {
    setFormatState(next)
    try {
      localStorage.setItem(formatKey, next)
    } catch {
      /* a convenience only */
    }
  }
  const asJson = isJson && format === 'json'
  const pretty = useMemo(() => (asJson && text !== null ? prettyJson(text) : ''), [asJson, text])
  const [draft, setDraft] = useState<string | null>(null)
  const [invalid, setInvalid] = useState('')
  const { saveString } = useRedisWrites(space, service, db)
  const editable = page.editable && text !== null

  const save = () => {
    if (draft === null || text === null) return
    let value = draft
    if (asJson) {
      try {
        JSON.parse(draft)
      } catch (err) {
        setInvalid(`Not valid JSON (${errText(err, 'parse error')}) — fix it, or switch to Text to save it as typed.`)
        return
      }
      // Stored compact stays compact. The text is re-spaced, never re-encoded:
      // a parse-and-print would round big numbers.
      if (!text.includes('\n')) value = compactJson(draft)
    }
    setInvalid('')
    saveString.mutate({ id, value, previous: text }, { onSuccess: () => setDraft(null) })
  }
  const cancel = () => {
    setDraft(null)
    setInvalid('')
    saveString.reset()
  }

  const shown = blobShown(blob)
  return (
    <>
      <ViewBar summary={redisSummary('string', page.size)}>
        {isJson && draft === null && (
          <Select
            variant="quiet"
            ariaLabel="Show as"
            value={format}
            onChange={setFormat}
            options={[
              { value: 'json', label: 'JSON' },
              { value: 'text', label: 'Text' },
            ]}
          />
        )}
        {draft === null ? (
          editable && (
            <Button kind="quiet" size="sm" icon={Pencil} onClick={() => setDraft(asJson ? pretty : text)}>
              Edit
            </Button>
          )
        ) : (
          <>
            <Button kind="quiet" size="sm" onClick={cancel} disabled={saveString.isPending}>
              Cancel
            </Button>
            <Button size="sm" onClick={save} disabled={saveString.isPending || draft === (asJson ? pretty : text)}>
              {saveString.isPending ? 'Saving…' : 'Save'}
            </Button>
          </>
        )}
      </ViewBar>
      {(invalid || saveString.error) && (
        <div className="shrink-0 border-b border-line p-3">
          <ErrorNote message={invalid || mutationError(saveString, 'Saving did not work.')} />
        </div>
      )}
      {!editable && (
        <div className="shrink-0 border-b border-line p-3">
          <Callout tone="neutral">
            {blobCut(blob)
              ? `Showing the first ${humanSize(redisPreviewShown(blob))} of ${humanSize(page.size)}. A value this large is read-only here — reach it with redis-cli through donkey addon tunnel.`
              : 'Not text — shown as base64, read-only.'}
          </Callout>
        </div>
      )}
      {draft !== null ? (
        <div className="flex min-h-0 flex-1 flex-col p-3">
          <TextArea
            size="sm"
            autoFocus
            spellCheck={false}
            aria-label="Value"
            value={draft}
            onChange={(e) => setDraft(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
                e.preventDefault()
                save()
              }
              if (e.key === 'Escape') {
                e.preventDefault()
                cancel()
              }
            }}
            className="min-h-0 flex-1 resize-none"
          />
          <p className="mt-1.5 text-[11px] text-muted">⌘ or Ctrl ↵ saves · Esc cancels · the expiry stays as it is</p>
        </div>
      ) : page.size === 0 ? (
        <p className="p-4 text-sm text-muted">An empty string.</p>
      ) : asJson ? (
        <div className="min-h-0 flex-1 overflow-auto bg-well">
          <SourceCode path="value.json" text={pretty} className="" />
        </div>
      ) : (
        <pre className="min-h-0 flex-1 overflow-auto whitespace-pre-wrap break-all bg-well p-3 font-mono text-xs leading-relaxed text-emphasis">
          {shown.text}
        </pre>
      )}
    </>
  )
}

function redisPreviewShown(blob: RedisBlob): number {
  const shown = blobShown(blob)
  return shown.binary ? atob(shown.text).length : new TextEncoder().encode(shown.text).length
}

// ─── Elements ──────────────────────────────────────────────────────────

// One element's text in a cell: clamped to a few lines until clicked, bytes
// marked as base64, a cut one with its full size.
function BlobText({ blob, className = '' }: { blob: RedisBlob; className?: string }) {
  const [open, setOpen] = useState(false)
  const shown = blobShown(blob)
  const long = shown.text.length > 160 || shown.text.split('\n').length > 3
  const body = (
    <>
      {shown.binary && <span className="mr-1 text-muted">base64</span>}
      {shown.text === '' ? <span className="text-muted">(empty)</span> : shown.text}
      {blobCut(blob) && <span className="ml-1 text-muted">… {humanSize(blobSize(blob))} in all</span>}
    </>
  )
  const cls = `block min-w-0 whitespace-pre-wrap break-all font-mono text-xs leading-relaxed text-ink ${open ? '' : 'line-clamp-3'} ${className}`
  if (!long) return <span className={cls}>{body}</span>
  return (
    <button
      type="button"
      onClick={() => setOpen(!open)}
      title={open ? 'Show less' : 'Show all'}
      className={`${cls} rounded text-left focus-visible:outline-2 focus-visible:outline-accent-strong`}
    >
      {body}
    </button>
  )
}

type Cell = { blob?: RedisBlob; text?: ReactNode; className?: string }

/**
 * One element of a collection as a table row: its cells, and — when the
 * element can be addressed — an inline editor for one cell and a delete
 * that asks first. Busy and error state are the row's own.
 */
function ElementRow({
  cells,
  edit,
  remove,
  label,
}: {
  cells: Cell[]
  /** Which cell edits, from what, saved how; absent when it cannot. */
  edit?: { cell: number; initial: string; multiline?: boolean; save: (draft: string) => Promise<unknown> }
  remove?: () => Promise<unknown>
  label: string
}) {
  const [mode, setMode] = useState<'view' | 'edit' | 'confirm'>('view')
  const [draft, setDraft] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const run = async (fn: () => Promise<unknown>, fallback: string) => {
    setBusy(true)
    setError('')
    try {
      await fn()
      setMode('view')
    } catch (err) {
      setError(errText(err, fallback))
    } finally {
      setBusy(false)
    }
  }
  const save = () => edit && run(() => edit.save(draft), 'Saving did not work.')
  const cancel = () => {
    setMode('view')
    setError('')
  }

  return (
    <>
      <tr className={`border-b border-line/60 align-top ${mode === 'confirm' ? 'bg-danger/10' : 'hover:bg-raised/40'}`}>
        {cells.map((cell, i) => (
          <td key={i} className={`min-w-0 px-3 py-2 ${cell.className ?? ''}`}>
            {mode === 'edit' && edit?.cell === i ? (
              edit.multiline ? (
                <TextArea
                  size="sm"
                  autoFocus
                  spellCheck={false}
                  aria-label={`New value for ${label}`}
                  rows={Math.min(12, Math.max(2, draft.split('\n').length))}
                  value={draft}
                  onChange={(e) => setDraft(e.target.value)}
                  onKeyDown={(e) => {
                    if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
                      e.preventDefault()
                      save()
                    }
                    if (e.key === 'Escape') {
                      e.preventDefault()
                      cancel()
                    }
                  }}
                  className="w-full"
                />
              ) : (
                <Input
                  size="sm"
                  autoFocus
                  aria-label={`New value for ${label}`}
                  value={draft}
                  onChange={(e) => setDraft(e.target.value)}
                  onKeyDown={(e) => {
                    if (e.key === 'Enter') {
                      e.preventDefault()
                      save()
                    }
                    if (e.key === 'Escape') {
                      e.preventDefault()
                      cancel()
                    }
                  }}
                  className="w-full"
                />
              )
            ) : cell.blob !== undefined ? (
              <BlobText blob={cell.blob} />
            ) : (
              cell.text
            )}
          </td>
        ))}
        <td className="w-28 py-1.5 pr-1.5 align-top">
          <div className="flex justify-end gap-0.5">
            {mode === 'view' && (
              <>
                {edit && (
                  <IconButton
                    icon={Pencil}
                    size="sm"
                    label={`Edit ${label}`}
                    onClick={() => {
                      setDraft(edit.initial)
                      setMode('edit')
                    }}
                  />
                )}
                {remove && (
                  <IconButton
                    icon={Trash2}
                    size="sm"
                    kind="danger"
                    label={`Delete ${label}`}
                    onClick={() => setMode('confirm')}
                  />
                )}
              </>
            )}
            {mode === 'edit' && (
              <>
                <Button size="sm" disabled={busy} onClick={save}>
                  {busy ? 'Saving…' : 'Save'}
                </Button>
                <IconButton icon={X} size="sm" label="Cancel" disabled={busy} onClick={cancel} />
              </>
            )}
            {mode === 'confirm' && remove && (
              <>
                <Button kind="danger" size="sm" disabled={busy} onClick={() => run(remove, 'Deleting did not work.')}>
                  {busy ? 'Deleting…' : 'Delete'}
                </Button>
                <IconButton icon={X} size="sm" label="Keep it" disabled={busy} onClick={cancel} />
              </>
            )}
          </div>
        </td>
      </tr>
      {error && (
        <tr>
          <td colSpan={cells.length + 1} className="px-3 pb-2">
            <ErrorNote message={error} />
          </td>
        </tr>
      )}
    </>
  )
}

function Table({ head, children }: { head: { label: string; className?: string }[]; children: ReactNode }) {
  return (
    <table className="w-full table-fixed border-collapse text-left">
      <thead className="sticky top-0 z-10 bg-card">
        <tr className="border-b border-line">
          {head.map((h) => (
            <th
              key={h.label}
              className={`px-3 py-2 font-mono text-[10px] font-semibold uppercase tracking-[0.14em] text-muted ${h.className ?? ''}`}
            >
              {h.label}
            </th>
          ))}
          <th className="w-28">
            <span className="sr-only">Actions</span>
          </th>
        </tr>
      </thead>
      <tbody>{children}</tbody>
    </table>
  )
}

function MoreRows({
  hasMore,
  loadingMore,
  onMore,
  left,
}: {
  hasMore: boolean
  loadingMore: boolean
  onMore: () => void
  left: number
}) {
  if (!hasMore) return null
  return (
    <div className="flex justify-center p-3">
      <Button kind="quiet" size="sm" disabled={loadingMore} onClick={onMore}>
        {loadingMore ? 'Loading…' : left > 0 ? `Load more · ${left} left` : 'Load more'}
      </Button>
    </div>
  )
}

// The add form at the top of a collection: a few fields and the ways to
// commit them (one button for most, two for a list's ends).
function AddRow({
  fields,
  actions,
  onClose,
}: {
  fields: { label: string; placeholder?: string; multiline?: boolean; className?: string }[]
  actions: { label: string; run: (values: string[]) => Promise<unknown> }[]
  onClose: () => void
}) {
  const [values, setValues] = useState(() => fields.map(() => ''))
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const set = (i: number, v: string) => setValues((cur) => cur.map((x, j) => (j === i ? v : x)))
  const run = async (action: (typeof actions)[number]) => {
    setBusy(true)
    setError('')
    try {
      await action.run(values)
      setValues(fields.map(() => ''))
    } catch (err) {
      setError(errText(err, 'Adding did not work.'))
    } finally {
      setBusy(false)
    }
  }
  return (
    <div className="shrink-0 space-y-2 border-b border-line bg-raised/40 p-3">
      <div className="flex flex-wrap items-start gap-2">
        {fields.map((f, i) =>
          f.multiline ? (
            <TextArea
              key={f.label}
              size="sm"
              autoFocus={i === 0}
              spellCheck={false}
              rows={2}
              aria-label={f.label}
              placeholder={f.placeholder ?? f.label}
              value={values[i]}
              onChange={(e) => set(i, e.target.value)}
              onKeyDown={(e) => {
                if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
                  e.preventDefault()
                  void run(actions[actions.length - 1])
                }
                if (e.key === 'Escape') {
                  e.preventDefault()
                  onClose()
                }
              }}
              className={f.className ?? 'min-w-48 flex-1'}
            />
          ) : (
            <Input
              key={f.label}
              size="sm"
              autoFocus={i === 0}
              aria-label={f.label}
              placeholder={f.placeholder ?? f.label}
              value={values[i]}
              onChange={(e) => set(i, e.target.value)}
              onKeyDown={(e) => {
                if (e.key === 'Enter') {
                  e.preventDefault()
                  void run(actions[actions.length - 1])
                }
                if (e.key === 'Escape') {
                  e.preventDefault()
                  onClose()
                }
              }}
              className={f.className ?? 'min-w-40 flex-1'}
            />
          ),
        )}
        <Toolbar>
          {actions.map((a) => (
            <Button
              key={a.label}
              size="sm"
              kind={actions.length > 1 ? 'quiet' : 'primary'}
              disabled={busy}
              onClick={() => void run(a)}
            >
              {a.label}
            </Button>
          ))}
          <IconButton icon={X} size="sm" label="Close" onClick={onClose} />
        </Toolbar>
      </div>
      <ErrorNote message={error} />
    </div>
  )
}

function useChange(space: string, service: string, db: string | undefined, id: string) {
  const { change } = useRedisWrites(space, service, db)
  return (c: RedisChange) => change.mutateAsync({ id, change: c })
}

// A filter over what is loaded: big collections are paged, and the server
// reads them in its own order, so a search there would be a SCAN MATCH of
// its own. This narrows the screenful.
function FilterInput({ value, onChange, noun }: { value: string; onChange: (v: string) => void; noun: string }) {
  return (
    <BarSearch
      value={value}
      onChange={onChange}
      placeholder={`Filter ${noun}`}
      label={`Filter the loaded ${noun}`}
      className="w-32"
    />
  )
}

const matches = (filter: string, ...blobs: RedisBlob[]) =>
  filter === '' || blobs.some((b) => blobShown(b).text.toLowerCase().includes(filter.toLowerCase()))

function HashView({ space, service, db, id, pages, hasMore, loadingMore, onMore }: ViewProps) {
  const items = pages.flatMap((p) => (p.items ?? []) as RedisHashItem[])
  const change = useChange(space, service, db, id)
  const [adding, setAdding] = useState(false)
  const [filter, setFilter] = useState('')
  const shown = items.filter((item) => matches(filter, item.field, item.value))
  return (
    <>
      <ViewBar summary={loadedSummary(pages[0], items.length)}>
        <FilterInput value={filter} onChange={setFilter} noun="fields" />
        <Button kind="quiet" size="sm" icon={Plus} onClick={() => setAdding(true)}>
          Add field
        </Button>
      </ViewBar>
      {adding && (
        <AddRow
          fields={[
            { label: 'Field', className: 'w-56' },
            { label: 'Value', multiline: true },
          ]}
          actions={[{ label: 'Add', run: ([field, value]) => change({ op: 'hset', field, value, create: true }) }]}
          onClose={() => setAdding(false)}
        />
      )}
      <div className="min-h-0 flex-1 overflow-auto">
        <Table head={[{ label: 'Field', className: 'w-1/3' }, { label: 'Value' }]}>
          {shown.map((item) => {
            const field = blobRef(item.field)
            const value = blobText(item.value)
            const name = blobShown(item.field).text
            return (
              <ElementRow
                key={JSON.stringify(item.field)}
                label={`field ${name}`}
                cells={[{ blob: item.field }, { blob: item.value }]}
                edit={
                  field !== null && value !== null
                    ? {
                        cell: 1,
                        initial: value,
                        multiline: true,
                        save: (draft) => change({ op: 'hset', field, value: draft, previous: value }),
                      }
                    : undefined
                }
                remove={field !== null ? () => change({ op: 'hdel', field }) : undefined}
              />
            )
          })}
        </Table>
        {shown.length === 0 && filter && <p className="p-4 text-sm text-muted">No loaded field matches.</p>}
        <MoreRows hasMore={hasMore} loadingMore={loadingMore} onMore={onMore} left={pages[0].size - items.length} />
      </div>
    </>
  )
}

function ListView({ space, service, db, id, pages, hasMore, loadingMore, onMore }: ViewProps) {
  const items = pages.flatMap((p) => (p.items ?? []) as RedisListItem[])
  const change = useChange(space, service, db, id)
  const [adding, setAdding] = useState(false)
  return (
    <>
      <ViewBar summary={loadedSummary(pages[0], items.length)}>
        <Button kind="quiet" size="sm" icon={Plus} onClick={() => setAdding(true)}>
          Push item
        </Button>
      </ViewBar>
      {adding && (
        <AddRow
          fields={[{ label: 'Item', multiline: true }]}
          actions={[
            { label: 'Push to head', run: ([value]) => change({ op: 'lpush', value }) },
            { label: 'Push to tail', run: ([value]) => change({ op: 'rpush', value }) },
          ]}
          onClose={() => setAdding(false)}
        />
      )}
      <div className="min-h-0 flex-1 overflow-auto">
        <Table head={[{ label: '#', className: 'w-16' }, { label: 'Item' }]}>
          {items.map((item) => {
            // Lists edit by position, so every edit names the item it
            // replaces: a list that shifted underneath is refused.
            const previous = blobRef(item.value)
            const value = blobText(item.value)
            return (
              <ElementRow
                key={item.index}
                label={`item ${item.index}`}
                cells={[
                  { text: <span className="font-mono text-xs text-muted">{item.index}</span> },
                  { blob: item.value },
                ]}
                edit={
                  previous !== null && value !== null
                    ? {
                        cell: 1,
                        initial: value,
                        multiline: true,
                        save: (draft) => change({ op: 'lset', index: item.index, previous, value: draft }),
                      }
                    : undefined
                }
                remove={previous !== null ? () => change({ op: 'lrem', index: item.index, previous }) : undefined}
              />
            )
          })}
        </Table>
        <MoreRows hasMore={hasMore} loadingMore={loadingMore} onMore={onMore} left={pages[0].size - items.length} />
      </div>
    </>
  )
}

function SetView({ space, service, db, id, pages, hasMore, loadingMore, onMore }: ViewProps) {
  const items = pages.flatMap((p) => (p.items ?? []) as RedisSetItem[])
  const change = useChange(space, service, db, id)
  const [adding, setAdding] = useState(false)
  const [filter, setFilter] = useState('')
  const shown = items.filter((item) => matches(filter, item.member))
  return (
    <>
      <ViewBar summary={loadedSummary(pages[0], items.length)}>
        <FilterInput value={filter} onChange={setFilter} noun="members" />
        <Button kind="quiet" size="sm" icon={Plus} onClick={() => setAdding(true)}>
          Add member
        </Button>
      </ViewBar>
      {adding && (
        <AddRow
          fields={[{ label: 'Member' }]}
          actions={[{ label: 'Add', run: ([member]) => change({ op: 'sadd', member }) }]}
          onClose={() => setAdding(false)}
        />
      )}
      <div className="min-h-0 flex-1 overflow-auto">
        <Table head={[{ label: 'Member' }]}>
          {shown.map((item) => {
            const member = blobRef(item.member)
            return (
              <ElementRow
                key={JSON.stringify(item.member)}
                label={`member ${blobShown(item.member).text}`}
                cells={[{ blob: item.member }]}
                remove={member !== null ? () => change({ op: 'srem', member }) : undefined}
              />
            )
          })}
        </Table>
        {shown.length === 0 && filter && <p className="p-4 text-sm text-muted">No loaded member matches.</p>}
        <MoreRows hasMore={hasMore} loadingMore={loadingMore} onMore={onMore} left={pages[0].size - items.length} />
      </div>
    </>
  )
}

function ZsetView({ space, service, db, id, pages, hasMore, loadingMore, onMore }: ViewProps) {
  const items = pages.flatMap((p) => (p.items ?? []) as RedisZsetItem[])
  const change = useChange(space, service, db, id)
  const [adding, setAdding] = useState(false)
  const [filter, setFilter] = useState('')
  const shown = items.filter((item) => matches(filter, item.member))
  return (
    <>
      <ViewBar summary={`${loadedSummary(pages[0], items.length)} · lowest score first`}>
        <FilterInput value={filter} onChange={setFilter} noun="members" />
        <Button kind="quiet" size="sm" icon={Plus} onClick={() => setAdding(true)}>
          Add member
        </Button>
      </ViewBar>
      {adding && (
        <AddRow
          fields={[{ label: 'Member' }, { label: 'Score', placeholder: 'Score, e.g. 1.5', className: 'w-36' }]}
          actions={[{ label: 'Add', run: ([member, score]) => change({ op: 'zadd', member, score, create: true }) }]}
          onClose={() => setAdding(false)}
        />
      )}
      <div className="min-h-0 flex-1 overflow-auto">
        <Table head={[{ label: 'Member' }, { label: 'Score', className: 'w-40' }]}>
          {shown.map((item) => {
            const member = blobRef(item.member)
            return (
              <ElementRow
                key={JSON.stringify(item.member)}
                label={`member ${blobShown(item.member).text}`}
                cells={[
                  { blob: item.member },
                  { text: <span className="font-mono text-xs text-ink">{item.score}</span>, className: 'w-40' },
                ]}
                edit={
                  member !== null
                    ? { cell: 1, initial: item.score, save: (score) => change({ op: 'zadd', member, score }) }
                    : undefined
                }
                remove={member !== null ? () => change({ op: 'zrem', member }) : undefined}
              />
            )
          })}
        </Table>
        {shown.length === 0 && filter && <p className="p-4 text-sm text-muted">No loaded member matches.</p>}
        <MoreRows hasMore={hasMore} loadingMore={loadingMore} onMore={onMore} left={pages[0].size - items.length} />
      </div>
    </>
  )
}

// A stream entry's ID starts with the millisecond it was added.
function entryTime(id: string): string {
  const ms = Number(id.split('-')[0])
  return Number.isFinite(ms) ? new Date(ms).toLocaleString() : ''
}

function StreamView({ space, service, db, id, pages, hasMore, loadingMore, onMore }: ViewProps) {
  const items = pages.flatMap((p) => (p.items ?? []) as RedisStreamItem[])
  const change = useChange(space, service, db, id)
  const [adding, setAdding] = useState(false)
  return (
    <>
      <ViewBar summary={`${loadedSummary(pages[0], items.length)} · oldest first`}>
        <Button kind="quiet" size="sm" icon={Plus} onClick={() => setAdding(true)}>
          Add entry
        </Button>
      </ViewBar>
      {adding && <AddEntry onAdd={(fields) => change({ op: 'xadd', fields })} onClose={() => setAdding(false)} />}
      <div className="min-h-0 flex-1 overflow-auto">
        <ul>
          {items.map((entry) => (
            <StreamEntry key={entry.id} entry={entry} onDelete={() => change({ op: 'xdel', entry: entry.id })} />
          ))}
        </ul>
        <MoreRows hasMore={hasMore} loadingMore={loadingMore} onMore={onMore} left={pages[0].size - items.length} />
      </div>
    </>
  )
}

function StreamEntry({ entry, onDelete }: { entry: RedisStreamItem; onDelete: () => Promise<unknown> }) {
  const [confirm, setConfirm] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  return (
    <li className={`border-b border-line/60 px-3 py-2 ${confirm ? 'bg-danger/10' : ''}`}>
      <div className="flex min-h-7 items-center gap-3">
        <span className="font-mono text-xs text-ink">{entry.id}</span>
        <span className="min-w-0 flex-1 truncate text-xs text-muted">{entryTime(entry.id)}</span>
        <div className="-mr-1.5 flex shrink-0 items-center gap-0.5">
          {confirm ? (
            <>
              <Button
                kind="danger"
                size="sm"
                disabled={busy}
                onClick={async () => {
                  setBusy(true)
                  setError('')
                  try {
                    await onDelete()
                  } catch (err) {
                    setError(errText(err, 'Deleting did not work.'))
                    setBusy(false)
                  }
                }}
              >
                {busy ? 'Deleting…' : 'Delete'}
              </Button>
              <IconButton icon={X} size="sm" label="Keep it" disabled={busy} onClick={() => setConfirm(false)} />
            </>
          ) : (
            <IconButton
              icon={Trash2}
              size="sm"
              kind="danger"
              label={`Delete entry ${entry.id}`}
              onClick={() => setConfirm(true)}
            />
          )}
        </div>
      </div>
      <dl className="mt-1 grid grid-cols-[minmax(5rem,max-content)_minmax(0,1fr)] gap-x-4 gap-y-1">
        {entry.fields.map((f, i) => (
          <div key={i} className="contents">
            <dt className="min-w-0">
              <BlobText blob={f.field} className="text-muted" />
            </dt>
            <dd className="min-w-0">
              <BlobText blob={f.value} />
            </dd>
          </div>
        ))}
      </dl>
      {error && (
        <div className="mt-2">
          <ErrorNote message={error} />
        </div>
      )}
    </li>
  )
}

function AddEntry({
  onAdd,
  onClose,
}: {
  onAdd: (fields: [string, string][]) => Promise<unknown>
  onClose: () => void
}) {
  const [rows, setRows] = useState<string[][]>([['', '']])
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const fields = rows.filter((r) => r[0] !== '' || r[1] !== '').map((r) => [r[0], r[1]] as [string, string])
  return (
    <form
      className="shrink-0 space-y-2 border-b border-line bg-raised/40 p-3"
      onSubmit={async (e) => {
        e.preventDefault()
        setBusy(true)
        setError('')
        try {
          await onAdd(fields)
          setRows([['', '']])
        } catch (err) {
          setError(errText(err, 'Adding did not work.'))
        } finally {
          setBusy(false)
        }
      }}
    >
      <RowsEditor columns={[{ label: 'Field' }, { label: 'Value' }]} rows={rows} onChange={setRows} noun="field" />
      <div className="flex items-center justify-end gap-2">
        <Button kind="quiet" size="sm" onClick={onClose}>
          Close
        </Button>
        <Button type="submit" size="sm" disabled={busy || fields.length === 0}>
          {busy ? 'Adding…' : 'Add entry'}
        </Button>
      </div>
      <ErrorNote message={error} />
    </form>
  )
}

/** Rows of a few inputs each — a new key's first elements, a stream entry. */
export function RowsEditor({
  columns,
  rows,
  onChange,
  noun,
}: {
  columns: { label: string; placeholder?: string; className?: string }[]
  rows: string[][]
  onChange: (rows: string[][]) => void
  noun: string
}) {
  const set = (r: number, c: number, v: string) =>
    onChange(rows.map((row, i) => (i === r ? row.map((x, j) => (j === c ? v : x)) : row)))
  return (
    <div className="space-y-1.5">
      {rows.map((row, r) => (
        <div key={r} className="flex items-center gap-1.5">
          {columns.map((col, c) => (
            <Input
              key={c}
              size="sm"
              aria-label={`${col.label} ${r + 1}`}
              placeholder={col.placeholder ?? col.label}
              value={row[c] ?? ''}
              onChange={(e) => set(r, c, e.target.value)}
              className={col.className ?? 'min-w-0 flex-1'}
            />
          ))}
          <IconButton
            icon={X}
            size="sm"
            label={`Remove ${noun} ${r + 1}`}
            disabled={rows.length === 1}
            onClick={() => onChange(rows.filter((_, i) => i !== r))}
          />
        </div>
      ))}
      <Button kind="quiet" size="sm" icon={Plus} onClick={() => onChange([...rows, columns.map(() => '')])}>
        Another {noun}
      </Button>
    </div>
  )
}
