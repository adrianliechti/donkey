import { RefreshCw } from 'lucide-react'
import { useDoctor } from './queries'
import { Card, CheckIcon, ErrorNote, SectionTitle, Spinner, Toolbar, UpdatedAgo, IconButton } from './ui'

export function SystemView() {
  const { data: report, error, updatedAt, reload } = useDoctor()

  return (
    <div className="space-y-4">
      <div className="flex min-h-8 items-center justify-between gap-3">
        <h1 className="font-mono text-base font-bold tracking-tight">System</h1>
        <Toolbar className="gap-3">
          <UpdatedAgo at={updatedAt} />
          <IconButton icon={RefreshCw} label="Check again" onClick={reload} />
        </Toolbar>
      </div>
      <ErrorNote message={error} />
      {report === null && !error && <Spinner label="Checking the control plane… (this probes everything, up to 15s)" />}
      {report && (
        <>
          <Card className="divide-y divide-line">
            {report.checks.map((check) => (
              <div key={check.name} className="flex items-start gap-3 px-4 py-3">
                <CheckIcon ok={check.ok} />
                <div className="min-w-0">
                  <div className="text-sm font-medium">{check.name}</div>
                  {check.detail && <div className="text-xs text-muted break-words">{check.detail}</div>}
                </div>
              </div>
            ))}
          </Card>
          <Card className="p-4">
            <SectionTitle>Apps are served on</SectionTitle>
            <p className="mt-1 font-mono text-sm">
              {'<app>'}.{'<space>'}.{report.domain}
            </p>
            <p className="mt-1 text-xs text-muted">
              Every exposed app gets an exact hostname at the shared gateway. When external-dns is configured, donkey
              publishes a matching DNSEndpoint for each one.
            </p>
          </Card>
        </>
      )}
    </div>
  )
}
