import {
  memo,
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
  type KeyboardEvent,
  type PointerEvent,
  type ReactNode,
  type RefObject,
} from 'react'
import { ArrowDown, ArrowUp, KeyRound } from 'lucide-react'
import { columnWidth, type SqlCell } from './sqlText'

/**
 * The SQL consoles' one grid — a table's page, a query's result. It behaves
 * like a spreadsheet: click a cell to select it, the arrows move, Enter (or
 * a double click) opens it, ⌘C copies it. What "open" means is the caller's
 * business — edit in place, or show the value in the side panel. Columns
 * start as wide as their content and are dragged wider at the header's edge.
 */

export type GridColumn = { name: string; type?: string; key?: boolean; numeric?: boolean; nullable?: boolean | null }

export type GridCell = { row: number; col: number }

/** A sort is by column name: the grid's own and the server's agree on it. */
export type GridSort = { column: string; desc: boolean } | null

export function DataGrid({
  label,
  columns,
  rows,
  firstRow = 1,
  sort = null,
  onSort,
  selected,
  onSelect,
  onOpen,
  editing = null,
  editor,
  rowActions,
  actionsKey,
  leadingRow,
  gridRef,
}: {
  /** The grid's accessible name: "Rows of users", "Query result". */
  label: string
  columns: GridColumn[]
  rows: SqlCell[][]
  /** The number the first row shows in the gutter (paging adds the offset). */
  firstRow?: number
  sort?: GridSort
  /** A header click; the caller decides what the next order is. */
  onSort?: (column: string) => void
  selected: GridCell | null
  onSelect: (cell: GridCell | null) => void
  onOpen?: (cell: GridCell) => void
  /** The cell being edited in place, and what to show in it. */
  editing?: GridCell | null
  editor?: ReactNode
  /** A row's trailing actions (delete). Re-rendered when actionsKey changes. */
  rowActions?: (row: number) => ReactNode
  actionsKey?: string
  /** A row pinned above the data — the new-row form: one cell per column. */
  leadingRow?: { cells: ReactNode[]; actions: ReactNode }
  /** The grid's element, for handing the keyboard back after an edit. */
  gridRef?: RefObject<HTMLDivElement | null>
}) {
  const root = useRef<HTMLDivElement | null>(null)
  const attach = (el: HTMLDivElement | null) => {
    root.current = el
    if (gridRef) gridRef.current = el
  }
  const [widths, setWidths] = useState<number[]>(() => fitWidths(columns, rows))
  const [copied, setCopied] = useState<GridCell | null>(null)
  // A different set of columns (a refreshed result, an altered table)
  // starts from fitted widths again; the same columns keep their drag.
  const shape = columns.map((c) => c.name).join('\u0000')
  const [fittedFor, setFittedFor] = useState(shape)
  if (fittedFor !== shape) {
    setFittedFor(shape)
    setWidths(fitWidths(columns, rows))
  }

  // Rows call their event handlers through here, so a memoized row that
  // skipped a render still reaches the parent's latest closures.
  const handlers = useRef({ onSelect, onOpen })
  useLayoutEffect(() => {
    handlers.current = { onSelect, onOpen }
  })

  useEffect(() => {
    if (!selected) return
    root.current
      ?.querySelector(`[data-cell="${selected.row}:${selected.col}"]`)
      ?.scrollIntoView({ block: 'nearest', inline: 'nearest' })
  }, [selected])

  const copy = (cell: GridCell) => {
    const value = rows[cell.row]?.[cell.col]
    if (value === undefined) return
    void navigator.clipboard?.writeText(value ?? '').then(() => {
      setCopied(cell)
      window.setTimeout(() => setCopied(null), 700)
    })
  }

  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    // Keys typed into the editor (or the new-row form) are theirs.
    if (e.target !== root.current) return
    if (!selected) {
      if (rows.length > 0 && e.key.startsWith('Arrow')) {
        e.preventDefault()
        onSelect({ row: 0, col: 0 })
      }
      return
    }
    const move = (row: number, col: number) => {
      e.preventDefault()
      onSelect({ row: clamp(row, 0, rows.length - 1), col: clamp(col, 0, columns.length - 1) })
    }
    if (e.key === 'ArrowDown') move(selected.row + 1, selected.col)
    else if (e.key === 'ArrowUp') move(selected.row - 1, selected.col)
    else if (e.key === 'ArrowRight') move(selected.row, selected.col + 1)
    else if (e.key === 'ArrowLeft') move(selected.row, selected.col - 1)
    else if (e.key === 'Enter' && onOpen) {
      e.preventDefault()
      onOpen(selected)
    } else if (e.key === 'Escape') {
      e.preventDefault()
      onSelect(null)
    } else if ((e.metaKey || e.ctrlKey) && e.key === 'c' && !window.getSelection()?.toString()) {
      e.preventDefault()
      copy(selected)
    }
  }

  const typed = columns.some((c) => c.type)
  const total = widths.reduce((sum, w) => sum + w, 0) + gutter + (rowActions || leadingRow ? actionsWidth : 0)

  return (
    <div
      ref={attach}
      tabIndex={0}
      role="grid"
      aria-label={label}
      aria-rowcount={rows.length}
      onKeyDown={onKeyDown}
      className="relative min-h-0 flex-1 overflow-auto outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-accent-strong/60"
    >
      <table
        className="table-fixed border-separate border-spacing-0 font-mono text-xs [font-variant-ligatures:none]"
        style={{ width: total }}
      >
        <colgroup>
          <col style={{ width: gutter }} />
          {columns.map((c, i) => (
            <col key={`${c.name}:${i}`} style={{ width: widths[i] ?? 160 }} />
          ))}
          {(rowActions || leadingRow) && <col style={{ width: actionsWidth }} />}
        </colgroup>
        <thead className="sticky top-0 z-20">
          <tr>
            <th className="sticky left-0 z-10 border-r border-b border-line bg-card" aria-hidden />
            {columns.map((c, i) => (
              <HeaderCell
                key={`${c.name}:${i}`}
                column={c}
                typed={typed}
                sort={sort?.column === c.name ? (sort.desc ? 'desc' : 'asc') : undefined}
                onSort={onSort && (() => onSort(c.name))}
                onResize={(w) => setWidths((current) => current.map((x, j) => (j === i ? w : x)))}
                onFit={() =>
                  setWidths((current) =>
                    current.map((x, j) =>
                      j === i
                        ? columnWidth(
                            c.name,
                            c.type,
                            rows.map((r) => r[i]),
                          )
                        : x,
                    ),
                  )
                }
                width={widths[i] ?? 160}
              />
            ))}
            {(rowActions || leadingRow) && <th className="border-b border-line bg-card" aria-hidden />}
          </tr>
        </thead>
        <tbody>
          {leadingRow && (
            <tr className="bg-accent-soft/40">
              <td className="sticky left-0 z-[1] border-r border-b border-line bg-accent-soft px-2 text-right text-[10px] text-accent">
                new
              </td>
              {leadingRow.cells.map((content, i) => (
                <td key={i} className="border-b border-line/60 px-2 py-0.5">
                  {content}
                </td>
              ))}
              <td className="border-b border-line/60 px-1">{leadingRow.actions}</td>
            </tr>
          )}
          {rows.map((row, i) => (
            <GridRow
              key={i}
              index={i}
              number={firstRow + i}
              row={row}
              columns={columns}
              selectedCol={selected?.row === i ? selected.col : -1}
              copiedCol={copied?.row === i ? copied.col : -1}
              editingCol={editing?.row === i ? editing.col : -1}
              editor={editing?.row === i ? editor : undefined}
              hasActions={!!rowActions || !!leadingRow}
              rowActions={rowActions}
              actionsKey={actionsKey}
              handlers={handlers}
              focus={() => root.current?.focus({ preventScroll: true })}
            />
          ))}
        </tbody>
      </table>
    </div>
  )
}

const gutter = 44
const actionsWidth = 40

function clamp(n: number, lo: number, hi: number) {
  return Math.max(lo, Math.min(hi, n))
}

function fitWidths(columns: GridColumn[], rows: SqlCell[][]) {
  return columns.map((c, i) =>
    columnWidth(
      c.name,
      c.type,
      rows.map((r) => r[i]),
    ),
  )
}

function HeaderCell({
  column,
  typed,
  sort,
  onSort,
  onResize,
  onFit,
  width,
}: {
  column: GridColumn
  typed: boolean
  sort?: 'asc' | 'desc'
  onSort?: () => void
  onResize: (width: number) => void
  onFit: () => void
  width: number
}) {
  const drag = useRef<{ x: number; width: number } | null>(null)
  const label = (
    <span className="flex min-w-0 flex-col items-start">
      <span className={`flex w-full min-w-0 items-center gap-1 ${column.numeric ? 'justify-end' : ''}`}>
        {column.key && <KeyRound className="size-3 shrink-0 text-accent" aria-label="primary key" />}
        <span className="truncate font-medium text-ink">{column.name}</span>
        {sort === 'asc' && <ArrowUp className="size-3 shrink-0 text-accent" aria-hidden />}
        {sort === 'desc' && <ArrowDown className="size-3 shrink-0 text-accent" aria-hidden />}
      </span>
      {typed && (
        <span className={`w-full truncate text-[10px] font-normal text-muted ${column.numeric ? 'text-right' : ''}`}>
          {column.type || ' '}
        </span>
      )}
    </span>
  )
  return (
    <th
      aria-sort={sort === 'asc' ? 'ascending' : sort === 'desc' ? 'descending' : undefined}
      className="group/th relative border-b border-line bg-card p-0 text-left align-bottom"
    >
      {onSort ? (
        <button
          type="button"
          onClick={onSort}
          title={`Sort by ${column.name}${column.type ? ` (${column.type})` : ''}`}
          className="flex w-full px-2 py-1.5 text-left hover:bg-line/30 focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-accent-strong"
        >
          {label}
        </button>
      ) : (
        <div className="px-2 py-1.5" title={column.type}>
          {label}
        </div>
      )}
      {/* The column's edge: drag to resize, double-click to fit. */}
      <span
        role="separator"
        aria-orientation="vertical"
        aria-label={`Resize ${column.name}`}
        onPointerDown={(e: PointerEvent<HTMLSpanElement>) => {
          e.preventDefault()
          e.currentTarget.setPointerCapture(e.pointerId)
          drag.current = { x: e.clientX, width }
        }}
        onPointerMove={(e) => {
          if (drag.current) onResize(clamp(drag.current.width + e.clientX - drag.current.x, 48, 960))
        }}
        onPointerUp={() => {
          drag.current = null
        }}
        onDoubleClick={onFit}
        className="absolute inset-y-0 -right-1 z-10 w-2 cursor-col-resize after:absolute after:inset-y-1 after:left-1/2 after:w-px after:bg-line-strong after:opacity-0 group-hover/th:after:opacity-100 hover:after:bg-accent hover:after:opacity-100"
      />
    </th>
  )
}

type RowProps = {
  index: number
  number: number
  row: SqlCell[]
  columns: GridColumn[]
  selectedCol: number
  copiedCol: number
  editingCol: number
  editor?: ReactNode
  hasActions: boolean
  /** Not compared: a row re-renders its actions when actionsKey changes. */
  rowActions?: (row: number) => ReactNode
  actionsKey?: string
  handlers: { current: { onSelect: (cell: GridCell | null) => void; onOpen?: (cell: GridCell) => void } }
  focus: () => void
}

// Moving the selection re-renders the two rows it left and entered, not
// five hundred: a row only changes with its own data and its own cells'
// states.
const GridRow = memo(
  function GridRow({
    index,
    number,
    row,
    columns,
    selectedCol,
    copiedCol,
    editingCol,
    editor,
    hasActions,
    rowActions,
    handlers,
    focus,
  }: RowProps) {
    return (
      <tr className="group/row">
        <td className="sticky left-0 z-[1] border-r border-b border-line/60 bg-card px-2 text-right text-[10px] text-muted tabular-nums group-hover/row:bg-raised">
          {number}
        </td>
        {columns.map((c, col) => {
          const value = row[col] ?? null
          const isSelected = selectedCol === col
          return (
            <td
              key={col}
              data-cell={`${index}:${col}`}
              role="gridcell"
              aria-selected={isSelected}
              onMouseDown={(e) => {
                if (editingCol === col) return
                e.preventDefault()
                focus()
                handlers.current.onSelect({ row: index, col })
              }}
              onDoubleClick={() => handlers.current.onOpen?.({ row: index, col })}
              title={value !== null && value.length > 24 ? value.slice(0, 1000) : undefined}
              style={{ scrollMarginTop: 48, scrollMarginLeft: gutter }}
              className={`h-7 cursor-default truncate border-b border-line/60 px-2 group-hover/row:bg-raised/60 ${
                c.numeric ? 'text-right' : ''
              } ${
                editingCol === col
                  ? 'bg-surface p-0 outline-2 -outline-offset-2 outline-accent-strong'
                  : isSelected
                    ? `outline-2 -outline-offset-2 outline-accent-strong ${copiedCol === col ? 'bg-success/15' : 'bg-accent-soft/50'}`
                    : ''
              }`}
            >
              {editingCol === col ? editor : <CellText value={value} />}
            </td>
          )
        })}
        {hasActions && (
          <td className="border-b border-line/60 px-1 group-hover/row:bg-raised/60">{rowActions?.(index)}</td>
        )}
      </tr>
    )
  },
  (a, b) =>
    a.row === b.row &&
    a.index === b.index &&
    a.number === b.number &&
    a.columns === b.columns &&
    a.selectedCol === b.selectedCol &&
    a.copiedCol === b.copiedCol &&
    a.editingCol === b.editingCol &&
    a.editor === b.editor &&
    a.hasActions === b.hasActions &&
    a.actionsKey === b.actionsKey,
)

/** A value as the grid shows it: null is not an empty string. */
export function CellText({ value }: { value: SqlCell }) {
  if (value === null) return <span className="text-muted/70 italic">null</span>
  return <span className="text-ink-soft">{value}</span>
}
