import { useState, type KeyboardEvent } from 'react'
import { Send, SquareTerminal, X } from 'lucide-react'
import type { ConsoleProps } from './consoles'
import {
  baseURLOf,
  cancelExchange,
  composeURL,
  curlFor,
  draftBox,
  exchange,
  exchangeBox,
  methods,
  methodTone,
  parseURL,
  type Draft,
} from './httpApi'
import { useBox } from './httpStore'
import { CopyAs } from './Workbench'
import { RequestPane } from './HttpRequest'
import { ResponsePane } from './HttpResponse'
import { useServiceBinding } from './queries'
import { Button, ErrorNote, Select } from './ui'

// The Requests tab of an HTTP service: a quick tester for the API behind it.
// One request across the top, what it sends and what came back side by
// side. Nothing is kept: the request lasts as long as the page.

export function Console({ space, service }: ConsoleProps) {
  const draft = useBox(draftBox(space, service))
  const current = useBox(exchangeBox(space, service))
  const { data: binding, error: bindingError } = useServiceBinding(space, service)
  const base = binding ? baseURLOf(binding.binding, binding.type, service) : ''
  const pending = current?.state === 'pending'

  const setDraft = (next: Draft) => draftBox(space, service).set(next)
  const send = () => {
    if (!pending) void exchange(space, service, draft)
  }
  const cancel = () => {
    if (current) cancelExchange(current.id)
  }

  const onKeyDown = (e: KeyboardEvent) => {
    if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') {
      e.preventDefault()
      send()
    } else if (e.key === 'Escape' && pending) {
      cancel()
    }
  }

  return (
    <div className="flex min-h-0 min-w-0 flex-1 flex-col" onKeyDown={onKeyDown}>
      <div className="empty:hidden px-3 pt-3">
        <ErrorNote message={bindingError} />
      </div>
      <RequestBar draft={draft} base={base} pending={pending} onChange={setDraft} onSend={send} onCancel={cancel} />
      <div className="grid min-h-0 flex-1 grid-rows-2 divide-y divide-line xl:grid-cols-2 xl:grid-rows-1 xl:divide-x xl:divide-y-0">
        <RequestPane draft={draft} onChange={setDraft} />
        <ResponsePane service={service} base={base} exchange={current ?? undefined} onCancel={cancel} />
      </div>
    </div>
  )
}

function RequestBar({
  draft,
  base,
  pending,
  onChange,
  onSend,
  onCancel,
}: {
  draft: Draft
  base: string
  pending: boolean
  onChange: (draft: Draft) => void
  onSend: () => void
  onCancel: () => void
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
      className="flex min-h-13 shrink-0 items-center gap-2 border-b border-line px-3 py-2.5"
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
      <CopyAs
        icon={SquareTerminal}
        text={curlFor(draft, base)}
        label="Copy as curl — runs from inside the space, where the service URL resolves"
      />
    </form>
  )
}
