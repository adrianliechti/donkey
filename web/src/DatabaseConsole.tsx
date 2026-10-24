import { useState, type ReactNode } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Plus, Trash2, X } from 'lucide-react'
import type { ConsoleProps } from './consoles'
import { Modal, ModalBody, ModalFooter, ModalHeader } from './dialog'
import {
  failure,
  runSql,
  sqlDatabasesQuery,
  sqlKey,
  sqlSchemasQuery,
  sqlTablesQuery,
  useSqlCreateDatabase,
  useSqlDropDatabase,
  type SqlPlace,
  type SqlTable,
} from './sqlApi'
import { PaneMessage, SidebarBar, SqlEntry, SqlSidebar, SqlTablePane, TableList } from './SqlTable'
import { Picker, Row } from './Workbench'
import { SqlWorkbench } from './SqlView'
import { plural } from './sqlText'
import { useServiceSearch, type ServiceSearch } from './serviceSearch'
import { Button, ErrorNote, Eyebrow, Field, IconButton, Input, Select, Spinner, Toolbar } from './ui'
import { errText, humanSize } from './format'

/**
 * The database console of a Postgres or MySQL service, one workbench: the
 * databases and tables on the left, and on the right the table picked — or
 * SQL, which sits in the same list. The place is in the URL (db, schema,
 * then a table and its view, or sql=open), so back walks it. The binding's
 * database is only the default: any of the service's databases can be
 * opened, or made.
 */
export function Console({ space, service, protocol }: ConsoleProps) {
  return <DatabaseView place={useDatabasePlace(space, service, protocol)} />
}

type DatabasePlace = ReturnType<typeof useDatabasePlace>

// Where the console stands: the dialect (the server's word over the
// listing's guess), the database (the URL's, else the binding's) and, for
// postgres, the schema (the URL's, else public).
function useDatabasePlace(space: string, service: string, protocol?: string) {
  const [search, move] = useServiceSearch()
  const databases = useQuery(sqlDatabasesQuery(space, service))
  const spoken = databases.data?.protocol ?? protocol
  const mysql = spoken === 'mysql' || spoken === 'mariadb'
  const db = search.db || databases.data?.default || ''
  const schemas = useQuery({ ...sqlSchemasQuery(space, service, db), enabled: !!db && !mysql && !!databases.data })
  const fallbackSchema = schemas.data
    ? schemas.data.schemas.includes(schemas.data.default)
      ? schemas.data.default
      : (schemas.data.schemas[0] ?? 'public')
    : ''
  const schema = mysql ? db : search.schema || fallbackSchema
  const tables = useQuery({
    ...sqlTablesQuery(space, service, db, mysql ? '' : schema),
    enabled: !!db && !!schema,
  })
  return {
    space,
    service,
    search,
    move,
    mysql,
    databases,
    schemas,
    tables,
    db,
    schema,
    defaultDb: databases.data?.default ?? '',
    /** Where tables of the open database and schema are. */
    sql: { space, service, db, schema } satisfies SqlPlace,
    label: mysql ? 'MySQL' : 'Postgres',
  }
}

function DatabaseView({ place }: { place: DatabasePlace }) {
  const { space, service, search, move, db, schema, mysql, tables, databases } = place
  const qc = useQueryClient()
  const [creating, setCreating] = useState(false)
  const list = tables.data?.tables ?? null
  const selected = search.table
  const table: SqlTable | undefined = selected
    ? (list?.find((t) => t.name === selected) ?? { schema, name: selected, rows: null })
    : undefined

  // The editor, once opened, stays mounted beside the tables (hidden while
  // one shows): what was typed and what came back are there on the way back.
  const sqlOpen = search.sql === 'open'
  const [sqlSeen, setSqlSeen] = useState(sqlOpen)
  if (sqlOpen && !sqlSeen) setSqlSeen(true)
  const [seed, setSeed] = useState<{ sql: string; n: number }>()
  const openSql = (sql?: string) => {
    if (sql !== undefined) setSeed({ sql, n: (seed?.n ?? 0) + 1 })
    setCreating(false)
    move({ ...placeSearch(place), sql: 'open' })
  }

  const open = (t: SqlTable) => {
    setCreating(false)
    move({ ...placeSearch(place), table: t.name, ...(search.view ? { view: search.view } : {}) })
  }
  const trail = [
    { label: db, onClick: () => move({ ...(search.db ? { db } : {}) }) },
    ...(mysql ? [] : [{ label: schema, onClick: () => move(placeSearch(place)) }]),
  ]

  let main: ReactNode
  if (databases.error) {
    main = (
      <PaneMessage>
        <ErrorNote message={errText(databases.error, 'The database did not answer.')} />
      </PaneMessage>
    )
  } else if (!db || !schema) {
    main = <PaneMessage>{!place.schemas.error && <Spinner label="Opening the database…" />}</PaneMessage>
  } else if (creating) {
    main = (
      <CreateTable
        place={place}
        onCancel={() => setCreating(false)}
        onCreated={(name) => {
          setCreating(false)
          move({ ...placeSearch(place), table: name })
        }}
      />
    )
  } else if (table) {
    main = (
      <SqlTablePane
        key={`${db}.${schema}.${table.name}`}
        place={place.sql}
        table={table}
        trail={trail}
        editable
        filterable
        onDropped={() => move(placeSearch(place))}
        onQuery={() =>
          openSql(
            `select * from ${mysql ? quoteMy(table.name) : `${quotePg(schema)}.${quotePg(table.name)}`} limit 100;`,
          )
        }
      />
    )
  } else if (list && list.length === 0) {
    main = (
      <PaneMessage>
        <p className="text-ink-soft">
          <span className="font-mono">{mysql ? db : `${db}.${schema}`}</span> has no tables yet. Apps that run
          migrations make theirs on first start — or make one now.
        </p>
        <div className="mt-4 flex justify-center gap-2">
          <Button icon={Plus} onClick={() => setCreating(true)}>
            New table
          </Button>
          <Button kind="quiet" onClick={() => openSql(mysql ? sampleTable.mysql : sampleTable.postgres)}>
            Write it in SQL
          </Button>
        </div>
      </PaneMessage>
    )
  } else {
    main = (
      <PaneMessage>
        {list ? 'Pick a table on the left to see its rows, or open SQL.' : <Spinner label="Loading tables…" />}
      </PaneMessage>
    )
  }

  return (
    <Row>
      <SqlSidebar>
        <DatabasePicker place={place} manage />
        <SqlEntry selected={sqlOpen} onOpen={() => openSql()} />
        <TableList
          tables={db && schema ? list : null}
          error={tables.error ? errText(tables.error, 'Listing the tables did not work.') : ''}
          selected={sqlOpen ? undefined : selected}
          onSelect={open}
          onRefresh={() => void tables.refetch()}
          refreshing={tables.isFetching}
          onCreate={() => {
            setCreating(true)
            if (sqlOpen) move(placeSearch(place))
          }}
          empty="Nothing here yet."
        />
      </SqlSidebar>
      {sqlSeen && db && schema && (
        <div className={sqlOpen ? 'flex min-h-0 min-w-0 flex-1 flex-col' : 'hidden'}>
          <SqlWorkbench
            storageKey={`${space}/${service}`}
            dialect={mysql ? 'mysql' : 'postgres'}
            target={mysql ? db : `${db} · ${schema}`}
            run={(sql, signal) => runSql(space, service, db, mysql ? '' : schema, sql, signal)}
            onWrote={() => void qc.invalidateQueries({ queryKey: sqlKey(space, service) })}
            seed={seed}
          />
        </div>
      )}
      {!sqlOpen && main}
    </Row>
  )
}

// Identifiers as each dialect quotes them, for the selects the console writes.
const quotePg = (name: string) => `"${name.replaceAll('"', '""')}"`
const quoteMy = (name: string) => `\`${name.replaceAll('`', '``')}\``

/** The search that names the open database and schema, and nothing below. */
function placeSearch(place: DatabasePlace): ServiceSearch {
  const out: ServiceSearch = {}
  if (place.search.db) out.db = place.search.db
  if (place.search.schema) out.schema = place.search.schema
  return out
}

// Names the create forms accept — plain lowercase identifiers sidestep the
// whole quoting swamp, and match what the addon's own objects use anyway.
const plainName = /^[a-z_][a-z0-9_]*$/

/**
 * The database and schema pickers. `manage` adds making and dropping
 * databases — the binding's own database stays: the apps point at it.
 */
function DatabasePicker({ place, manage = false }: { place: DatabasePlace; manage?: boolean }) {
  const { databases, schemas, db, schema, defaultDb, mysql, move, search } = place
  const [newName, setNewName] = useState<string | null>(null)
  const [dropping, setDropping] = useState(false)
  const create = useSqlCreateDatabase(place.space, place.service)
  const list = databases.data?.databases ?? []
  const nameError = newName && !plainName.test(newName.trim()) ? 'Lowercase letters, digits and underscores.' : ''

  // A database of its own is a new place: everything below it goes.
  const pick = (next: string) => move(next === defaultDb && !search.db ? {} : { db: next })

  return (
    <>
      {newName !== null ? (
        <form
          onSubmit={(e) => {
            e.preventDefault()
            const name = newName.trim()
            if (!plainName.test(name)) return
            create.mutate(name, {
              onSuccess: () => {
                setNewName(null)
                move({ db: name })
              },
            })
          }}
          className="shrink-0 space-y-1.5 border-b border-line p-1.5"
        >
          <div className="flex items-center gap-1">
            <Input
              size="sm"
              autoFocus
              value={newName}
              onChange={(e) => setNewName(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === 'Escape') setNewName(null)
              }}
              placeholder="new_database"
              aria-label="New database name"
              className="w-full"
            />
            <Button size="sm" type="submit" disabled={create.isPending || !newName.trim() || !!nameError}>
              {create.isPending ? 'Creating…' : 'Create'}
            </Button>
            <IconButton
              icon={X}
              size="sm"
              label="Cancel"
              onClick={() => setNewName(null)}
              disabled={create.isPending}
            />
          </div>
          {nameError && <p className="px-1 text-[11px] text-muted">{nameError}</p>}
          <ErrorNote message={failure(create, 'Creating the database did not work.')} />
        </form>
      ) : (
        <SidebarBar
          actions={
            manage && (
              <>
                <IconButton icon={Plus} size="sm" label="New database" onClick={() => setNewName('')} />
                {db && db !== defaultDb && (
                  <IconButton
                    icon={Trash2}
                    size="sm"
                    kind="danger"
                    label={`Drop the database ${db}…`}
                    onClick={() => setDropping(true)}
                  />
                )}
              </>
            )
          }
        >
          {databases.data ? (
            <Picker
              label="Database"
              value={db}
              onChange={pick}
              options={list.map((d) => ({
                value: d.name,
                label: d.name,
                detail: [d.size === null ? '' : humanSize(d.size), d.name === defaultDb ? 'default' : '']
                  .filter(Boolean)
                  .join(' · '),
              }))}
            />
          ) : databases.error ? (
            <p className="px-1.5 text-xs text-danger">Not answering.</p>
          ) : (
            <span className="px-1.5">
              <Spinner inline label="Connecting…" />
            </span>
          )}
          {!mysql && schemas.data && schemas.data.schemas.length > 1 && (
            <>
              <span className="font-mono text-sm text-line-strong select-none" aria-hidden>
                /
              </span>
              <Picker
                label="Schema"
                value={schema}
                onChange={(next) => move({ ...(search.db ? { db: search.db } : {}), schema: next })}
                options={schemas.data.schemas.map((s) => ({ value: s, label: s }))}
              />
            </>
          )}
        </SidebarBar>
      )}
      <ErrorNote message={schemas.error ? errText(schemas.error, 'Listing the schemas did not work.') : ''} />
      {dropping && (
        <DropDatabaseDialog
          place={place}
          onClose={() => setDropping(false)}
          onDropped={() => {
            setDropping(false)
            move({})
          }}
        />
      )}
    </>
  )
}

function DropDatabaseDialog({
  place,
  onClose,
  onDropped,
}: {
  place: DatabasePlace
  onClose: () => void
  onDropped: () => void
}) {
  const drop = useSqlDropDatabase(place.space, place.service)
  const busy = drop.isPending
  const tables = place.tables.data?.tables
  return (
    <Modal onClose={busy ? () => {} : onClose} size="sm">
      <ModalHeader
        title={
          <>
            Drop the database <span className="font-mono">{place.db}</span>?
          </>
        }
        onClose={onClose}
        closeDisabled={busy}
      />
      <ModalBody className="space-y-3">
        <p className="text-sm leading-relaxed text-ink-soft">
          Everything in it
          {tables ? ` — ${plural(tables.length, 'table')}${place.mysql ? '' : ` in ${place.schema} alone`}` : ''} — is
          gone for good. Connected apps use <span className="font-mono">{place.defaultDb}</span>, not this one.
        </p>
        <ErrorNote message={failure(drop, 'Dropping the database did not work.')} />
      </ModalBody>
      <ModalFooter>
        <Button kind="quiet" onClick={onClose} disabled={busy}>
          Keep it
        </Button>
        <Button kind="danger" disabled={busy} onClick={() => drop.mutate(place.db, { onSuccess: onDropped })}>
          {busy ? 'Dropping…' : 'Drop the database'}
        </Button>
      </ModalFooter>
    </Modal>
  )
}

const sampleTable = {
  postgres:
    'create table notes (\n  id bigint generated always as identity primary key,\n  text text not null,\n  at timestamptz default now()\n);',
  mysql:
    'create table notes (\n  id bigint auto_increment primary key,\n  text text not null,\n  at datetime default current_timestamp\n);',
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

type ColumnDraft = { name: string; type: string; pk: boolean; required: boolean }

/**
 * A table without SQL: name, columns, done. The form builds the CREATE
 * TABLE itself and runs it through the query endpoint — names are plain
 * identifiers, so nothing ever needs escaping.
 */
function CreateTable({
  place,
  onCancel,
  onCreated,
}: {
  place: DatabasePlace
  onCancel: () => void
  onCreated: (name: string) => void
}) {
  const qc = useQueryClient()
  const { mysql } = place
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
    if (!plainName.test(name))
      return setError('The table needs a plain name — lowercase letters, digits and underscores.')
    const used = columns.filter((c) => c.name.trim() !== '')
    if (used.length === 0) return setError('The table needs at least one column.')
    const odd = used.find((c) => !plainName.test(c.name))
    if (odd) return setError(`"${odd.name}" won't do as a column name — lowercase letters, digits and underscores.`)
    const quote = (ident: string) => (mysql ? `\`${ident}\`` : `"${ident}"`)
    const defs = used.map((c) => {
      const type =
        c.type === 'auto' ? (mysql ? 'bigint auto_increment' : 'bigint generated always as identity') : c.type
      return `  ${quote(c.name)} ${type}${c.required && c.type !== 'auto' ? ' not null' : ''}`
    })
    const pk = used.filter((c) => c.pk).map((c) => quote(c.name))
    if (pk.length > 0) defs.push(`  primary key (${pk.join(', ')})`)
    const target = mysql ? quote(name) : `${quote(place.schema)}.${quote(name)}`
    setBusy(true)
    setError('')
    try {
      await runSql(place.space, place.service, place.db, '', `create table ${target} (\n${defs.join(',\n')}\n)`)
      await qc.invalidateQueries({
        queryKey: sqlTablesQuery(place.space, place.service, place.db, mysql ? '' : place.schema).queryKey,
      })
      onCreated(name)
    } catch (err) {
      setError(errText(err, 'Creating the table did not work.'))
    } finally {
      setBusy(false)
    }
  }

  return (
    <section className="flex min-h-0 min-w-0 flex-1 flex-col overflow-hidden">
      <div className="flex h-11 shrink-0 items-center justify-between border-b border-line pr-1.5 pl-3">
        <p className="text-sm font-medium text-ink">
          New table in <span className="font-mono">{mysql ? place.db : `${place.db}.${place.schema}`}</span>
        </p>
        <IconButton icon={X} size="sm" label="Cancel" onClick={onCancel} disabled={busy} />
      </div>
      <form
        onSubmit={(e) => {
          e.preventDefault()
          void create()
        }}
        className="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto p-4"
      >
        <Field label="Table name" className="w-64">
          <Input autoFocus value={name} onChange={(e) => setName(e.target.value)} placeholder="notes" />
        </Field>
        <div className="space-y-1.5">
          <Eyebrow>Columns</Eyebrow>
          {columns.map((c, i) => (
            <div key={i} className="flex flex-wrap items-center gap-2">
              <Input
                size="sm"
                value={c.name}
                onChange={(e) => patch(i, { name: e.target.value })}
                placeholder="column_name"
                aria-label={`Column ${i + 1} name`}
                className="w-48"
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
          <Button
            kind="quiet"
            size="sm"
            icon={Plus}
            onClick={() => setColumns([...columns, { name: '', type: 'text', pk: false, required: false }])}
          >
            Add column
          </Button>
        </div>
        <ErrorNote message={error} />
        <div className="mt-auto flex items-center justify-between gap-2 border-t border-line pt-3">
          <p className="text-xs text-muted">“auto id” counts up by itself; columns without a name are skipped.</p>
          <Toolbar className="gap-2">
            <Button kind="quiet" onClick={onCancel} disabled={busy}>
              Cancel
            </Button>
            <Button type="submit" disabled={busy || !name.trim()}>
              {busy ? 'Creating…' : 'Create table'}
            </Button>
          </Toolbar>
        </div>
      </form>
    </section>
  )
}
