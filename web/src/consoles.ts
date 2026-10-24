import type { ComponentType } from 'react'
import {
  Bot,
  Boxes,
  Cable,
  FolderTree,
  KeyRound,
  ListTree,
  Search,
  Send,
  SlidersHorizontal,
  SquareTerminal,
  Table2,
  type LucideIcon,
} from 'lucide-react'
import { serviceUI } from './serviceProtocol'

// A service's page is a few tabs of its console, then Connection and Settings
// (see ServiceDetail). Which console a service gets follows its protocol —
// serviceUI(protocol).console.
export type ConsoleKind = 'database' | 'trino' | 'redis' | 's3' | 'openai' | 'http' | 'exa'

/** What every console receives. `tab` is one of its own tab ids. */
export type ConsoleProps = { space: string; service: string; protocol?: string; tab: string }

export type ConsoleTab = { id: string; label: string; icon: LucideIcon }

// The consoles are their own chunks (the AI chat alone brings a markdown
// renderer). They are fetched ahead of time — when the switcher lists a
// space's services, and again on hover — so a page opens on the console
// rather than on a spinner.
export const consoleModules: Record<ConsoleKind, () => Promise<{ Console: ComponentType<ConsoleProps> }>> = {
  database: () => import('./DatabaseConsole'),
  trino: () => import('./TrinoConsole'),
  redis: () => import('./RedisConsole'),
  s3: () => import('./S3Console'),
  openai: () => import('./AiConsole'),
  http: () => import('./HttpConsole'),
  exa: () => import('./ExaConsole'),
}

// The console's own tabs, known before its chunk is: the tab row never waits.
// The first one is where the page opens.
export const consoleTabs: Record<ConsoleKind, ConsoleTab[]> = {
  database: [
    { id: 'data', label: 'Data', icon: Table2 },
    { id: 'query', label: 'Query', icon: SquareTerminal },
  ],
  trino: [
    { id: 'data', label: 'Catalogs', icon: ListTree },
    { id: 'query', label: 'Query', icon: SquareTerminal },
  ],
  redis: [{ id: 'keys', label: 'Keys', icon: KeyRound }],
  s3: [{ id: 'files', label: 'Files', icon: FolderTree }],
  openai: [
    { id: 'chat', label: 'Chat', icon: Bot },
    { id: 'models', label: 'Models', icon: Boxes },
  ],
  http: [{ id: 'requests', label: 'Requests', icon: Send }],
  exa: [{ id: 'search', label: 'Search', icon: Search }],
}

// Every service page ends with these two, after its console's own tabs.
const commonTabs: ConsoleTab[] = [
  { id: 'connection', label: 'Connection', icon: Cable },
  { id: 'settings', label: 'Settings', icon: SlidersHorizontal },
]

/** The tabs a service's page shows, its console's first. */
export function serviceTabs(protocol?: string): ConsoleTab[] {
  const kind = serviceUI(protocol).console
  return [...(kind ? consoleTabs[kind] : []), ...commonTabs]
}

export const preloadConsole = (kind?: ConsoleKind) => {
  if (kind) void consoleModules[kind]()
}
