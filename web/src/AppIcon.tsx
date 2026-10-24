import { AppWindow } from 'lucide-react'
import {
  siBun,
  siDeno,
  siDotnet,
  siElixir,
  siGo,
  siHtml5,
  siNodedotjs,
  siOpenjdk,
  siPhp,
  siPython,
  siRuby,
  siRust,
  type SimpleIcon,
} from 'simple-icons'
import type { Health } from './vocabulary'

// The stacks the server reads off an app's build recipe (detect.Stack), and
// their logos. Anything else gets the plain window.
const logos: Record<string, SimpleIcon> = {
  python: siPython,
  node: siNodedotjs,
  bun: siBun,
  deno: siDeno,
  go: siGo,
  java: siOpenjdk,
  ruby: siRuby,
  php: siPhp,
  rust: siRust,
  dotnet: siDotnet,
  elixir: siElixir,
  static: siHtml5,
}

const tone: Record<Health, string> = {
  ok: 'text-muted',
  bad: 'text-danger',
  unknown: 'text-warning',
}

/**
 * An app's mark wherever it is listed: the logo of what it is written in, or
 * a plain window when its recipe does not say. It stays quiet while the app
 * runs and only takes a color when something is off — red when it is down,
 * amber when the cluster cannot say.
 */
export function AppIcon({ health, stack, className = 'size-4' }: { health: Health; stack?: string; className?: string }) {
  const logo = stack ? logos[stack] : undefined
  const classes = `shrink-0 ${tone[health]} ${className}`
  if (!logo) return <AppWindow className={classes} aria-hidden />
  // Logos fill their whole box; the margin gives them lucide's optical size.
  return (
    <svg viewBox="-2 -2 28 28" fill="currentColor" className={classes} aria-hidden>
      <title>{logo.title}</title>
      <path d={logo.path} />
    </svg>
  )
}
