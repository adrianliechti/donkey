import { useEffect, useRef, useState } from 'react'
import { Dialog, DialogBackdrop, DialogPanel, DialogTitle } from '@headlessui/react'
import { Globe, Send, Trash2, X } from 'lucide-react'
import { api, type ServiceHTTPResponse } from './api'
import { Button, ErrorNote, IconButton, Select, Spinner, errText } from './ui'

const methods = ['GET', 'POST', 'PUT', 'PATCH', 'DELETE', 'HEAD', 'OPTIONS']
const field = 'w-full rounded border border-line bg-surface px-2 py-1.5 font-mono text-xs focus:outline-2 focus:outline-accent-strong'

function parseHeaders(raw: string): Record<string, string> {
  const headers: Record<string, string> = {}
  for (const line of raw.split('\n')) {
    if (!line.trim()) continue
    const colon = line.indexOf(':')
    if (colon < 1) throw new Error('Enter one header per line as Name: value.')
    const name = line.slice(0, colon).trim()
    if (Object.keys(headers).some((key) => key.toLowerCase() === name.toLowerCase())) {
      throw new Error(`Enter ${name} only once.`)
    }
    headers[name] = line.slice(colon + 1).trim()
  }
  return headers
}

function responseBody(body: string) {
  try { return JSON.stringify(JSON.parse(body), null, 2) }
  catch { return body }
}

export function HttpDialog({ space, service, onClose, onDelete }: {
  space: string; service: string; onClose: () => void; onDelete: () => void
}) {
  const [baseURL, setBaseURL] = useState('')
  const [loading, setLoading] = useState(true)
  const [method, setMethod] = useState('GET')
  const [url, setURL] = useState('/')
  const [headers, setHeaders] = useState('')
  const [body, setBody] = useState('')
  const [response, setResponse] = useState<ServiceHTTPResponse | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const pending = useRef<AbortController | null>(null)

  useEffect(() => {
    let active = true
    api.serviceBinding(space, service).then(({ binding, type }) => {
      if (!active) return
      let key = (type ?? service).toUpperCase().replaceAll('-', '_')
      if (/^[0-9]/.test(key)) key = `SERVICE_${key}`
      setBaseURL(binding[`${key}_URL`] ?? '')
    }).catch((err) => { if (active) setError(errText(err, 'Loading the service URL failed.')) })
      .finally(() => { if (active) setLoading(false) })
    return () => { active = false; pending.current?.abort() }
  }, [space, service])

  async function send() {
    setError('')
    setResponse(null)
    try {
      const requestHeaders = parseHeaders(headers)
      setBusy(true)
      const controller = new AbortController()
      pending.current = controller
      setResponse(await api.testServiceHTTP(space, service, { method, url, headers: requestHeaders, body }, controller.signal))
    } catch (err) {
      if (!(err instanceof DOMException && err.name === 'AbortError')) setError(errText(err, 'The request failed.'))
    } finally {
      pending.current = null
      setBusy(false)
    }
  }

  return <Dialog open onClose={onClose} className="relative z-50">
    <DialogBackdrop className="fixed inset-0 bg-black/60" />
    <div className="fixed inset-0 flex items-center justify-center p-4">
      <DialogPanel className="flex max-h-[calc(100dvh-2rem)] w-full max-w-5xl flex-col overflow-hidden rounded-lg border border-line bg-card shadow-2xl">
        <div className="flex shrink-0 items-center justify-between gap-3 border-b border-line px-4 py-3">
          <DialogTitle className="flex min-w-0 items-center gap-2 font-mono text-[11px] font-semibold uppercase tracking-[0.14em] text-muted">
            <Globe className="size-3.5 shrink-0" aria-hidden /><span className="truncate">{service} · {space}</span>
          </DialogTitle>
          <div className="flex items-center gap-1">
            <IconButton icon={Trash2} label={`Remove ${service}`} kind="danger" onClick={onDelete} />
            <IconButton icon={X} label="Close request tester" onClick={onClose} />
          </div>
        </div>
        <div className="min-h-0 overflow-y-auto p-4">
          <ErrorNote message={error} />
          {loading ? <Spinner label="Loading service…" /> : <>
            <form onSubmit={(event) => { event.preventDefault(); void send() }}>
              <fieldset disabled={busy || !baseURL} className="min-w-0 space-y-3">
                <div className="flex flex-wrap items-end gap-2">
                  <label className="space-y-1 text-xs text-muted">Method
                    <Select ariaLabel="HTTP method" value={method} onChange={setMethod} size="sm" options={methods.map((value) => ({ value, label: value }))} />
                  </label>
                  <label className="min-w-48 flex-1 space-y-1 text-xs text-muted">URL
                    <input aria-label="Request URL" value={url} onChange={(event) => setURL(event.target.value)} className={field}
                      placeholder="/customers?limit=10" autoCapitalize="none" spellCheck={false} />
                  </label>
                  <Button type="submit"><span className="flex items-center gap-1.5"><Send className="size-3.5" aria-hidden />{busy ? 'Sending…' : 'Send'}</span></Button>
                </div>
                <p className="break-all text-xs text-muted">Paths are relative to <span className="font-mono">{baseURL}</span></p>
                <label className="block space-y-1 text-xs text-muted">Headers
                  <textarea aria-label="Request headers" value={headers} onChange={(event) => setHeaders(event.target.value)}
                    rows={3} placeholder={'Accept: application/json\nAuthorization: Bearer …'} spellCheck={false} className={field} />
                  <span className="block">One header per line: Name: value.</span>
                </label>
                <details>
                  <summary className="cursor-pointer text-xs font-medium">Request body (optional)</summary>
                  <textarea aria-label="Request body" value={body} onChange={(event) => setBody(event.target.value)} rows={5}
                    placeholder={'{"message": "hello"}'} spellCheck={false} className={`${field} mt-3`} />
                </details>
              </fieldset>
            </form>
            <section aria-label="Response" className="mt-4 border-t border-line pt-3">
              <div className="flex flex-wrap items-center justify-between gap-3">
                <h2 className="text-sm font-semibold">Response</h2>
                <div aria-live="polite" className="flex items-center gap-3 text-xs">
                  {busy ? <Spinner label="Waiting for response…" /> : response && <>
                    <span className={`rounded-md px-2 py-1 font-mono font-medium ${response.status < 400 ? 'bg-success/10 text-success' : 'bg-danger/10 text-danger'}`}>{response.status} {response.statusText}</span>
                    <span className="text-muted">{response.durationMs} ms</span>
                  </>}
                </div>
              </div>
              {!response && !busy && <p className="mt-3 text-sm text-muted">Send a request to check the service connection.</p>}
              {response && <div className="mt-3 space-y-3">
                <details>
                  <summary className="cursor-pointer text-xs font-medium">Response headers</summary>
                  <pre className="mt-2 overflow-x-auto rounded-lg border border-line bg-surface p-3 font-mono text-xs">{Object.entries(response.headers).map(([name, values]) => values.map((value) => `${name}: ${value}`).join('\n')).join('\n')}</pre>
                </details>
                {response.truncated && <p className="text-xs text-muted">Showing the first 1 MiB of the response.</p>}
                <pre className="max-h-80 overflow-auto whitespace-pre-wrap break-words rounded border border-line bg-surface p-3 font-mono text-xs">{response.body ? responseBody(response.body) : '(empty body)'}</pre>
              </div>}
            </section>
          </>}
        </div>
      </DialogPanel>
    </div>
  </Dialog>
}
