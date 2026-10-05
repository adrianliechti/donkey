import { useEffect, useRef, useState } from 'react'
import { Send, Trash2 } from 'lucide-react'
import { api, type ServiceHTTPResponse } from './api'
import { Modal, ModalBody, ModalHeader, ModalNotice } from './dialog'
import { Button, Disclosure, Field, IconButton, Input, Select, Spinner, TextArea, errText, wellClass } from './ui'

const methods = ['GET', 'POST', 'PUT', 'PATCH', 'DELETE', 'HEAD', 'OPTIONS']

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

/**
 * The request tester for a restricted HTTP service: compose a request on the
 * left, read the answer on the right. The server makes the call with the
 * service's credential, so the console never sees it.
 */
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

  const ok = response && response.status < 400

  return (
    <Modal onClose={onClose} size="xl" height="fill">
      <ModalHeader eyebrow={`Request tester · ${space}`} title={<span className="font-mono">{service}</span>}
        actions={<IconButton icon={Trash2} label={`Remove ${service}`} kind="danger" onClick={onDelete} />}
        onClose={onClose} closeLabel="Close request tester" />
      <ModalNotice message={error} />
      <ModalBody padded={false}>
        {loading ? <div className="flex flex-1 items-center justify-center"><Spinner label="Loading service…" /></div> : (
          <div className="grid min-h-0 flex-1 lg:grid-cols-2">
            {/* Request */}
            <form className="flex min-h-0 flex-col overflow-y-auto border-b border-line lg:border-b-0 lg:border-r"
              onSubmit={(event) => { event.preventDefault(); void send() }}>
              <fieldset disabled={busy || !baseURL} className="flex min-w-0 flex-1 flex-col gap-4 px-5 py-4">
                <div className="flex items-end gap-2">
                  <Field label="Method">
                    <Select ariaLabel="HTTP method" value={method} onChange={setMethod} options={methods.map((value) => ({ value, label: value }))} className="w-28" />
                  </Field>
                  <Field label="Path" className="flex-1">
                    <Input aria-label="Request URL" value={url} onChange={(event) => setURL(event.target.value)}
                      placeholder="/customers?limit=10" autoCapitalize="none" spellCheck={false} />
                  </Field>
                  <Button type="submit" icon={Send}>{busy ? 'Sending…' : 'Send'}</Button>
                </div>
                <p className="-mt-2 break-all text-xs text-muted">Paths are relative to <span className="font-mono text-ink-soft">{baseURL || '—'}</span></p>
                <Field label="Headers" hint="One header per line: Name: value.">
                  <TextArea size="sm" aria-label="Request headers" value={headers} onChange={(event) => setHeaders(event.target.value)}
                    rows={4} placeholder={'Accept: application/json\nAuthorization: Bearer …'} spellCheck={false} />
                </Field>
                <Field label="Body" hint="Sent as written. Leave empty for GET and HEAD." className="min-h-0 flex-1">
                  <TextArea size="sm" aria-label="Request body" value={body} onChange={(event) => setBody(event.target.value)}
                    rows={8} placeholder={'{"message": "hello"}'} spellCheck={false} className="min-h-32 flex-1 resize-none" />
                </Field>
              </fieldset>
            </form>

            {/* Response */}
            <section aria-label="Response" className="flex min-h-0 flex-col">
              <div className="flex min-h-14 shrink-0 items-center justify-between gap-3 border-b border-line px-5">
                <h2 className="font-mono text-[11px] font-semibold uppercase tracking-[0.14em] text-muted">Response</h2>
                <div aria-live="polite" className="flex items-center gap-3 text-xs">
                  {busy ? <Spinner inline label="Waiting…" /> : response && <>
                    <span className={`rounded-md px-2 py-1 font-mono font-medium ${ok ? 'bg-success/10 text-success' : 'bg-danger/10 text-danger'}`}>{response.status} {response.statusText}</span>
                    <span className="font-mono text-muted">{response.durationMs} ms</span>
                  </>}
                </div>
              </div>
              <div className="min-h-0 flex-1 overflow-y-auto px-5 py-4">
                {!response && !busy && <p className="py-12 text-center text-sm text-muted">Send a request to check the service connection.</p>}
                {response && <div className="space-y-4">
                  <Disclosure summary={<>Headers <span className="text-muted">({Object.keys(response.headers).length})</span></>}>
                    <pre className={wellClass}>{Object.entries(response.headers).map(([name, values]) => values.map((value) => `${name}: ${value}`).join('\n')).join('\n')}</pre>
                  </Disclosure>
                  {response.truncated && <p className="text-xs text-muted">Showing the first 1 MiB of the response.</p>}
                  <pre className={`${wellClass} whitespace-pre-wrap break-words`}>{response.body ? responseBody(response.body) : '(empty body)'}</pre>
                </div>}
              </div>
            </section>
          </div>
        )}
      </ModalBody>
    </Modal>
  )
}
