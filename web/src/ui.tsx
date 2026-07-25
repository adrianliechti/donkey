// Small shared pieces so every view speaks the same visual language.
import type { ReactNode } from 'react'
import { CircleAlert, CircleCheck, Loader2 } from 'lucide-react'

export function StatusDot({ healthy }: { healthy: boolean }) {
  return (
    <span
      className={`inline-block size-2.5 rounded-full ${healthy ? 'bg-moss' : 'bg-brick'}`}
      aria-hidden
    />
  )
}

export function CheckIcon({ ok }: { ok: boolean }) {
  return ok ? (
    <CircleCheck className="size-4 shrink-0 text-moss" aria-label="ok" />
  ) : (
    <CircleAlert className="size-4 shrink-0 text-brick" aria-label="problem" />
  )
}

export function Spinner({ label }: { label: string }) {
  return (
    <div className="flex items-center gap-2 py-10 justify-center text-stone text-sm">
      <Loader2 className="size-4 animate-spin" aria-hidden /> {label}
    </div>
  )
}

export function Card({ children, className = '' }: { children: ReactNode; className?: string }) {
  return (
    <div className={`rounded-lg border border-line bg-card ${className}`}>{children}</div>
  )
}

export function SectionTitle({ children }: { children: ReactNode }) {
  return (
    <h2 className="text-xs font-semibold uppercase tracking-wider text-stone">{children}</h2>
  )
}

export function Button({
  children,
  onClick,
  kind = 'primary',
  disabled = false,
  type = 'button',
}: {
  children: ReactNode
  onClick?: () => void
  kind?: 'primary' | 'quiet' | 'danger'
  disabled?: boolean
  type?: 'button' | 'submit'
}) {
  const styles = {
    primary:
      'bg-hay text-ink hover:bg-hay-deep hover:text-white border border-transparent font-semibold',
    quiet: 'bg-transparent text-ink-soft hover:bg-line/50 border border-line',
    danger: 'bg-transparent text-brick hover:bg-brick hover:text-white border border-brick/40',
  }[kind]
  return (
    <button
      type={type}
      onClick={onClick}
      disabled={disabled}
      className={`rounded-md px-3 py-1.5 text-sm transition-colors disabled:opacity-40 disabled:pointer-events-none focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-hay-deep ${styles}`}
    >
      {children}
    </button>
  )
}

export function ErrorNote({ message }: { message: string }) {
  if (!message) return null
  return (
    <p className="rounded-md border border-brick/30 bg-brick/5 px-3 py-2 text-sm text-brick">
      {message}
    </p>
  )
}
