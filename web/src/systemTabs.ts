import { Activity, Layers, Server, ShieldAlert, type LucideIcon } from 'lucide-react'

// The platform's own page, for whoever runs it: is donkey itself well
// (System), how full are the machines under it (Nodes), which findings are
// in which apps and who answers for them (Vulnerabilities), and what the
// apps are built with (Technologies). Each tab is one question, and each
// asks the server only while it is open.
export type SystemTab = 'system' | 'nodes' | 'vulnerabilities' | 'technologies'

export const SYSTEM_TABS: { id: SystemTab; label: string; icon: LucideIcon }[] = [
  { id: 'system', label: 'System', icon: Activity },
  { id: 'nodes', label: 'Nodes', icon: Server },
  { id: 'vulnerabilities', label: 'Vulnerabilities', icon: ShieldAlert },
  { id: 'technologies', label: 'Technologies', icon: Layers },
]
