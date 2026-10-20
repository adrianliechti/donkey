import { useEffect, useRef, useState } from 'react'
import { getCoreRowModel, getSortedRowModel, useReactTable, type ColumnDef, type SortingState } from '@tanstack/react-table'
import type { DbCell, DbQueryResult } from './api'
import { DataGrid } from './DataGrid'
import { Button, ErrorNote, TextArea, errText } from './ui'

/** One statement, run as written; shared by the database and Trino browsers. */
export function SqlView({ contextLabel, execute, initial = '', onChanged }: {
  contextLabel: string
  execute: (sql: string, signal: AbortSignal) => Promise<DbQueryResult>
  initial?: string
  onChanged?: () => void
}) {
  const [sql, setSql] = useState(initial)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [result, setResult] = useState<DbQueryResult | null>(null)
  const pending = useRef<AbortController | null>(null)
  useEffect(() => () => pending.current?.abort(), [])

  async function runSql() {
    if (!sql.trim() || pending.current) return
    const controller = new AbortController()
    pending.current = controller
    setBusy(true)
    setError('')
    setResult(null)
    try {
      const response = await execute(sql, controller.signal)
      if (!controller.signal.aborted) {
        setResult(response)
        onChanged?.()
      }
    } catch (err) {
      if (!controller.signal.aborted) setError(errText(err, 'The statement failed.'))
    } finally {
      pending.current = null
      setBusy(false)
    }
  }

  return (
    <div className="flex min-h-0 flex-1 flex-col gap-3 px-5 py-4">
      <TextArea size="sm" value={sql} onChange={(e) => setSql(e.target.value)}
        onKeyDown={(e) => { if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') { e.preventDefault(); void runSql() } }}
        placeholder={`select * from … — runs against ${contextLabel}`} aria-label="SQL statement"
        spellCheck={false} className="h-32 shrink-0 resize-y" />
      <div className="flex items-center justify-between gap-2">
        <p className="text-xs text-muted">Runs exactly what you wrote, against {contextLabel} — ⌘↵ works too.</p>
        <div className="flex gap-2">
          {busy && <Button onClick={() => pending.current?.abort()}>Cancel</Button>}
          <Button onClick={runSql} disabled={busy || !sql.trim()}>{busy ? 'Running…' : 'Run'}</Button>
        </div>
      </div>
      <ErrorNote message={error} />
      {result && <div className="min-h-0 flex-1 overflow-auto">
        {result.columns.length === 0 ? (
          <p className="text-sm text-ink-soft">Done — {result.affected} {result.affected === 1 ? 'row' : 'rows'} affected.</p>
        ) : <>
          <p className="pb-1 text-xs text-muted">{result.truncated ? `Showing the first ${result.rows.length} rows.` : `${result.rows.length} ${result.rows.length === 1 ? 'row' : 'rows'}.`}</p>
          <SqlResultGrid columns={result.columns} rows={result.rows} />
        </>}
      </div>}
    </div>
  )
}

/** The index identifies a column: SELECT may return repeated column names. */
export function SqlResultGrid({ columns, rows }: { columns: string[]; rows: DbCell[][] }) {
  const [sorting, setSorting] = useState<SortingState>([])
  const defs: ColumnDef<DbCell[]>[] = columns.map((name, i) => ({
    id: String(i), accessorFn: (row) => row[i] ?? '', header: name,
    cell: ({ row }) => row.original[i] === null ? <span className="italic text-muted">null</span> : row.original[i],
  }))
  const table = useReactTable({ data: rows, columns: defs, state: { sorting }, onSortingChange: setSorting,
    getCoreRowModel: getCoreRowModel(), getSortedRowModel: getSortedRowModel() })
  return <DataGrid table={table} />
}
