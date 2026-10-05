import { CircleAlert, Info, TriangleAlert } from 'lucide-react'
import type { Finding } from './api'
import { Callout, type CalloutTone } from './ui'

const appearance: Record<Finding['level'], { tone: CalloutTone; icon: typeof Info; label: string }> = {
  block: { tone: 'danger', icon: CircleAlert, label: 'Blocker' },
  warn: { tone: 'warning', icon: TriangleAlert, label: 'Warning' },
  info: { tone: 'neutral', icon: Info, label: 'Note' },
}

// Findings carry separate evidence and remediation from the server. Each is
// a tinted callout — the tone says how serious, the body keeps what was seen
// apart from what to do about it.
export function FindingsList({ findings }: { findings: Finding[] }) {
  if (findings.length === 0) return null
  return (
    <ul className="space-y-2">
      {findings.map((finding, index) => {
        const look = appearance[finding.level]
        return (
          <li key={`${finding.code}-${index}`}>
            <Callout tone={look.tone} icon={look.icon} title={<span title={look.label}>{finding.title}</span>}>
              {finding.file && <span className="block font-mono text-[11px] text-muted">{finding.file}</span>}
              {finding.detail && <span className="block text-xs leading-relaxed">{finding.detail}</span>}
              {finding.fix && <span className="mt-1 block text-xs leading-relaxed text-ink">{finding.fix}</span>}
            </Callout>
          </li>
        )
      })}
    </ul>
  )
}
