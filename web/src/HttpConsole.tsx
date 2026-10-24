import { useState, type KeyboardEvent } from 'react'
import { CopyPlus, FileQuestion, Plus, Save, Send, SquareTerminal, Trash2, X } from 'lucide-react'
import type { ConsoleProps } from './consoles'
import {
  baseURLOf,
  cancelExchange,
  composeURL,
  curlFor,
  draftTitle,
  editsBox,
  emptyDraft,
  exchange,
  exchangesBox,
  fitsSaved,
  historyBox,
  maxSaved,
  maxSavedBytes,
  methods,
  methodTone,
  parseURL,
  sameDraft,
  savedBox,
  scratchBox,
  type Draft,
  type Saved,
} from './httpApi'
import { newId, useBox } from './httpStore'
import { ConfirmIconButton, CopyAs, Keys, Pane, SideHeader, SideItem } from './HttpWorkbench'
import { RequestPane } from './HttpRequest'
import { ResponsePane } from './HttpResponse'
import { useServiceBinding } from './queries'
import { useServiceSearch } from './serviceSearch'
import { Button, Callout, ErrorNote, IconButton, Select } from './ui'
import { ago, humanSize } from './format'

// The Requests tab of an HTTP service: a REST client for the API behind it.
// Saved requests and the history on the left; the request being written and
// its answer side by side. The open saved request is the console's place
// (?saved=…), so back and forward walk between requests and one can be
// linked — within this browser, which is where saved requests live.

export function Console({ space, service }: ConsoleProps) {
  const [search, move] = useServiceSearch()
  const saved = useBox(savedBox(space, service))
  const scratch = useBox(scratchBox(space, service))
  const edits = useBox(editsBox(space, service))
  const exchanges = useBox(exchangesBox(space, service))
  const { data: binding, error: bindingError } = useServiceBinding(space, service)
  const base = binding ? baseURLOf(binding.binding, binding.type, service) : ''
  const [renaming, setRenaming] = useState('')
  const [saveError, setSaveError] = useState('')

  const open = search.saved ? saved.find((s) => s.id === search.saved) : undefined
  const missing = Boolean(search.saved && !open)
  const place = open ? `saved:${open.id}` : 'scratch'
  const draft = open ? (edits[open.id] ?? open.draft) : scratch
  const dirty = open ? !sameDraft(draft, open.draft) : false
  const current = exchanges[place]
  const pending = current?.state === 'pending'

  const setDraft = (next: Draft) => {
    if (open) editsBox(space, service).set((all) => ({ ...all, [open.id]: next }))
    else scratchBox(space, service).set(next)
  }

  const send = () => {
    if (pending) return
    void exchange(space, service, place, draft)
  }
  const cancel = () => {
    if (current) cancelExchange(current.id)
  }

  const goTo = (id?: string) => {
    if ((id ?? '') === (search.saved ?? '')) return
    move({ ...search, saved: id ?? '' })
  }

  // Saving a saved request writes its edits back; anything else becomes a
  // new saved request, named after itself until it is given a name — the
  // row opens for renaming right away.
  const save = (asNew = false) => {
    setSaveError('')
    if (JSON.stringify(draft).length > maxSavedBytes) {
      setSaveError(`Too large to keep in this browser — saved requests hold up to ${humanSize(maxSavedBytes)}.`)
      return
    }
    const full = `This browser keeps up to ${maxSaved} saved requests, within 1 MB — delete one first.`
    if (open && !asNew) {
      const next = saved.map((s) => (s.id === open.id ? { ...s, draft, savedAt: Date.now() } : s))
      if (!fitsSaved(next)) return setSaveError(full)
      savedBox(space, service).set(next)
      dropEdits(open.id)
      return
    }
    const entry: Saved = { id: newId(), name: open && asNew ? `${open.name} copy` : draftTitle(draft), draft, savedAt: Date.now() }
    if (!fitsSaved([entry, ...saved])) return setSaveError(full)
    savedBox(space, service).set([entry, ...saved])
    const exchanges = exchangesBox(space, service)
    if (!open) {
      // The scratch pad's answer moves with it; the pad starts over.
      exchanges.set((all) => {
        const { scratch: moved, ...rest } = all
        return moved ? { ...rest, [`saved:${entry.id}`]: moved } : rest
      })
      scratchBox(space, service).set(emptyDraft())
    } else {
      dropEdits(open.id)
    }
    setRenaming(entry.id)
    move({ ...search, saved: entry.id }, open ? 'push' : 'replace')
  }
  const dropEdits = (id: string) =>
    editsBox(space, service).set((all) => {
      const next = { ...all }
      delete next[id]
      return next
    })

  const remove = (id: string) => {
    savedBox(space, service).set((list) => list.filter((s) => s.id !== id))
    dropEdits(id)
    if (open?.id === id) move({ ...search, saved: '' }, 'replace')
  }

  // A sent request opens on the scratch pad, ready to send again.
  const reopen = (d: Draft) => {
    scratchBox(space, service).set(d)
    goTo()
  }

  const newRequest = () => {
    scratchBox(space, service).set(emptyDraft())
    exchangesBox(space, service).set((all) => {
      const rest = { ...all }
      delete rest.scratch
      return rest
    })
    goTo()
  }

  const onKeyDown = (e: KeyboardEvent) => {
    const mod = e.metaKey || e.ctrlKey
    if (mod && e.key === 'Enter') {
      e.preventDefault()
      send()
    } else if (mod && e.key.toLowerCase() === 's') {
      e.preventDefault()
      if (!open || dirty) save()
    } else if (e.key === 'Escape' && pending) {
      cancel()
    }
  }

  return (
    <div className="flex min-h-0 flex-1 gap-4" onKeyDown={onKeyDown}>
      <Places
        space={space}
        service={service}
        saved={saved}
        edits={edits}
        openId={open?.id}
        renaming={renaming}
        onRenamed={() => setRenaming('')}
        onOpen={goTo}
        onRemove={remove}
        onReopen={reopen}
        onNew={newRequest}
      />
      <div className="flex min-w-0 flex-1 flex-col gap-3">
        {missing && (
          <Callout
            tone="warning"
            icon={FileQuestion}
            action={
              <Button kind="quiet" size="sm" onClick={() => move({ ...search, saved: '' }, 'replace')}>
                Dismiss
              </Button>
            }
          >
            That saved request is not in this browser — saved requests stay where they were made.
          </Callout>
        )}
        <ErrorNote message={bindingError} />
        <RequestBar
          draft={draft}
          base={base}
          pending={pending}
          open={open}
          dirty={dirty}
          onChange={setDraft}
          onSend={send}
          onCancel={cancel}
          onSave={save}
        />
        <ErrorNote message={saveError} />
        <div className="grid min-h-0 flex-1 grid-rows-2 gap-3 xl:grid-cols-2 xl:grid-rows-1">
          <RequestPane draft={draft} onChange={setDraft} />
          <ResponsePane service={service} base={base} exchange={current} onCancel={cancel} />
        </div>
      </div>
    </div>
  )
}

// ─── The sidebar ───────────────────────────────────────────────────────

function Places({
  space,
  service,
  saved,
  edits,
  openId,
  renaming,
  onRenamed,
  onOpen,
  onRemove,
  onReopen,
  onNew,
}: {
  space: string
  service: string
  saved: Saved[]
  edits: Record<string, Draft>
  openId?: string
  renaming: string
  onRenamed: () => void
  onOpen: (id: string) => void
  onRemove: (id: string) => void
  onReopen: (draft: Draft) => void
  onNew: () => void
}) {
  const history = useBox(historyBox(space, service))
  const sorted = [...saved].sort((a, b) => a.name.localeCompare(b.name))
  const rename = (id: string, name: string) =>
    savedBox(space, service).set((list) => list.map((s) => (s.id === id ? { ...s, name } : s)))
  return (
    <Pane label="Saved requests and history" className="hidden w-64 shrink-0 md:flex">
      <SideHeader
        title="Saved"
        count={saved.length}
        actions={<IconButton icon={Plus} size="sm" label="New request" onClick={onNew} />}
      />
      <ul className="max-h-[45%] shrink-0 overflow-y-auto pb-1">
        {sorted.length === 0 && (
          <li className="px-3 pb-2 text-xs leading-relaxed text-muted">
            Save a request to keep it here, under a name. <Keys chord="save" /> saves.
          </li>
        )}
        {sorted.map((s) => (
          <SideItem
            key={s.id}
            selected={s.id === openId}
            onOpen={() => onOpen(s.id)}
            lead={<MethodLabel method={s.draft.method} />}
            title={
              <>
                {s.name}
                {edits[s.id] && !sameDraft(edits[s.id], s.draft) && (
                  <span className="ml-1.5 inline-block size-1.5 rounded-full bg-accent align-middle" title="Changed, not saved" />
                )}
              </>
            }
            meta={<span className="font-mono">{composeURL(s.draft.path, s.draft.params)}</span>}
            onRename={{ name: s.name, set: (name) => rename(s.id, name) }}
            renameNow={renaming === s.id}
            onRenameDone={onRenamed}
            onDelete={() => onRemove(s.id)}
            deleteLabel={`Delete ${s.name}`}
          />
        ))}
      </ul>
      <div className="border-t border-line" />
      <SideHeader
        title="History"
        count={history.length}
        actions={
          history.length > 0 && (
            <ConfirmIconButton icon={Trash2} label="Clear the history" onConfirm={() => historyBox(space, service).set([])} />
          )
        }
      />
      <ul className="min-h-0 flex-1 overflow-y-auto pb-1">
        {history.length === 0 && (
          <li className="px-3 pb-2 text-xs leading-relaxed text-muted">Requests you send show up here.</li>
        )}
        {history.map((h) => (
          <SideItem
            key={h.id}
            onOpen={() => onReopen(h.draft)}
            lead={<MethodLabel method={h.draft.method} />}
            title={<span className="font-mono text-xs">{composeURL(h.draft.path, h.draft.params)}</span>}
            meta={
              <>
                {h.status !== undefined ? (
                  <span className={h.status >= 400 ? 'text-danger' : 'text-ink-soft'}>{h.status}</span>
                ) : (
                  <span className="text-danger">failed</span>
                )}
                {h.durationMs !== undefined && <> · {h.durationMs} ms</>} · {ago(new Date(h.at).toISOString())} ago
                {h.bodyDropped && <> · body not kept</>}
              </>
            }
            onDelete={() => historyBox(space, service).set((list) => list.filter((x) => x.id !== h.id))}
            deleteLabel="Remove from the history"
          />
        ))}
      </ul>
    </Pane>
  )
}

function MethodLabel({ method }: { method: string }) {
  return (
    <span className={`w-11 shrink-0 pt-0.5 font-mono text-[10px] leading-4 font-semibold ${methodTone(method)}`}>
      {method === 'OPTIONS' ? 'OPT' : method === 'DELETE' ? 'DEL' : method}
    </span>
  )
}

// ─── The request ───────────────────────────────────────────────────────

function RequestBar({
  draft,
  base,
  pending,
  open,
  dirty,
  onChange,
  onSend,
  onCancel,
  onSave,
}: {
  draft: Draft
  base: string
  pending: boolean
  open?: Saved
  dirty: boolean
  onChange: (draft: Draft) => void
  onSend: () => void
  onCancel: () => void
  onSave: (asNew?: boolean) => void
}) {
  // While the URL is being typed it is shown as typed; the parameters table
  // follows every keystroke, and on leaving the field the URL is shown as
  // the table composes it.
  const [typed, setTyped] = useState<string | null>(null)
  const composed = composeURL(draft.path, draft.params)
  const shown = typed ?? composed
  const prefix = base.replace(/\/$/, '')

  return (
    <form
      className="flex shrink-0 items-center gap-2"
      onSubmit={(e) => {
        e.preventDefault()
        onSend()
      }}
    >
      <Select
        ariaLabel="Method"
        value={draft.method}
        onChange={(method) => onChange({ ...draft, method })}
        options={methods.map((m) => ({ value: m, label: m }))}
        className={`w-28 font-semibold ${methodTone(draft.method)}`}
      />
      <div className="flex h-8 min-w-0 flex-1 items-center rounded-md border border-line bg-surface font-mono text-sm focus-within:outline-2 focus-within:outline-accent-strong">
        {prefix && (
          // Cut from the left: the service's own part of the base is the end.
          <span dir="rtl" title={base} className="max-w-[45%] shrink-0 truncate pl-3 text-muted select-none">
            <span dir="ltr">{prefix}</span>
          </span>
        )}
        <input
          aria-label="Path and query"
          value={shown}
          placeholder="/items?limit=10"
          spellCheck={false}
          autoCapitalize="none"
          autoComplete="off"
          onFocus={() => setTyped(composed)}
          onBlur={() => setTyped(null)}
          onChange={(e) => {
            setTyped(e.target.value)
            onChange({ ...draft, ...parseURL(e.target.value, base, draft.params) })
          }}
          className={`h-full min-w-0 flex-1 bg-transparent pr-3 text-ink outline-none placeholder:text-muted/60 ${prefix ? 'pl-0' : 'pl-3'}`}
        />
      </div>
      {pending ? (
        <Button kind="quiet" icon={X} onClick={onCancel} title="Cancel the request (Esc)">
          Cancel
        </Button>
      ) : (
        <Button type="submit" icon={Send} title="Send the request (⌘↵ / Ctrl+↵)">
          Send
        </Button>
      )}
      <Button
        kind="quiet"
        icon={Save}
        onClick={() => onSave()}
        disabled={Boolean(open) && !dirty}
        title={open ? (dirty ? `Save the changes to ${open.name}` : 'No changes to save') : 'Save this request under a name'}
      >
        {open && !dirty ? 'Saved' : 'Save'}
      </Button>
      {open && <IconButton icon={CopyPlus} label="Save as a new request" onClick={() => onSave(true)} />}
      <CopyAs
        icon={SquareTerminal}
        text={curlFor(draft, base)}
        label="Copy as curl — runs from inside the space, where the service URL resolves"
      />
    </form>
  )
}
