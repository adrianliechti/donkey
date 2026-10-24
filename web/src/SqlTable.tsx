import { useMemo, useRef, useState, type FormEvent, type KeyboardEvent, type ReactNode } from 'react'
import { useQuery } from '@tanstack/react-query'
import {
  Check,
  ChevronLeft,
  ChevronRight,
  ChevronsLeft,
  Download,
  Eye,
  ListFilter,
  PanelRight,
  Plus,
  RefreshCw,
  SquareTerminal,
  Table2,
  Trash2,
  X,
} from 'lucide-react'
import { saveBlob } from './api'
import { DataGrid, type GridCell, type GridColumn, type GridSort } from './DataGrid'
import { Modal, ModalBody, ModalFooter, ModalHeader } from './dialog'
import { SqlCellPanel } from './SqlCellPanel'
import {
  failure,
  sqlRowsQuery,
  sqlStructureQuery,
  useSqlDropTable,
  useSqlRowWrite,
  type SqlColumn,
  type SqlFilter,
  type SqlFilterOp,
  type SqlPage,
  type SqlPageView,
  type SqlPlace,
  type SqlStructure,
  type SqlTable,
} from './sqlApi'
import { isLongValue, isNumericType, plural, toCsv, type SqlCell } from './sqlText'
import { useServiceSearch } from './serviceSearch'
import { Picker, SearchRow, Sidebar } from './Workbench'
import { Button, Chip, CopyButton, ErrorNote, Eyebrow, IconButton, Spinner, Toolbar } from './ui'
import { errText, humanSize } from './format'

// The Data tabs' pieces, shared by the database and Trino consoles: the
// sidebar frame, the searchable table list, and the table itself — its rows
// (sorted, filtered, paged and, where the dialect allows, edited) and its
// structure. Which table, view, sort, page and filter are open lives in the
// URL: sort, dir, page, where/op/value, view.

const pageSize = 100

const compact = new Intl.NumberFormat(undefined, { notation: 'compact', maximumFractionDigits: 1 })

// ─── Sidebar ────────────────────────────────────────────────────────────

/** The workbench's left pane: pickers on top, a list below. */
export function SqlSidebar({ children }: { children: ReactNode }) {
  return <Sidebar label="Tables">{children}</Sidebar>
}

/** The sidebar's top bar: the pickers, and what can be done to what they pick. */
export function SidebarBar({ children, actions }: { children: ReactNode; actions?: ReactNode }) {
  return (
    <div className="flex min-h-11 shrink-0 items-center gap-0.5 border-b border-line pr-1.5 pl-1.5">
      <div className="flex min-w-0 flex-1 items-center gap-0.5">{children}</div>
      {actions && <Toolbar className="gap-0">{actions}</Toolbar>}
    </div>
  )
}

/**
 * The tables of a schema, searchable by name. Typing filters; Enter opens
 * the first match; the arrows walk the list.
 */
export function TableList({
  tables,
  error,
  selected,
  onSelect,
  onRefresh,
  refreshing = false,
  onCreate,
  empty,
}: {
  tables: SqlTable[] | null
  error: string
  selected?: string
  onSelect: (table: SqlTable) => void
  onRefresh: () => void
  refreshing?: boolean
  onCreate?: () => void
  empty: ReactNode
}) {
  const [find, setFind] = useState('')
  const list = useRef<HTMLUListElement>(null)
  const shown = useMemo(() => {
    const needle = find.trim().toLowerCase()
    return (tables ?? []).filter((t) => t.name.toLowerCase().includes(needle))
  }, [tables, find])

  const step = (e: KeyboardEvent, dir: 1 | -1) => {
    const buttons = [...(list.current?.querySelectorAll('button') ?? [])]
    const at = buttons.indexOf(document.activeElement as HTMLButtonElement)
    const next = buttons[at < 0 ? 0 : Math.max(0, Math.min(buttons.length - 1, at + dir))]
    if (next) {
      e.preventDefault()
      next.focus()
    }
  }

  return (
    <>
      {/* The list's head is its search: typing narrows it, the count and
          the actions sit at its end. */}
      <SearchRow
        line="top"
        value={find}
        onChange={setFind}
        placeholder="Find a table"
        onKeyDown={(e) => {
          if (e.key === 'Enter' && shown[0]) onSelect(shown[0])
          if (e.key === 'ArrowDown') step(e, 1)
          if (e.key === 'Escape') setFind('')
        }}
      >
        {tables && tables.length > 0 && !find && (
          <span className="px-1 text-[11px] text-muted tabular-nums">{tables.length}</span>
        )}
        {onCreate && <IconButton icon={Plus} size="sm" label="New table" onClick={onCreate} />}
        <IconButton
          icon={RefreshCw}
          size="sm"
          label="Refresh the tables"
          onClick={onRefresh}
          className={refreshing ? '[&>svg]:animate-spin' : ''}
        />
      </SearchRow>
      <ErrorNoteBox message={error} />
      {tables === null && !error && <Spinner label="Loading tables…" />}
      {tables !== null && tables.length === 0 && (
        <div className="px-3 pb-3 text-xs leading-relaxed text-muted">{empty}</div>
      )}
      {tables !== null && tables.length > 0 && shown.length === 0 && (
        <p className="px-3 pb-3 text-xs text-muted">No table matches “{find}”.</p>
      )}
      <ul
        ref={list}
        className="min-h-0 flex-1 overflow-y-auto px-1.5 pb-1.5"
        onKeyDown={(e) => {
          if (e.key === 'ArrowDown') step(e, 1)
          if (e.key === 'ArrowUp') step(e, -1)
        }}
      >
        {shown.map((t) => {
          const current = t.name === selected
          const Icon = t.kind === 'view' ? Eye : Table2
          return (
            <li key={`${t.schema}.${t.name}`}>
              <button
                type="button"
                onClick={() => onSelect(t)}
                aria-current={current || undefined}
                title={t.kind === 'view' ? `${t.name} (view)` : t.name}
                className={`flex h-7 w-full items-center gap-2 rounded-md px-1.5 text-left font-mono text-xs transition-colors focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-accent-strong ${
                  current ? 'bg-accent-soft text-ink' : 'text-ink-soft hover:bg-line/50 hover:text-ink'
                }`}
              >
                <Icon className={`size-3.5 shrink-0 ${current ? 'text-accent' : 'text-muted'}`} aria-hidden />
                <span className="min-w-0 flex-1 truncate">{t.name}</span>
                {t.rows !== null && (
                  <span
                    className="shrink-0 text-[10px] text-muted tabular-nums"
                    title={`about ${t.rows.toLocaleString()} rows`}
                  >
                    {compact.format(t.rows)}
                  </span>
                )}
              </button>
            </li>
          )
        })}
      </ul>
    </>
  )
}

/**
 * The SQL editor's place in the sidebar, between the pickers and the
 * tables: the editor opens in the main pane, where a table would.
 */
export function SqlEntry({ selected, onOpen }: { selected: boolean; onOpen: () => void }) {
  return (
    <div className="shrink-0 p-1.5">
      <button
        type="button"
        onClick={onOpen}
        aria-current={selected || undefined}
        className={`flex h-8 w-full items-center gap-2 rounded-md px-1.5 text-left text-sm transition-colors focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-accent-strong ${
          selected ? 'bg-accent-soft text-ink' : 'text-ink-soft hover:bg-line/50 hover:text-ink'
        }`}
      >
        <SquareTerminal className={`size-4 shrink-0 ${selected ? 'text-accent' : 'text-muted'}`} aria-hidden />
        SQL
      </button>
    </div>
  )
}

function ErrorNoteBox({ message }: { message: string }) {
  if (!message) return null
  return (
    <div className="px-2 pb-2">
      <ErrorNote message={message} />
    </div>
  )
}

// ─── The table ──────────────────────────────────────────────────────────

export type TrailStep = { label: string; onClick?: () => void }

/** The main pane's empty state, centred. */
export function PaneMessage({ children }: { children: ReactNode }) {
  return (
    <section className="flex min-h-0 min-w-0 flex-1 items-center justify-center p-8">
      <div className="max-w-md text-center text-sm leading-relaxed text-muted">{children}</div>
    </section>
  )
}

const ops: { value: SqlFilterOp; label: string }[] = [
  { value: 'eq', label: '=' },
  { value: 'ne', label: '≠' },
  { value: 'lt', label: '<' },
  { value: 'le', label: '≤' },
  { value: 'gt', label: '>' },
  { value: 'ge', label: '≥' },
  { value: 'contains', label: 'contains' },
  { value: 'starts', label: 'starts with' },
  { value: 'null', label: 'is null' },
  { value: 'notnull', label: 'is not null' },
]
const valueless = (op: SqlFilterOp) => op === 'null' || op === 'notnull'

/**
 * One table, in two views. Rows: a page at a time, sorted by a header
 * click, narrowed by a filter, cells edited in place where the table has a
 * primary key. Structure: its columns and indexes. `editable` is the
 * dialect's ability (Trino has none); whether this table can be edited is
 * the server's answer — a key, and not a view.
 */
export function SqlTablePane({
  place,
  table,
  trail,
  editable,
  filterable,
  views = { rows: 'Rows', structure: 'Structure' },
  defaultView = 'rows',
  onDropped,
  onQuery,
}: {
  place: SqlPlace
  table: SqlTable
  trail: TrailStep[]
  editable: boolean
  filterable: boolean
  views?: { rows: string; structure: string }
  defaultView?: 'rows' | 'structure'
  onDropped?: () => void
  /** Opens the SQL editor on a select of this table. */
  onQuery?: () => void
}) {
  const [search, move] = useServiceSearch()
  const view = (search.view as 'rows' | 'structure' | undefined) ?? defaultView
  const structure = useQuery(sqlStructureQuery(place, table.name))
  const [dropping, setDropping] = useState(false)

  const setView = (next: 'rows' | 'structure') => move({ ...search, view: next === defaultView ? '' : next })

  return (
    <section className="flex min-h-0 min-w-0 flex-1 flex-col overflow-hidden">
      <div className="flex min-h-11 shrink-0 flex-wrap items-center justify-between gap-x-3 gap-y-1 border-b border-line py-1 pr-1.5 pl-3">
        <div className="flex min-w-0 items-center gap-3">
          <Trail steps={[...trail, { label: table.name }]} />
          {table.kind === 'view' && <Chip>view</Chip>}
          <Segmented
            label="Show"
            value={view}
            onChange={setView}
            // The view a table opens on comes first.
            options={[
              { value: 'rows' as const, label: views.rows },
              { value: 'structure' as const, label: views.structure },
            ].sort((a) => (a.value === defaultView ? -1 : 1))}
          />
        </div>
        <Toolbar className="gap-0.5">
          {onQuery && <IconButton icon={SquareTerminal} size="sm" label="Query this table" onClick={onQuery} />}
          {editable && table.kind !== 'view' && (
            <IconButton
              icon={Trash2}
              size="sm"
              kind="danger"
              label={`Drop the table ${table.name}…`}
              onClick={() => setDropping(true)}
            />
          )}
        </Toolbar>
      </div>
      {view === 'structure' ? (
        <StructureView
          structure={structure.data}
          error={structure.error ? errText(structure.error, 'Reading the structure did not work.') : ''}
        />
      ) : (
        <RowsView
          key={`${place.db}.${place.schema}.${table.name}`}
          place={place}
          table={table}
          structure={structure.data}
          editable={editable && table.kind !== 'view'}
          filterable={filterable}
        />
      )}
      {dropping && (
        <DropTableDialog
          place={place}
          table={table}
          onClose={() => setDropping(false)}
          onDropped={() => {
            setDropping(false)
            onDropped?.()
          }}
        />
      )}
    </section>
  )
}

function Trail({ steps }: { steps: TrailStep[] }) {
  return (
    <nav aria-label="Table" className="flex min-w-0 items-center gap-0.5 font-mono text-xs">
      {steps.map((step, i) => {
        const last = i === steps.length - 1
        return (
          // On a phone the table alone names the place; the picker above has the rest.
          <span key={i} className={`items-center gap-0.5 ${last ? 'flex min-w-0' : 'hidden shrink-0 sm:flex'}`}>
            {i > 0 && <ChevronRight className="hidden size-3 shrink-0 text-muted sm:block" aria-hidden />}
            {step.onClick && !last ? (
              <button
                type="button"
                onClick={step.onClick}
                className="max-w-40 truncate rounded px-0.5 text-muted underline-offset-2 hover:text-ink hover:underline focus-visible:outline-2 focus-visible:outline-accent-strong"
              >
                {step.label}
              </button>
            ) : (
              <span
                aria-current={last ? 'page' : undefined}
                className={`truncate px-0.5 ${last ? 'text-sm font-medium text-ink' : 'text-muted'}`}
              >
                {step.label}
              </span>
            )}
          </span>
        )
      })}
    </nav>
  )
}

function Segmented<T extends string>({
  label,
  value,
  onChange,
  options,
}: {
  label: string
  value: T
  onChange: (value: T) => void
  options: { value: T; label: string }[]
}) {
  return (
    <div role="radiogroup" aria-label={label} className="flex shrink-0 rounded-md bg-raised p-0.5">
      {options.map((o) => (
        <button
          key={o.value}
          type="button"
          role="radio"
          aria-checked={value === o.value}
          onClick={() => onChange(o.value)}
          className={`h-6 rounded px-2 text-xs transition-colors focus-visible:outline-2 focus-visible:outline-accent-strong ${
            value === o.value ? 'bg-card font-medium text-ink shadow-sm' : 'text-muted hover:text-ink'
          }`}
        >
          {o.label}
        </button>
      ))}
    </div>
  )
}

function RowsView({
  place,
  table,
  structure,
  editable,
  filterable,
}: {
  place: SqlPlace
  table: SqlTable
  structure?: SqlStructure
  editable: boolean
  filterable: boolean
}) {
  const [search, move] = useServiceSearch()
  const pageNumber = Math.max(1, Math.floor(Number(search.page)) || 1)
  const filter: SqlFilter | undefined =
    filterable && search.where
      ? {
          column: search.where,
          op: (ops.some((o) => o.value === search.op) ? search.op : 'eq') as SqlFilterOp,
          value: search.value ?? '',
        }
      : undefined
  const sort: GridSort = search.sort ? { column: search.sort, desc: search.dir === 'desc' } : null
  // Rebuilt every render on purpose: query keys compare by value.
  const pageView: SqlPageView = {
    limit: pageSize,
    offset: (pageNumber - 1) * pageSize,
    sort: sort?.column,
    desc: sort?.desc,
    filter,
  }
  const rowsQ = useQuery(sqlRowsQuery(place, table.name, pageView))
  const page = rowsQ.data
  const write = useSqlRowWrite(place, table.name, pageView)
  const grid = useRef<HTMLDivElement>(null)

  // What is selected or being edited belongs to one page of one order:
  // another page starts with nothing selected.
  const viewKey = JSON.stringify(pageView)
  const [selection, setSelection] = useState<{ cell: GridCell | null; for: string }>({ cell: null, for: viewKey })
  const [edit, setEdit] = useState<{ cell: GridCell | null; for: string }>({ cell: null, for: viewKey })
  const selected = selection.for === viewKey ? selection.cell : null
  const editing = edit.for === viewKey ? edit.cell : null
  const select = (cell: GridCell | null) => setSelection({ cell, for: viewKey })
  const setEditing = (cell: GridCell | null) => setEdit({ cell, for: viewKey })
  const [inspect, setInspect] = useState(false)
  const [adding, setAdding] = useState(false)
  const [deleting, setDeleting] = useState<number | null>(null)

  const columns = useMemo<GridColumn[]>(() => {
    const known = new Map((structure?.columns ?? []).map((c) => [c.name, c]))
    return (page?.columns ?? []).map((name) => {
      const c = known.get(name)
      return {
        name,
        type: c?.type,
        key: page?.key.includes(name) || c?.key,
        numeric: isNumericType(c?.type),
        nullable: c?.nullable,
      }
    })
  }, [page?.columns, page?.key, structure])

  const canEdit = editable && (page?.key.length ?? 0) > 0
  const go = (next: Record<string, string | undefined>) => {
    const merged: Record<string, string> = { ...search }
    for (const [k, v] of Object.entries(next)) {
      if (v === undefined || v === '') delete merged[k]
      else merged[k] = v
    }
    move(merged)
  }
  const goPage = (n: number) => go({ page: n > 1 ? String(n) : undefined })

  const rowKey = (row: SqlCell[]): Record<string, string> => {
    const key: Record<string, string> = {}
    for (const k of page?.key ?? []) {
      const v = row[page!.columns.indexOf(k)]
      if (v !== null && v !== undefined) key[k] = v
    }
    return key
  }

  const commit = async (cell: GridCell, value: string | null): Promise<boolean> => {
    const row = page?.rows[cell.row]
    if (!page || !row) return false
    if (value === row[cell.col]) {
      setEditing(null)
      return true
    }
    try {
      await write.mutateAsync({
        method: 'PUT',
        change: { key: rowKey(row), values: { [page.columns[cell.col]]: value } },
        row: cell.row,
      })
      setEditing(null)
      return true
    } catch {
      return false
    }
  }

  const open = (cell: GridCell) => {
    const column = columns[cell.col]
    const value = page?.rows[cell.row]?.[cell.col] ?? null
    if (canEdit && !column?.key && !isLongValue(value)) {
      write.reset()
      setEditing(cell)
    } else setInspect(true)
  }

  const finishEdit = () => {
    setEditing(null)
    grid.current?.focus({ preventScroll: true })
  }

  const where = (row?: SqlCell[]) =>
    row && page && page.key.length > 0
      ? page.key.map((k) => `${k} = ${row[page.columns.indexOf(k)] ?? 'null'}`).join(', ')
      : selected
        ? `row ${page ? page.offset + selected.row + 1 : selected.row + 1}`
        : undefined

  const selectedRow = selected ? page?.rows[selected.row] : undefined
  const selectedColumn = selected ? columns[selected.col] : undefined

  return (
    <>
      {/* From the structure too: a filter the database refused must stay
          on screen to be put right. */}
      {filterable && (page || structure) && (
        <FilterBar
          columns={page?.columns ?? structure?.columns.map((c) => c.name) ?? []}
          active={filter}
          onApply={(f) =>
            go({ where: f?.column, op: f && f.op !== 'eq' ? f.op : undefined, value: f?.value, page: undefined })
          }
        />
      )}
      <div className="flex min-h-0 flex-1">
        <div className="flex min-h-0 min-w-0 flex-1 flex-col">
          {(rowsQ.error || (write.error && !inspect && deleting === null)) && (
            <div className="shrink-0 p-2 pb-0">
              <ErrorNote
                message={
                  rowsQ.error
                    ? errText(rowsQ.error, 'Loading the rows did not work.')
                    : failure(write, 'That change did not stick.')
                }
              />
            </div>
          )}
          {!page ? (
            !rowsQ.error && <Spinner label="Loading rows…" />
          ) : page.rows.length === 0 && !adding ? (
            <div className="p-6 text-center text-sm text-muted">
              {filter ? (
                <>
                  No rows match the filter.{' '}
                  <button
                    type="button"
                    onClick={() => go({ where: undefined, op: undefined, value: undefined, page: undefined })}
                    className="text-accent-strong hover:underline"
                  >
                    Clear it
                  </button>
                </>
              ) : pageNumber > 1 ? (
                <>
                  Nothing on this page.{' '}
                  <button type="button" onClick={() => goPage(1)} className="text-accent-strong hover:underline">
                    Back to the first
                  </button>
                </>
              ) : canEdit ? (
                <>
                  No rows yet.{' '}
                  <button type="button" onClick={() => setAdding(true)} className="text-accent-strong hover:underline">
                    Add the first one
                  </button>
                </>
              ) : (
                'No rows.'
              )}
            </div>
          ) : (
            <DataGrid
              gridRef={grid}
              label={`Rows of ${table.name}`}
              columns={columns}
              rows={page.rows}
              firstRow={page.offset + 1}
              sort={sort}
              onSort={(column) => {
                // asc → desc → the table's own order; a new order starts on
                // its first page.
                const next =
                  sort?.column !== column
                    ? { sort: column, dir: undefined }
                    : sort.desc
                      ? { sort: undefined, dir: undefined }
                      : { sort: column, dir: 'desc' }
                go({ ...next, page: undefined })
              }}
              selected={selected}
              onSelect={(cell) => {
                if (editing) setEditing(null)
                select(cell)
              }}
              onOpen={open}
              editing={editing}
              editor={
                editing && (
                  <CellInput
                    key={`${editing.row}:${editing.col}`}
                    initial={page.rows[editing.row]?.[editing.col] ?? null}
                    busy={write.isPending}
                    label={`New value of ${columns[editing.col]?.name}`}
                    onCommit={async (value) => {
                      if (await commit(editing, value)) finishEdit()
                    }}
                    onCancel={finishEdit}
                  />
                )
              }
              rowActions={
                canEdit
                  ? (row) => (
                      <button
                        type="button"
                        title="Delete this row…"
                        aria-label="Delete this row…"
                        onClick={() => setDeleting(row)}
                        className="flex size-6 items-center justify-center rounded text-muted opacity-0 group-hover/row:opacity-100 hover:bg-danger/10 hover:text-danger focus-visible:opacity-100 focus-visible:outline-2 focus-visible:outline-accent-strong"
                      >
                        <Trash2 className="size-3" aria-hidden />
                      </button>
                    )
                  : undefined
              }
              actionsKey={String(canEdit)}
              leadingRow={
                adding
                  ? {
                      cells: page.columns.map((c, i) => (
                        <NewValue
                          key={c}
                          name={c}
                          column={structure?.columns.find((s) => s.name === c)}
                          first={i === 0}
                          form="sql-new-row"
                          onCancel={() => setAdding(false)}
                        />
                      )),
                      actions: <NewRowActions busy={write.isPending} onCancel={() => setAdding(false)} />,
                    }
                  : undefined
              }
            />
          )}
          {adding && (
            // The new row's inputs live in grid cells; one form outside the
            // table gathers them (form="…"), so Enter in any of them saves.
            <form
              id="sql-new-row"
              hidden
              onSubmit={(e: FormEvent<HTMLFormElement>) => {
                e.preventDefault()
                const values: Record<string, string> = {}
                // Empty stays unsent, so the column's default does its job.
                for (const [k, v] of new FormData(e.currentTarget).entries())
                  if (typeof v === 'string' && v !== '') values[k] = v
                write.mutate({ method: 'POST', change: { values } }, { onSuccess: () => setAdding(false) })
              }}
            />
          )}
        </div>
        {inspect && (
          <SqlCellPanel
            column={selectedColumn}
            value={selectedRow ? (selectedRow[selected!.col] ?? null) : undefined}
            where={where(selectedRow)}
            editable={canEdit && !selectedColumn?.key}
            busy={write.isPending}
            error={failure(write, 'That change did not stick.')}
            onSave={(value) => (selected ? commit(selected, value) : Promise.resolve(false))}
            onClose={() => setInspect(false)}
          />
        )}
      </div>
      {page && (
        <Footer
          page={page}
          pageNumber={pageNumber}
          filtered={!!filter}
          canEdit={canEdit}
          kind={table.kind}
          fetching={rowsQ.isFetching}
          onPage={goPage}
          toolbar={
            <>
              {canEdit && (
                <IconButton icon={Plus} size="sm" label="Add a row" disabled={adding} onClick={() => setAdding(true)} />
              )}
              <IconButton
                icon={PanelRight}
                size="sm"
                active={inspect}
                label={inspect ? 'Close the value panel' : 'Show the whole value'}
                onClick={() => setInspect(!inspect)}
              />
              <IconButton
                icon={Download}
                size="sm"
                label="Download this page as CSV"
                disabled={page.rows.length === 0}
                onClick={() =>
                  saveBlob(
                    new Blob([toCsv(page.columns, page.rows)], { type: 'text/csv' }),
                    `${table.name}${pageNumber > 1 ? `-page${pageNumber}` : ''}.csv`,
                  )
                }
              />
              <IconButton
                icon={RefreshCw}
                size="sm"
                label="Refresh the rows"
                onClick={() => void rowsQ.refetch()}
                className={rowsQ.isFetching ? '[&>svg]:animate-spin' : ''}
              />
            </>
          }
        />
      )}
      {deleting !== null && page?.rows[deleting] && (
        <DeleteRowDialog
          where={where(page.rows[deleting]) ?? ''}
          table={table.name}
          busy={write.isPending}
          error={failure(write, 'Deleting the row did not work.')}
          onClose={() => {
            write.reset()
            setDeleting(null)
          }}
          onDelete={() =>
            write.mutate(
              { method: 'DELETE', change: { key: rowKey(page.rows[deleting]) } },
              {
                onSuccess: () => {
                  setDeleting(null)
                  select(null)
                },
              },
            )
          }
        />
      )}
    </>
  )
}

// The filter is a draft until applied; the applied one is the URL's, and a
// back step brings the draft along with it.
function FilterBar({
  columns,
  active,
  onApply,
}: {
  columns: string[]
  active?: SqlFilter
  onApply: (filter?: SqlFilter) => void
}) {
  const initial = (): SqlFilter => active ?? { column: columns[0] ?? '', op: 'eq', value: '' }
  const [draft, setDraft] = useState<SqlFilter>(initial)
  const activeKey = active ? `${active.column}\u0000${active.op}\u0000${active.value}` : ''
  const [shownFor, setShownFor] = useState(activeKey)
  if (shownFor !== activeKey) {
    setShownFor(activeKey)
    setDraft(initial())
  }
  const column = columns.includes(draft.column) ? draft.column : (columns[0] ?? '')

  return (
    <form
      onSubmit={(e) => {
        e.preventDefault()
        onApply({ ...draft, column, value: valueless(draft.op) ? '' : draft.value })
      }}
      className={`flex min-h-10 shrink-0 flex-wrap items-center gap-1.5 border-b border-line py-1.5 pr-1.5 pl-3 ${active ? 'bg-accent-soft/30' : ''}`}
    >
      <ListFilter className={`size-3.5 shrink-0 ${active ? 'text-accent' : 'text-muted'}`} aria-hidden />
      <Picker
        label="Filter column"
        value={column}
        onChange={(c) => setDraft({ ...draft, column: c })}
        options={columns.map((c) => ({ value: c, label: c }))}
        className="max-w-48"
      />
      <Picker label="Comparison" value={draft.op} onChange={(op) => setDraft({ ...draft, op })} options={ops} />
      {!valueless(draft.op) && (
        <input
          value={draft.value}
          onChange={(e) => setDraft({ ...draft, value: e.target.value })}
          placeholder="value"
          aria-label="Filter value"
          spellCheck={false}
          className="h-7 w-48 min-w-24 rounded-md bg-transparent px-2 font-mono text-sm text-ink placeholder:font-sans placeholder:text-muted hover:bg-line/40 focus:bg-line/40 focus:outline-none"
        />
      )}
      <Button size="sm" kind="quiet" type="submit">
        Filter
      </Button>
      {active && <IconButton icon={X} size="sm" label="Clear the filter" onClick={() => onApply(undefined)} />}
    </form>
  )
}

function Footer({
  page,
  pageNumber,
  filtered,
  canEdit,
  kind,
  fetching,
  onPage,
  toolbar,
}: {
  page: SqlPage
  pageNumber: number
  filtered: boolean
  canEdit: boolean
  kind?: SqlTable['kind']
  fetching: boolean
  onPage: (n: number) => void
  toolbar: ReactNode
}) {
  const pages = page.total !== null && page.count === 'exact' ? Math.max(1, Math.ceil(page.total / page.limit)) : null
  const hint = canEdit
    ? 'Double-click a cell to edit · ⌘C copies'
    : kind === 'view'
      ? 'A view — read-only'
      : page.key.length === 0 && page.total !== null
        ? 'Read-only: no primary key to find a row by'
        : '⌘C copies a cell'
  return (
    <div className="flex min-h-10 shrink-0 flex-wrap items-center justify-between gap-x-3 border-t border-line pr-1.5 pl-3">
      <p className="min-w-0 truncate text-xs text-muted">
        <span className={`text-ink-soft ${fetching ? 'opacity-60' : ''}`}>{countText(page, filtered)}</span>
        <span className="hidden sm:inline"> · {hint}</span>
      </p>
      <Toolbar className="gap-0.5">
        {toolbar}
        <span className="mx-1 h-4 w-px bg-line" aria-hidden />
        {pageNumber > 2 && <IconButton icon={ChevronsLeft} size="sm" label="First page" onClick={() => onPage(1)} />}
        <IconButton
          icon={ChevronLeft}
          size="sm"
          label="Previous page"
          disabled={pageNumber <= 1}
          onClick={() => onPage(pageNumber - 1)}
        />
        <span className="min-w-16 px-1 text-center text-[11px] text-muted tabular-nums">
          {pages ? `${pageNumber} / ${pages}` : `page ${pageNumber}`}
        </span>
        <IconButton
          icon={ChevronRight}
          size="sm"
          label="Next page"
          disabled={!page.hasMore}
          onClick={() => onPage(pageNumber + 1)}
        />
      </Toolbar>
    </div>
  )
}

function countText(page: SqlPage, filtered: boolean): string {
  const n = page.rows.length
  if (n === 0) return filtered ? 'No rows match' : 'No rows'
  const range = `${(page.offset + 1).toLocaleString()}–${(page.offset + n).toLocaleString()}`
  if (page.total === null) return `Rows ${range}`
  const total =
    page.count === 'estimated'
      ? `≈ ${page.total.toLocaleString()}`
      : page.count === 'atLeast'
        ? `${page.total.toLocaleString()}+`
        : page.total.toLocaleString()
  return `${range} of ${total} ${filtered ? 'matching ' : ''}${page.total === 1 && page.count === 'exact' ? 'row' : 'rows'}`
}

/**
 * A cell being edited: the value as text, Enter or leaving the cell saves,
 * Escape puts it back. Null stays null until something is typed.
 */
function CellInput({
  initial,
  busy,
  label,
  onCommit,
  onCancel,
}: {
  initial: SqlCell
  busy: boolean
  label: string
  onCommit: (value: string | null) => void | Promise<void>
  onCancel: () => void
}) {
  const [value, setValue] = useState(initial ?? '')
  const done = useRef(false)
  const finish = (save: boolean) => {
    if (done.current) return
    done.current = true
    if (!save || (initial === null && value === '') || value === initial) onCancel()
    else
      void Promise.resolve(onCommit(value)).finally(() => {
        done.current = false
      })
  }
  return (
    <input
      autoFocus
      value={value}
      disabled={busy}
      onChange={(e) => setValue(e.target.value)}
      onFocus={(e) => e.currentTarget.select()}
      onBlur={() => finish(true)}
      onKeyDown={(e) => {
        if (e.key === 'Enter') {
          e.preventDefault()
          finish(true)
        }
        if (e.key === 'Escape') {
          e.preventDefault()
          finish(false)
        }
      }}
      placeholder={initial === null ? 'null' : undefined}
      aria-label={label}
      className="h-7 w-full bg-transparent px-2 font-mono text-xs text-ink outline-none placeholder:text-muted/60 placeholder:italic"
    />
  )
}

function NewValue({
  name,
  column,
  first,
  form,
  onCancel,
}: {
  name: string
  column?: SqlColumn
  first: boolean
  form: string
  onCancel: () => void
}) {
  // What an empty field becomes: the column's default, null, or an error.
  const fallback = column?.extra || column?.default ? 'default' : column?.nullable === false ? 'required' : 'null'
  return (
    <input
      name={name}
      form={form}
      autoFocus={first}
      onKeyDown={(e) => {
        if (e.key === 'Escape') {
          e.preventDefault()
          onCancel()
        }
      }}
      placeholder={fallback}
      aria-label={`New ${name}`}
      title={column?.default ? `default: ${column.default}` : undefined}
      className="h-6 w-full border-b border-transparent bg-transparent font-mono text-xs text-ink outline-none placeholder:text-muted/60 placeholder:italic focus:border-accent-strong"
    />
  )
}

function NewRowActions({ busy, onCancel }: { busy: boolean; onCancel: () => void }) {
  return (
    <div className="flex">
      <button
        type="submit"
        form="sql-new-row"
        title="Save the row (Enter)"
        aria-label="Save the row"
        disabled={busy}
        className="flex size-6 items-center justify-center rounded text-success hover:bg-success/10 disabled:opacity-40"
      >
        <Check className="size-3.5" aria-hidden />
      </button>
      <button
        type="button"
        title="Discard (Esc)"
        aria-label="Discard the row"
        onClick={onCancel}
        className="flex size-6 items-center justify-center rounded text-muted hover:bg-line/50"
      >
        <X className="size-3.5" aria-hidden />
      </button>
    </div>
  )
}

function StructureView({ structure, error }: { structure?: SqlStructure; error: string }) {
  if (error) {
    return (
      <div className="p-3">
        <ErrorNote message={error} />
      </div>
    )
  }
  if (!structure) return <Spinner label="Reading the structure…" />
  // Trino says nothing about nullability or defaults; empty columns go.
  const nullable = structure.columns.some((c) => c.nullable !== null)
  const defaults = structure.columns.some((c) => c.default !== null)
  const extras = structure.columns.some((c) => c.extra)
  const comments = structure.columns.some((c) => c.comment)
  const facts = [
    structure.kind === 'view' ? 'A view' : null,
    plural(structure.columns.length, 'column'),
    structure.rows !== null ? `about ${plural(structure.rows, 'row')}` : null,
    structure.size !== null ? humanSize(structure.size) : null,
  ].filter(Boolean)

  return (
    <div className="min-h-0 flex-1 space-y-5 overflow-auto p-4">
      <p className="text-xs text-muted">{facts.join(' · ')}</p>
      <div className="overflow-x-auto rounded-lg border border-line">
        <table className="w-full border-separate border-spacing-0 text-left text-xs">
          <thead>
            <tr className="[&>th]:border-b [&>th]:border-line [&>th]:px-3 [&>th]:py-2 [&>th]:font-normal">
              <th>
                <Eyebrow>Column</Eyebrow>
              </th>
              <th>
                <Eyebrow>Type</Eyebrow>
              </th>
              {nullable && (
                <th>
                  <Eyebrow>Null</Eyebrow>
                </th>
              )}
              {defaults && (
                <th>
                  <Eyebrow>Default</Eyebrow>
                </th>
              )}
              {extras && (
                <th>
                  <Eyebrow>Fills</Eyebrow>
                </th>
              )}
              {comments && (
                <th>
                  <Eyebrow>Comment</Eyebrow>
                </th>
              )}
            </tr>
          </thead>
          <tbody>
            {structure.columns.map((c) => (
              <tr
                key={c.name}
                className="[&>td]:border-b [&>td]:border-line/60 [&>td]:px-3 [&>td]:py-1.5 last:[&>td]:border-b-0"
              >
                <td className="font-mono text-ink">
                  <span className="flex items-center gap-1.5">
                    {c.name}
                    {c.key && <Chip title="part of the primary key">key</Chip>}
                  </span>
                </td>
                <td className="font-mono text-ink-soft">{c.type}</td>
                {nullable && (
                  <td className="text-muted">{c.nullable === null ? '' : c.nullable ? 'null' : 'not null'}</td>
                )}
                {defaults && (
                  <td className="max-w-72 truncate font-mono text-ink-soft" title={c.default ?? undefined}>
                    {c.default}
                  </td>
                )}
                {extras && <td className="text-muted">{c.extra}</td>}
                {comments && <td className="text-muted">{c.comment}</td>}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {structure.indexes.length > 0 && (
        <div className="space-y-2">
          <Eyebrow>Indexes</Eyebrow>
          <ul className="divide-y divide-line/60 overflow-hidden rounded-lg border border-line">
            {structure.indexes.map((index) => (
              <li key={index.name} className="flex min-h-10 items-center gap-3 py-1.5 pr-1.5 pl-3">
                <span className="min-w-0 flex-1">
                  <span className="flex flex-wrap items-center gap-1.5">
                    <span className="font-mono text-xs text-ink">{index.name}</span>
                    {index.primary ? <Chip>primary</Chip> : index.unique && <Chip>unique</Chip>}
                    <span className="font-mono text-[11px] text-muted">({index.columns.join(', ')})</span>
                  </span>
                  {index.definition && (
                    <span className="mt-0.5 block truncate font-mono text-[10px] text-muted" title={index.definition}>
                      {index.definition}
                    </span>
                  )}
                </span>
                {index.definition && <CopyButton text={index.definition} label="Copy the definition" />}
              </li>
            ))}
          </ul>
        </div>
      )}
    </div>
  )
}

function DeleteRowDialog({
  where,
  table,
  busy,
  error,
  onClose,
  onDelete,
}: {
  where: string
  table: string
  busy: boolean
  error: string
  onClose: () => void
  onDelete: () => void
}) {
  return (
    <Modal onClose={busy ? () => {} : onClose} size="sm">
      <ModalHeader title="Delete this row?" onClose={onClose} closeDisabled={busy} />
      <ModalBody className="space-y-3">
        <p className="text-sm text-ink-soft">
          The row of <span className="font-mono text-ink">{table}</span> where{' '}
          <span className="font-mono text-ink">{where}</span> is deleted for good.
        </p>
        <ErrorNote message={error} />
      </ModalBody>
      <ModalFooter>
        <Button kind="quiet" onClick={onClose} disabled={busy}>
          Keep it
        </Button>
        <Button kind="danger" onClick={onDelete} disabled={busy}>
          {busy ? 'Deleting…' : 'Delete the row'}
        </Button>
      </ModalFooter>
    </Modal>
  )
}

function DropTableDialog({
  place,
  table,
  onClose,
  onDropped,
}: {
  place: SqlPlace
  table: SqlTable
  onClose: () => void
  onDropped: () => void
}) {
  const drop = useSqlDropTable(place)
  const busy = drop.isPending
  return (
    <Modal onClose={busy ? () => {} : onClose} size="sm">
      <ModalHeader
        title={
          <>
            Drop <span className="font-mono">{table.name}</span>?
          </>
        }
        onClose={onClose}
        closeDisabled={busy}
      />
      <ModalBody className="space-y-3">
        <p className="text-sm leading-relaxed text-ink-soft">
          The table and {table.rows !== null ? `its ${plural(table.rows, 'row')}` : 'its rows'} are gone for good.
          Anything that depends on it — a view, a foreign key — stops the drop: that error is better than a silent
          cascade.
        </p>
        <ErrorNote message={failure(drop, 'Dropping the table did not work.')} />
      </ModalBody>
      <ModalFooter>
        <Button kind="quiet" onClick={onClose} disabled={busy}>
          Keep it
        </Button>
        <Button kind="danger" onClick={() => drop.mutate(table.name, { onSuccess: onDropped })} disabled={busy}>
          {busy ? 'Dropping…' : 'Drop the table'}
        </Button>
      </ModalFooter>
    </Modal>
  )
}
