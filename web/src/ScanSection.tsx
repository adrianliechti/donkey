import { ExternalLink, KeyRound, ShieldCheck } from 'lucide-react'
import type { AppScan, Vulnerability } from './api'
import { FindingsList } from './FindingsList'
import { Disclosure, List, ListRow, Section, SectionHeader, StatGrid } from './ui'

// The vulnerability scan of a release, read top to bottom: the numbers by
// severity, what the policy made of them (the same findings the deploy
// said), then every finding the scanner reported, worst first, for whoever
// wants the whole picture. Nothing here is a card: the strip of numbers and
// the list are the section's only outlines.

const order: Vulnerability['severity'][] = ['critical', 'high', 'medium', 'low', 'unknown']

const severityLabel: Record<Vulnerability['severity'], string> = {
  critical: 'Critical',
  high: 'High',
  medium: 'Medium',
  low: 'Low',
  unknown: 'Unknown',
}

// Severity as a small chip: the two that can refuse an image carry a tone,
// the rest stay quiet.
const severityChip: Record<Vulnerability['severity'], string> = {
  critical: 'bg-danger/15 text-danger font-semibold',
  high: 'bg-warning/15 text-warning font-semibold',
  medium: 'bg-raised text-ink-soft',
  low: 'bg-raised text-muted',
  unknown: 'border border-dashed border-line text-muted',
}

function SeverityChip({ severity }: { severity: Vulnerability['severity'] }) {
  return (
    <span
      className={`inline-block w-16 shrink-0 rounded px-1.5 py-0.5 text-center font-mono text-[10px] uppercase tracking-wide ${severityChip[severity] ?? severityChip.unknown}`}
    >
      {severity}
    </span>
  )
}

function shortDate(iso?: string): string {
  if (!iso) return ''
  const when = new Date(iso)
  return Number.isNaN(when.getTime()) ? iso : when.toLocaleDateString()
}

// The findings the deploy said, minus the three that only count: the strip
// of numbers and the list below say those better.
const counted = new Set(['vuln-fixable', 'vuln-unfixed', 'vuln-clean'])

function policyText(scan: AppScan): string {
  const { policy } = scan
  const parts: string[] = []
  parts.push(
    policy.block === 'none'
      ? 'Findings are reported, never refused'
      : `Refuses ${policy.block} findings that have a fix${policy.ignoreUnfixed ? '; unfixed ones inform' : ', fix or not'}`,
  )
  if (policy.secrets !== 'off')
    parts.push(policy.secrets === 'block' ? 'a credential in the image refuses' : 'a credential in the image warns')
  if (policy.triage) parts.push('the assistant may waive what it reads as not affecting the app')
  return parts.join(' · ') + '.'
}

export function ScanSection({ scan }: { scan: AppScan }) {
  const vulnerabilities = [...(scan.vulnerabilities ?? [])].sort(
    (a, b) =>
      order.indexOf(a.severity) - order.indexOf(b.severity) ||
      a.package.localeCompare(b.package) ||
      a.id.localeCompare(b.id),
  )
  const bySeverity = (severity: Vulnerability['severity']) => vulnerabilities.filter((v) => v.severity === severity)
  const stats = (['critical', 'high', 'medium', 'low'] as const).map((severity) => {
    const all = bySeverity(severity)
    const fixable = all.filter((v) => v.fixed).length
    return {
      label: severityLabel[severity],
      value: all.length,
      note:
        all.length === 0
          ? undefined
          : fixable === 0
            ? 'no fix yet'
            : fixable === all.length
              ? 'all with a fix'
              : `${fixable} with a fix`,
      tone: severity === 'critical' && fixable > 0 ? ('danger' as const) : undefined,
    }
  })
  const secrets = scan.secrets ?? []
  const findings = (scan.findings ?? []).filter((finding) => !counted.has(finding.code))
  const details = [scan.os, scan.dbUpdatedAt && `database from ${shortDate(scan.dbUpdatedAt)}`]
    .filter(Boolean)
    .join(' · ')

  return (
    <Section className="space-y-3">
      <SectionHeader
        title="Security scan"
        description={
          scan.blocked
            ? `Refused by the policy. ${details ? details + '. ' : ''}${policyText(scan)}`
            : `${details ? details + '. ' : ''}${policyText(scan)}`
        }
      />
      <StatGrid stats={stats} />
      {vulnerabilities.length === 0 && secrets.length === 0 && (
        <p className="flex items-center gap-2 text-sm text-muted">
          <ShieldCheck className="size-4 text-success" aria-hidden />
          Nothing known in this image's packages.
        </p>
      )}
      <FindingsList findings={findings} />
      {secrets.length > 0 && (
        <List>
          {secrets.map((secret, index) => (
            <ListRow key={`${secret.rule}-${secret.target}-${index}`} className="items-start">
              <KeyRound className="mt-0.5 size-4 shrink-0 text-danger" aria-hidden />
              <div className="min-w-0 flex-1">
                <span className="text-sm text-ink">{secret.title}</span>
                <span className="ml-2 font-mono text-[11px] text-muted">
                  {secret.target}
                  {secret.line ? `:${secret.line}` : ''}
                </span>
                {secret.match && <p className="truncate font-mono text-xs text-muted">{secret.match}</p>}
              </div>
              <span className="shrink-0 font-mono text-[11px] text-muted">{secret.rule}</span>
            </ListRow>
          ))}
        </List>
      )}
      {vulnerabilities.length > 0 && (
        <Disclosure
          summary={`All ${vulnerabilities.length} ${vulnerabilities.length === 1 ? 'finding' : 'findings'}, worst first`}
          defaultOpen={vulnerabilities.length <= 8}
        >
          <List>
            {vulnerabilities.map((v, index) => (
              <ListRow key={`${v.id}-${v.package}-${index}`} className="items-start">
                <SeverityChip severity={v.severity} />
                <div className="min-w-0 flex-1">
                  <span className="flex min-w-0 items-baseline gap-2">
                    {v.url ? (
                      <a
                        href={v.url}
                        target="_blank"
                        rel="noreferrer"
                        className="inline-flex items-center gap-1 font-mono text-sm text-ink hover:underline"
                      >
                        {v.id}
                        <ExternalLink className="size-3 text-muted" aria-hidden />
                      </a>
                    ) : (
                      <span className="font-mono text-sm text-ink">{v.id}</span>
                    )}
                    {v.title && <span className="min-w-0 truncate text-xs text-muted">{v.title}</span>}
                  </span>
                  <span className="block font-mono text-xs text-ink-soft">
                    {v.package} {v.installed}
                    {v.fixed ? (
                      <>
                        <span className="text-muted"> → </span>
                        {v.fixed}
                      </>
                    ) : (
                      <span className="text-muted"> · no fix yet</span>
                    )}
                    {(v.path || v.target) && <span className="text-muted"> · {v.path || v.target}</span>}
                  </span>
                </div>
                {v.status && <span className="shrink-0 text-[11px] text-muted">{v.status.replace(/_/g, ' ')}</span>}
              </ListRow>
            ))}
          </List>
        </Disclosure>
      )}
    </Section>
  )
}
