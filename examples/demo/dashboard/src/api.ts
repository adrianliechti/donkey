// The backend's shapes and the three ways this app talks to it: plain GETs,
// calls that may answer 503 while a space service is still missing (a state
// to show, not an error to throw), and one streaming POST for the AI draft.

export type Priority = 'low' | 'normal' | 'high' | 'urgent'
export type Sentiment = 'calm' | 'frustrated' | 'angry'

export interface Ticket {
  id: number
  subject: string
  customer: string
  status: 'open' | 'closed'
  created: string
  closed: string | null
  category: string | null
  priority: Priority | null
  sentiment: Sentiment | null
  summary: string | null
  messages?: number
  last_activity?: string
}

export interface Message {
  id: number
  author: 'customer' | 'agent'
  body: string
  created: string
}

export interface Attachment {
  name: string
  size: number
  modified: string
}

export interface TicketDetail {
  ticket: Ticket
  messages: Message[]
  attachments: Attachment[] | null
  attachments_hint: string | null
}

export interface Stats {
  open: number
  attention: number
  new_this_week: number
  median_resolution_seconds: number | null
  weekly: { week: string; count: number }[]
  categories: { category: string; total: number; open: number }[]
  priorities: Partial<Record<Priority, number>>
}

export async function call<T>(path: string, init?: RequestInit): Promise<{ data?: T; hint?: string }> {
  const res = await fetch(path, init)
  if (res.status === 503) {
    const body = (await res.json().catch(() => null)) as { detail?: string } | null
    return { hint: body?.detail ?? 'This service is not connected yet.' }
  }
  if (!res.ok) {
    const body = (await res.json().catch(() => null)) as { detail?: string } | null
    throw new Error(body?.detail ?? `${path} answered ${res.status}`)
  }
  return { data: (await res.json()) as T }
}

export function post<T>(path: string, body?: unknown): Promise<{ data?: T; hint?: string }> {
  return call<T>(path, {
    method: 'POST',
    ...(body !== undefined && {
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(body),
    }),
  })
}

/** POST and hand every chunk of the answer to the caller as it arrives. */
export async function stream(path: string, onChunk: (text: string) => void): Promise<{ hint?: string }> {
  const res = await fetch(path, { method: 'POST' })
  if (res.status === 503) {
    const body = (await res.json().catch(() => null)) as { detail?: string } | null
    return { hint: body?.detail ?? 'This service is not connected yet.' }
  }
  if (!res.ok || !res.body) throw new Error(`${path} answered ${res.status}`)
  const reader = res.body.getReader()
  const decoder = new TextDecoder()
  for (;;) {
    const { done, value } = await reader.read()
    if (done) break
    onChunk(decoder.decode(value, { stream: true }))
  }
  return {}
}

// --- small formatters the whole app shares ---

export const bytes = (n: number) =>
  n >= 1_048_576 ? `${(n / 1_048_576).toFixed(1)} MB` : n >= 1024 ? `${Math.round(n / 1024)} kB` : `${n} B`

/** "just now", "35 m", "3 h", "6 d", then a date — inbox time, not ISO time. */
export function ago(iso: string): string {
  const ms = Date.now() - new Date(iso).getTime()
  const minutes = Math.round(ms / 60_000)
  if (minutes < 2) return 'just now'
  if (minutes < 60) return `${minutes} m`
  const hours = Math.round(minutes / 60)
  if (hours < 48) return `${hours} h`
  const days = Math.round(hours / 24)
  if (days < 14) return `${days} d`
  return new Date(iso).toLocaleDateString('en-US', { month: 'short', day: 'numeric' })
}

export function duration(seconds: number): string {
  const hours = seconds / 3600
  if (hours < 48) return `${Math.round(hours)} h`
  return `${(hours / 24).toFixed(1)} d`
}
