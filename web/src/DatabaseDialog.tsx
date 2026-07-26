import { useCallback, useEffect, useState } from 'react'
import { Dialog, DialogBackdrop, DialogPanel, DialogTitle } from '@headlessui/react'
import { ChevronLeft, ChevronRight, Plus, RefreshCw, Table2, Terminal, Trash2 } from 'lucide-react'
import { api, type DbCell, type DbDatabase, type DbRowsPage, type DbTable } from './api'
import { Button, ErrorNote, Select, Spinner, errText } from './ui'

const inputStyle =
  'rounded border border-line bg-surface px-1.5 py-1 font-mono text-xs focus:outline-2 focus:outline-hay-deep'

// Names the create forms accept — plain lowercase identifiers sidestep the
// whole quoting swamp, and match what the addon's own objects use anyway.
const plainName = /^[a-z_][a-z0-9_]*$/

function fmtBytes(n: number): string {
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${Math.round(n / 1024)} KB`
  if (n < 1024 * 1024 * 1024) return `${Math.round(n / 1024 / 1024)} MB`
  return `${(n / 1024 / 1024 / 1024).toFixed(1)} GB`
}

function CellText({ value }: { value: DbCell }) {
  if (value === null) return <span className="italic text-stone">null</span>
  return <>{value}</>
}

/**
 * The database browser: a space service's postgres, one page of rows at a
 * time, edited in place through the server. The binding's database is only
 * the default — the header picks any of the service's databases, or makes a
 * new one. The SQL view is the escape hatch for everything the forms don't
 * do — including tables without a primary key, which the grid refuses to
 * change rather than guess at.
 */
export function DatabaseDialog({
  space,
  service,
  onClose,
}: {
  space: string
  service: string
  onClose: () => void
}) {
  const [databases, setDatabases] = useState<DbDatabase[]>([])
  const [db, setDb] = useState('')
  const [defaultDb, setDefaultDb] = useState('')
  const [newDb, setNewDb] = useState<string | null>(null)
  const [droppingDb, setDroppingDb] = useState(false)
  const [tables, setTables] = useState<DbTable[] | null>(null)
  const [current, setCurrent] = useState<DbTable | null>(null)
  // What the main panel shows: a table, the new-table form, or SQL.
  const [view, setView] = useState<'tables' | 'sql' | 'create'>('tables')
  const [seedSql, setSeedSql] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  useEffect(() => {
    api
      .dbDatabases(space, service)
      .then((res) => {
        setDatabases(res.databases ?? [])
        setDb(res.default)
        setDefaultDb(res.default)
      })
      .catch((err) => setError(errText(err, 'The database did not answer.')))
  }, [space, service])

  const loadTables = useCallback(() => {
    if (!db) return
    api
      .dbTables(space, service, db)
      .then((res) => {
        setTables(res.tables ?? [])
        setError('')
      })
      .catch((err) => setError(errText(err, 'The database did not answer.')))
  }, [space, service, db])

  useEffect(() => {
    loadTables()
  }, [loadTables])

  async function createDatabase() {
    const name = (newDb ?? '').trim()
    if (!plainName.test(name)) {
      setError(`"${name}" won't do as a database name — lowercase letters, digits and underscores.`)
      return
    }
    setBusy(true)
    setError('')
    try {
      await api.dbCreateDatabase(space, service, name)
      const fresh = await api.dbDatabases(space, service)
      setDatabases(fresh.databases ?? [])
      setNewDb(null)
      setDb(name)
      setCurrent(null)
    } catch (err) {
      setError(errText(err, 'Creating the database did not work.'))
    } finally {
      setBusy(false)
    }
  }

  async function dropDatabase() {
    setBusy(true)
    setError('')
    try {
      await api.dbDropDatabase(space, service, db)
      const fresh = await api.dbDatabases(space, service)
      setDatabases(fresh.databases ?? [])
      setDroppingDb(false)
      setDb(defaultDb)
      setCurrent(null)
    } catch (err) {
      setError(errText(err, 'Deleting the database did not work.'))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Dialog open onClose={onClose} className="relative z-50">
      <DialogBackdrop className="fixed inset-0 bg-black/60" />
      <div className="fixed inset-0 flex items-center justify-center p-4">
        <DialogPanel className="flex h-[85vh] w-full max-w-5xl flex-col rounded-lg border border-line bg-card shadow-2xl">
          <div className="flex flex-wrap items-center justify-between gap-3 border-b border-line px-4 py-3">
            <div className="flex items-center gap-3">
              <DialogTitle className="flex items-center gap-2 font-mono text-[11px] font-semibold uppercase tracking-[0.14em] text-stone">
                <Table2 className="size-3.5" aria-hidden />
                {service} · {space}
              </DialogTitle>
              {droppingDb ? (
                <div className="flex items-center gap-1.5">
                  <span className="text-xs text-brick">
                    Delete <span className="font-mono">{db}</span> and everything in it?
                  </span>
                  <Button kind="danger" onClick={dropDatabase} disabled={busy}>
                    {busy ? 'Deleting…' : `Delete ${db}`}
                  </Button>
                  <Button kind="quiet" onClick={() => setDroppingDb(false)} disabled={busy}>
                    ✕
                  </Button>
                </div>
              ) : newDb === null ? (
                <div className="flex items-center gap-1">
                  {databases.length > 0 && db && (
                    <Select
                      size="sm"
                      ariaLabel="Database"
                      value={db}
                      onChange={(d) => {
                        setDb(d)
                        setCurrent(null)
                        setView('tables')
                      }}
                      options={databases.map((d) => ({
                        value: d.name,
                        label: `${d.name} · ${fmtBytes(d.size)}`,
                      }))}
                    />
                  )}
                  <button
                    type="button"
                    title="New database"
                    aria-label="New database"
                    onClick={() => setNewDb('')}
                    className="rounded p-1.5 text-stone hover:bg-line/50 hover:text-ink"
                  >
                    <Plus className="size-3.5" aria-hidden />
                  </button>
                  {db && db !== defaultDb && (
                    <button
                      type="button"
                      title={`Delete the database ${db}`}
                      aria-label={`Delete the database ${db}`}
                      onClick={() => setDroppingDb(true)}
                      className="rounded p-1.5 text-stone hover:bg-brick/10 hover:text-brick"
                    >
                      <Trash2 className="size-3.5" aria-hidden />
                    </button>
                  )}
                </div>
              ) : (
                <form
                  onSubmit={(e) => {
                    e.preventDefault()
                    createDatabase()
                  }}
                  className="flex items-center gap-1.5"
                >
                  <input
                    autoFocus
                    value={newDb}
                    onChange={(e) => setNewDb(e.target.value)}
                    placeholder="new_database"
                    aria-label="New database name"
                    className={`${inputStyle} w-36`}
                  />
                  <Button type="submit" disabled={busy || !newDb.trim()}>
                    {busy ? 'Creating…' : 'Create'}
                  </Button>
                  <Button kind="quiet" onClick={() => setNewDb(null)} disabled={busy}>
                    ✕
                  </Button>
                </form>
              )}
            </div>
            <div className="flex items-center gap-2">
              <Button kind="quiet" onClick={() => setView(view === 'sql' ? 'tables' : 'sql')}>
                <span className="flex items-center gap-1.5">
                  {view === 'sql' ? (
                    <>
                      <Table2 className="size-3.5" aria-hidden /> Tables
                    </>
                  ) : (
                    <>
                      <Terminal className="size-3.5" aria-hidden /> SQL
                    </>
                  )}
                </span>
              </Button>
              <Button kind="quiet" onClick={onClose}>
                Close
              </Button>
            </div>
          </div>
          <div className="px-4 pt-2 empty:hidden">
            <ErrorNote message={error} />
          </div>

          {view === 'sql' ? (
            <SqlView key={`${db}:${seedSql}`} space={space} service={service} db={db} initial={seedSql} onChanged={loadTables} />
          ) : view === 'create' ? (
            <CreateTableView
              space={space}
              service={service}
              db={db}
              onDone={(name) => {
                loadTables()
                setCurrent({ schema: 'public', name, rows: 0 })
                setView('tables')
              }}
              onCancel={() => setView('tables')}
            />
          ) : (
            <div className="flex min-h-0 flex-1">
              <aside className="w-52 shrink-0 overflow-y-auto border-r border-line p-2">
                <div className="flex items-center justify-between px-1 pb-1">
                  <span className="font-mono text-[10px] font-semibold uppercase tracking-[0.14em] text-stone">
                    Tables
                  </span>
                  <div className="flex items-center">
                    <button
                      type="button"
                      onClick={() => setView('create')}
                      title="New table"
                      aria-label="New table"
                      className="rounded p-1 text-stone hover:bg-line/50 hover:text-ink"
                    >
                      <Plus className="size-3" aria-hidden />
                    </button>
                    <button
                      type="button"
                      onClick={loadTables}
                      title="Refresh tables"
                      aria-label="Refresh tables"
                      className="rounded p-1 text-stone hover:bg-line/50"
                    >
                      <RefreshCw className="size-3" aria-hidden />
                    </button>
                  </div>
                </div>
                {tables === null && <Spinner label="Loading…" />}
                {tables !== null && tables.length === 0 && (
                  <p className="px-1 text-xs text-stone">Nothing here yet.</p>
                )}
                {(tables ?? []).map((t) => (
                  <button
                    key={`${t.schema}.${t.name}`}
                    type="button"
                    onClick={() => setCurrent(t)}
                    className={`flex w-full items-baseline justify-between gap-2 rounded-md px-2 py-1.5 text-left font-mono text-xs ${
                      current?.schema === t.schema && current?.name === t.name
                        ? 'bg-hay-wash text-ink'
                        : 'text-ink-soft hover:bg-line/40'
                    }`}
                  >
                    <span className="truncate">
                      {t.schema !== 'public' ? `${t.schema}.` : ''}
                      {t.name}
                    </span>
                    <span className="text-[10px] text-stone">~{t.rows}</span>
                  </button>
                ))}
              </aside>
              {current ? (
                <TableView
                  key={`${db}.${current.schema}.${current.name}`}
                  space={space}
                  service={service}
                  db={db}
                  table={current}
                  onDropped={() => {
                    setCurrent(null)
                    loadTables()
                  }}
                />
              ) : tables !== null && tables.length === 0 ? (
                <div className="flex flex-1 items-center justify-center p-8">
                  <div className="max-w-md text-center">
                    <p className="text-sm text-ink-soft">
                      <span className="font-mono">{db}</span> is empty — brand new, so that's right.
                      Apps that run migrations create their tables on first start, or you can make
                      one now.
                    </p>
                    <div className="mt-4 flex justify-center gap-2">
                      <Button onClick={() => setView('create')}>
                        <span className="flex items-center gap-1">
                          <Plus className="size-3.5" aria-hidden /> New table
                        </span>
                      </Button>
                      <Button
                        kind="quiet"
                        onClick={() => {
                          setSeedSql(
                            'create table notes (\n  id bigint generated always as identity primary key,\n  text text not null,\n  at timestamptz default now()\n);',
                          )
                          setView('sql')
                        }}
                      >
                        Write it in SQL
                      </Button>
                    </div>
                    <p className="mt-4 text-xs text-stone">
                      Only your tables show here — postgres' own bookkeeping (pg_catalog,
                      information_schema) is hidden on purpose.
                    </p>
                  </div>
                </div>
              ) : (
                <div className="flex flex-1 items-center justify-center text-sm text-stone">
                  Pick a table on the left.
                </div>
              )}
            </div>
          )}
        </DialogPanel>
      </div>
    </Dialog>
  )
}

const columnTypes = [
  { value: 'auto', label: 'auto id' },
  { value: 'text', label: 'text' },
  { value: 'integer', label: 'integer' },
  { value: 'bigint', label: 'bigint' },
  { value: 'boolean', label: 'boolean' },
  { value: 'numeric', label: 'numeric' },
  { value: 'timestamptz', label: 'timestamp' },
  { value: 'date', label: 'date' },
  { value: 'jsonb', label: 'json' },
  { value: 'uuid', label: 'uuid' },
]

interface ColumnDraft {
  name: string
  type: string
  pk: boolean
  required: boolean
}

/**
 * A table without SQL: name, columns, done. The form builds the CREATE
 * TABLE itself and runs it through the same query endpoint — names are
 * restricted to plain identifiers, so nothing ever needs escaping.
 */
function CreateTableView({
  space,
  service,
  db,
  onDone,
  onCancel,
}: {
  space: string
  service: string
  db: string
  onDone: (table: string) => void
  onCancel: () => void
}) {
  const [name, setName] = useState('')
  const [columns, setColumns] = useState<ColumnDraft[]>([
    { name: 'id', type: 'auto', pk: true, required: false },
    { name: '', type: 'text', pk: false, required: true },
  ])
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  const patch = (i: number, change: Partial<ColumnDraft>) =>
    setColumns(columns.map((c, ci) => (ci === i ? { ...c, ...change } : c)))

  async function create() {
    if (!plainName.test(name)) {
      setError('The table needs a plain name — lowercase letters, digits and underscores.')
      return
    }
    const used = columns.filter((c) => c.name.trim() !== '')
    if (used.length === 0) {
      setError('The table needs at least one column.')
      return
    }
    for (const c of used) {
      if (!plainName.test(c.name)) {
        setError(`"${c.name}" won't do as a column name — lowercase letters, digits and underscores.`)
        return
      }
    }
    const defs = used.map((c) => {
      const type = c.type === 'auto' ? 'bigint generated always as identity' : c.type
      return `  "${c.name}" ${type}${c.required && c.type !== 'auto' ? ' not null' : ''}`
    })
    const pk = used.filter((c) => c.pk).map((c) => `"${c.name}"`)
    if (pk.length > 0) defs.push(`  primary key (${pk.join(', ')})`)
    const sql = `create table "${name}" (\n${defs.join(',\n')}\n)`
    setBusy(true)
    setError('')
    try {
      await api.dbQuery(space, service, db, sql)
      onDone(name)
    } catch (err) {
      setError(errText(err, 'Creating the table did not work.'))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="flex min-h-0 flex-1 flex-col gap-3 overflow-y-auto p-4">
      <label className="flex flex-col gap-1.5">
        <span className="font-mono text-[10px] font-semibold uppercase tracking-[0.14em] text-stone">
          Table name
        </span>
        <input
          autoFocus
          value={name}
          onChange={(e) => setName(e.target.value)}
          placeholder="notes"
          className={`${inputStyle} w-56 px-2 py-1.5 text-sm`}
        />
      </label>
      <div className="flex flex-col gap-1.5">
        <span className="font-mono text-[10px] font-semibold uppercase tracking-[0.14em] text-stone">
          Columns
        </span>
        <div className="flex flex-col gap-1.5">
          {columns.map((c, i) => (
            <div key={i} className="flex flex-wrap items-center gap-2">
              <input
                value={c.name}
                onChange={(e) => patch(i, { name: e.target.value })}
                placeholder="column_name"
                aria-label={`Column ${i + 1} name`}
                className={`${inputStyle} w-44`}
              />
              <Select
                size="sm"
                ariaLabel={`Column ${i + 1} type`}
                value={c.type}
                onChange={(t) => patch(i, { type: t })}
                options={columnTypes}
              />
              <label className="flex items-center gap-1 text-xs text-ink-soft">
                <input
                  type="checkbox"
                  checked={c.pk}
                  onChange={(e) => patch(i, { pk: e.target.checked })}
                  className="size-3.5 accent-hay-deep"
                />
                key
              </label>
              {c.type !== 'auto' && (
                <label className="flex items-center gap-1 text-xs text-ink-soft">
                  <input
                    type="checkbox"
                    checked={c.required}
                    onChange={(e) => patch(i, { required: e.target.checked })}
                    className="size-3.5 accent-hay-deep"
                  />
                  required
                </label>
              )}
              <button
                type="button"
                title="Remove column"
                aria-label={`Remove column ${i + 1}`}
                onClick={() => setColumns(columns.filter((_, ci) => ci !== i))}
                className="rounded p-1 text-stone hover:bg-brick/10 hover:text-brick"
              >
                <Trash2 className="size-3" aria-hidden />
              </button>
            </div>
          ))}
        </div>
        <div>
          <Button
            kind="quiet"
            onClick={() => setColumns([...columns, { name: '', type: 'text', pk: false, required: false }])}
          >
            <span className="flex items-center gap-1">
              <Plus className="size-3.5" aria-hidden /> Add column
            </span>
          </Button>
        </div>
      </div>
      <ErrorNote message={error} />
      <div className="mt-auto flex items-center justify-between gap-2 border-t border-line pt-3">
        <p className="text-xs text-stone">
          "auto id" counts up by itself; empty column rows are skipped.
        </p>
        <div className="flex gap-2">
          <Button kind="quiet" onClick={onCancel} disabled={busy}>
            Cancel
          </Button>
          <Button onClick={create} disabled={busy || !name.trim()}>
            {busy ? 'Creating…' : 'Create table'}
          </Button>
        </div>
      </div>
    </div>
  )
}

/** One table: a page of rows, edited in place. */
function TableView({
  space,
  service,
  db,
  table,
  onDropped,
}: {
  space: string
  service: string
  db: string
  table: DbTable
  onDropped: () => void
}) {
  const [page, setPage] = useState<DbRowsPage | null>(null)
  const [offset, setOffset] = useState(0)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  // One row is in hand at a time: being edited, confirmed for deletion, or
  // the fresh one being added.
  const [editing, setEditing] = useState<number | null>(null)
  const [deleting, setDeleting] = useState<number | null>(null)
  const [adding, setAdding] = useState(false)
  const [dropping, setDropping] = useState(false)
  const [draft, setDraft] = useState<Record<string, string>>({})

  const limit = 50

  const load = useCallback(() => {
    api
      .dbRows(space, service, db, table.schema, table.name, limit, offset)
      .then((res) => {
        setPage(res)
        setError('')
      })
      .catch((err) => setError(errText(err, 'Loading rows did not work.')))
  }, [space, service, db, table.schema, table.name, offset])

  useEffect(() => {
    load()
  }, [load])

  const editable = (page?.key.length ?? 0) > 0

  function rowKey(row: DbCell[]): Record<string, string> {
    const key: Record<string, string> = {}
    page?.key.forEach((k) => {
      const v = row[page.columns.indexOf(k)]
      if (v !== null) key[k] = v
    })
    return key
  }

  async function run(work: () => Promise<unknown>) {
    setBusy(true)
    setError('')
    try {
      await work()
      setEditing(null)
      setDeleting(null)
      setAdding(false)
      setDraft({})
      load()
    } catch (err) {
      setError(errText(err, 'That did not work.'))
    } finally {
      setBusy(false)
    }
  }

  const save = (row: DbCell[]) => {
    const values: Record<string, string> = {}
    page?.columns.forEach((c, i) => {
      const now = draft[c] ?? ''
      const before = row[i] ?? ''
      if (now !== before && !page.key.includes(c)) values[c] = now
    })
    return run(() =>
      api.dbUpdateRow(space, service, db, table.schema, table.name, { key: rowKey(row), values }),
    )
  }

  const insert = () => {
    // Empty fields stay unsent, so column defaults (auto ids, now()) do
    // their job.
    const values: Record<string, string> = {}
    Object.entries(draft).forEach(([k, v]) => {
      if (v !== '') values[k] = v
    })
    return run(() => api.dbInsertRow(space, service, db, table.schema, table.name, { values }))
  }

  if (page === null) {
    return (
      <div className="flex flex-1 items-center justify-center">
        {error ? <ErrorNote message={error} /> : <Spinner label="Loading rows…" />}
      </div>
    )
  }

  return (
    <div className="flex min-w-0 flex-1 flex-col">
      <div className="flex flex-wrap items-center justify-between gap-2 border-b border-line px-3 py-2">
        <span className="font-mono text-xs text-ink-soft">
          {page.total} {page.total === 1 ? 'row' : 'rows'}
          {!editable && <span className="ml-2 text-stone">read-only (no primary key)</span>}
        </span>
        <div className="flex items-center gap-1.5">
          {dropping ? (
            <>
              <span className="text-xs text-brick">
                Drop <span className="font-mono">{table.name}</span> and its rows?
              </span>
              <Button
                kind="danger"
                disabled={busy}
                onClick={() =>
                  run(() => api.dbDropTable(space, service, db, table.schema, table.name)).then(onDropped)
                }
              >
                {busy ? 'Dropping…' : 'Drop table'}
              </Button>
              <Button kind="quiet" onClick={() => setDropping(false)} disabled={busy}>
                ✕
              </Button>
            </>
          ) : (
            <button
              type="button"
              title={`Drop the table ${table.name}`}
              aria-label={`Drop the table ${table.name}`}
              onClick={() => setDropping(true)}
              className="rounded p-1.5 text-stone hover:bg-brick/10 hover:text-brick"
            >
              <Trash2 className="size-3.5" aria-hidden />
            </button>
          )}
          <Button kind="quiet" onClick={() => setAdding(true)} disabled={busy || adding}>
            <span className="flex items-center gap-1">
              <Plus className="size-3.5" aria-hidden /> Add row
            </span>
          </Button>
          <button
            type="button"
            onClick={load}
            title="Refresh rows"
            aria-label="Refresh rows"
            className="rounded p-1.5 text-stone hover:bg-line/50"
          >
            <RefreshCw className="size-3.5" aria-hidden />
          </button>
          <button
            type="button"
            onClick={() => setOffset(Math.max(0, offset - limit))}
            disabled={offset === 0}
            title="Previous page"
            aria-label="Previous page"
            className="rounded p-1.5 text-stone hover:bg-line/50 disabled:opacity-30"
          >
            <ChevronLeft className="size-3.5" aria-hidden />
          </button>
          <span className="font-mono text-[10px] text-stone">
            {page.total === 0 ? '0' : `${offset + 1}–${Math.min(offset + limit, page.total)}`}
          </span>
          <button
            type="button"
            onClick={() => setOffset(offset + limit)}
            disabled={offset + limit >= page.total}
            title="Next page"
            aria-label="Next page"
            className="rounded p-1.5 text-stone hover:bg-line/50 disabled:opacity-30"
          >
            <ChevronRight className="size-3.5" aria-hidden />
          </button>
        </div>
      </div>
      <div className="px-3 pt-2 empty:hidden">
        <ErrorNote message={error} />
      </div>
      <div className="min-h-0 flex-1 overflow-auto px-3 pb-3">
        <table className="w-full border-separate border-spacing-0 font-mono text-xs">
          <thead className="sticky top-0 z-10">
            <tr>
              {page.columns.map((c) => (
                <th
                  key={c}
                  className="border-b border-line bg-card px-2 py-1.5 text-left font-semibold text-stone"
                >
                  {c}
                  {page.key.includes(c) && (
                    <span className="ml-1 text-[9px] text-hay" title="primary key">
                      key
                    </span>
                  )}
                </th>
              ))}
              <th className="w-16 border-b border-line bg-card" />
            </tr>
          </thead>
          <tbody>
            {adding && (
              <tr className="bg-hay-wash/50">
                {page.columns.map((c) => (
                  <td key={c} className="border-b border-line/50 px-1 py-1">
                    <input
                      value={draft[c] ?? ''}
                      onChange={(e) => setDraft({ ...draft, [c]: e.target.value })}
                      placeholder="default"
                      aria-label={`New ${c}`}
                      className={`${inputStyle} w-full`}
                    />
                  </td>
                ))}
                <td className="border-b border-line/50 px-1 py-1">
                  <div className="flex gap-1">
                    <Button onClick={insert} disabled={busy}>
                      Save
                    </Button>
                    <Button
                      kind="quiet"
                      onClick={() => {
                        setAdding(false)
                        setDraft({})
                      }}
                      disabled={busy}
                    >
                      ✕
                    </Button>
                  </div>
                </td>
              </tr>
            )}
            {page.rows.map((row, i) => (
              <tr key={i} className="group/row hover:bg-line/20">
                {page.columns.map((c, ci) => (
                  <td key={c} className="max-w-64 truncate border-b border-line/50 px-2 py-1 text-ink-soft">
                    {editing === i && !page.key.includes(c) ? (
                      <input
                        value={draft[c] ?? ''}
                        onChange={(e) => setDraft({ ...draft, [c]: e.target.value })}
                        aria-label={c}
                        className={`${inputStyle} w-full`}
                      />
                    ) : (
                      <span title={row[ci] ?? undefined}>
                        <CellText value={row[ci]} />
                      </span>
                    )}
                  </td>
                ))}
                <td className="border-b border-line/50 px-1 py-1">
                  {editing === i ? (
                    <div className="flex gap-1">
                      <Button onClick={() => save(row)} disabled={busy}>
                        Save
                      </Button>
                      <Button kind="quiet" onClick={() => setEditing(null)} disabled={busy}>
                        ✕
                      </Button>
                    </div>
                  ) : deleting === i ? (
                    <div className="flex gap-1">
                      <Button
                        kind="danger"
                        onClick={() =>
                          run(() =>
                            api.dbDeleteRow(space, service, db, table.schema, table.name, {
                              key: rowKey(row),
                            }),
                          )
                        }
                        disabled={busy}
                      >
                        Delete
                      </Button>
                      <Button kind="quiet" onClick={() => setDeleting(null)} disabled={busy}>
                        ✕
                      </Button>
                    </div>
                  ) : (
                    editable && (
                      <div className="hidden gap-0.5 group-hover/row:flex">
                        <button
                          type="button"
                          title="Edit row"
                          aria-label="Edit row"
                          onClick={() => {
                            setEditing(i)
                            setDeleting(null)
                            const d: Record<string, string> = {}
                            page.columns.forEach((c, ci) => {
                              d[c] = row[ci] ?? ''
                            })
                            setDraft(d)
                          }}
                          className="rounded p-1 text-stone hover:bg-line/50 hover:text-ink"
                        >
                          ✎
                        </button>
                        <button
                          type="button"
                          title="Delete row"
                          aria-label="Delete row"
                          onClick={() => {
                            setDeleting(i)
                            setEditing(null)
                          }}
                          className="rounded p-1 text-stone hover:bg-brick/10 hover:text-brick"
                        >
                          <Trash2 className="size-3" aria-hidden />
                        </button>
                      </div>
                    )
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
        {page.rows.length === 0 && !adding && (
          <p className="py-8 text-center text-sm text-stone">No rows yet — Add row puts the first one in.</p>
        )}
      </div>
    </div>
  )
}

/** The escape hatch: one statement, run as written. */
function SqlView({
  space,
  service,
  db,
  initial = '',
  onChanged,
}: {
  space: string
  service: string
  db: string
  initial?: string
  onChanged: () => void
}) {
  const [sql, setSql] = useState(initial)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [result, setResult] = useState<Awaited<ReturnType<typeof api.dbQuery>> | null>(null)

  async function runSql() {
    if (!sql.trim()) return
    setBusy(true)
    setError('')
    try {
      setResult(await api.dbQuery(space, service, db, sql))
      // A CREATE/DROP just changed what the sidebar shows.
      onChanged()
    } catch (err) {
      setError(errText(err, 'The statement failed.'))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="flex min-h-0 flex-1 flex-col gap-2 p-3">
      <textarea
        value={sql}
        onChange={(e) => setSql(e.target.value)}
        onKeyDown={(e) => {
          if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') runSql()
        }}
        placeholder={`select * from … — runs against ${db}`}
        aria-label="SQL statement"
        spellCheck={false}
        className="h-28 shrink-0 resize-y rounded-md border border-line bg-surface p-2 font-mono text-xs focus:outline-2 focus:outline-hay-deep"
      />
      <div className="flex items-center justify-between gap-2">
        <p className="text-xs text-stone">Runs exactly what you wrote, against {db} — ⌘↵ works too.</p>
        <Button onClick={runSql} disabled={busy || !sql.trim()}>
          {busy ? 'Running…' : 'Run'}
        </Button>
      </div>
      <ErrorNote message={error} />
      {result && (
        <div className="min-h-0 flex-1 overflow-auto">
          {result.columns.length === 0 || result.rows.length === 0 ? (
            <p className="text-sm text-ink-soft">
              Done — {result.affected} {result.affected === 1 ? 'row' : 'rows'} affected.
            </p>
          ) : (
            <>
              {result.truncated && (
                <p className="pb-1 text-xs text-stone">Showing the first {result.rows.length} rows.</p>
              )}
              <table className="w-full border-separate border-spacing-0 font-mono text-xs">
                <thead className="sticky top-0">
                  <tr>
                    {result.columns.map((c) => (
                      <th
                        key={c}
                        className="border-b border-line bg-card px-2 py-1.5 text-left font-semibold text-stone"
                      >
                        {c}
                      </th>
                    ))}
                  </tr>
                </thead>
                <tbody>
                  {result.rows.map((row, i) => (
                    <tr key={i}>
                      {row.map((v, vi) => (
                        <td key={vi} className="max-w-64 truncate border-b border-line/50 px-2 py-1 text-ink-soft">
                          <CellText value={v} />
                        </td>
                      ))}
                    </tr>
                  ))}
                </tbody>
              </table>
            </>
          )}
        </div>
      )}
    </div>
  )
}
