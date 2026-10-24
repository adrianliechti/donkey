// The sizing and focus classes every control shares, so a button, an input
// and an icon button placed in one row line up without per-call fiddling:
// `sm` = 28px, `md` = 32px tall.

export type ControlSize = 'sm' | 'md'

// One height per size, shared by every control that can sit in a row.
export const controlSize: Record<ControlSize, string> = {
  sm: 'h-7 px-2.5 text-xs',
  md: 'h-8 px-3 text-sm',
}

export const iconSize: Record<ControlSize, string> = { sm: 'size-3.5', md: 'size-4' }

export const focusRing =
  'focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-accent-strong'

// The text controls. Every input in the console wears this: mono, on the
// surface colour, the accent ring on focus. Heights follow controlSize so a
// field and a button in one row share a baseline.
export const fieldBase =
  'min-w-0 rounded-md border border-line bg-surface font-mono text-ink placeholder:text-muted/60 focus:outline-2 focus:outline-accent-strong disabled:opacity-50'

export function inputClass(size: ControlSize = 'md', extra = ''): string {
  return `${fieldBase} ${controlSize[size]} ${extra}`
}
