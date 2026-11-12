import { Archive, Database, Globe, HardDrive, KeyRound, Server, Sparkles, Search, type LucideIcon } from 'lucide-react'

type ServiceUI = {
  Icon: LucideIcon
  console?: 'database' | 'trino' | 'redis' | 'store' | 'openai' | 'http' | 'exa'
  noun: string
  // A store that is one tree, as a WebDAV service is, has no containers to
  // pick from: its browser opens on the files.
  fixedRoot?: boolean
}

// Provider names are deliberately absent: compatible plugins and external
// services share the same presentation and console.
const protocols: Record<string, ServiceUI> = {
  postgres: { Icon: Database, console: 'database', noun: 'database' },
  mysql: { Icon: Database, console: 'database', noun: 'database' },
  trino: { Icon: Database, console: 'trino', noun: 'catalogs' },
  redis: { Icon: KeyRound, console: 'redis', noun: 'keys' },
  s3: { Icon: Archive, console: 'store', noun: 'buckets' },
  webdav: { Icon: HardDrive, console: 'store', noun: 'files', fixedRoot: true },
  openai: { Icon: Sparkles, console: 'openai', noun: 'chat' },
  http: { Icon: Globe, console: 'http', noun: 'requests' },
  exa: { Icon: Search, console: 'exa', noun: 'search' },
}
const unknown: ServiceUI = { Icon: Server, noun: 'service' }

export function serviceUI(protocol?: string): ServiceUI {
  if (protocol === 'postgresql') protocol = 'postgres'
  if (protocol === 'mariadb') protocol = 'mysql'
  return protocol && Object.hasOwn(protocols, protocol) ? protocols[protocol] : unknown
}
