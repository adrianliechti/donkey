import { useMemo, useState } from 'react'
import { Link } from '@tanstack/react-router'
import { ExternalLink, KeyRound, Search, ShieldCheck, ShieldOff } from 'lucide-react'
import {
  api,
  type AffectedApp,
  type FindingGroup,
  type FindingState,
  type ReportApp,
  type VulnerabilityReport,
} from './api'
import { useVulnerabilities } from './queries'
import {
  appIndex,
  bySpace,
  contactAddresses,
  filterFindings,
  severities,
  severityCounts,
  severityLabel,
  worstState,
  type SeverityFilter,
} from './reports'
import {
  AppLine,
  ExportButton,
  HeadCell,
  ListHead,
  MoreButton,
  Note,
  OpenRow,
  PanelBar,
  SpaceBlock,
} from './ReportParts'
import { SeverityChip } from './ScanSection'
import {
  Button,
  Callout,
  CopyButton,
  EmptyState,
  ErrorNote,
  SearchField,
  Segmented,
  Skeleton,
  StatGrid,
  Toggle,
  panelClass,
} from './ui'

// Every finding in the images that run right now, turned around: not what is
// wrong with one app, but where one finding is — the apps and spaces it is
// in, what today's policy makes of it in each, and who answers for them.
// Worst first, and among equals the one in the most apps.

// What the policy makes of a finding in an app, as a chip. Only what asks
// something of somebody carries a tone.
const stateLook: Record<FindingState, { label: string; className: string; title: string }> = {
  blocking: {
    label: 'needs action',
    className: 'bg-danger/10 text-danger',
    title: 'Today’s policy would refuse this image: the app keeps running, its next deploy needs the fix or a waiver',
  },
  warning: {
    label: 'warning',
    className: 'bg-warning/15 text-warning',
    title: 'Reported by the policy without refusing the image',
  },
  judged: {
    label: 'read as not affected',
    className: 'bg-warning/15 text-warning',
    title: 'The assistant read the app’s code and judged it not affected',
  },
  waived: { label: 'waived', className: 'bg-warning/15 text-warning', title: 'A space admin accepted it' },
  fixable: {
    label: 'fix available',
    className: 'bg-raised text-ink-soft',
    title: 'A fixed version exists; the finding is below what the policy refuses',
  },
  unfixed: { label: 'no fix yet', className: 'bg-raised text-muted', title: 'No fixed version is published' },
}

function StateChip({ state }: { state?: FindingState }) {
  const look = state ? stateLook[state] : undefined
  if (!look) return null
  return (
    <span
      title={look.title}
      className={`inline-block rounded px-1.5 py-0.5 font-mono text-[11px] leading-4 whitespace-nowrap ${look.className}`}
    >
      {look.label}
    </span>
  )
}

const date = (iso: string) => {
  const when = new Date(iso)
  return Number.isNaN(when.getTime()) ? iso : new Intl.DateTimeFormat(undefined, { dateStyle: 'medium' }).format(when)
}

const plural = (n: number, noun: string) => `${n} ${noun}${n === 1 ? '' : 's'}`

// The columns a finding's row and the list's header share.
const cells = {
  apps: 'w-16 shrink-0 text-right max-sm:hidden',
  state: 'w-40 shrink-0 max-md:hidden',
}

function FindingRow({
  finding,
  report,
  apps,
  open,
  onToggle,
}: {
  finding: FindingGroup
  report: VulnerabilityReport
  apps: Map<string, ReportApp>
  open: boolean
  onToggle: () => void
}) {
  const spaces = bySpace(finding.apps)
  const addresses = contactAddresses(
    spaces.map(({ space }) => report.spaces[space]),
    report.platformAdmins,
  )
  const facts = [
    finding.kind === 'secret' ? 'A credential baked into an image' : '',
    finding.score ? `CVSS ${finding.score.toFixed(1)}` : '',
    finding.published ? `published ${date(finding.published)}` : '',
    `${plural(finding.apps.length, 'app')} in ${plural(spaces.length, 'space')}`,
  ].filter(Boolean)
  return (
    <OpenRow
      open={open}
      onToggle={onToggle}
      summary={
        <>
          <SeverityChip severity={finding.severity} />
          <span className="flex min-w-0 flex-1 items-baseline gap-2">
            <span className="flex shrink-0 items-center gap-1.5 font-mono text-sm text-ink">
              {finding.kind === 'secret' && <KeyRound className="size-3.5 text-danger" aria-hidden />}
              {finding.id}
            </span>
            {finding.title && <span className="min-w-0 truncate text-xs text-muted">{finding.title}</span>}
          </span>
          <span className={`font-mono text-xs text-ink-soft ${cells.apps}`}>{finding.apps.length}</span>
          <span className={cells.state}>
            <StateChip state={worstState(finding.apps.map((app) => app.state))} />
          </span>
        </>
      }
    >
      <div className="space-y-3">
        <div className="flex flex-wrap items-center gap-x-4 gap-y-1">
          <p className="text-xs text-muted">{facts.join(' · ')}</p>
          {finding.url && (
            <a
              href={finding.url}
              target="_blank"
              rel="noreferrer"
              className="inline-flex items-center gap-1 text-xs text-ink-soft underline-offset-2 hover:text-ink hover:underline"
            >
              Advisory <ExternalLink className="size-3" aria-hidden />
            </a>
          )}
          {addresses.length > 0 && spaces.length > 1 && (
            <span className="-my-1.5 ml-auto flex items-center gap-1 text-xs text-muted">
              Everyone to write to
              <CopyButton text={addresses.join(', ')} label={`Copy ${plural(addresses.length, 'address')}`} />
            </span>
          )}
        </div>
        {spaces.map(({ space, rows }) => (
          <SpaceBlock key={space} space={space} contacts={report.spaces[space]} platformAdmins={report.platformAdmins}>
            {rows.map((affected) => (
              <AffectedLine
                key={affected.app}
                affected={affected}
                finding={finding}
                info={apps.get(`${space}/${affected.app}`)}
              />
            ))}
          </SpaceBlock>
        ))}
      </div>
    </OpenRow>
  )
}

function AffectedLine({ affected, finding, info }: { affected: AffectedApp; finding: FindingGroup; info?: ReportApp }) {
  return (
    <AppLine
      app={affected}
      info={info}
      aside={
        <>
          {/* The same finding can weigh differently in another distribution. */}
          {affected.severity !== finding.severity && <SeverityChip severity={affected.severity} />}
          <StateChip state={affected.state} />
        </>
      }
    >
      {(affected.packages ?? []).map((pkg) => (
        <p
          key={`${pkg.name}/${pkg.installed}/${pkg.path ?? ''}`}
          className="font-mono text-xs break-words text-ink-soft"
        >
          {pkg.name} {pkg.installed}
          {/* A package without an arrow has no fixed version yet. */}
          {pkg.fixed && (
            <>
              <span className="text-muted"> → </span>
              {pkg.fixed}
            </>
          )}
          {(pkg.path || pkg.type) && <span className="text-muted"> · {pkg.path || pkg.type}</span>}
        </p>
      ))}
      {(affected.locations ?? []).map((location) => (
        <p key={location} className="font-mono text-xs break-words text-ink-soft">
          {location}
        </p>
      ))}
      {affected.note && <p className="mt-0.5 text-xs leading-relaxed text-muted">{affected.note}</p>}
    </AppLine>
  )
}

// What the findings do not cover, said where it matters: an app with no scan
// on record has no findings, which is not the same as having none.
function Coverage({ report }: { report: VulnerabilityReport }) {
  const unscanned = report.apps.filter((app) => !app.scanned && !app.problem)
  const unread = report.apps.filter((app) => app.problem)
  const stale = report.apps.filter((app) => app.security?.refreshError)
  const names = (apps: ReportApp[]) =>
    apps.map((app, index) => (
      <span key={`${app.space}/${app.app}`}>
        {index > 0 && ', '}
        <Link
          to="/app/$space/$name/{-$tab}"
          params={{ space: app.space, name: app.app, tab: undefined }}
          className="font-mono text-ink-soft underline-offset-2 hover:text-ink hover:underline"
        >
          {app.space}/{app.app}
        </Link>
      </span>
    ))
  return (
    <>
      {!report.scanning && (
        <Callout tone="warning" icon={ShieldOff} title="Scanning is off">
          Images are published without a scan (build.scan.enabled: false). What is listed here was found while it was
          on.
        </Callout>
      )}
      {unread.length > 0 && (
        <Callout tone="danger" title={`The records of ${plural(unread.length, 'app')} could not be read`}>
          {names(unread)} — {unread[0].problem}
        </Callout>
      )}
      {unscanned.length > 0 && (
        <Note icon={ShieldOff}>
          No scan is on record for {names(unscanned)} — deployed before scanning, or with it off. Deploying again scans{' '}
          {unscanned.length === 1 ? 'it' : 'them'}.
        </Note>
      )}
      {stale.length > 0 && (
        <Note icon={ShieldOff}>
          The latest check of {names(stale)} failed; {stale.length === 1 ? 'its' : 'their'} previous findings are
          listed.
        </Note>
      )}
    </>
  )
}

export function VulnerabilitiesPanel() {
  const { data: report, error, updatedAt, reload, fetching } = useVulnerabilities()
  const [severity, setSeverity] = useState<SeverityFilter>('all')
  const [query, setQuery] = useState('')
  const [fixable, setFixable] = useState(false)
  const [open, setOpen] = useState<string | null>(null)
  const [limit, setLimit] = useState(100)
  const [exportError, setExportError] = useState('')

  const findings = report?.findings
  const apps = useMemo(() => appIndex(report?.apps ?? []), [report])
  // The counts beside each severity are what the search and the toggle leave.
  const narrowed = useMemo(
    () => filterFindings(findings ?? [], { severity: 'all', query, fixable }),
    [findings, query, fixable],
  )
  const counts = severityCounts(narrowed)
  const shown = severity === 'all' ? narrowed : narrowed.filter((finding) => finding.severity === severity)
  const all = severityCounts(findings ?? [])
  const scanned = (report?.apps ?? []).filter((app) => app.scanned).length

  const reset = () => {
    setSeverity('all')
    setQuery('')
    setFixable(false)
  }

  return (
    <>
      <PanelBar
        updatedAt={updatedAt}
        onReload={reload}
        busy={fetching}
        actions={
          <ExportButton
            hint="One row per finding, app and package, with the contacts of the app’s space — what the filters show"
            disabled={!report || shown.length === 0}
            onError={setExportError}
            download={() =>
              api.exportVulnerabilities({
                severity: severity === 'all' ? undefined : severity,
                q: query.trim(),
                fixable,
              })
            }
          />
        }
      >
        <Segmented
          label="Severity"
          value={severity}
          onChange={(value) => {
            setSeverity(value)
            setLimit(100)
          }}
          options={[
            { value: 'all' as SeverityFilter, label: 'All', count: counts.all },
            ...severities
              .filter((s) => s !== 'unknown' || all.unknown > 0)
              .map((s) => ({ value: s as SeverityFilter, label: severityLabel[s], count: counts[s] })),
          ]}
        />
        <SearchField
          value={query}
          onChange={(value) => {
            setQuery(value)
            setLimit(100)
          }}
          placeholder="Find a CVE, package, space or app…"
          className="w-72"
        />
        <label className="flex h-8 items-center gap-2 text-xs text-ink-soft">
          <Toggle size="sm" label="Only findings with a fix" checked={fixable} onChange={setFixable} />
          With a fix
        </label>
      </PanelBar>

      <ErrorNote message={error || exportError} />
      {!report && !error && (
        <>
          <Skeleton className="h-20" />
          <Skeleton className="h-64" />
        </>
      )}

      {report && (
        <>
          <StatGrid
            stats={(['critical', 'high', 'medium', 'low'] as const).map((s) => {
              const of = (findings ?? []).filter((finding) => finding.severity === s)
              const withFix = of.filter((finding) => finding.fixable).length
              return {
                label: severityLabel[s],
                value: of.length,
                note:
                  of.length === 0
                    ? undefined
                    : withFix === 0
                      ? 'no fix yet'
                      : withFix === of.length
                        ? 'all with a fix'
                        : `${withFix} with a fix`,
                tone:
                  s === 'critical' && of.some((finding) => finding.apps.some((app) => app.state === 'blocking'))
                    ? ('danger' as const)
                    : undefined,
              }
            })}
          />
          <Coverage report={report} />

          <section aria-label="Findings" className={`overflow-hidden ${panelClass}`}>
            <ListHead className="flex items-center gap-3">
              <HeadCell className="w-16 shrink-0">Severity</HeadCell>
              <HeadCell className="min-w-0 flex-1">Finding</HeadCell>
              <HeadCell className={cells.apps}>Apps</HeadCell>
              <HeadCell className={cells.state}>Policy</HeadCell>
              <span className="w-4 shrink-0" />
            </ListHead>
            {shown.length === 0 ? (
              (findings ?? []).length === 0 ? (
                <EmptyState
                  icon={ShieldCheck}
                  title={report.apps.length === 0 ? 'No apps are running' : 'No known vulnerabilities'}
                >
                  {report.apps.length === 0
                    ? 'What the scans of running images find is listed here once an app is deployed.'
                    : scanned > 0
                      ? `Nothing is known in the images of the ${plural(scanned, 'app')} scanned.`
                      : 'No running app has a scan on record yet.'}
                </EmptyState>
              ) : (
                <EmptyState
                  icon={Search}
                  title="Nothing matches"
                  action={
                    <Button kind="quiet" size="sm" onClick={reset}>
                      Show all findings
                    </Button>
                  }
                >
                  Findings are searched by id, title and package, and by the spaces and apps they are in.
                </EmptyState>
              )
            ) : (
              <>
                <ul className="divide-y divide-line">
                  {shown.slice(0, limit).map((finding) => {
                    const key = `${finding.kind}/${finding.id}`
                    return (
                      <FindingRow
                        key={key}
                        finding={finding}
                        report={report}
                        apps={apps}
                        open={open === key}
                        onToggle={() => setOpen(open === key ? null : key)}
                      />
                    )
                  })}
                </ul>
                <MoreButton hidden={shown.length - limit} onClick={() => setLimit(limit + 100)} noun="findings" />
              </>
            )}
          </section>
          {report.apps.length > 0 && (
            <p className="text-xs leading-relaxed text-muted">
              Read from the latest scan of {scanned} of {plural(report.apps.length, 'running app')}.{' '}
              {report.block === 'none'
                ? 'The policy reports findings and refuses none.'
                : `The policy refuses ${report.block} findings that have a fix.`}{' '}
              The contacts of a space are its admins and developers.
            </p>
          )}
        </>
      )}
    </>
  )
}
