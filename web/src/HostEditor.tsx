import { useId } from 'react'
import { Select } from './ui'

const inputClass = 'min-w-0 flex-1 rounded-md border border-line bg-surface px-2 py-1.5 font-mono text-sm focus:outline-2 focus:outline-accent-strong'

export function HostEditor({ value, domains, onChange }: {
  value: string
  domains: string[]
  onChange: (host: string) => void
}) {
  const id = useId()
  if (domains.length === 0) {
    return (
      <div>
        <label htmlFor={id} className="mb-1 block text-xs text-muted">Base URL</label>
        <div className="flex items-center gap-1.5">
          <span className="font-mono text-sm text-muted">https://</span>
          <input id={id} value={value} onChange={(e) => onChange(e.target.value)}
            required placeholder="api.example.com" autoCapitalize="none" spellCheck={false}
            className={inputClass} />
        </div>
        <p className="mt-1 text-xs text-muted">Enter the full hostname. This space allows any domain.</p>
      </div>
    )
  }

  // Prefer the most specific suffix when the list contains overlapping domains.
  const matched = [...domains].sort((a, b) => b.length - a.length)
    .find((domain) => value === domain || value.endsWith(`.${domain}`))
  const domain = matched ?? domains[0]
  const hostname = matched ? (value === matched ? '' : value.slice(0, -matched.length - 1)) : value
  const join = (name: string, suffix: string) => name ? `${name}.${suffix}` : suffix

  return (
    <div>
      <label htmlFor={id} className="mb-1 block text-xs text-muted">Hostname</label>
      <div className="flex flex-wrap items-center gap-1.5">
        <span className="font-mono text-sm text-muted">https://</span>
        <input id={id} value={hostname} onChange={(e) => onChange(join(e.target.value, domain))}
          placeholder="api" autoCapitalize="none" spellCheck={false} className={inputClass} />
        <span className="text-muted">.</span>
        {domains.length === 1 ? (
          <span className="break-all font-mono text-sm">{domain}</span>
        ) : (
          <Select ariaLabel="Domain" value={domain} onChange={(suffix) => onChange(join(hostname, suffix))}
            options={domains.map((suffix) => ({ value: suffix, label: suffix }))} />
        )}
      </div>
      {value && !matched && <p className="mt-1 text-xs text-danger">Choose a hostname under one of this space’s domains.</p>}
    </div>
  )
}
