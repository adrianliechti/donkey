import { CircleAlert, Info, TriangleAlert, Wrench } from 'lucide-react'
import type { Finding } from './api'

const appearance: Record<
  Finding['level'],
  { label: string; border: string; badge: string; icon: React.ReactNode }
> = {
  block: {
    label: 'Blocker',
    border: 'border-danger/35 bg-danger/5',
    badge: 'border-danger/30 bg-danger/10 text-danger',
    icon: <CircleAlert className="size-4 text-danger" aria-hidden />,
  },
  warn: {
    label: 'Warning',
    border: 'border-accent/30 bg-accent/5',
    badge: 'border-accent/30 bg-accent/10 text-accent',
    icon: <TriangleAlert className="size-4 text-accent" aria-hidden />,
  },
  info: {
    label: 'Note',
    border: 'border-line bg-surface/45',
    badge: 'border-line bg-raised text-muted',
    icon: <Info className="size-4 text-muted" aria-hidden />,
  },
}

// Findings carry separate evidence and remediation from the server. Keeping
// those visually separate makes a real problem scannable instead of turning
// it into one long paragraph in a mixed-severity list.
export function FindingsList({ findings }: { findings: Finding[] }) {
  if (findings.length === 0) return null
  return (
    <ul className="space-y-2">
      {findings.map((finding, index) => {
        const tone = appearance[finding.level]
        return (
          <li
            key={`${finding.code}-${index}`}
            className={`rounded-md border px-3 py-2.5 ${tone.border}`}
          >
            <div className="flex items-start gap-2.5">
              <span className="mt-0.5 shrink-0">{tone.icon}</span>
              <div className="min-w-0 flex-1">
                <div className="flex flex-wrap items-center gap-1.5">
                  <span
                    className={`rounded border px-1.5 py-0.5 font-mono text-[9px] font-semibold uppercase tracking-wider ${tone.badge}`}
                  >
                    {tone.label}
                  </span>
                  {finding.file && (
                    <span className="truncate font-mono text-[10px] text-muted">{finding.file}</span>
                  )}
                </div>
                <p className="mt-1 text-sm text-ink">{finding.title}</p>
                {finding.detail && (
                  <p className="mt-1 text-xs leading-relaxed text-muted">{finding.detail}</p>
                )}
                {finding.fix && (
                  <p className="mt-2 flex items-start gap-1.5 text-xs leading-relaxed text-ink-soft">
                    <Wrench className="mt-0.5 size-3 shrink-0 text-accent" aria-hidden />
                    <span>{finding.fix}</span>
                  </p>
                )}
              </div>
            </div>
          </li>
        )
      })}
    </ul>
  )
}
