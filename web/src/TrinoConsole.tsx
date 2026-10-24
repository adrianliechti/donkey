import { useState, type ReactNode } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import type { ConsoleProps } from './consoles'
import { runSql, sqlDatabasesQuery, sqlKey, sqlSchemasQuery, sqlTablesQuery, type SqlTable } from './sqlApi'
import { PaneMessage, SidebarBar, SqlEntry, SqlSidebar, SqlTablePane, TableList } from './SqlTable'
import { Picker, Row } from './Workbench'
import { SqlWorkbench } from './SqlView'
import { useServiceSearch, type ServiceSearch } from './serviceSearch'
import { ErrorNote, Spinner } from './ui'
import { errText } from './format'

/**
 * The Trino console, one workbench: catalog → schema → table on the left,
 * each table's columns and a preview of its rows on the right — or SQL,
 * which sits in the same list and runs with the catalog and schema picked.
 * Everything runs as the signed-in user — the add-on exchanges their token —
 * so what shows is what they may see. Nothing but SQL writes.
 */
export function Console({ space, service }: ConsoleProps) {
  return <CatalogsView place={useTrinoPlace(space, service)} />
}

type TrinoPlace = ReturnType<typeof useTrinoPlace>

// The URL names catalog, schema and table; without them the binding's
// catalog and schema are where the console opens, then the first there is.
function useTrinoPlace(space: string, service: string) {
  const [search, move] = useServiceSearch()
  const catalogs = useQuery(sqlDatabasesQuery(space, service))
  const names = catalogs.data?.databases.map((d) => d.name) ?? []
  const catalog =
    search.catalog || (names.includes(catalogs.data?.default ?? '') ? catalogs.data!.default : (names[0] ?? ''))
  const schemas = useQuery({ ...sqlSchemasQuery(space, service, catalog), enabled: !!catalog })
  const list = schemas.data?.schemas ?? []
  const schema =
    search.schema ||
    (list.includes(schemas.data?.default ?? '')
      ? schemas.data!.default
      : (list.find((s) => s !== 'information_schema') ?? list[0] ?? ''))
  const tables = useQuery({ ...sqlTablesQuery(space, service, catalog, schema), enabled: !!catalog && !!schema })
  return { space, service, search, move, catalogs, catalog, schemas, schema, tables }
}

function at(place: TrinoPlace, depth: 'catalog' | 'schema'): ServiceSearch {
  const out: ServiceSearch = {}
  if (place.search.catalog) out.catalog = place.search.catalog
  if (depth === 'schema' && place.search.schema) out.schema = place.search.schema
  return out
}

function Pickers({ place }: { place: TrinoPlace }) {
  const { catalogs, catalog, schemas, schema, move } = place
  return (
    <>
      <SidebarBar>
        {catalogs.data ? (
          catalogs.data.databases.length > 0 ? (
            <Picker
              label="Catalog"
              value={catalog}
              onChange={(next) => move({ catalog: next })}
              options={catalogs.data.databases.map((d) => ({ value: d.name, label: d.name }))}
            />
          ) : (
            <p className="px-1.5 text-xs text-muted">No catalogs to see.</p>
          )
        ) : catalogs.error ? (
          <p className="px-1.5 text-xs text-danger">Not answering.</p>
        ) : (
          <span className="px-1.5">
            <Spinner inline label="Connecting…" />
          </span>
        )}
        {catalog && schemas.data && schemas.data.schemas.length > 0 && (
          <>
            <span className="font-mono text-sm text-line-strong select-none" aria-hidden>
              .
            </span>
            <Picker
              label="Schema"
              value={schema}
              onChange={(next) => move({ ...at(place, 'catalog'), schema: next })}
              options={schemas.data.schemas.map((s) => ({ value: s, label: s }))}
            />
          </>
        )}
      </SidebarBar>
      <ErrorNote message={schemas.error ? errText(schemas.error, 'Listing the schemas did not work.') : ''} />
    </>
  )
}

function CatalogsView({ place }: { place: TrinoPlace }) {
  const { space, service, search, move, catalog, schema, catalogs, tables } = place
  const qc = useQueryClient()
  const list = tables.data?.tables ?? null

  // The editor, once opened, stays mounted (hidden while a table shows).
  const sqlOpen = search.sql === 'open'
  const [sqlSeen, setSqlSeen] = useState(sqlOpen)
  if (sqlOpen && !sqlSeen) setSqlSeen(true)
  const [seed, setSeed] = useState<{ sql: string; n: number }>()
  const openSql = (sql?: string) => {
    if (sql !== undefined) setSeed({ sql, n: (seed?.n ?? 0) + 1 })
    move({ ...at(place, 'schema'), sql: 'open' })
  }
  const table: SqlTable | undefined = search.table
    ? (list?.find((t) => t.name === search.table) ?? { schema, name: search.table, rows: null })
    : undefined

  let main: ReactNode
  if (catalogs.error) {
    main = (
      <PaneMessage>
        <ErrorNote message={errText(catalogs.error, 'Trino did not answer.')} />
      </PaneMessage>
    )
  } else if (table && catalog && schema) {
    main = (
      <SqlTablePane
        key={`${catalog}.${schema}.${table.name}`}
        place={{ space: place.space, service: place.service, db: catalog, schema }}
        table={table}
        trail={[
          { label: catalog, onClick: () => move(at(place, 'catalog')) },
          { label: schema, onClick: () => move(at(place, 'schema')) },
        ]}
        editable={false}
        filterable={false}
        views={{ rows: 'Preview', structure: 'Columns' }}
        // Columns first: the catalog answers at once, while even a preview
        // is a query on whatever the table sits on.
        defaultView="structure"
        onQuery={() => openSql(`select * from ${[catalog, schema, table.name].map(quote).join('.')} limit 100`)}
      />
    )
  } else {
    main = (
      <PaneMessage>
        {catalogs.data && list ? (
          list.length > 0 ? (
            'Pick a table on the left to see its columns and a preview of its rows, or open SQL.'
          ) : (
            'Nothing to browse here — pick another schema or catalog.'
          )
        ) : (
          <Spinner label="Asking Trino…" />
        )}
      </PaneMessage>
    )
  }

  return (
    <Row>
      <SqlSidebar>
        <Pickers place={place} />
        <SqlEntry selected={sqlOpen} onOpen={() => openSql()} />
        <TableList
          // No catalog or no schema to look in is an empty list, not a wait.
          tables={catalog && schema ? list : catalogs.data && (!catalog || place.schemas.data) ? [] : null}
          error={tables.error ? errText(tables.error, 'Listing the tables did not work.') : ''}
          selected={sqlOpen ? undefined : search.table}
          onSelect={(t) =>
            move({ ...at(place, 'schema'), table: t.name, ...(search.view ? { view: search.view } : {}) })
          }
          onRefresh={() => void tables.refetch()}
          refreshing={tables.isFetching}
          empty={schema ? `No tables in ${schema}.` : 'Nothing to list.'}
        />
      </SqlSidebar>
      {sqlSeen && (
        <div className={sqlOpen ? 'flex min-h-0 min-w-0 flex-1 flex-col' : 'hidden'}>
          <SqlWorkbench
            storageKey={`${space}/${service}`}
            dialect="trino"
            target={[catalog, schema].filter(Boolean).join('.') || 'Trino'}
            run={(sql, signal) => runSql(space, service, catalog, schema, sql, signal)}
            onWrote={() => void qc.invalidateQueries({ queryKey: sqlKey(space, service) })}
            seed={seed}
          />
        </div>
      )}
      {!sqlOpen && main}
    </Row>
  )
}

const quote = (name: string) => `"${name.replaceAll('"', '""')}"`
