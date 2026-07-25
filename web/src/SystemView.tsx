import { useEffect, useState } from 'react'
import { api, type DoctorReport } from './api'
import { Card, CheckIcon, ErrorNote, SectionTitle, Spinner } from './ui'

export function SystemView() {
  const [report, setReport] = useState<DoctorReport | null>(null)
  const [error, setError] = useState('')

  useEffect(() => {
    api
      .doctor()
      .then(setReport)
      .catch((err) => setError(err instanceof Error ? err.message : 'Loading failed.'))
  }, [])

  return (
    <div className="space-y-4">
      <h1 className="text-lg font-semibold">System</h1>
      <ErrorNote message={error} />
      {report === null && !error && <Spinner label="Checking the control plane…" />}
      {report && (
        <>
          <Card className="divide-y divide-line">
            {report.checks.map((check) => (
              <div key={check.name} className="flex items-center gap-3 px-4 py-3">
                <CheckIcon ok={check.ok} />
                <div className="min-w-0">
                  <div className="text-sm font-medium">{check.name}</div>
                  {check.detail && (
                    <div className="truncate text-xs text-stone">{check.detail}</div>
                  )}
                </div>
              </div>
            ))}
          </Card>
          <Card className="p-4">
            <SectionTitle>Apps are served on</SectionTitle>
            <p className="mt-1 font-mono text-sm">
              {'<app>'}.{report.baseDomain}
            </p>
          </Card>
        </>
      )}
    </div>
  )
}
