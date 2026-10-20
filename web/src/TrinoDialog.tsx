import { useEffect, useState } from 'react'
import { RefreshCw, Terminal, Trash2 } from 'lucide-react'
import { api, type DbRowsPage } from './api'
import { Modal, ModalBody, ModalHeader, ModalNotice, SplitPane } from './dialog'
import { SqlResultGrid, SqlView } from './SqlView'
import { Eyebrow, IconButton, Select, Spinner, errText } from './ui'

export function TrinoDialog({ space, service, onClose, onDelete }: {
  space: string; service: string; onClose: () => void; onDelete: () => void
}) {
  const [catalogs, setCatalogs] = useState<string[] | null>(null)
  const [catalog, setCatalog] = useState('')
  const [catalogError, setCatalogError] = useState('')
  const [refresh, setRefresh] = useState(0)

  useEffect(() => {
    const controller = new AbortController()
    setCatalogError('')
    api.dbDatabases(space, service, controller.signal).then((res) => {
      if (controller.signal.aborted) return
      const names = res.databases.map((database) => database.name)
      setCatalogs(names)
      setCatalog((current) => names.includes(current) ? current : names.includes(res.default) ? res.default : names[0] ?? '')
    }).catch((err) => { if (!controller.signal.aborted) setCatalogError(errText(err, 'Loading catalogs failed.')) })
    return () => controller.abort()
  }, [space, service, refresh])

  return (
    <Modal onClose={onClose} size="xl" height="fill">
      <ModalHeader eyebrow={`Trino · ${space}`} title={<span className="font-mono">{service}</span>}
        actions={<>
          <IconButton icon={RefreshCw} label="Refresh catalogs" onClick={() => setRefresh((n) => n + 1)} />
          <IconButton icon={Trash2} label="Delete service" kind="danger" onClick={onDelete} />
        </>} onClose={onClose} closeLabel="Close Trino browser">
        {catalogs && catalogs.length > 0 && <Select variant="quiet" ariaLabel="Catalog" value={catalog} onChange={setCatalog}
          options={catalogs.map((name) => ({ value: name, label: name }))} />}
      </ModalHeader>
      <ModalNotice message={catalogError} />
      <ModalBody padded={false}>
        {catalogs === null ? <div className="px-5 py-4">{!catalogError && <Spinner label="Loading catalogs…" />}</div> : (
          <CatalogView key={`${space}:${service}:${catalog}:${refresh}`} space={space} service={service} catalog={catalog} />
        )}
      </ModalBody>
    </Modal>
  )
}

function CatalogView({ space, service, catalog }: { space: string; service: string; catalog: string }) {
  const [schemas, setSchemas] = useState<string[] | null>(catalog ? null : [])
  const [schema, setSchema] = useState('')
  const [schemaError, setSchemaError] = useState('')
  const [sql, setSql] = useState(false)

  useEffect(() => {
    if (!catalog) return
    const controller = new AbortController()
    api.dbSchemas(space, service, catalog, controller.signal).then((res) => {
      if (controller.signal.aborted) return
      setSchemas(res.schemas)
      setSchema(res.schemas.includes(res.default) ? res.default : res.schemas.find((name) => name !== 'information_schema') ?? res.schemas[0] ?? '')
    }).catch((err) => { if (!controller.signal.aborted) setSchemaError(errText(err, 'Loading schemas failed.')) })
    return () => controller.abort()
  }, [space, service, catalog])

  const contextLabel = [catalog, schema].filter(Boolean).join('.') || 'Trino'
  return <>
    <div className="flex h-12 shrink-0 items-center justify-between gap-3 border-b border-line px-5">
      <div className="flex min-w-0 items-center gap-2">
        <Eyebrow>Schema</Eyebrow>
        {schemas === null && !schemaError ? <Spinner inline label="Loading…" /> : schemas && schemas.length > 0 ? (
          <Select variant="quiet" ariaLabel="Schema" value={schema} onChange={setSchema}
            options={schemas.map((name) => ({ value: name, label: name }))} />
        ) : <span className="text-xs text-muted">{catalog ? 'No schemas available.' : 'No catalogs available.'}</span>}
      </div>
      <IconButton icon={Terminal} label={sql ? 'Browse tables' : 'Run SQL'} active={sql} onClick={() => setSql(!sql)} />
    </div>
    <ModalNotice message={schemaError} />
    {sql ? <SqlView key={contextLabel} contextLabel={contextLabel}
      execute={(statement, signal) => api.dbQuery(space, service, catalog, statement, signal, schema)} /> : (
      <SchemaView key={schema} space={space} service={service} catalog={catalog} schema={schema} />
    )}
  </>
}

function SchemaView({ space, service, catalog, schema }: { space: string; service: string; catalog: string; schema: string }) {
  const [tables, setTables] = useState<string[] | null>(schema ? null : [])
  const [current, setCurrent] = useState('')
  const [refresh, setRefresh] = useState(0)
  const [error, setError] = useState('')

  useEffect(() => {
    if (!catalog || !schema) return
    const controller = new AbortController()
    setError('')
    api.dbTables(space, service, catalog, schema, controller.signal).then((res) => {
      if (controller.signal.aborted) return
      const names = res.tables.map((table) => table.name)
      setTables(names)
      setCurrent((selected) => names.includes(selected) ? selected : '')
    }).catch((err) => { if (!controller.signal.aborted) setError(errText(err, 'Loading tables failed.')) })
    return () => controller.abort()
  }, [space, service, catalog, schema, refresh])

  return <>
    <ModalNotice message={error} />
    <SplitPane sidebar={<>
      <div className="flex h-12 shrink-0 items-center justify-between border-b border-line pl-3 pr-2">
        <Eyebrow>Tables</Eyebrow>
        <IconButton icon={RefreshCw} size="sm" label="Refresh tables" disabled={!schema} onClick={() => setRefresh((n) => n + 1)} />
      </div>
      <div className="min-h-0 flex-1 overflow-y-auto p-2">
        {tables === null && !error && <Spinner label="Loading…" />}
        {tables?.length === 0 && <p className="px-1 text-xs text-muted">No tables in this schema.</p>}
        {(tables ?? []).map((name) => <button type="button" key={name} onClick={() => setCurrent(name)}
          className={`block w-full truncate rounded-md px-2 py-1.5 text-left font-mono text-xs ${current === name ? 'bg-accent/10 text-accent' : 'text-ink-soft hover:bg-line/30'}`} title={name}>{name}</button>)}
      </div>
    </>}>
      {current ? <TablePreview key={current} space={space} service={service} catalog={catalog} schema={schema} table={current} /> : (
        <p className="px-5 py-12 text-center text-sm text-muted">Select a table to preview its rows, or open Run SQL.</p>
      )}
    </SplitPane>
  </>
}

function TablePreview({ space, service, catalog, schema, table }: {
  space: string; service: string; catalog: string; schema: string; table: string
}) {
  const [result, setResult] = useState<DbRowsPage | null>(null)
  const [refresh, setRefresh] = useState(0)
  const [error, setError] = useState('')
  useEffect(() => {
    const controller = new AbortController()
    setResult(null)
    setError('')
    api.dbRows(space, service, catalog, schema, table, 200, 0, undefined, controller.signal).then((res) => {
      if (!controller.signal.aborted) setResult(res)
    }).catch((err) => { if (!controller.signal.aborted) setError(errText(err, 'Loading rows failed.')) })
    return () => controller.abort()
  }, [space, service, catalog, schema, table, refresh])

  return <>
    <div className="flex h-12 shrink-0 items-center justify-between border-b border-line px-5">
      <span className="truncate font-mono text-xs text-ink-soft" title={`${catalog}.${schema}.${table}`}>{table}</span>
      <IconButton icon={RefreshCw} size="sm" label="Refresh rows" onClick={() => setRefresh((n) => n + 1)} />
    </div>
    <ModalNotice message={error} />
    <div className="min-h-0 flex-1 overflow-auto px-5 py-4">
      {!result && !error && <Spinner label="Loading rows…" />}
      {result && <>
        <p className="pb-2 text-xs text-muted">{result.hasMore ? `Showing the first ${result.rows.length} rows.` : `${result.rows.length} ${result.rows.length === 1 ? 'row' : 'rows'}.`} Read-only preview.</p>
        <SqlResultGrid columns={result.columns} rows={result.rows} />
      </>}
    </div>
  </>
}
