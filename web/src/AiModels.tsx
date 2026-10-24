import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { useNavigate } from '@tanstack/react-router'
import { Boxes, ChevronRight, MessageSquare, RefreshCw, SearchX } from 'lucide-react'
import { draftBox, freshConversation, modelsQuery, type Model } from './aiApi'
import { Pane, PaneEmpty, PaneHeader } from './HttpWorkbench'
import { useServiceBinding } from './queries'
import { useServiceSearch } from './serviceSearch'
import { SourceCode } from './SourceCode'
import { Button, Chip, CopyButton, ErrorNote, IconButton, Input, Spinner } from './ui'
import { errText } from './format'

// The Models tab: what the service lists under /models, as it lists it —
// providers add fields of their own (context length, pricing, modalities),
// so a row opens to show everything it said. "Chat" opens a new conversation
// on that model.

export function ModelsView({ space, service }: { space: string; service: string }) {
  const query = useQuery(modelsQuery(space, service))
  const { data: binding } = useServiceBinding(space, service)
  const bound = binding?.binding.OPENAI_MODEL ?? ''
  const [filter, setFilter] = useState('')
  const [open, setOpen] = useState('')
  const [search] = useServiceSearch()
  const navigate = useNavigate()

  const chatWith = (model: string) => {
    draftBox(space, service).set(freshConversation(model))
    const rest = { ...search }
    delete rest.chat
    navigate({ to: '/service/$space/$name/{-$tab}', params: { space, name: service, tab: 'chat' }, search: rest, resetScroll: false })
  }

  const models = query.data ?? []
  // The binding's model leads, listed or not: it is the one apps get.
  const all: Model[] = bound && !models.some((m) => m.id === bound) ? [{ id: bound }, ...models] : models
  const needle = filter.trim().toLowerCase()
  const shown = all
    .filter((m) => !needle || m.id.toLowerCase().includes(needle) || String(m.owned_by ?? '').toLowerCase().includes(needle))
    .sort((a, b) => Number(b.id === bound) - Number(a.id === bound) || a.id.localeCompare(b.id))

  return (
    <div className="flex min-h-0 flex-1 flex-col gap-3">
      <div className="flex shrink-0 items-center gap-2">
        <Input
          aria-label="Filter models"
          placeholder="Filter models"
          value={filter}
          onChange={(e) => setFilter(e.target.value)}
          className="w-72"
          spellCheck={false}
        />
        <span className="text-xs text-muted">
          {query.isSuccess && (needle ? `${shown.length} of ${all.length}` : `${all.length} ${all.length === 1 ? 'model' : 'models'}`)}
        </span>
        <IconButton
          icon={RefreshCw}
          label="Ask the service again"
          onClick={() => void query.refetch()}
          disabled={query.isFetching}
          className={query.isFetching ? 'ml-auto [&>svg]:animate-spin' : 'ml-auto'}
        />
      </div>
      {query.isError && (
        <ErrorNote
          message={`${errText(query.error, 'The service did not list its models.')}${bound ? ` Chat still works with ${bound}, the service's own model.` : ''}`}
        />
      )}
      <Pane label="Models" className="flex-1">
        <PaneHeader className="grid grid-cols-[1.5rem_minmax(0,1fr)_10rem_7rem_8.5rem] gap-3 font-mono text-[10px] font-semibold tracking-[0.14em] text-muted uppercase">
          <span />
          <span>Model</span>
          <span>Owner</span>
          <span>Created</span>
          <span />
        </PaneHeader>
        <div className="min-h-0 flex-1 overflow-y-auto">
          {query.isPending && <Spinner label="Asking the service for its models…" />}
          {query.isSuccess && all.length === 0 && (
            <PaneEmpty icon={Boxes} title="No models listed">
              The service answered with an empty list. A model id typed in Chat still works if the service knows it.
            </PaneEmpty>
          )}
          {all.length > 0 && shown.length === 0 && (
            <PaneEmpty icon={SearchX} title="No model matches">
              Nothing listed contains “{filter.trim()}”.
            </PaneEmpty>
          )}
          <ul className="divide-y divide-line/60">
            {shown.map((m) => (
              <ModelRow
                key={m.id}
                model={m}
                bound={m.id === bound}
                open={open === m.id}
                onToggle={() => setOpen(open === m.id ? '' : m.id)}
                onChat={() => chatWith(m.id)}
              />
            ))}
          </ul>
        </div>
      </Pane>
    </div>
  )
}

function ModelRow({
  model,
  bound,
  open,
  onToggle,
  onChat,
}: {
  model: Model
  bound: boolean
  open: boolean
  onToggle: () => void
  onChat: () => void
}) {
  const created = typeof model.created === 'number' && model.created > 0 ? new Date(model.created * 1000) : undefined
  // Only the fields beyond the standard four make the details worth opening.
  const extra = Object.keys(model).some((k) => !['id', 'object', 'created', 'owned_by'].includes(k))
  return (
    <li>
      <div className="group grid min-h-11 grid-cols-[1.5rem_minmax(0,1fr)_10rem_7rem_8.5rem] items-center gap-3 pr-1.5 pl-3 hover:bg-line/30">
        <span>
          {extra && (
            <button
              type="button"
              onClick={onToggle}
              aria-expanded={open}
              aria-label={open ? `Hide what the service says about ${model.id}` : `Show what the service says about ${model.id}`}
              className="flex size-6 items-center justify-center rounded text-muted hover:text-ink focus-visible:outline-2 focus-visible:outline-accent-strong"
            >
              <ChevronRight className={`size-3.5 transition-transform ${open ? 'rotate-90' : ''}`} aria-hidden />
            </button>
          )}
        </span>
        <span className="flex min-w-0 items-center gap-2">
          <span className="truncate font-mono text-sm text-ink" title={model.id}>
            {model.id}
          </span>
          {bound && <Chip title="The model the service's binding names — what apps get as OPENAI_MODEL">default</Chip>}
        </span>
        <span className="truncate text-xs text-ink-soft" title={String(model.owned_by ?? '')}>
          {String(model.owned_by ?? '—')}
        </span>
        <span className="font-mono text-xs text-muted" title={created?.toLocaleString()}>
          {created ? created.toISOString().slice(0, 10) : '—'}
        </span>
        <span className="flex items-center justify-end gap-0.5">
          <CopyButton text={model.id} label={`Copy ${model.id}`} />
          <Button kind="quiet" size="sm" icon={MessageSquare} onClick={onChat}>
            Chat
          </Button>
        </span>
      </div>
      {open && (
        <div className="border-t border-line/60 px-3 py-2">
          <SourceCode path="model.json" text={JSON.stringify(model, null, 2)} />
        </div>
      )}
    </li>
  )
}
