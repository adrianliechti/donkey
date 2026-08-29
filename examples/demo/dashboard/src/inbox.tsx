import { useEffect, useRef, useState } from 'react'
import {
  ArrowLeft, Check, CircleDot, Inbox as InboxIcon, Paperclip, Plug, RotateCcw, Search, Send, Sparkles, Upload,
} from 'lucide-react'
import { ago, bytes, call, post, stream } from './api'
import type { Ticket, TicketDetail } from './api'
import { button, Hint, primaryButton, TicketChips } from './ui'

// The queue on the left, one ticket on the right — the shape every support
// tool converges on, because it is the shape of the work.

export function Inbox({ focus, refresh }: { focus: number | null; refresh: number }) {
  const [tickets, setTickets] = useState<Ticket[]>([])
  const [hint, setHint] = useState('')
  const [error, setError] = useState('')
  const [tab, setTab] = useState<'open' | 'closed'>('open')
  const [q, setQ] = useState('')
  const [selected, setSelected] = useState<number | null>(null)

  async function load() {
    try {
      const r = await call<Ticket[]>('/api/tickets')
      setHint(r.hint ?? '')
      if (!r.hint) setTickets(r.data ?? [])
      setError('')
    } catch (err) {
      setError(err instanceof Error ? err.message : 'The backend did not answer.')
    }
  }

  useEffect(() => {
    load()
  }, [refresh])

  // A simulated walk-in lands selected, so the whole story stays on screen.
  useEffect(() => {
    if (focus !== null) {
      setTab('open')
      setSelected(focus)
    }
  }, [focus, refresh])

  const patch = (t: Ticket) => setTickets((list) => list.map((x) => (x.id === t.id ? { ...x, ...t } : x)))

  const needle = q.trim().toLowerCase()
  const shown = tickets.filter(
    (t) =>
      t.status === tab &&
      (!needle || t.subject.toLowerCase().includes(needle) || t.customer.toLowerCase().includes(needle)),
  )
  const counts = {
    open: tickets.filter((t) => t.status === 'open').length,
    closed: tickets.filter((t) => t.status === 'closed').length,
  }

  if (hint) {
    return (
      <div className="mx-auto max-w-xl px-5 py-10">
        <Hint text={hint} />
      </div>
    )
  }

  return (
    <div className="mx-auto flex h-[calc(100dvh-3.5rem)] max-w-7xl">
      {/* The queue. On small screens it yields to the open ticket. */}
      <div
        className={`w-full shrink-0 flex-col border-line md:flex md:w-90 md:border-r ${selected ? 'hidden' : 'flex'}`}
      >
        <div className="space-y-2.5 border-b border-line px-4 py-3">
          {error && (
            <p role="alert" className="rounded-md bg-critical/10 px-3 py-2 text-sm">
              {error}
            </p>
          )}
          <div className="relative">
            <Search className="pointer-events-none absolute left-2.5 top-2 size-4 text-muted" aria-hidden />
            <input
              value={q}
              onChange={(e) => setQ(e.target.value)}
              placeholder="Search subject or customer…"
              aria-label="Search tickets"
              className="w-full rounded-md border border-line bg-card py-1.5 pl-8 pr-3 text-sm placeholder:text-muted focus-visible:outline-2 focus-visible:outline-offset-1 focus-visible:outline-accent"
            />
          </div>
          <div className="flex gap-1" role="tablist" aria-label="Ticket status">
            {(['open', 'closed'] as const).map((s) => (
              <button
                key={s}
                role="tab"
                aria-selected={tab === s}
                onClick={() => setTab(s)}
                className={`rounded-md px-2.5 py-1 text-sm capitalize transition-colors ${
                  tab === s ? 'bg-accent-soft font-medium text-ink' : 'text-muted hover:text-ink-soft'
                }`}
              >
                {s} {counts[s] > 0 && <span className="text-xs text-muted">{counts[s]}</span>}
              </button>
            ))}
          </div>
        </div>
        <ul className="flex-1 overflow-y-auto">
          {shown.map((t) => (
            <li key={t.id}>
              <button
                onClick={() => setSelected(t.id)}
                aria-current={selected === t.id}
                className={`w-full border-b border-line/60 px-4 py-3 text-left transition-colors hover:bg-line/30 ${
                  selected === t.id ? 'bg-accent-soft/60 shadow-[inset_2px_0_0_var(--color-accent)]' : ''
                }`}
              >
                <div className="flex items-baseline justify-between gap-2">
                  <span className="truncate text-sm font-medium">{t.subject}</span>
                  <span className="shrink-0 text-xs text-muted">{ago(t.last_activity ?? t.created)}</span>
                </div>
                <div className="mt-0.5 text-xs text-muted">{t.customer}</div>
                <div className="mt-1.5 flex flex-wrap gap-1">
                  <TicketChips t={t} />
                </div>
              </button>
            </li>
          ))}
          {shown.length === 0 && (
            <li className="px-4 py-8 text-center text-sm text-muted">
              {needle ? 'Nothing matches that search.' : `No ${tab} tickets.`}
            </li>
          )}
        </ul>
      </div>

      <div className={`min-w-0 flex-1 flex-col md:flex ${selected ? 'flex' : 'hidden'}`}>
        {selected === null ? (
          <div className="m-auto max-w-sm px-6 text-center">
            <InboxIcon className="mx-auto size-8 text-muted" aria-hidden />
            <p className="mt-3 text-sm text-ink-soft">Pick a ticket from the queue.</p>
            <p className="mt-2 text-xs text-muted">
              Or work it from an assistant: the backend speaks MCP at{' '}
              <code className="font-mono">/mcp</code> — the same queue, as tools.
            </p>
          </div>
        ) : (
          <TicketPane id={selected} onBack={() => setSelected(null)} onChange={patch} />
        )}
      </div>
    </div>
  )
}

function TicketPane({ id, onBack, onChange }: { id: number; onBack: () => void; onChange: (t: Ticket) => void }) {
  const [detail, setDetail] = useState<TicketDetail | null>(null)
  const [error, setError] = useState('')
  const [aiHint, setAiHint] = useState('')
  const [triaging, setTriaging] = useState(false)
  const [draft, setDraft] = useState('')
  const [drafting, setDrafting] = useState(false)
  const [sending, setSending] = useState(false)
  const [uploading, setUploading] = useState(false)
  const threadEnd = useRef<HTMLDivElement>(null)

  async function load() {
    try {
      const r = await call<TicketDetail>(`/api/tickets/${id}`)
      if (r.data) setDetail(r.data)
      setError('')
    } catch (err) {
      setError(err instanceof Error ? err.message : 'The backend did not answer.')
    }
  }

  useEffect(() => {
    setDetail(null)
    setDraft('')
    setAiHint('')
    load()
  }, [id])

  async function triage() {
    setTriaging(true)
    try {
      const r = await post<Ticket>(`/api/tickets/${id}/triage`)
      setAiHint(r.hint ?? '')
      if (r.data) {
        setDetail((d) => (d ? { ...d, ticket: { ...d.ticket, ...r.data } } : d))
        onChange(r.data)
      }
    } catch (err) {
      setAiHint(err instanceof Error ? err.message : 'Triage did not answer — try again.')
    } finally {
      setTriaging(false)
    }
  }

  async function draftWithAI() {
    setDrafting(true)
    setDraft('')
    setAiHint('')
    try {
      const r = await stream(`/api/tickets/${id}/draft`, (chunk) => setDraft((d) => d + chunk))
      setAiHint(r.hint ?? '')
    } catch {
      setAiHint('The draft did not arrive — try again.')
    } finally {
      setDrafting(false)
    }
  }

  async function send(close: boolean) {
    const body = draft.trim()
    if (!body) return
    setSending(true)
    try {
      const r = await post(`/api/tickets/${id}/messages`, { body, close })
      if (r.hint) return setAiHint(r.hint)
      setDraft('')
      await load()
      if (detail) onChange({ ...detail.ticket, status: close ? 'closed' : detail.ticket.status })
      threadEnd.current?.scrollIntoView({ behavior: 'smooth' })
    } finally {
      setSending(false)
    }
  }

  async function setStatus(status: 'open' | 'closed') {
    const r = await call<Ticket>(`/api/tickets/${id}`, {
      method: 'PATCH',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ status }),
    })
    if (r.hint) return setAiHint(r.hint)
    setDetail((d) => (d ? { ...d, ticket: { ...d.ticket, status } } : d))
    if (detail) onChange({ ...detail.ticket, status })
  }

  async function upload(list: FileList | null) {
    const file = list?.[0]
    if (!file) return
    setUploading(true)
    try {
      const body = new FormData()
      body.append('file', file)
      const r = await call(`/api/tickets/${id}/attachments`, { method: 'POST', body })
      if (!r.hint) await load()
    } finally {
      setUploading(false)
    }
  }

  if (error) {
    return (
      <p role="alert" className="m-6 rounded-md bg-critical/10 px-3 py-2 text-sm">
        {error}
      </p>
    )
  }
  if (!detail) return <p className="m-6 text-sm text-muted">Opening the ticket…</p>

  const { ticket, messages, attachments, attachments_hint } = detail
  const untriaged = !ticket.category && !ticket.priority

  return (
    <>
      <div className="border-b border-line px-5 py-3.5">
        <div className="flex flex-wrap items-center gap-2">
          <button onClick={onBack} aria-label="Back to the queue" className={`md:hidden ${button} px-2`}>
            <ArrowLeft className="size-4" aria-hidden />
          </button>
          <h2 className="min-w-0 flex-1 truncate text-base font-semibold tracking-tight">{ticket.subject}</h2>
          {untriaged && (
            <button onClick={triage} disabled={triaging} className={button}>
              <Sparkles className={`size-3.5 text-accent ${triaging ? 'animate-pulse' : ''}`} aria-hidden />
              {triaging ? 'Reading the ticket…' : 'Triage with AI'}
            </button>
          )}
          {ticket.status === 'open' ? (
            <button onClick={() => setStatus('closed')} className={button}>
              <Check className="size-3.5" aria-hidden /> Close
            </button>
          ) : (
            <button onClick={() => setStatus('open')} className={button}>
              <RotateCcw className="size-3.5" aria-hidden /> Reopen
            </button>
          )}
        </div>
        <div className="mt-1.5 flex flex-wrap items-center gap-x-2 gap-y-1 text-xs text-muted">
          <span className="font-medium text-ink-soft">{ticket.customer}</span>
          <span>· #{ticket.id}</span>
          <span>· opened {ago(ticket.created)} ago</span>
          <span
            className={`inline-flex items-center gap-1 rounded-full px-2 py-0.5 ${
              ticket.status === 'open' ? 'bg-accent-soft text-ink-soft' : 'border border-line'
            }`}
          >
            {ticket.status === 'open' ? (
              <CircleDot className="size-3 text-accent" aria-hidden />
            ) : (
              <Check className="size-3" aria-hidden />
            )}
            {ticket.status}
          </span>
          <TicketChips t={ticket} />
        </div>
        {ticket.summary && (
          <p className="mt-2.5 flex items-start gap-2 rounded-lg bg-accent-soft px-3 py-2 text-sm">
            <Sparkles className="mt-0.5 size-3.5 shrink-0 text-accent" aria-hidden />
            {ticket.summary}
          </p>
        )}
        {aiHint && <p className="mt-2 flex items-center gap-1.5 text-sm text-muted"><Plug className="size-4 shrink-0" aria-hidden />{aiHint}</p>}
      </div>

      <div className="flex-1 space-y-3 overflow-y-auto px-5 py-4">
        {messages.map((m) => (
          <div
            key={m.id}
            className={`max-w-2xl rounded-xl border border-line px-4 py-3 ${
              m.author === 'agent' ? 'ml-auto bg-accent-soft/50' : 'bg-card'
            }`}
          >
            <div className="mb-1 flex items-baseline justify-between gap-3 text-xs text-muted">
              <span className="font-medium text-ink-soft">
                {m.author === 'agent' ? 'Packmule support' : ticket.customer}
              </span>
              <span>{ago(m.created)} ago</span>
            </div>
            <p className="whitespace-pre-wrap text-sm leading-relaxed">{m.body}</p>
          </div>
        ))}
        <div ref={threadEnd} />
      </div>

      <div className="border-t border-line px-5 py-3">
        <div className="mb-2 flex flex-wrap items-center gap-1.5 text-xs text-muted">
          <Paperclip className="size-3.5" aria-hidden />
          {attachments_hint && <span>{attachments_hint}</span>}
          {attachments?.map((a) => (
            <a
              key={a.name}
              href={`/api/tickets/${ticket.id}/attachments/${encodeURIComponent(a.name)}`}
              download
              className="inline-flex items-center gap-1 rounded-full border border-line px-2 py-0.5 text-ink-soft hover:border-accent hover:text-accent"
            >
              {a.name} <span className="text-muted">{bytes(a.size)}</span>
            </a>
          ))}
          {attachments && attachments.length === 0 && <span>no attachments</span>}
          {!attachments_hint && (
            <label className="inline-flex cursor-pointer items-center gap-1 rounded-full border border-dashed border-line px-2 py-0.5 hover:text-ink-soft">
              <Upload className={`size-3 ${uploading ? 'animate-pulse' : ''}`} aria-hidden />
              {uploading ? 'uploading…' : 'add a file'}
              <input
                type="file"
                className="sr-only"
                onChange={(e) => {
                  upload(e.target.files)
                  e.target.value = ''
                }}
              />
            </label>
          )}
        </div>
        <textarea
          value={draft}
          onChange={(e) => setDraft(e.target.value)}
          rows={3}
          placeholder={`Reply to ${ticket.customer.split(' ')[0]}…`}
          aria-label="Reply"
          className="w-full resize-y rounded-lg border border-line bg-card px-3 py-2 text-sm leading-relaxed placeholder:text-muted focus-visible:outline-2 focus-visible:outline-offset-1 focus-visible:outline-accent"
        />
        <div className="mt-2 flex flex-wrap items-center gap-2">
          <button onClick={draftWithAI} disabled={drafting} className={button}>
            <Sparkles className={`size-3.5 text-accent ${drafting ? 'animate-pulse' : ''}`} aria-hidden />
            {drafting ? 'Writing…' : 'Draft with AI'}
          </button>
          <span className="flex-1" />
          <button onClick={() => send(true)} disabled={sending || !draft.trim()} className={button}>
            Reply & close
          </button>
          <button onClick={() => send(false)} disabled={sending || !draft.trim()} className={primaryButton}>
            <Send className="size-3.5" aria-hidden /> Reply
          </button>
        </div>
      </div>
    </>
  )
}
