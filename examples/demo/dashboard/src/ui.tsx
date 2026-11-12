import { Frown, Meh, Plug, Smile, Sparkles } from 'lucide-react'
import type { Priority, Sentiment } from './api'

// The shared vocabulary of the app: buttons, cards, and the chips that carry
// a ticket's triage verdict. Status colors live only here, and always beside
// a label or icon — color never carries a state alone.

export const button =
  'inline-flex items-center gap-1.5 rounded-md border border-line bg-card px-3 py-1.5 text-sm text-ink-soft transition-colors hover:bg-line/40 disabled:opacity-50 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-accent'

export const primaryButton =
  'inline-flex items-center gap-1.5 rounded-md bg-accent px-3 py-1.5 text-sm font-medium text-white transition-opacity hover:opacity-90 disabled:opacity-50 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-accent'

export function Card({ title, children, className = '' }: { title?: string; children: React.ReactNode; className?: string }) {
  return (
    <section className={`rounded-xl border border-line bg-card p-4 ${className}`}>
      {title && <h2 className="mb-3 text-xs font-semibold uppercase tracking-wider text-muted">{title}</h2>}
      {children}
    </section>
  )
}

export function Hint({ text }: { text: string }) {
  return (
    <p className="flex items-center gap-2 rounded-lg border border-dashed border-line px-3 py-2.5 text-sm text-muted">
      <Plug className="size-4 shrink-0" aria-hidden />
      {text}
    </p>
  )
}

const chip =
  'inline-flex items-center gap-1 rounded-full border border-line px-2 py-0.5 text-[11px] leading-4 text-ink-soft'

// Priority is a state, so it wears the status palette — the dot carries the
// color, the text carries the meaning.
const priorityDot: Record<Priority, string> = {
  urgent: 'bg-critical',
  high: 'bg-serious',
  normal: 'bg-muted',
  low: 'bg-line',
}

export function PriorityChip({ value }: { value: Priority }) {
  return (
    <span className={chip}>
      <span className={`size-2 rounded-full ${priorityDot[value]}`} aria-hidden />
      {value}
    </span>
  )
}

const sentimentFace: Record<Sentiment, { icon: typeof Smile; tone: string }> = {
  calm: { icon: Smile, tone: 'text-muted' },
  frustrated: { icon: Meh, tone: 'text-warn' },
  angry: { icon: Frown, tone: 'text-critical' },
}

export function SentimentChip({ value }: { value: Sentiment }) {
  const { icon: Icon, tone } = sentimentFace[value]
  return (
    <span className={chip}>
      <Icon className={`size-3 ${tone}`} aria-hidden />
      {value}
    </span>
  )
}

// Category is identity, not state — a plain chip, no color to misread.
export function CategoryChip({ value }: { value: string }) {
  return <span className={chip}>{value}</span>
}

export function TriagePending() {
  return (
    <span className={`${chip} border-dashed text-muted`}>
      <Sparkles className="size-3" aria-hidden />
      needs triage
    </span>
  )
}

export function TicketChips({
  t,
}: {
  t: { category: string | null; priority: Priority | null; sentiment: Sentiment | null }
}) {
  if (!t.category && !t.priority) return <TriagePending />
  return (
    <>
      {t.priority && <PriorityChip value={t.priority} />}
      {t.category && <CategoryChip value={t.category} />}
      {t.sentiment && <SentimentChip value={t.sentiment} />}
    </>
  )
}

export function Stat({ label, value, detail }: { label: string; value: string; detail?: string }) {
  return (
    <div className="rounded-xl border border-line bg-card p-4">
      <div className="text-xs text-muted">{label}</div>
      {/* Proportional figures on purpose: tabular digits look loose at
          display sizes and nothing needs to align here. */}
      <div className="mt-1 text-2xl font-semibold tracking-tight">{value}</div>
      {detail && <div className="mt-0.5 text-xs text-muted">{detail}</div>}
    </div>
  )
}
