// The SQL consoles' data layer: the database, Trino and query calls beside
// their TanStack Query options. Keys start with ['sql', space, service], so
// one invalidation refreshes everything a statement may have changed.
// Postgres, MySQL and Trino share the routes; the server picks the dialect.
import { keepPreviousData, queryOptions, useMutation, useQueryClient } from '@tanstack/react-query'
import { ApiError, request, servicePath, type DbDatabase, type DbQueryResult } from './api'
import type { SqlCell } from './sqlText'
import { errText } from './format'

export type SqlTable = { schema: string; name: string; kind?: 'table' | 'view'; rows: number | null }

export type SqlColumn = {
  name: string
  type: string
  /** Null where the dialect does not say (Trino). */
  nullable: boolean | null
  default: string | null
  key: boolean
  extra?: string
  comment?: string
}

export type SqlIndex = { name: string; columns: string[]; unique: boolean; primary: boolean; definition?: string }

export type SqlStructure = {
  columns: SqlColumn[]
  indexes: SqlIndex[]
  kind: 'table' | 'view'
  rows: number | null
  size: number | null
}

export type SqlFilterOp = 'eq' | 'ne' | 'lt' | 'le' | 'gt' | 'ge' | 'contains' | 'starts' | 'null' | 'notnull'

export type SqlFilter = { column: string; op: SqlFilterOp; value: string }

/**
 * One page of a table. `count` says how far `total` is to be trusted: the
 * server counts exactly up to a cap, then estimates or says "at least".
 * Trino does not count at all (total null).
 */
export type SqlPage = {
  columns: string[]
  key: string[]
  rows: SqlCell[][]
  total: number | null
  count?: 'exact' | 'estimated' | 'atLeast'
  limit: number
  offset: number
  hasMore: boolean
}

export type SqlPageView = { limit: number; offset: number; sort?: string; desc?: boolean; filter?: SqlFilter }

// Where a table lives. Postgres has schemas; for MySQL the schema is the
// database itself; Trino has catalog (db) and schema.
export type SqlPlace = { space: string; service: string; db: string; schema: string }

const dbPath = (space: string, service: string, db: string, suffix = '') =>
  servicePath(space, service, `/databases/${encodeURIComponent(db)}${suffix}`)

const tablePath = (p: SqlPlace, table: string, suffix = '', params: Record<string, string> = {}) =>
  dbPath(p.space, p.service, p.db, `/tables/${encodeURIComponent(table)}${suffix}`) +
  `?${new URLSearchParams({ ...(p.schema ? { schema: p.schema } : {}), ...params })}`

// A 4xx is the request's own fault — a bad filter value, a table that is
// gone — and asking again will not change the answer.
const retry = (failures: number, err: unknown) => !(err instanceof ApiError && err.status < 500) && failures < 1

export const sqlKey = (space: string, service: string) => ['sql', space, service] as const

export const sqlDatabasesQuery = (space: string, service: string) =>
  queryOptions({
    queryKey: [...sqlKey(space, service), 'databases'],
    queryFn: ({ signal }) =>
      request<{ databases: DbDatabase[]; default: string; protocol: string }>(servicePath(space, service, '/databases'), {
        signal,
      }),
    retry,
  })

export const sqlSchemasQuery = (space: string, service: string, db: string) =>
  queryOptions({
    queryKey: [...sqlKey(space, service), 'schemas', db],
    queryFn: ({ signal }) => request<{ schemas: string[]; default: string }>(dbPath(space, service, db, '/schemas'), { signal }),
    retry,
  })

export const sqlTablesQuery = (space: string, service: string, db: string, schema: string) =>
  queryOptions({
    queryKey: [...sqlKey(space, service), 'tables', db, schema],
    queryFn: ({ signal }) =>
      request<{ tables: SqlTable[] }>(
        dbPath(space, service, db, `/tables${schema ? `?${new URLSearchParams({ schema })}` : ''}`),
        { signal },
      ),
    retry,
  })

export const sqlStructureQuery = (p: SqlPlace, table: string) =>
  queryOptions({
    queryKey: [...sqlKey(p.space, p.service), 'structure', p.db, p.schema, table],
    queryFn: ({ signal }) => request<SqlStructure>(tablePath(p, table), { signal }),
    retry,
  })

const rowsKey = (p: SqlPlace, table: string) => [...sqlKey(p.space, p.service), 'rows', p.db, p.schema, table] as const

const sameTable = (key: readonly unknown[], table: readonly unknown[]) => table.every((part, i) => key[i] === part)

export const sqlRowsQuery = (p: SqlPlace, table: string, view: SqlPageView) =>
  queryOptions({
    queryKey: [...rowsKey(p, table), view],
    queryFn: ({ signal }) => {
      const params: Record<string, string> = { limit: String(view.limit), offset: String(view.offset) }
      if (view.sort) Object.assign(params, { order: view.sort, dir: view.desc ? 'desc' : 'asc' })
      if (view.filter) Object.assign(params, { where: view.filter.column, op: view.filter.op, value: view.filter.value })
      return request<SqlPage>(tablePath(p, table, '/rows', params), { signal })
    },
    // Another page, order or filter of the same table keeps the rows on
    // screen until the new ones arrive; another table starts clean.
    placeholderData: (previous, query) =>
      query && sameTable(query.queryKey, rowsKey(p, table)) ? keepPreviousData(previous) : undefined,
    retry,
  })

/** Runs one statement against a database (or Trino catalog) and schema. */
export function runSql(space: string, service: string, db: string, schema: string, sql: string, signal?: AbortSignal) {
  return request<DbQueryResult>(servicePath(space, service, db ? `/databases/${encodeURIComponent(db)}/query` : '/query'), {
    method: 'POST',
    body: JSON.stringify({ sql, schema: schema || undefined }),
    signal,
  })
}

// ─── Writes ─────────────────────────────────────────────────────────────

/** A mutation's failure in the consoles' voice, or ''. */
export const failure = (m: { error: unknown }, fallback: string) => (m.error ? errText(m.error, fallback) : '')

export function useSqlCreateDatabase(space: string, service: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (name: string) =>
      request<{ ok: boolean; name: string }>(servicePath(space, service, '/databases'), {
        method: 'POST',
        body: JSON.stringify({ name }),
      }),
    onSuccess: () => qc.invalidateQueries({ queryKey: sqlDatabasesQuery(space, service).queryKey }),
  })
}

// Only the list is refreshed: what was open in the dropped database is still
// on screen until the caller moves away, and asking it again would fail.
export function useSqlDropDatabase(space: string, service: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (db: string) => request<{ ok: boolean }>(dbPath(space, service, db), { method: 'DELETE' }),
    onSuccess: () => qc.invalidateQueries({ queryKey: sqlDatabasesQuery(space, service).queryKey }),
  })
}

export function useSqlDropTable(p: SqlPlace) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (table: string) => request<{ ok: boolean }>(tablePath(p, table), { method: 'DELETE' }),
    onSuccess: () => qc.invalidateQueries({ queryKey: [...sqlKey(p.space, p.service), 'tables', p.db] }),
  })
}

/** What a row write sends: the key names the row, values the columns (null writes NULL). */
export type SqlRowChange = { key?: Record<string, string>; values?: Record<string, string | null> }

/**
 * Row writes. An insert or a delete refreshes the table's pages; an edit
 * patches the page it was made on — the grid must not reload, or re-sort,
 * under the cursor — and leaves the table's other cached pages to refetch
 * when they are next shown.
 */
export function useSqlRowWrite(p: SqlPlace, table: string, page: SqlPageView) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: ({ method, change }: { method: 'POST' | 'PUT' | 'DELETE'; change: SqlRowChange; row?: number }) =>
      request<{ ok: boolean; affected?: number }>(tablePath(p, table, '/rows'), {
        method,
        body: JSON.stringify(change),
      }),
    onSuccess: (_, { method, change, row }) => {
      if (method !== 'PUT' || row === undefined) {
        void qc.invalidateQueries({ queryKey: rowsKey(p, table) })
        return
      }
      void qc.invalidateQueries({ queryKey: rowsKey(p, table), refetchType: 'none' })
      qc.setQueryData(sqlRowsQuery(p, table, page).queryKey, (current) =>
        current && {
          ...current,
          rows: current.rows.map((r, i) =>
            i === row ? r.map((v, ci) => (Object.hasOwn(change.values ?? {}, current.columns[ci]) ? change.values![current.columns[ci]] : v)) : r,
          ),
        },
      )
    },
  })
}
