// The console's shared words for an app: how healthy it is and what kind of
// traffic it takes — said the same way wherever they are shown.
import { Globe, KeyRound, type LucideIcon } from 'lucide-react'
import type { AppMode } from './api'

export type Health = 'ok' | 'bad' | 'unknown'

export function healthOf(healthy: boolean, ready: string): Health {
  if (ready === 'status unavailable') return 'unknown'
  return healthy ? 'ok' : 'bad'
}

// One wording for an app's state, wherever it is shown.
export const healthLine: Record<Health, (ready: string) => string> = {
  ok: (ready) => `running (${ready})`,
  bad: (ready) => `not fully up (${ready})`,
  unknown: () => 'status unknown — checking again',
}

// APP_MODES names the kind of traffic an app takes. The choice only bites
// behind the login: a browser is sent to the sign-in page, an API client is
// asked for an OAuth access token. Public and internal apps skip both.
export const APP_MODES: { value: AppMode; icon: LucideIcon; label: string; hint: string }[] = [
  {
    value: 'web',
    icon: Globe,
    label: 'Web app',
    hint: 'Pages people open in a browser. Behind the login, visitors sign in through your identity provider.',
  },
  {
    value: 'api',
    icon: KeyRound,
    label: 'API',
    hint: 'REST APIs, MCP servers, webhook receivers. Behind the login, clients present an OAuth access token instead of signing in.',
  },
]

// loginHint spells out what "behind the login" means for the given mode —
// the one exposure whose meaning depends on it.
export function loginHint(mode: AppMode): string {
  return mode === 'api'
    ? 'Clients present an OAuth access token from your identity provider; MCP clients find the sign-in by discovery.'
    : 'Visitors sign in through your identity provider.'
}
