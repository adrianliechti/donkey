// What the platform's reports do with their data before showing it: the
// filters, the grouping by space, the contacts. Pure, so it is tested
// without a browser.
//
// The filters are applied here to what the console already holds, and by
// the server to an export (vulnerabilities.go, technologies.go) — the same
// rules in both, so a file holds what was on screen. Change them together.
import type {
  AccessConfig,
  FindingGroup,
  FindingState,
  NodeResource,
  ReportApp,
  Severity,
  SpaceContacts,
  Technology,
  TechnologyCategory,
} from './api.ts'

export const severities: Severity[] = ['critical', 'high', 'medium', 'low', 'unknown']

export const severityLabel: Record<Severity, string> = {
  critical: 'Critical',
  high: 'High',
  medium: 'Medium',
  low: 'Low',
  unknown: 'Unknown',
}

export type SeverityFilter = 'all' | Severity

export interface FindingFilter {
  severity: SeverityFilter
  query: string
  fixable: boolean
}

const has = (text: string | undefined, needle: string) => !!text && text.toLowerCase().includes(needle)

// A query that names the finding — its id, its title, one of its packages —
// keeps it whole. One that names a space or an app keeps the finding with
// only those apps, since that is the question it asks.
export function filterFindings(findings: FindingGroup[], filter: FindingFilter): FindingGroup[] {
  const needle = filter.query.trim().toLowerCase()
  const kept: FindingGroup[] = []
  for (const finding of findings) {
    if (filter.severity !== 'all' && finding.severity !== filter.severity) continue
    if (filter.fixable && !finding.fixable) continue
    if (
      !needle ||
      has(finding.id, needle) ||
      has(finding.title, needle) ||
      finding.apps.some((app) => (app.packages ?? []).some((pkg) => has(pkg.name, needle)))
    ) {
      kept.push(finding)
      continue
    }
    const apps = finding.apps.filter((app) => has(`${app.space}/${app.app}`, needle))
    if (apps.length > 0) kept.push({ ...finding, apps })
  }
  return kept
}

/** How many findings each severity has, and how many there are in all. */
export function severityCounts(findings: FindingGroup[]): Record<SeverityFilter, number> {
  const counts: Record<SeverityFilter, number> = {
    all: findings.length,
    critical: 0,
    high: 0,
    medium: 0,
    low: 0,
    unknown: 0,
  }
  for (const finding of findings) counts[finding.severity] = (counts[finding.severity] ?? 0) + 1
  return counts
}

export function filterTechnologies(categories: TechnologyCategory[], query: string): TechnologyCategory[] {
  const needle = query.trim().toLowerCase()
  if (!needle) return categories
  return categories.map((category) => ({
    ...category,
    technologies: category.technologies.flatMap((technology): Technology[] => {
      if (has(technology.name, needle)) return [technology]
      const apps = technology.apps.filter((app) => has(`${app.space}/${app.app}`, needle))
      return apps.length > 0 ? [{ ...technology, apps }] : []
    }),
  }))
}

/** Rows that name an app, gathered under their spaces, both in name order. */
export function bySpace<T extends { space: string; app: string }>(rows: T[]): { space: string; rows: T[] }[] {
  const spaces = new Map<string, T[]>()
  for (const row of rows) spaces.set(row.space, [...(spaces.get(row.space) ?? []), row])
  return [...spaces.entries()]
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([space, list]) => ({ space, rows: [...list].sort((a, b) => a.app.localeCompare(b.app)) }))
}

const empty = (access?: AccessConfig) =>
  !access || ((access.users ?? []).length === 0 && (access.groups ?? []).length === 0)

/** Nobody holds a role in the space: its platform's admins answer for it. */
export const unassigned = (contacts?: SpaceContacts) =>
  !contacts || (empty(contacts.admins) && empty(contacts.developers))

// The addresses to write to about some spaces: their admins, or their
// developers where a space has no admin of its own. Subjects (sub:…) and
// groups are not addresses, and are left to be read off the page.
export function contactAddresses(spaces: (SpaceContacts | undefined)[], fallback?: AccessConfig): string[] {
  const addresses = new Set<string>()
  const add = (access?: AccessConfig) => {
    let added = false
    for (const user of access?.users ?? []) {
      if (user.includes('@') && !user.startsWith('sub:')) {
        addresses.add(user)
        added = true
      }
    }
    return added
  }
  for (const contacts of spaces) {
    if (!add(contacts?.admins) && !add(contacts?.developers)) add(fallback)
  }
  return [...addresses].sort((a, b) => a.localeCompare(b))
}

/** The apps of a report, by "space/app". */
export function appIndex(apps: ReportApp[]): Map<string, ReportApp> {
  return new Map(apps.map((app) => [`${app.space}/${app.app}`, app]))
}

// A finding in several apps is as urgent as its most urgent app.
const stateRank: Record<FindingState, number> = {
  blocking: 6,
  warning: 5,
  judged: 4,
  waived: 3,
  fixable: 2,
  unfixed: 1,
}

export function worstState(states: FindingState[]): FindingState | undefined {
  return states.reduce<FindingState | undefined>(
    (worst, state) => (!worst || (stateRank[state] ?? 0) > (stateRank[worst] ?? 0) ? state : worst),
    undefined,
  )
}

/** How full a resource is: the share in use, or reserved, of what there is. */
export function share(part: number | undefined, whole: number): number | undefined {
  if (part === undefined || !(whole > 0)) return undefined
  return Math.max(0, Math.min(1, part / whole))
}

// Past these a meter stops being quiet: a node that full has no room for a
// spike, or for the next app.
export type Fullness = 'ok' | 'high' | 'full'

export function fullness(used: number | undefined): Fullness {
  if (used === undefined) return 'ok'
  return used >= 0.9 ? 'full' : used >= 0.75 ? 'high' : 'ok'
}

/** One resource summed over nodes; a part only when every node reports it. */
export function sumResource(resources: NodeResource[]): NodeResource {
  const sum = (pick: (r: NodeResource) => number | undefined) =>
    resources.length > 0 && resources.every((r) => pick(r) !== undefined)
      ? resources.reduce((total, r) => total + (pick(r) ?? 0), 0)
      : undefined
  return {
    capacity: resources.reduce((total, r) => total + r.capacity, 0),
    used: sum((r) => r.used),
    reserved: sum((r) => r.reserved),
  }
}

/** Cores the way people say them: 0.3, 1.5, 12. */
export function cores(value: number): string {
  if (value >= 10) return String(Math.round(value))
  const rounded = Math.round(value * 10) / 10
  return rounded === 0 && value > 0 ? '<0.1' : String(rounded)
}

/** A share as a percentage: 31%. */
export const percent = (value: number) => `${Math.round(value * 100)}%`
