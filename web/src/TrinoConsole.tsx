import { useState, type ReactNode } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import type { ConsoleProps } from './consoles'
import { runSql, sqlDatabasesQuery, sqlKey, sqlSchemasQuery, sqlTablesQuery, type SqlTable } from './sqlApi'
import { PaneMessage, SidebarField, SqlSidebar, SqlTablePane, TableList } from './SqlTable'
import { SqlWorkbench } from './SqlView'
import { useServiceSearch, type ServiceSearch } from './serviceSearch'
import { ErrorNote, Select, Spinner } from './ui'
import { errText } from './format'

/**
 * The Trino console: Catalogs walks catalog → schema → table, with each
 * table's columns and a preview of its rows; Query runs SQL with a catalog
 * and schema as its context. Everything runs as the signed-in user — the
 * add-on exchanges their token — so what shows is what they may see.
 * Nothing here writes; that is what Query is for.
 */
export function Console({ space, service, tab }: ConsoleProps) {
  const [visited, setVisited] = useState<string[]>([tab])
  if (!visited.includes(tab)) setVisited([...visited, tab])
  const place = useTrinoPlace(space, service)
  const qc = useQueryClient()
  return (
    <>
      {visited.includes('data') && (
        <div className={tab === 'data' ? 'flex min-h-0 min-w-0 flex-1 flex-col' : 'hidden'}>
          <CatalogsTab place={place} />
        </div>
      )}
      {visited.includes('query') && (
        <div className={tab === 'query' ? 'flex min-h-0 min-w-0 flex-1 flex-col' : 'hidden'}>
          <SqlWorkbench
            storageKey={`${space}/${service}`}
            dialect="trino"
            context={<Pickers place={place} />}
            target={[place.catalog, place.schema].filter(Boolean).join('.') || 'Trino'}
            run={(sql, signal) => runSql(space, service, place.catalog, place.schema, sql, signal)}
            onWrote={() => void qc.invalidateQueries({ queryKey: sqlKey(space, service) })}
          />
        </div>
      )}
    </>
  )
}

type TrinoPlace = ReturnType<typeof useTrinoPlace>

// The URL names catalog, schema and table; without them the binding's
// catalog and schema are where the console opens, then the first there is.
function useTrinoPlace(space: string, service: string) {
  const [search, move] = useServiceSearch()
  const catalogs = useQuery(sqlDatabasesQuery(space, service))
  const names = catalogs.data?.databases.map((d) => d.name) ?? []
  const catalog = search.catalog || (names.includes(catalogs.data?.default ?? '') ? catalogs.data!.default : (names[0] ?? ''))
  const schemas = useQuery({ ...sqlSchemasQuery(space, service, catalog), enabled: !!catalog })
  const list = schemas.data?.schemas ?? []
  const schema =
    search.schema ||
    (list.includes(schemas.data?.default ?? '') ? schemas.data!.default : (list.find((s) => s !== 'information_schema') ?? list[0] ?? ''))
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
      <SidebarField label="Catalog">
        {catalogs.data ? (
          catalogs.data.databases.length > 0 ? (
            <Select
              size="sm"
              ariaLabel="Catalog"
              value={catalog}
              onChange={(next) => move({ catalog: next })}
              options={catalogs.data.databases.map((d) => ({ value: d.name, label: d.name }))}
              className="w-full"
            />
          ) : (
            <p className="text-xs text-muted">No catalogs to see.</p>
          )
        ) : catalogs.error ? (
          <p className="text-xs text-danger">Not answering.</p>
        ) : (
          <Spinner inline label="Connecting…" />
        )}
      </SidebarField>
      {catalog && (
        <SidebarField label="Schema">
          {schemas.data ? (
            schemas.data.schemas.length > 0 ? (
              <Select
                size="sm"
                ariaLabel="Schema"
                value={schema}
                onChange={(next) => move({ ...at(place, 'catalog'), schema: next })}
                options={schemas.data.schemas.map((s) => ({ value: s, label: s }))}
                className="w-full"
              />
            ) : (
              <p className="text-xs text-muted">No schemas in {catalog}.</p>
            )
          ) : schemas.error ? (
            <ErrorNote message={errText(schemas.error, 'Listing the schemas did not work.')} />
          ) : (
            <Spinner inline label="Loading…" />
          )}
        </SidebarField>
      )}
    </>
  )
}

function CatalogsTab({ place }: { place: TrinoPlace }) {
  const { search, move, catalog, schema, catalogs, tables } = place
  const list = tables.data?.tables ?? null
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
      />
    )
  } else {
    main = (
      <PaneMessage>
        {catalogs.data && list ? (
          list.length > 0 ? (
            'Pick a table on the left to see its columns and a preview of its rows.'
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
    <div className="flex min-h-0 flex-1 flex-col gap-4 md:flex-row">
      <SqlSidebar>
        <div className="shrink-0 space-y-3 p-3">
          <Pickers place={place} />
        </div>
        <TableList
          // No catalog or no schema to look in is an empty list, not a wait.
          tables={catalog && schema ? list : catalogs.data && (!catalog || place.schemas.data) ? [] : null}
          error={tables.error ? errText(tables.error, 'Listing the tables did not work.') : ''}
          selected={search.table}
          onSelect={(t) => move({ ...at(place, 'schema'), table: t.name, ...(search.view ? { view: search.view } : {}) })}
          onRefresh={() => void tables.refetch()}
          refreshing={tables.isFetching}
          empty={schema ? `No tables in ${schema}.` : 'Nothing to list.'}
        />
      </SqlSidebar>
      {main}
    </div>
  )
}
