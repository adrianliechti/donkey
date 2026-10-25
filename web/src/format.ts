// Small text formatters every view shares, so errors, sizes and ages read
// the same everywhere.

// errText gives every catch block the same voice: the server's own message
// when there is one, the caller's plain-language fallback otherwise.
export function errText(err: unknown, fallback: string): string {
  return err instanceof Error ? err.message : fallback
}

// humanSize turns a byte count into "512 B", "1.4 MB" — files, objects, disks.
export function humanSize(n: number): string {
  if (n < 1024) return `${n} B`
  const units = ['KB', 'MB', 'GB', 'TB']
  let size = n / 1024
  let i = 0
  while (size >= 1024 && i < units.length - 1) {
    size /= 1024
    i++
  }
  return `${size.toFixed(1)} ${units[i]}`
}

// ago turns an RFC 3339 timestamp into "3m", "2h", "5d" — fleet-triage units.
export function ago(iso: string): string {
  const secs = Math.max(0, Math.round((Date.now() - Date.parse(iso)) / 1000))
  if (secs < 90) return `${secs}s`
  if (secs < 5400) return `${Math.round(secs / 60)}m`
  if (secs < 129600) return `${Math.round(secs / 3600)}h`
  return `${Math.round(secs / 86400)}d`
}

/** The scan's numbers in one phrase: "2 critical, 5 high", or none. */
export function scanCounts(summary: { critical: number; high: number; medium: number; low: number }): string {
  const parts = (['critical', 'high', 'medium', 'low'] as const)
    .filter((severity) => summary[severity] > 0)
    .map((severity) => `${summary[severity]} ${severity}`)
  return parts.length === 0 ? 'no known vulnerabilities' : parts.join(', ')
}
