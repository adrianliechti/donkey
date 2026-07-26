import { flexRender, type Table } from '@tanstack/react-table'
import { ChevronDown, ChevronUp } from 'lucide-react'
import type { ReactNode } from 'react'

/**
 * The console's one grid: a TanStack Table instance rendered in donkey's
 * clothes. The call site owns the table (columns, sorting mode, editing);
 * this owns nothing but markup — sticky header, sort chevrons, and an
 * optional row pinned above the data (the "add row" form).
 */
export function DataGrid<T>({ table, leadingRow }: { table: Table<T>; leadingRow?: ReactNode }) {
  return (
    <table className="w-full border-separate border-spacing-0 font-mono text-xs">
      <thead className="sticky top-0 z-10">
        {table.getHeaderGroups().map((hg) => (
          <tr key={hg.id}>
            {hg.headers.map((header) => {
              const dir = header.column.getIsSorted()
              return (
                <th
                  key={header.id}
                  className="border-b border-line bg-card px-2 py-1.5 text-left font-semibold text-stone"
                >
                  {header.column.getCanSort() ? (
                    <button
                      type="button"
                      onClick={header.column.getToggleSortingHandler()}
                      title={`Sort by ${header.column.id}`}
                      className="inline-flex items-center gap-1 hover:text-ink focus-visible:outline-2 focus-visible:outline-hay-deep"
                    >
                      {flexRender(header.column.columnDef.header, header.getContext())}
                      {dir === 'asc' && <ChevronUp className="size-3" aria-hidden />}
                      {dir === 'desc' && <ChevronDown className="size-3" aria-hidden />}
                    </button>
                  ) : (
                    flexRender(header.column.columnDef.header, header.getContext())
                  )}
                </th>
              )
            })}
          </tr>
        ))}
      </thead>
      <tbody>
        {leadingRow}
        {table.getRowModel().rows.map((row) => (
          <tr key={row.id} className="group/row hover:bg-line/20">
            {row.getVisibleCells().map((cell) => (
              <td key={cell.id} className="max-w-64 truncate border-b border-line/50 px-2 py-1 text-ink-soft">
                {flexRender(cell.column.columnDef.cell, cell.getContext())}
              </td>
            ))}
          </tr>
        ))}
      </tbody>
    </table>
  )
}
