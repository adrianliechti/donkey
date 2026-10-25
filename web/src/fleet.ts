import type { ActiveDeploy, AppSummary } from './api'
import { healthOf, type StatusTone } from './vocabulary.ts'

export type SpaceFilter = 'all' | 'pinned' | 'attention'

export interface SpaceIssue {
  name: string
  app?: AppSummary
  deploy?: ActiveDeploy
}

export interface SpaceSummary {
  name: string
  apps: AppSummary[]
  deploys: ActiveDeploy[]
  issues: SpaceIssue[]
  running: number
  down: number
  unknown: number
}

export const isDeploying = (deploy: ActiveDeploy) =>
  !deploy.finishedAt && deploy.stage !== 'done' && deploy.stage !== 'failed'

export const deployTime = (deploy: ActiveDeploy) => deploy.finishedAt || deploy.startedAt

// Active work stays above recent completions, then newest first. The board
// only retains finished deploys for 15 minutes; this is not a history query.
export function recentDeploys(deploys: ActiveDeploy[]): ActiveDeploy[] {
  return [...deploys].sort(
    (a, b) => Number(isDeploying(b)) - Number(isDeploying(a)) || Date.parse(deployTime(b)) - Date.parse(deployTime(a)),
  )
}

export function summarizeSpaces(names: string[], apps: AppSummary[], deploys: ActiveDeploy[]): SpaceSummary[] {
  const spaces = new Map<string, SpaceSummary>(
    names.map((name) => [name, { name, apps: [], deploys: [], issues: [], running: 0, down: 0, unknown: 0 }]),
  )
  for (const app of apps) spaces.get(app.space || 'default')?.apps.push(app)
  for (const deploy of recentDeploys(deploys)) spaces.get(deploy.space || 'default')?.deploys.push(deploy)

  for (const space of spaces.values()) {
    const issues = new Map<string, SpaceIssue>()
    space.apps.sort((a, b) => a.name.localeCompare(b.name))
    for (const app of space.apps) {
      const health = healthOf(app.healthy, app.ready)
      if (health === 'ok') space.running++
      else {
        if (health === 'bad') space.down++
        else space.unknown++
        issues.set(app.name, { name: app.name, app })
      }
    }
    for (const deploy of space.deploys) {
      if (deploy.stage === 'failed') {
        // A failed redeploy still matters when the previous release runs.
        // Count the app once when its health and its deploy both fail.
        issues.set(deploy.app, { ...issues.get(deploy.app), name: deploy.app, deploy })
      }
    }
    space.issues = [...issues.values()]
  }
  return [...spaces.values()]
}

export function filterSpaces(spaces: SpaceSummary[], query: string, filter: SpaceFilter, pinned: Set<string>) {
  const needle = query.trim().toLowerCase()
  return spaces
    .filter(
      (space) =>
        (filter === 'all' || (filter === 'pinned' ? pinned.has(space.name) : space.issues.length > 0)) &&
        (!needle ||
          space.name.toLowerCase().includes(needle) ||
          space.apps.some((app) => app.name.toLowerCase().includes(needle)) ||
          space.deploys.some((deploy) => deploy.app.toLowerCase().includes(needle))),
    )
    .sort((a, b) => Number(pinned.has(b.name)) - Number(pinned.has(a.name)) || a.name.localeCompare(b.name))
}

export interface SpaceState {
  tone: StatusTone
  label: string
  /** Something is under way and nothing is wrong: show a spinner, not a dot. */
  busy: boolean
  /** Worth a word: an exception, or a deploy under way. A healthy space is
   *  only a dot, an empty one says nothing — the label is for screen readers. */
  news: boolean
}

// How a space is doing, the worst news first: apps down, a failed deploy,
// apps the cluster cannot see, a deploy under way — and otherwise quiet.
// The fleet's rows and the space's own page say it alike.
export function spaceState(space: SpaceSummary): SpaceState {
  if (space.down) return { tone: 'danger', label: `${space.down} not fully up`, busy: false, news: true }
  if (space.deploys.some((deploy) => deploy.stage === 'failed'))
    return { tone: 'danger', label: 'Deploy failed', busy: false, news: true }
  if (space.unknown) return { tone: 'warning', label: `${space.unknown} unknown`, busy: false, news: true }
  if (space.deploys.some(isDeploying)) return { tone: 'accent', label: 'Deploying', busy: true, news: true }
  if (space.apps.length) return { tone: 'success', label: 'Healthy', busy: false, news: false }
  return { tone: 'muted', label: 'No apps', busy: false, news: false }
}
