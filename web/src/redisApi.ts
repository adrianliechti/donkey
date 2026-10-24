// The key browser's calls and their caches. The server reads every value a
// page at a time and cuts long elements short (see redisadmin.go), so what
// arrives here is bounded however large the key is.
import {
  infiniteQueryOptions,
  queryOptions,
  useInfiniteQuery,
  useMutation,
  useQuery,
  useQueryClient,
  type InfiniteData,
  type QueryClient,
} from '@tanstack/react-query'
import { request, servicePath } from './api'
import { humanSize } from './format'

export const redisTypes = ['string', 'hash', 'list', 'set', 'zset', 'stream'] as const
export type RedisType = (typeof redisTypes)[number]

/**
 * One string of a value: its text, {base64} when the bytes are not text,
 * and either one cut short ({cut, size}) when it was too long to bring
 * whole.
 */
export type RedisBlob =
  | string
  | { base64: string; cut?: undefined }
  | { cut: true; size: number; text?: string; base64?: string }

export interface RedisKeyEntry {
  id: string
  name: string
  type: string
  /** Seconds left when listed; -1 for no expiry. */
  ttl: number
  /** When it expires (ms), or null — counted from when the page arrived. */
  expiresAt: number | null
}

export interface RedisKeyPage {
  keys: RedisKeyEntry[]
  cursor: string
  db: number
}

export interface RedisDatabases {
  /** The database the binding names — the one apps use. */
  default: number
  /** How many there are, or 0 when the server would not say. */
  count: number
  /** The ones holding anything, with their key counts. */
  databases: { db: number; keys: number }[]
}

export type RedisHashItem = { field: RedisBlob; value: RedisBlob }
export type RedisListItem = { index: number; value: RedisBlob }
export type RedisSetItem = { member: RedisBlob }
export type RedisZsetItem = { member: RedisBlob; score: string }
export type RedisStreamItem = { id: string; fields: RedisHashItem[] }

export interface RedisValuePage {
  type: string
  ttl: number
  /** When the key expires (ms), or null; see RedisKeyEntry. */
  expiresAt: number | null
  size: number
  /** Where the next page starts, or '' when this was the last. */
  next: string
  /** Some element of this page arrived cut short. */
  truncated: boolean
  /** A string that arrived whole and as text. */
  editable: boolean
  value?: RedisBlob
  items?: unknown[]
}

/** An element change; see handleRedisChange for what each op takes. */
export type RedisChange =
  | { op: 'expire'; ttl: number }
  | { op: 'persist' }
  | { op: 'rename'; name: string }
  | { op: 'hset'; field: RedisRef; value: string; previous?: RedisRef; create?: boolean }
  | { op: 'hdel'; field: RedisRef }
  | { op: 'lpush' | 'rpush'; value: string }
  | { op: 'lset'; index: number; previous: RedisRef; value: string }
  | { op: 'lrem'; index: number; previous: RedisRef }
  | { op: 'sadd' | 'srem' | 'zrem'; member: RedisRef }
  | { op: 'zadd'; member: RedisRef; score: string; create?: boolean }
  | { op: 'xadd'; fields: [string, string][] }
  | { op: 'xdel'; entry: string }

/** What addresses an element on the way back: its text, or its bytes. */
export type RedisRef = string | { base64: string }

export interface RedisNewKey {
  name: string
  type: RedisType
  value?: string
  items?: string[][]
  ttl?: number
}

// ─── Words ─────────────────────────────────────────────────────────────

export const typeLabel: Record<string, string> = {
  string: 'String',
  hash: 'Hash',
  list: 'List',
  set: 'Set',
  zset: 'Sorted set',
  stream: 'Stream',
}

const shortNames: Record<string, string> = { string: 'str', stream: 'strm' }

/** The type as its badge says it. */
export const typeShort = (type: string) => shortNames[type] ?? type.slice(0, 4).toLowerCase()

const plural = (n: number, one: string, many = `${one}s`) => `${n.toLocaleString()} ${n === 1 ? one : many}`

/** What a value holds, in its own unit: bytes, fields, items, members. */
export function redisSummary(type: string, size: number): string {
  switch (type) {
    case 'string':
      return humanSize(size)
    case 'hash':
      return plural(size, 'field')
    case 'list':
      return plural(size, 'item')
    case 'set':
    case 'zset':
      return plural(size, 'member')
    case 'stream':
      return plural(size, 'entry', 'entries')
  }
  return type
}

/** A countdown in the largest units that matter: "4m 12s", "3h 5m", "2d". */
export function redisDuration(seconds: number): string {
  const two = (big: number, bigUnit: string, small: number, smallUnit: string) =>
    small ? `${big}${bigUnit} ${small}${smallUnit}` : `${big}${bigUnit}`
  if (seconds < 60) return `${seconds}s`
  if (seconds < 3600) return two(Math.floor(seconds / 60), 'm', seconds % 60, 's')
  if (seconds < 86400) return two(Math.floor(seconds / 3600), 'h', Math.floor((seconds % 3600) / 60), 'm')
  return two(Math.floor(seconds / 86400), 'd', Math.floor((seconds % 86400) / 3600), 'h')
}

// ─── JSON ──────────────────────────────────────────────────────────────
//
// A string value (and the bucket preview's .json) is shown re-spaced, and
// saved back compact when it was compact. Both work on the text itself:
// strings and numbers are copied as they are, so a big number is never
// rounded on its way through a parse.

export function looksLikeJson(text: string): boolean {
  const t = text.trim()
  if (!(t.startsWith('{') || t.startsWith('['))) return false
  try {
    JSON.parse(t)
    return true
  } catch {
    return false
  }
}

// JSON re-spaced as text: strings and numbers are copied as they are, only
// the whitespace between tokens changes.
export function prettyJson(text: string): string {
  let out = ''
  let depth = 0
  let inString = false
  const pad = () => '\n' + '  '.repeat(depth)
  for (let i = 0; i < text.length; i++) {
    const c = text[i]
    if (inString) {
      out += c
      if (c === '\\') out += text[++i] ?? ''
      else if (c === '"') inString = false
      continue
    }
    if (c === '"') {
      inString = true
      out += c
    } else if (c === '{' || c === '[') {
      // An empty object or array stays on its line.
      let j = i + 1
      while (j < text.length && /\s/.test(text[j])) j++
      if (text[j] === (c === '{' ? '}' : ']')) {
        out += c + text[j]
        i = j
      } else {
        depth++
        out += c + pad()
      }
    } else if (c === '}' || c === ']') {
      depth--
      out += pad() + c
    } else if (c === ',') {
      out += ',' + pad()
    } else if (c === ':') {
      out += ': '
    } else if (!/\s/.test(c)) {
      out += c
    }
  }
  return out
}

export function compactJson(text: string): string {
  let out = ''
  let inString = false
  for (let i = 0; i < text.length; i++) {
    const c = text[i]
    if (inString) {
      out += c
      if (c === '\\') out += text[++i] ?? ''
      else if (c === '"') inString = false
    } else if (c === '"') {
      inString = true
      out += c
    } else if (!/\s/.test(c)) {
      out += c
    }
  }
  return out
}

// ─── Blobs ─────────────────────────────────────────────────────────────

/** The element's text when it arrived whole and as text. */
export function blobText(b: RedisBlob): string | null {
  return typeof b === 'string' ? b : null
}

export function blobCut(b: RedisBlob): boolean {
  return typeof b !== 'string' && b.cut === true
}

/** Its full size in bytes, as far as the console can tell. */
export function blobSize(b: RedisBlob): number {
  if (typeof b === 'string') return new TextEncoder().encode(b).length
  if (b.cut) return b.size
  return atob(b.base64).length
}

/** What to send back to name this element, or null when it was cut. */
export function blobRef(b: RedisBlob): RedisRef | null {
  if (typeof b === 'string') return b
  if (b.cut) return null
  return { base64: b.base64 }
}

/** The shown text and whether it is base64 rather than the value itself. */
export function blobShown(b: RedisBlob): { text: string; binary: boolean } {
  if (typeof b === 'string') return { text: b, binary: false }
  if (b.cut && b.text !== undefined) return { text: b.text, binary: false }
  return { text: b.base64 ?? '', binary: true }
}

// ─── Key IDs ───────────────────────────────────────────────────────────

// A key travels as the base64url of its bytes, so a name JSON cannot carry
// still round-trips. The URL keeps the readable name when there is one.
export function redisId(name: string): string {
  const bytes = new TextEncoder().encode(name)
  let binary = ''
  for (const b of bytes) binary += String.fromCharCode(b)
  return btoa(binary).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')
}

/** The key's name as the console shows it: base64 when it is not text. */
export function redisLabel(id: string): string {
  const name = redisNameOf(id)
  if (name !== null) return name
  return `base64:${id.replace(/-/g, '+').replace(/_/g, '/')}${'='.repeat((4 - (id.length % 4)) % 4)}`
}

/** The key's name when its bytes are text, else null. */
export function redisNameOf(id: string): string | null {
  try {
    const binary = atob(id.replace(/-/g, '+').replace(/_/g, '/'))
    const bytes = Uint8Array.from(binary, (c) => c.charCodeAt(0))
    return new TextDecoder('utf-8', { fatal: true }).decode(bytes)
  } catch {
    return null
  }
}

// ─── Calls ─────────────────────────────────────────────────────────────

// `db` is the picked database; undefined is the binding's own.
const keysPath = (space: string, service: string, suffix: string, params: Record<string, string | undefined>) => {
  const query = new URLSearchParams()
  for (const [k, v] of Object.entries(params)) if (v !== undefined) query.set(k, v)
  const qs = query.toString()
  return servicePath(space, service, `/keys${suffix}${qs ? `?${qs}` : ''}`)
}

// A TTL is seconds from the moment it was read; the console keeps the
// deadline, so a countdown stays right however old the page is.
const expiry = (ttl: number) => (ttl < 0 ? null : Date.now() + ttl * 1000)

export const redisApi = {
  keys: async (space: string, service: string, db: string | undefined, pattern: string, type: string, cursor: string, signal?: AbortSignal) => {
    const page = await request<RedisKeyPage>(keysPath(space, service, '', { db, pattern, type: type || undefined, cursor }), { signal })
    return { ...page, keys: page.keys.map((k) => ({ ...k, expiresAt: expiry(k.ttl) })) }
  },
  databases: (space: string, service: string) => request<RedisDatabases>(keysPath(space, service, '/databases', {})),
  value: async (space: string, service: string, db: string | undefined, id: string, at: string, signal?: AbortSignal) => {
    const page = await request<RedisValuePage>(keysPath(space, service, '/value', { db, id, at: at || undefined }), { signal })
    return { ...page, expiresAt: expiry(page.ttl) }
  },
  create: (space: string, service: string, db: string | undefined, key: RedisNewKey) =>
    request<{ ok: boolean; id: string; name: string }>(keysPath(space, service, '', { db }), {
      method: 'POST',
      body: JSON.stringify(key),
    }),
  saveString: (space: string, service: string, db: string | undefined, id: string, value: string, previous: string) =>
    request<{ ok: boolean }>(keysPath(space, service, '/value', { db, id }), {
      method: 'PUT',
      body: JSON.stringify({ value, previous }),
    }),
  change: (space: string, service: string, db: string | undefined, id: string, change: RedisChange) =>
    request<{ ok: boolean; exists: boolean; id?: string; name?: string }>(keysPath(space, service, '/value', { db, id }), {
      method: 'PATCH',
      body: JSON.stringify(change),
    }),
  remove: (space: string, service: string, db: string | undefined, id: string) =>
    request<{ ok: boolean }>(keysPath(space, service, '/value', { db, id }), { method: 'DELETE' }),
}

// ─── Queries ───────────────────────────────────────────────────────────

const root = (space: string, service: string) => ['redis', space, service] as const
const dbKey = (db: string | undefined) => db ?? 'default'

export const redisDatabasesQuery = (space: string, service: string) =>
  queryOptions({
    queryKey: [...root(space, service), 'databases'],
    queryFn: () => redisApi.databases(space, service),
    staleTime: 30_000,
  })

// A page of keys is one bounded SCAN; "load more" carries its cursor on.
export const redisKeysQuery = (space: string, service: string, db: string | undefined, pattern: string, type: string) =>
  infiniteQueryOptions({
    queryKey: [...root(space, service), 'keys', dbKey(db), pattern, type],
    queryFn: ({ pageParam, signal }) => redisApi.keys(space, service, db, pattern, type, pageParam, signal),
    initialPageParam: '0',
    getNextPageParam: (last) => (last.cursor === '0' ? undefined : last.cursor),
  })

export const redisValueQuery = (space: string, service: string, db: string | undefined, id: string) =>
  infiniteQueryOptions({
    queryKey: [...root(space, service), 'value', dbKey(db), id],
    queryFn: ({ pageParam, signal }) => redisApi.value(space, service, db, id, pageParam, signal),
    initialPageParam: '',
    getNextPageParam: (last) => last.next || undefined,
    // A key that is gone stays gone; asking again only repeats the 404.
    retry: (count, err) => (err as { status?: number }).status !== 404 && count < 2,
  })

export function useRedisDatabases(space: string, service: string) {
  return useQuery(redisDatabasesQuery(space, service))
}

export function useRedisKeys(space: string, service: string, db: string | undefined, pattern: string, type: string) {
  return useInfiniteQuery(redisKeysQuery(space, service, db, pattern, type))
}

export function useRedisValue(space: string, service: string, db: string | undefined, id: string) {
  return useInfiniteQuery(redisValueQuery(space, service, db, id))
}

// The listed keys of a database, edited in place: deleting or renaming one
// key must not rescan every page the sidebar has loaded.
function patchKeys(qc: QueryClient, space: string, service: string, db: string | undefined, fn: (keys: RedisKeyEntry[]) => RedisKeyEntry[]) {
  qc.setQueriesData<InfiniteData<RedisKeyPage>>({ queryKey: [...root(space, service), 'keys', dbKey(db)] }, (data) =>
    data ? { ...data, pages: data.pages.map((page) => ({ ...page, keys: fn(page.keys) })) } : data,
  )
}

/** Every write to a key, with the caches it touches kept in step. */
export function useRedisWrites(space: string, service: string, db: string | undefined) {
  const qc = useQueryClient()
  const valueKey = (id: string) => [...root(space, service), 'value', dbKey(db), id]
  const refresh = (id: string) => qc.invalidateQueries({ queryKey: valueKey(id) })
  // A key that is gone leaves the list. Its value is dropped when the view
  // closes with it, or read again when the view stays — to say it is gone.
  const forget = (id: string, open: boolean) => {
    patchKeys(qc, space, service, db, (keys) => keys.filter((k) => k.id !== id))
    if (open) qc.invalidateQueries({ queryKey: valueKey(id) })
    else qc.removeQueries({ queryKey: valueKey(id) })
    qc.invalidateQueries({ queryKey: redisDatabasesQuery(space, service).queryKey })
  }

  const change = useMutation({
    mutationFn: ({ id, change }: { id: string; change: RedisChange }) => redisApi.change(space, service, db, id, change),
    onSuccess: (result, { id, change }) => {
      if (change.op === 'rename' && result.id && result.name !== undefined) {
        const renamed = { id: result.id, name: result.name }
        patchKeys(qc, space, service, db, (keys) => keys.map((k) => (k.id === id ? { ...k, ...renamed } : k)))
        qc.removeQueries({ queryKey: valueKey(id) })
        return
      }
      // A collection goes with its last element.
      if (!result.exists) return forget(id, true)
      if (change.op === 'expire' || change.op === 'persist') {
        const ttl = change.op === 'expire' ? change.ttl : -1
        const expiresAt = expiry(ttl)
        patchKeys(qc, space, service, db, (keys) => keys.map((k) => (k.id === id ? { ...k, ttl, expiresAt } : k)))
      }
      return refresh(id)
    },
  })

  const saveString = useMutation({
    mutationFn: ({ id, value, previous }: { id: string; value: string; previous: string }) =>
      redisApi.saveString(space, service, db, id, value, previous),
    onSuccess: (_, { id }) => refresh(id),
  })

  const create = useMutation({
    mutationFn: (key: RedisNewKey) => redisApi.create(space, service, db, key),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: [...root(space, service), 'keys', dbKey(db)] })
      qc.invalidateQueries({ queryKey: redisDatabasesQuery(space, service).queryKey })
    },
  })

  const remove = useMutation({
    mutationFn: (id: string) => redisApi.remove(space, service, db, id),
    onSuccess: (_, id) => forget(id, false),
  })

  return { change, saveString, create, remove, refresh }
}
