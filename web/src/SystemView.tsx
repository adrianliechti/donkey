import { useCallback } from 'react'
import { RefreshCw } from 'lucide-react'
import { api } from './api'
import { usePoll } from './hooks'
import { Card, CheckIcon, ErrorNote, SectionTitle, Spinner, UpdatedAgo } from './ui'

export function SystemView() {
  const fetcher = useCallback(() => api.doctor(), [])
  // Doctor probes everything (build engine, registry, gateway, assistant)
  // and can take up to ~15s — poll gently and keep the last report visible
  // while a new one is on its way.
  const { data: report, error, updatedAt, reload } = usePoll(fetcher, 60_000)

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <h1 className="text-lg font-semibold">System</h1>
        <div className="flex items-center gap-3">
          <UpdatedAgo at={updatedAt} />
          <button
            onClick={reload}
            className="flex items-center gap-1.5 rounded-md px-2 py-1 text-xs text-stone hover:bg-line/50 focus-visible:outline-2 focus-visible:outline-hay-deep"
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
                    <div className="text-xs text-stone break-words">{check.detail}</div>
                  )}
                </div>
              </div>
            ))}
          </Card>
          <Card className="p-4">
            <SectionTitle>Apps are served on</SectionTitle>
            <p className="mt-1 font-mono text-sm">
              {'<app>'}.{'<space>'}.{report.baseDomain}
            </p>
            <p className="mt-1 text-xs text-stone">
              One DNS wildcard per space: *.{'<space>'}.{report.baseDomain} → the gateway's address.
            </p>
          </Card>
        </>
      )}
    </div>
  )
}
