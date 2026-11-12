import { Flame, Hourglass, type LucideIcon } from 'lucide-react'
import type { AppInsight, AppSummary } from './api'

// Insights are what donkey has noticed about an app, good or bad: worked out
// on the server, never set by anyone. The server sends each one's kind and
// the facts behind it; this registry is the one place that says how an
// insight looks and reads, so a new one is one entry here. Kinds this
// console does not know yet are left out rather than shown half-dressed.
export type InsightKind = 'unmaintained' | 'popular'

export interface InsightLook {
  icon: LucideIcon
  title: string
  /** Good news wears the success colour; a nudge stays quiet. */
  tone: 'neutral' | 'success'
  /** Why the app has it, in a sentence. */
  describe: (insight: AppInsight) => string
  /** What would clear it, for people who can act on the app. */
  remedy?: string
  /** Worth finding across spaces: counted on each space's row, and a filter. */
  fleet?: true
}

const date = (iso: string) => new Intl.DateTimeFormat(undefined, { dateStyle: 'medium' }).format(new Date(iso))

export const insightLooks: Record<InsightKind, InsightLook> = {
  unmaintained: {
    icon: Hourglass,
    title: 'Unmaintained',
    tone: 'neutral',
    describe: (insight) =>
      insight.since
        ? `Its running build is from ${date(insight.since)}, older than this platform allows.`
        : 'Its running build is older than this platform allows.',
    remedy: 'Deploy it again to pick up fixed dependencies and a fresh base image.',
    fleet: true,
  },
  popular: {
    icon: Flame,
    title: 'Popular',
    tone: 'success',
    describe: (insight) =>
      `About ${insight.people} people use it on an average day, which makes it one of the most used apps here.`,
  },
}

export const fleetInsights = (Object.keys(insightLooks) as InsightKind[]).filter((kind) => insightLooks[kind].fleet)

/** The app's insights this console knows how to show. */
export function appInsights(app: AppSummary): (AppInsight & { kind: InsightKind })[] {
  return (app.insights ?? []).filter(
    (insight): insight is AppInsight & { kind: InsightKind } => insight.kind in insightLooks,
  )
}
