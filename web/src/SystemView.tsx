import { RefreshCw } from 'lucide-react'
import { useDoctor } from './queries'
import { Card, CheckIcon, ErrorNote, SectionTitle, Spinner, UpdatedAgo } from './ui'

export function SystemView() {
  const { data: report, error, updatedAt, reload } = useDoctor()

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <h1 className="text-lg font-semibold">System</h1>
        <div className="flex items-center gap-3">
          <UpdatedAgo at={updatedAt} />
          <button
            onClick={reload}
            className="flex items-center gap-1.5 rounded-md px-2 py-1 text-xs text-muted hover:bg-line/50 focus-visible:outline-2 focus-visible:outline-accent-strong"
          >
            <RefreshCw className="size-3.5" aria-hidden /> Check again
          </button>
        </div>
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
                  {check.detail && (
                    <div className="text-xs text-muted break-words">{check.detail}</div>
                  )}
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
              Every exposed app gets an exact hostname at the shared gateway. When external-dns is
              configured, donkey publishes a matching DNSEndpoint for each one.
            </p>
          </Card>
        </>
      )}
    </div>
  )
}
