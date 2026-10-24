import { useCallback, useEffect, useState } from 'react'
import {
  getCoreRowModel,
  useReactTable,
  type ColumnDef,
  type Row,
  type SortingState,
} from '@tanstack/react-table'
import { Check, ChevronLeft, ChevronRight, Plus, RefreshCw, Terminal, Trash2, X } from 'lucide-react'
import { api, type DbCell, type DbDatabase, type DbRowsPage, type DbTable } from './api'
import { CellText, SqlView } from './SqlView'
import { DataGrid } from './DataGrid'
import { Modal, ModalBody, ModalHeader, ModalNotice, SplitPane } from './dialog'
import { Button, ErrorNote, Eyebrow, Field, IconButton, Input, Select, Spinner, errText } from './ui'


// Names the create forms accept — plain lowercase identifiers sidestep the
// whole quoting swamp, and match what the addon's own objects use anyway.
const plainName = /^[a-z_][a-z0-9_]*$/

function fmtBytes(n: number): string {
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${Math.round(n / 1024)} KB`
  if (n < 1024 * 1024 * 1024) return `${Math.round(n / 1024 / 1024)} MB`
  return `${(n / 1024 / 1024 / 1024).toFixed(1)} GB`
}

/**
 * A cell that is quietly always an input — it reads as text until focused,
 * commits its own change on blur, reverts on Escape (or when the server
 * says no). No edit mode, no buttons: the grid behaves like a spreadsheet.
 */
function EditableCell({
  initial,
  isNull,
  label,
  onCommit,
}: {
  initial: string
  isNull: boolean
  label: string
  onCommit: (value: string) => Promise<boolean>
}) {
  const [value, setValue] = useState(initial)
  useEffect(() => {
    setValue(initial)
  }, [initial])

  return (
    <input
      value={value}
      onChange={(e) => setValue(e.target.value)}
      onBlur={async () => {
        if (value === initial) return
        if (!(await onCommit(value))) setValue(initial)
      }}
      onKeyDown={(e) => {
        if (e.key === 'Escape') {
          // This Escape is ours (revert the cell) — without stopping it,
          // the dialog underneath reads it as "close".
          e.preventDefault()
          e.stopPropagation()
          setValue(initial)
          e.currentTarget.blur()
        }
        if (e.key === 'Enter') e.currentTarget.blur()
      }}
      placeholder={isNull ? 'null' : undefined}
      aria-label={label}
      title={isNull && value === '' ? 'null — type to set a value' : undefined}
      className="w-full border-b border-transparent bg-transparent font-mono text-xs text-ink-soft outline-none placeholder:italic placeholder:text-muted focus:border-accent-strong focus:text-ink"
    />
  )
}

/**
 * The database browser: a space service's database, one page of rows at a
 * time, edited in place through the server. The binding's database is only
 * the default — the header picks any of the service's databases, or makes a
 * new one. The SQL view is the escape hatch for everything the forms don't
 * do — including tables without a primary key, which the grid refuses to
 * change rather than guess at.
 */
export function DatabaseDialog({
  space,
  service,
  protocol: initialProtocol = 'postgres',
  onClose,
  onDelete,
}: {
  space: string
  service: string
  /** A first guess from the listing; the server's own answer wins once the
   *  databases load. */
  protocol?: string
  onClose: () => void
  onDelete: () => void
}) {
  const [protocol, setProtocol] = useState(initialProtocol)
  const mysql = protocol === 'mysql'
  const [databases, setDatabases] = useState<DbDatabase[]>([])
  const [db, setDb] = useState('')
  const [defaultDb, setDefaultDb] = useState('')
  const [newDb, setNewDb] = useState<string | null>(null)
  const [droppingDb, setDroppingDb] = useState(false)
  const [tables, setTables] = useState<DbTable[] | null>(null)
  const [current, setCurrent] = useState<DbTable | null>(null)
  const [droppingTable, setDroppingTable] = useState<DbTable | null>(null)
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
        if (res.protocol) setProtocol(res.protocol)
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

  async function dropTable() {
    const t = droppingTable
    if (!t) return
    setBusy(true)
    setError('')
    try {
      await api.dbDropTable(space, service, db, t.schema, t.name)
      setDroppingTable(null)
      if (current?.schema === t.schema && current?.name === t.name) setCurrent(null)
      loadTables()
    } catch (err) {
      setError(errText(err, 'Dropping the table did not work.'))
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
    <Modal onClose={onClose} size="xl" height="fill">
      <ModalHeader eyebrow={`${mysql ? 'MySQL' : 'Postgres'} · ${space}`} title={<span className="font-mono">{service}</span>}
        actions={<>
          <IconButton
            icon={Terminal}
            label={view === 'sql' ? 'Browse tables' : 'Run SQL'}
            active={view === 'sql'}
            onClick={() => setView(view === 'sql' ? 'tables' : 'sql')}
          />
          <IconButton icon={Trash2} label="Delete service" kind="danger" onClick={onDelete} />
        </>}
        onClose={onClose} closeLabel="Close database browser">
        {droppingDb ? (
          <div className="flex items-center gap-1.5">
            <span className="text-xs text-danger">
              Delete <span className="font-mono">{db}</span> and everything in it?
            </span>
            <Button kind="danger" size="sm" onClick={dropDatabase} disabled={busy}>
              {busy ? 'Deleting…' : `Delete ${db}`}
            </Button>
            <IconButton icon={X} label="Keep the database" onClick={() => setDroppingDb(false)} disabled={busy} />
          </div>
        ) : newDb === null ? (
          <div className="flex items-center gap-1">
            {databases.length > 0 && db && (
              <Select
                variant="quiet"
                ariaLabel="Database"
                value={db}
                onChange={(d) => {
                  setDb(d)
                  setCurrent(null)
                  setView('tables')
                }}
                options={databases.map((d) => ({
                  value: d.name,
                  label: d.size === null ? d.name : `${d.name} · ${fmtBytes(d.size)}`,
                }))}
                className="max-w-56"
              />
            )}
            <IconButton icon={Plus} label="New database" onClick={() => setNewDb('')} />
            {db && db !== defaultDb && (
              <IconButton icon={Trash2} label={`Delete the database ${db}`} kind="danger" onClick={() => setDroppingDb(true)} />
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
            <Input
              size="sm"
              autoFocus
              value={newDb}
              onChange={(e) => setNewDb(e.target.value)}
              placeholder="new_database"
              aria-label="New database name"
              onKeyDown={(e) => {
                if (e.key === 'Escape') {
                  // Ours: drop the new-database form, keep the dialog.
                  e.preventDefault()
                  setNewDb(null)
                }
              }}
              className="w-40"
            />
            <Button size="sm" type="submit" disabled={busy || !newDb.trim()}>
              {busy ? 'Creating…' : 'Create'}
            </Button>
            <IconButton icon={X} label="Cancel" onClick={() => setNewDb(null)} disabled={busy} />
          </form>
        )}
      </ModalHeader>
      <ModalNotice message={error} />
      <ModalBody padded={false}>
        {view === 'sql' ? (
          <SqlView key={`${db}:${seedSql}`} contextLabel={db} execute={(sql, signal) => api.dbQuery(space, service, db, sql, signal)} initial={seedSql} onChanged={loadTables} />
        ) : view === 'create' ? (
          <CreateTableView
            space={space}
            service={service}
            db={db}
            mysql={mysql}
            onDone={(name) => {
              loadTables()
              setCurrent({ schema: mysql ? db : 'public', name, rows: 0 })
              setView('tables')
            }}
            onCancel={() => setView('tables')}
          />
        ) : (
          <SplitPane
            sidebar={<>
              <div className="flex h-12 shrink-0 items-center justify-between border-b border-line pl-3 pr-2">
                <Eyebrow>Tables</Eyebrow>
                <div className="flex items-center">
                  <IconButton icon={Plus} size="sm" label="New table" onClick={() => setView('create')} />
                  <IconButton icon={RefreshCw} size="sm" label="Refresh tables" onClick={loadTables} />
                </div>
              </div>
              <div className="min-h-0 flex-1 overflow-y-auto p-2">
                {tables === null && <Spinner label="Loading…" />}
                {tables !== null && tables.length === 0 && (
                  <p className="px-1 text-xs text-muted">Nothing here yet.</p>
                )}
                {(tables ?? []).map((t) =>
                  droppingTable?.schema === t.schema && droppingTable?.name === t.name ? (
                    // Dropping is decided where the table lives — in its
                    // own list row, compactly.
                    <div
                      key={`${t.schema}.${t.name}`}
                      className="flex h-8 w-full items-center justify-between gap-1 rounded-md bg-danger/10 px-2 font-mono text-xs text-danger"
                    >
                      <span className="truncate">drop {t.name}?</span>
                      <span className="flex shrink-0 gap-0.5">
                        <button
                          type="button"
                          title={`Drop ${t.name} and its rows`}
                          aria-label={`Drop ${t.name} and its rows`}
                          onClick={dropTable}
                          disabled={busy}
                          className="rounded p-0.5 hover:bg-danger/20 disabled:opacity-40"
                        >
                          <Check className="size-3" aria-hidden />
                        </button>
                        <button
                          type="button"
                          title="Keep it"
                          aria-label="Keep the table"
                          onClick={() => setDroppingTable(null)}
                          className="rounded p-0.5 text-muted hover:bg-line/50"
                        >
                          <X className="size-3" aria-hidden />
                        </button>
                      </span>
                    </div>
                  ) : (
                    <div
                      key={`${t.schema}.${t.name}`}
                      className={`group/tbl flex h-8 w-full items-center rounded-md pr-0.5 ${
                        current?.schema === t.schema && current?.name === t.name
                          ? 'bg-accent-soft text-ink'
                          : 'text-ink-soft hover:bg-line/40'
                      }`}
                    >
                      <button
                        type="button"
                        onClick={() => setCurrent(t)}
                        className="flex h-full min-w-0 flex-1 items-center justify-between gap-2 px-2 text-left font-mono text-xs"
                      >
                        <span className="truncate">
                          {t.schema !== 'public' && t.schema !== db ? `${t.schema}.` : ''}
                          {t.name}
                        </span>
                        {t.rows !== null && <span className="text-[10px] text-muted">~{t.rows}</span>}
                      </button>
                      <IconButton
                        icon={Trash2}
                        size="sm"
                        kind="danger"
                        label={`Drop the table ${t.name}`}
                        onClick={() => setDroppingTable(t)}
                        className=""
                      />
                    </div>
                  ),
                )}
              </div>
            </>}
          >
            {current ? (
              <TableView
                key={`${db}.${current.schema}.${current.name}`}
                space={space}
                service={service}
                db={db}
                table={current}
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
                    <Button icon={Plus} onClick={() => setView('create')}>New table</Button>
                    <Button
                      kind="quiet"
                      onClick={() => {
                        setSeedSql(
                          mysql
                            ? 'create table notes (\n  id bigint auto_increment primary key,\n  text text not null,\n  at datetime default current_timestamp\n);'
                            : 'create table notes (\n  id bigint generated always as identity primary key,\n  text text not null,\n  at timestamptz default now()\n);',
                        )
                        setView('sql')
                      }}
                    >
                      Write it in SQL
                    </Button>
                  </div>
                  <p className="mt-4 text-xs text-muted">
                    Only tables available to this service are shown.
                  </p>
                </div>
              </div>
            ) : (
              <div className="flex flex-1 items-center justify-center text-sm text-muted">
                Pick a table on the left.
              </div>
            )}
          </SplitPane>
        )}
      </ModalBody>
    </Modal>
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

const mysqlColumnTypes = [
  { value: 'auto', label: 'auto id' },
  { value: 'text', label: 'text' },
  { value: 'varchar(255)', label: 'short text' },
  { value: 'integer', label: 'integer' },
  { value: 'bigint', label: 'bigint' },
  { value: 'boolean', label: 'boolean' },
  { value: 'decimal(20,6)', label: 'decimal' },
  { value: 'datetime(6)', label: 'timestamp' },
  { value: 'date', label: 'date' },
  { value: 'json', label: 'json' },
  { value: 'char(36)', label: 'uuid' },
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
  mysql,
  onDone,
  onCancel,
}: {
  space: string
  service: string
  db: string
  mysql: boolean
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
    const quote = (ident: string) => mysql ? `\`${ident}\`` : `"${ident}"`
    const defs = used.map((c) => {
      const type = c.type === 'auto' ? (mysql ? 'bigint auto_increment' : 'bigint generated always as identity') : c.type
      return `  ${quote(c.name)} ${type}${c.required && c.type !== 'auto' ? ' not null' : ''}`
    })
    const pk = used.filter((c) => c.pk).map((c) => quote(c.name))
    if (pk.length > 0) defs.push(`  primary key (${pk.join(', ')})`)
    const sql = `create table ${quote(name)} (\n${defs.join(',\n')}\n)`
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
    <div className="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto px-5 py-4">
      <Field label="Table name" className="w-56">
        <Input
          autoFocus
          value={name}
          onChange={(e) => setName(e.target.value)}
          placeholder="notes"
        />
      </Field>
      <div className="flex flex-col gap-1.5">
        <Eyebrow>Columns</Eyebrow>
        <div className="flex flex-col gap-1.5">
          {columns.map((c, i) => (
            <div key={i} className="flex flex-wrap items-center gap-2">
              <Input
                size="sm"
                value={c.name}
                onChange={(e) => patch(i, { name: e.target.value })}
                placeholder="column_name"
                aria-label={`Column ${i + 1} name`}
                className="w-44"
              />
              <Select
                size="sm"
                ariaLabel={`Column ${i + 1} type`}
                value={c.type}
                onChange={(t) => patch(i, { type: t })}
                options={mysql ? mysqlColumnTypes : columnTypes}
              />
              <label className="flex items-center gap-1 text-xs text-ink-soft">
                <input
                  type="checkbox"
                  checked={c.pk}
                  onChange={(e) => patch(i, { pk: e.target.checked })}
                  className="size-3.5"
                />
                key
              </label>
              {c.type !== 'auto' && (
                <label className="flex items-center gap-1 text-xs text-ink-soft">
                  <input
                    type="checkbox"
                    checked={c.required}
                    onChange={(e) => patch(i, { required: e.target.checked })}
                    className="size-3.5"
                  />
                  required
                </label>
              )}
              <IconButton
                icon={Trash2}
                size="sm"
                kind="danger"
                label={`Remove column ${i + 1}`}
                onClick={() => setColumns(columns.filter((_, ci) => ci !== i))}
              />
            </div>
          ))}
        </div>
        <div>
          <Button
            kind="quiet"
            size="sm"
            icon={Plus}
            onClick={() => setColumns([...columns, { name: '', type: 'text', pk: false, required: false }])}
          >
            Add column
          </Button>
        </div>
      </div>
      <ErrorNote message={error} />
      <div className="mt-auto flex items-center justify-between gap-2 border-t border-line pt-3">
        <p className="text-xs text-muted">
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
}: {
  space: string
  service: string
  db: string
  table: DbTable
}) {
  const [page, setPage] = useState<DbRowsPage | null>(null)
  const [offset, setOffset] = useState(0)
  const [sorting, setSorting] = useState<SortingState>([])
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  // Cells edit themselves in place; only deletion, dropping the table and
  // the add-row form still hold state here.
  const [deleting, setDeleting] = useState<number | null>(null)
  const [adding, setAdding] = useState(false)
  const [draft, setDraft] = useState<Record<string, string>>({})

  const limit = 50

  const load = useCallback(() => {
    const order = sorting.length > 0 ? { column: sorting[0].id, desc: sorting[0].desc } : undefined
    api
      .dbRows(space, service, db, table.schema, table.name, limit, offset, order)
      .then((res) => {
        setPage(res)
        setError('')
      })
      .catch((err) => setError(errText(err, 'Loading rows did not work.')))
  }, [space, service, db, table.schema, table.name, offset, sorting])

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

  // One cell, one UPDATE. The page is patched in place on success, so
  // tabbing across a row never reloads the grid under the cursor.
  async function commitCell(rowIndex: number, colIndex: number, column: string, value: string) {
    const row = page?.rows[rowIndex]
    if (!row) return false
    try {
      await api.dbUpdateRow(space, service, db, table.schema, table.name, {
        key: rowKey(row),
        values: { [column]: value },
      })
      setPage(
        (p) =>
          p && {
            ...p,
            rows: p.rows.map((r, i) => (i === rowIndex ? r.map((v, ci) => (ci === colIndex ? value : v)) : r)),
          },
      )
      setError('')
      return true
    } catch (err) {
      setError(errText(err, 'That change did not stick.'))
      return false
    }
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

  // Column defs are rebuilt every render on purpose: the cell renderers
  // close over live editing state, which spares the meta indirection the
  // memoized-columns pattern needs. At fifty rows a page, that's free.
  const columns: ColumnDef<DbCell[]>[] = [
    ...(page?.columns ?? []).map(
      (c, ci): ColumnDef<DbCell[]> => ({
        id: c,
        accessorFn: (r: DbCell[]) => r[ci] ?? '',
        header: () => (
          <>
            {c}
            {page?.key.includes(c) && (
              <span className="ml-1 text-[9px] text-accent" title="primary key">
                key
              </span>
            )}
          </>
        ),
        cell: ({ row }: { row: Row<DbCell[]> }) =>
          editable && !page?.key.includes(c) ? (
            <EditableCell
              initial={row.original[ci] ?? ''}
              isNull={row.original[ci] === null}
              label={c}
              onCommit={(value) => commitCell(row.index, ci, c, value)}
            />
          ) : (
            <span title={row.original[ci] ?? undefined}>
              <CellText value={row.original[ci]} />
            </span>
          ),
      }),
    ),
    {
      id: '__actions',
      header: () => null,
      enableSorting: false,
      cell: ({ row }) =>
        deleting === row.index ? (
          <div className="flex gap-1">
            <Button
              kind="danger"
              size="sm"
              onClick={() =>
                run(() =>
                  api.dbDeleteRow(space, service, db, table.schema, table.name, {
                    key: rowKey(row.original),
                  }),
                )
              }
              disabled={busy}
            >
              Delete
            </Button>
            <IconButton icon={X} size="sm" label="Keep the row" onClick={() => setDeleting(null)} disabled={busy} />
          </div>
        ) : editable ? (
          // invisible, not hidden: the button keeps its space at all times,
          // so hovering never changes the row's height.
          <div className="invisible flex gap-0.5 group-hover/row:visible">
            <button
              type="button"
              title="Delete row"
              aria-label="Delete row"
              onClick={() => setDeleting(row.index)}
              className="rounded p-1 text-muted hover:bg-danger/10 hover:text-danger"
            >
              <Trash2 className="size-3" aria-hidden />
            </button>
          </div>
        ) : null,
    },
  ]

  const grid = useReactTable({
    data: page?.rows ?? [],
    columns,
    state: { sorting },
    onSortingChange: (updater) => {
      // A new order starts from the first page — offsets into the old
      // order would show arbitrary rows.
      setOffset(0)
      setSorting(updater)
    },
    manualSorting: true,
    getCoreRowModel: getCoreRowModel(),
  })

  if (page === null) {
    return (
      <div className="flex flex-1 items-center justify-center">
        {error ? <ErrorNote message={error} /> : <Spinner label="Loading rows…" />}
      </div>
    )
  }

  return (
    <div className="flex min-w-0 flex-1 flex-col">
      <div className="flex h-12 shrink-0 flex-wrap items-center justify-between gap-2 border-b border-line px-3">
        <span className="font-mono text-xs text-ink-soft">
          {page.total === null ? 'Row count unavailable' : `${page.total} ${page.total === 1 ? 'row' : 'rows'}`}
          {editable ? (
            <span className="ml-2 text-muted">click a cell to edit — blur saves, Esc reverts</span>
          ) : (
            <span className="ml-2 text-muted">read-only (no primary key)</span>
          )}
        </span>
        <div className="flex items-center gap-0.5">
          <IconButton icon={Plus} size="sm" label="Add a row" onClick={() => setAdding(true)} disabled={busy || adding} />
          <IconButton icon={RefreshCw} size="sm" label="Refresh rows" onClick={load} />
          <span className="mx-1 h-4 w-px bg-line" aria-hidden />
          <IconButton icon={ChevronLeft} size="sm" label="Previous page" onClick={() => setOffset(Math.max(0, offset - limit))} disabled={offset === 0} />
          <span className="min-w-12 text-center font-mono text-[10px] text-muted">
            {page.total === 0 ? '0' : `${offset + 1}–${Math.min(offset + limit, page.total ?? offset + page.rows.length)}`}
          </span>
          <IconButton icon={ChevronRight} size="sm" label="Next page" onClick={() => setOffset(offset + limit)} disabled={!page.hasMore} />
        </div>
      </div>
      <div className="px-3 pt-2 empty:hidden">
        <ErrorNote message={error} />
      </div>
      <div className="min-h-0 flex-1 overflow-auto px-3 pb-3">
        <DataGrid
          table={grid}
          leadingRow={
            adding ? (
              // The pending row speaks the same language as the cells
              // around it: bare inputs, Enter saves, Escape discards — the
              // tint is what says "not stored yet".
              <tr className="bg-accent-soft/40">
                {page.columns.map((c, ci) => (
                  <td key={c} className="border-b border-line/50 px-2 py-1">
                    <input
                      autoFocus={ci === 0}
                      value={draft[c] ?? ''}
                      onChange={(e) => setDraft({ ...draft, [c]: e.target.value })}
                      onKeyDown={(e) => {
                        if (e.key === 'Enter') insert()
                        if (e.key === 'Escape') {
                          // Ours too: discard the pending row, keep the
                          // dialog open.
                          e.preventDefault()
          e.stopPropagation()
                          setAdding(false)
                          setDraft({})
                        }
                      }}
                      placeholder="default"
                      aria-label={`New ${c}`}
                      className="w-full border-b border-transparent bg-transparent font-mono text-xs text-ink outline-none placeholder:italic placeholder:text-muted/60 focus:border-accent-strong"
                    />
                  </td>
                ))}
                <td className="border-b border-line/50 px-1 py-1">
                  <div className="flex gap-0.5">
                    <button
                      type="button"
                      title="Save the row (Enter)"
                      aria-label="Save the row"
                      onClick={insert}
                      disabled={busy}
                      className="rounded p-1 text-success hover:bg-success/10 disabled:opacity-40"
                    >
                      <Check className="size-3" aria-hidden />
                    </button>
                    <button
                      type="button"
                      title="Discard (Esc)"
                      aria-label="Discard the row"
                      onClick={() => {
                        setAdding(false)
                        setDraft({})
                      }}
                      className="rounded p-1 text-muted hover:bg-line/50"
                    >
                      <X className="size-3" aria-hidden />
                    </button>
                  </div>
                </td>
              </tr>
            ) : undefined
          }
        />
        {page.rows.length === 0 && !adding && (
          <p className="py-8 text-center text-sm text-muted">No rows yet — Add row puts the first one in.</p>
        )}
      </div>
    </div>
  )
}
