import { queryOptions } from '@tanstack/react-query'
import { request, servicePath } from './api'
import { box, dropOldest, isRecord, listOf, newId, num, str } from './httpStore'

// Web search and page contents through an Exa-compatible service. The
// server forwards the request body as it is to the service's /search and
// /contents, with the service's key — so every option Exa takes is
// available; the console offers the ones worth a field.

/** A search as the URL carries it — every value a string, empty when unset. */
export type SearchOptions = {
  q: string
  type: string
  n: string
  cat: string
  inc: string
  exc: string
  from: string
  to: string
}

export const searchKeys = ['q', 'type', 'n', 'cat', 'inc', 'exc', 'from', 'to'] as const

export function optionsFrom(search: Record<string, string>): SearchOptions {
  const out = {} as SearchOptions
  for (const key of searchKeys) out[key] = search[key] ?? ''
  return out
}

export const searchTypes = [
  { value: '', label: 'Auto' },
  { value: 'neural', label: 'Neural' },
  { value: 'keyword', label: 'Keyword' },
  { value: 'fast', label: 'Fast' },
]

export const categories = [
  { value: '', label: 'Any kind' },
  { value: 'news', label: 'News' },
  { value: 'research paper', label: 'Research papers' },
  { value: 'company', label: 'Companies' },
  { value: 'pdf', label: 'PDFs' },
  { value: 'github', label: 'GitHub' },
  { value: 'tweet', label: 'Tweets' },
  { value: 'personal site', label: 'Personal sites' },
  { value: 'financial report', label: 'Financial reports' },
]

export const resultCounts = [5, 10, 25, 50, 100]
export const defaultCount = 10

const domains = (text: string) =>
  text
    .split(/[\s,]+/)
    .map((d) => d.trim().replace(/^https?:\/\//, '').replace(/\/.*$/, ''))
    .filter(Boolean)

export function searchBody(o: SearchOptions): Record<string, unknown> {
  const n = Math.min(100, Math.max(1, parseInt(o.n, 10) || defaultCount))
  const body: Record<string, unknown> = {
    query: o.q,
    numResults: n,
    // Highlights are the snippet a result is judged by; the full text is
    // fetched for the one that is opened.
    contents: { highlights: { numSentences: 2, highlightsPerUrl: 3 } },
  }
  if (o.type) body.type = o.type
  if (o.cat) body.category = o.cat
  if (domains(o.inc).length) body.includeDomains = domains(o.inc)
  if (domains(o.exc).length) body.excludeDomains = domains(o.exc)
  if (o.from) body.startPublishedDate = `${o.from}T00:00:00.000Z`
  if (o.to) body.endPublishedDate = `${o.to}T23:59:59.999Z`
  return body
}

export type ExaResult = {
  id: string
  url: string
  title: string
  publishedDate: string
  author: string
  score?: number
  highlights: string[]
  summary: string
  text: string
}

export type ExaSearch = { results: ExaResult[]; resolvedType: string; cost?: number }

function parseResult(r: Record<string, unknown>): ExaResult | undefined {
  const url = str(r.url)
  if (!url) return undefined
  return {
    id: str(r.id) || url,
    url,
    title: str(r.title),
    publishedDate: str(r.publishedDate),
    author: str(r.author),
    score: num(r.score),
    highlights: Array.isArray(r.highlights) ? r.highlights.filter((h): h is string => typeof h === 'string') : [],
    summary: str(r.summary),
    text: str(r.text),
  }
}

const cost = (raw: unknown) => (isRecord(raw) ? num(raw.total) : undefined)

// Searches cost money: a result is kept for good while the console is open
// and never fetched twice on its own — back and forward show it again, and
// only "search again" asks anew.
export const searchQuery = (space: string, service: string, o: SearchOptions) =>
  queryOptions({
    queryKey: ['exa', space, service, 'search', o],
    queryFn: async ({ signal }): Promise<ExaSearch> => {
      const answer = await request<Record<string, unknown>>(servicePath(space, service, '/exa/search'), {
        method: 'POST',
        body: JSON.stringify(searchBody(o)),
        signal,
      })
      const found: ExaSearch = {
        results: listOf(answer.results, parseResult),
        resolvedType: str(answer.resolvedSearchType),
        cost: cost(answer.costDollars),
      }
      rememberSearch(space, service, o, found.results.length)
      return found
    },
    enabled: o.q.trim() !== '',
    staleTime: Infinity,
    gcTime: 30 * 60_000,
    retry: false,
  })

export type ExaPage = { url: string; title: string; author: string; publishedDate: string; text: string }

export const contentsQuery = (space: string, service: string, url: string) =>
  queryOptions({
    queryKey: ['exa', space, service, 'contents', url],
    queryFn: async ({ signal }): Promise<ExaPage> => {
      const answer = await request<Record<string, unknown>>(servicePath(space, service, '/exa/contents'), {
        method: 'POST',
        body: JSON.stringify({ urls: [url], text: true }),
        signal,
      })
      // A page Exa could not read comes back as a status, not an error.
      const failed = listOf(answer.statuses, (s) => (s.status === 'error' ? s : undefined))[0]
      const page = listOf(answer.results, parseResult)[0]
      if (!page) {
        const why = isRecord(failed?.error) ? str(failed.error.tag).replaceAll('_', ' ').toLowerCase() : ''
        throw new Error(why ? `Exa could not read this page: ${why}.` : 'Exa has no contents for this page.')
      }
      return { url: page.url, title: page.title, author: page.author, publishedDate: page.publishedDate, text: page.text }
    },
    enabled: url !== '',
    staleTime: Infinity,
    gcTime: 30 * 60_000,
    retry: false,
  })

// ─── Recent searches ───────────────────────────────────────────────────

export type Recent = { id: string; options: SearchOptions; at: number; count?: number }

const maxRecent = 30

export const recentBox = (space: string, service: string) =>
  box<Recent[]>(
    `donkey:exa:recent:${space}/${service}`,
    (raw) =>
      listOf(raw, (r) => {
        if (!isRecord(r.options)) return undefined
        const options = optionsFrom(Object.fromEntries(Object.entries(r.options).map(([k, v]) => [k, str(v)])))
        return options.q ? { id: str(r.id) || newId(), options, at: num(r.at) ?? 0, count: num(r.count) } : undefined
      }),
    { maxBytes: 64 * 1024, shrink: dropOldest },
  )

const sameSearch = (a: SearchOptions, b: SearchOptions) => searchKeys.every((k) => a[k] === b[k])

function rememberSearch(space: string, service: string, options: SearchOptions, count: number) {
  recentBox(space, service).set((list) =>
    [{ id: newId(), options, at: Date.now(), count }, ...list.filter((r) => !sameSearch(r.options, options))].slice(0, maxRecent),
  )
}

/** The options of a search beyond its words, said briefly. */
export function describe(o: SearchOptions): string {
  return [
    searchTypes.find((t) => t.value === o.type)?.label ?? o.type,
    o.cat && (categories.find((c) => c.value === o.cat)?.label ?? o.cat),
    o.n && o.n !== String(defaultCount) && `${o.n} results`,
    o.inc && `only ${domains(o.inc).join(', ')}`,
    o.exc && `not ${domains(o.exc).join(', ')}`,
    (o.from || o.to) && `${o.from || '…'} – ${o.to || '…'}`,
  ]
    .filter(Boolean)
    .join(' · ')
}

/** A URL's host, for showing where a result is from. */
export function hostOf(url: string): string {
  try {
    return new URL(url).hostname.replace(/^www\./, '')
  } catch {
    return url
  }
}

export const safeLink = (url: string) => /^https?:\/\//i.test(url)
