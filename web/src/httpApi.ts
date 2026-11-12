import { request, servicePath, type ServiceHTTPRequest, type ServiceHTTPResponse } from './api'
import { box, newId } from './httpStore'
import { errText } from './format'

// The REST client behind an HTTP service's Requests tab. A request is sent by
// the server (POST …/http/request), through the shared proxy that adds the
// service's credential — the console never holds it. What the console keeps
// is the viewer's own: the request being written, saved requests and a
// history, all in this browser.

export const methods = ['GET', 'POST', 'PUT', 'PATCH', 'DELETE', 'HEAD', 'OPTIONS']

/** A line of a key/value table: a query parameter, a header, a form field. */
export type Row = { id: string; key: string; value: string; on: boolean }

export type BodyKind = 'none' | 'json' | 'text' | 'form'

/** A request as it is written: the query lives in `params`, not the path. */
export type Draft = {
  method: string
  path: string
  params: Row[]
  headers: Row[]
  bodyKind: BodyKind
  body: string
  form: Row[]
}

export const emptyDraft = (): Draft => ({
  method: 'GET',
  path: '/',
  params: [],
  headers: [],
  bodyKind: 'none',
  body: '',
  form: [],
})

/** What the server answers. */
export type HttpResult = ServiceHTTPResponse

export function sendHTTP(space: string, service: string, input: ServiceHTTPRequest, signal?: AbortSignal) {
  return request<HttpResult>(servicePath(space, service, '/http/request'), {
    method: 'POST',
    body: JSON.stringify(input),
    signal,
  })
}

// The service's base URL is its binding's <TYPE>_URL — the shared proxy's
// route for it, which is what an app calls too.
export function baseURLOf(binding: Record<string, string>, type: string | undefined, service: string): string {
  let key = (type || service).toUpperCase().replaceAll('-', '_')
  if (/^[0-9]/.test(key)) key = `SERVICE_${key}`
  return (
    binding[`${key}_URL`] ??
    Object.entries(binding).find(([k, v]) => k.endsWith('_URL') && v.includes('/services/'))?.[1] ??
    ''
  )
}

// ─── The URL and its query ─────────────────────────────────────────────

// A query part is escaped only where it would change the query's shape (&,
// #, %, = in a key), and for spaces and non-ASCII — the rest reads as typed.
// A plus stays a plus, as in any address bar.
function encodePart(s: string, key: boolean): string {
  try {
    return s.replace(key ? /[%&=#\s]|[^\x21-\x7e]/gu : /[%&#\s]|[^\x21-\x7e]/gu, (c) => encodeURIComponent(c))
  } catch {
    return s
  }
}

function decodePart(s: string): string {
  try {
    return decodeURIComponent(s)
  } catch {
    return s
  }
}

export function queryOf(params: Row[]): string {
  return params
    .filter((p) => p.on && (p.key || p.value))
    .map((p) => (p.value === '' ? encodePart(p.key, true) : `${encodePart(p.key, true)}=${encodePart(p.value, false)}`))
    .join('&')
}

/** The path and the enabled parameters, as the URL bar shows them. */
export function composeURL(path: string, params: Row[]): string {
  const query = queryOf(params)
  return query ? `${path}?${query}` : path
}

/**
 * What a typed URL means for the draft: its path, and its query as the
 * enabled parameters. Disabled ones are not in the URL, so they stay; a URL
 * pasted whole loses the service's base, which the server adds back.
 */
export function parseURL(text: string, base: string, previous: Row[]): { path: string; params: Row[] } {
  let rest = text.trim()
  if (base && rest.startsWith(base)) rest = '/' + rest.slice(base.length)
  const at = rest.indexOf('?')
  const path = at < 0 ? rest : rest.slice(0, at)
  const query = at < 0 ? '' : rest.slice(at + 1)
  const enabled = previous.filter((p) => p.on)
  const parsed = query
    .split('&')
    .filter(Boolean)
    .map((part, i): Row => {
      const eq = part.indexOf('=')
      const key = decodePart(eq < 0 ? part : part.slice(0, eq))
      const value = eq < 0 ? '' : decodePart(part.slice(eq + 1))
      // Rows keep their ids by position, so typing in the URL bar does not
      // remount the table's inputs.
      return { id: enabled[i]?.id ?? newId(), key, value, on: true }
    })
  return { path, params: [...parsed, ...previous.filter((p) => !p.on)] }
}

/** Where a path lands under the base URL — the same join the server makes. */
export function fullURL(base: string, url: string): string {
  if (!base || /^https?:\/\//i.test(url)) return url
  return base.replace(/\/?$/, '/') + url.replace(/^\//, '')
}

// ─── Headers and body ──────────────────────────────────────────────────

const bodyTypes: Record<BodyKind, string | undefined> = {
  none: undefined,
  json: 'application/json',
  text: 'text/plain; charset=utf-8',
  form: 'application/x-www-form-urlencoded',
}

const hasHeader = (d: Draft, name: string) =>
  d.headers.some((h) => h.on && h.key.trim().toLowerCase() === name.toLowerCase())

/** The Content-Type the body brings, unless a header of the draft sets one. */
export function autoContentType(d: Draft): string | undefined {
  return hasHeader(d, 'content-type') ? undefined : bodyTypes[d.bodyKind]
}

export function bodyOf(d: Draft): string {
  if (d.bodyKind === 'json' || d.bodyKind === 'text') return d.body
  if (d.bodyKind === 'form') {
    const form = new URLSearchParams()
    for (const f of d.form) if (f.on && f.key) form.append(f.key, f.value)
    return form.toString()
  }
  return ''
}

/** The headers that will be sent, the body's Content-Type included. */
export function headersOf(d: Draft): Record<string, string> {
  const out: Record<string, string> = {}
  const auto = autoContentType(d)
  if (auto) out['Content-Type'] = auto
  for (const h of d.headers) {
    const name = h.key.trim()
    if (!h.on || !name) continue
    // One value per name: the server takes a map, and the last line wins.
    for (const known of Object.keys(out)) if (known.toLowerCase() === name.toLowerCase()) delete out[known]
    out[name] = h.value
  }
  return out
}

export function toRequest(d: Draft): ServiceHTTPRequest {
  return { method: d.method, url: composeURL(d.path || '/', d.params), headers: headersOf(d), body: bodyOf(d) }
}

/** The JSON body's problem, or '' when it parses (or is empty). */
export function jsonProblem(text: string): string {
  if (!text.trim()) return ''
  try {
    JSON.parse(text)
    return ''
  } catch (err) {
    return err instanceof Error ? err.message : 'Not valid JSON.'
  }
}

// ─── Copy as curl ──────────────────────────────────────────────────────

const shellQuote = (s: string) => `'${s.replaceAll("'", `'\\''`)}'`

// The same request as an app in the space would make it: to the service's
// base URL, which the shared proxy answers with the credential added.
export function curlFor(d: Draft, base: string): string {
  const input = toRequest(d)
  const method = input.method === 'HEAD' ? '--head ' : input.method !== 'GET' || input.body ? `-X ${input.method} ` : ''
  const lines = [`curl ${method}${shellQuote(fullURL(base, input.url))}`]
  for (const [name, value] of Object.entries(input.headers)) lines.push(`-H ${shellQuote(`${name}: ${value}`)}`)
  if (input.body && input.method !== 'HEAD') lines.push(`--data-raw ${shellQuote(input.body)}`)
  return lines.join(' \\\n  ')
}

// ─── The answer ────────────────────────────────────────────────────────

export function headerValue(headers: Record<string, string[]>, name: string): string {
  const key = Object.keys(headers).find((k) => k.toLowerCase() === name.toLowerCase())
  return key ? (headers[key][0] ?? '') : ''
}

/** How a body is best shown: its highlighter language, or none. */
export function bodyLanguage(contentType: string, body: string): 'json' | 'html' | 'css' | 'js' | 'yaml' | '' {
  const type = contentType.toLowerCase()
  if (type.includes('json')) return 'json'
  if (type.includes('html') || type.includes('xml')) return 'html'
  if (type.includes('css')) return 'css'
  if (type.includes('javascript')) return 'js'
  if (type.includes('yaml')) return 'yaml'
  // Some services answer JSON as text/plain.
  if (/^\s*[[{]/.test(body) && !jsonProblem(body)) return 'json'
  return ''
}

export function prettyJSON(body: string): string {
  try {
    return JSON.stringify(JSON.parse(body), null, 2)
  } catch {
    return body
  }
}

export function decodeBase64(body: string): Uint8Array<ArrayBuffer> {
  try {
    const raw = atob(body)
    const bytes = new Uint8Array(raw.length)
    for (let i = 0; i < raw.length; i++) bytes[i] = raw.charCodeAt(i)
    return bytes
  } catch {
    return new Uint8Array(0)
  }
}

/** A status code's class, as a tone: success, redirect, client or server error. */
export function statusTone(status: number): string {
  if (status >= 500) return 'bg-danger/10 text-danger'
  if (status >= 400) return 'bg-warning/10 text-warning'
  if (status >= 300) return 'bg-accent-soft text-accent-strong'
  if (status >= 200) return 'bg-success/10 text-success'
  return 'bg-raised text-ink-soft'
}

/** Methods in their usual colours — read at a glance down a history. */
export function methodTone(method: string): string {
  switch (method) {
    case 'GET':
      return 'text-success'
    case 'POST':
      return 'text-warning'
    case 'PUT':
    case 'PATCH':
      return 'text-accent-strong'
    case 'DELETE':
      return 'text-danger'
    default:
      return 'text-muted'
  }
}

// ─── The request on the page ───────────────────────────────────────────

// The console is a quick tester: one request and its answer, kept in memory
// only — they outlast a look at the Connection tab, not a reload.
export const draftBox = (space: string, service: string) =>
  box<Draft>(`donkey:http:draft:${space}/${service}`, () => emptyDraft(), { memory: true })

// An exchange is one send and how it ended. It lives in a box rather than a
// mutation's state, so a request still running when the tab is left lands
// where the next visit finds it.
export type Exchange = {
  id: string
  state: 'pending' | 'done' | 'failed' | 'cancelled'
  startedAt: number
  draft: Draft
  result?: HttpResult
  error?: string
}

export const exchangeBox = (space: string, service: string) =>
  box<Exchange | null>(`donkey:http:exchange:${space}/${service}`, () => null, { memory: true })

const running = new Map<string, AbortController>()

/** Cancels the exchange with this id, if it is still running. */
export const cancelExchange = (id: string) => running.get(id)?.abort()

/** Sends a draft; its answer replaces the last one. */
export async function exchange(space: string, service: string, draft: Draft) {
  const current = exchangeBox(space, service)
  const id = newId()
  const controller = new AbortController()
  running.set(id, controller)
  current.set({ id, state: 'pending', startedAt: Date.now(), draft })
  // A newer send has taken the place by the time this one ends: it stays.
  const settle = (patch: Partial<Exchange>) => current.set((now) => (now?.id === id ? { ...now, ...patch } : now))
  try {
    settle({ state: 'done', result: await sendHTTP(space, service, toRequest(draft), controller.signal) })
  } catch (err) {
    if (controller.signal.aborted) settle({ state: 'cancelled' })
    else settle({ state: 'failed', error: errText(err, 'The request did not work.') })
  } finally {
    running.delete(id)
  }
}
