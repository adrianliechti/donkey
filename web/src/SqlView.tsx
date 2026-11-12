import { useEffect, useMemo, useRef, useState, type PointerEvent, type ReactNode } from 'react'
import { useMutation } from '@tanstack/react-query'
import { Menu, MenuButton, MenuItem, MenuItems } from '@headlessui/react'
import { Download, History, PanelRight, Play, Trash2, TriangleAlert } from 'lucide-react'
import { saveBlob, type DbQueryResult } from './api'
import { DataGrid, type GridCell, type GridColumn, type GridSort } from './DataGrid'
import { SqlCellPanel } from './SqlCellPanel'
import { SqlEditor } from './SqlEditor'
import { onlyReads, plural, statementAt, statements, toCsv, type SqlCell, type SqlDialect } from './sqlText'
import { useNow } from './hooks'
import { Button, Callout, ErrorNote, IconButton, Spinner, Toolbar } from './ui'
import { ago, errText } from './format'

// The SQL editor of the database and Trino consoles: the editor over the
// results, in the main pane beside the table list, with what ran lately a
// menu away. What is typed, the history and the editor's height stay in this
// browser (localStorage, per service) — conveniences, so every access may
// fail.

const historySize = 50
const maxRemembered = 20_000

type HistoryEntry = { sql: string; at: string; target: string; ok: boolean; rows?: number; ms?: number; error?: string }

function load<T>(key: string, fallback: T): T {
  try {
    const raw = localStorage.getItem(key)
    return raw === null ? fallback : (JSON.parse(raw) as T)
  } catch {
    return fallback
  }
}

function store(key: string, value: unknown) {
  try {
    localStorage.setItem(key, JSON.stringify(value))
  } catch {
    /* private mode, full, or blocked — the convenience just doesn't stick */
  }
}

type Outcome = { result: DbQueryResult; ms: number; count: number }

export function SqlWorkbench({
  storageKey,
  dialect,
  target,
  run,
  onWrote,
  seed,
}: {
  /** `${space}/${service}` — whose draft and history these are. */
  storageKey: string
  dialect: SqlDialect
  /** What a statement runs against, in words: "app · public". */
  target: string
  run: (statements: string[], signal: AbortSignal) => Promise<DbQueryResult>
  /** After a statement that may have changed something. */
  onWrote?: () => void
  /** Text handed over from elsewhere ("write it in SQL", "query this table"). */
  seed?: { sql: string; n: number }
}) {
  const draftKey = `donkey.sql.draft:${storageKey}`
  const historyKey = `donkey.sql.history:${storageKey}`
  const [text, setText] = useState(() => load(draftKey, ''))
  const [history, setHistory] = useState<HistoryEntry[]>(() => load(historyKey, []))
  const [height, setHeight] = useState(() => load('donkey.sql.editor-height', 220))
  const editor = useRef<HTMLTextAreaElement>(null)
  const pending = useRef<AbortController | null>(null)
  useEffect(() => () => pending.current?.abort(), [])

  // The draft is saved a moment after typing stops, not on every key.
  useEffect(() => {
    const timer = window.setTimeout(() => store(draftKey, text), 300)
    return () => window.clearTimeout(timer)
  }, [draftKey, text])

  // A seed wins over the stored draft, also on the tab's first showing.
  const [seeded, setSeeded] = useState<number>()
  if (seed && seed.n !== seeded) {
    setSeeded(seed.n)
    setText(seed.sql)
  }
  useEffect(() => {
    if (seed) editor.current?.focus()
  }, [seed])

  const remember = (entry: HistoryEntry) => {
    // A pasted dump would crowd out the rest of this browser's storage.
    if (entry.sql.length > maxRemembered) return
    setHistory((current) => {
      const next = [entry, ...current.filter((e) => e.sql !== entry.sql)].slice(0, historySize)
      store(historyKey, next)
      return next
    })
  }

  const execution = useMutation({
    mutationFn: async ({ list, signal }: { list: string[]; signal: AbortSignal }): Promise<Outcome> => {
      const started = performance.now()
      if (dialect === 'trino' && list.length > 1) {
        throw new Error('The Trino console supports one statement per run. Select a statement to run it.')
      }
      const result = await run(list, signal)
      return { result, ms: performance.now() - started, count: list.length }
    },
  })

  const start = (list: string[]) => {
    if (list.length === 0 || execution.isPending) return
    const controller = new AbortController()
    pending.current = controller
    const sql = list.join(';\n')
    const began = performance.now()
    execution.mutate(
      { list, signal: controller.signal },
      {
        onSuccess: ({ result, ms }) =>
          remember({
            sql,
            at: new Date().toISOString(),
            target,
            ok: true,
            rows: result.rows.length,
            ms: Math.round(ms),
          }),
        onError: (err) =>
          !controller.signal.aborted &&
          remember({
            sql,
            at: new Date().toISOString(),
            target,
            ok: false,
            ms: Math.round(performance.now() - began),
            error: errText(err, 'failed'),
          }),
        onSettled: () => {
          pending.current = null
          if (list.some((s) => !onlyReads(s))) onWrote?.()
        },
      },
    )
  }

  // ⌘↵ runs the selection, or everything; ⇧⌘↵ the statement at the cursor,
  // which is then selected — what ran is what is highlighted.
  const runFrom = (which: 'selection' | 'statement', el: HTMLTextAreaElement | null) => {
    if (!el) return start(statements(text, dialect))
    const { selectionStart: from, selectionEnd: to } = el
    if (which === 'statement') {
      const at = statementAt(text, from, dialect)
      if (!at) return
      el.setSelectionRange(at.start, at.end)
      return start([text.slice(at.start, at.end)])
    }
    start(statements(from === to ? text : text.slice(from, to), dialect))
  }

  const resize = useRef<{ y: number; height: number } | null>(null)
  const onResizeMove = (e: PointerEvent<HTMLDivElement>) => {
    if (!resize.current) return
    setHeight(Math.round(Math.min(Math.max(resize.current.height + e.clientY - resize.current.y, 96), 720)))
  }

  const cancelled = execution.isError && execution.variables?.signal.aborted

  return (
    <section className="flex min-h-0 min-w-0 flex-1 flex-col overflow-hidden">
      <div className="flex h-11 shrink-0 items-center justify-between gap-3 border-b border-line pr-2 pl-3">
        <p className="min-w-0 truncate text-xs text-muted">
          Runs against <span className="font-mono text-ink-soft">{target}</span>
          {dialect !== 'trino' && ' · Transactions must finish in the same run'}
        </p>
        <Toolbar className="gap-2">
          <span className="hidden text-[11px] text-muted lg:inline">
            <Kbd>⌘↵</Kbd> selection or all · <Kbd>⇧⌘↵</Kbd> statement at cursor
          </span>
          <HistoryMenu
            entries={history}
            onPick={(entry) => {
              setText(entry.sql)
              editor.current?.focus()
            }}
            onForget={() => {
              setHistory([])
              store(historyKey, [])
            }}
          />
          {execution.isPending && (
            <Button kind="quiet" size="sm" onClick={() => pending.current?.abort()}>
              Cancel
            </Button>
          )}
          <Button
            size="sm"
            icon={Play}
            disabled={execution.isPending || !text.trim()}
            onClick={() => runFrom('selection', editor.current)}
            title="Run the selection, or everything (⌘↵)"
          >
            {execution.isPending ? 'Running…' : 'Run'}
          </Button>
        </Toolbar>
      </div>

      <div className="flex shrink-0 flex-col" style={{ height }}>
        <SqlEditor
          textareaRef={editor}
          value={text}
          onChange={setText}
          onRun={runFrom}
          label={`SQL against ${target}`}
          placeholder={placeholders[dialect]}
        />
      </div>
      <div
        role="separator"
        aria-orientation="horizontal"
        aria-label="Resize the editor"
        onPointerDown={(e) => {
          e.preventDefault()
          e.currentTarget.setPointerCapture(e.pointerId)
          resize.current = { y: e.clientY, height }
        }}
        onPointerMove={onResizeMove}
        onPointerUp={() => {
          resize.current = null
          store('donkey.sql.editor-height', height)
        }}
        className="h-1.5 shrink-0 cursor-row-resize border-y border-line bg-raised/60 transition-colors hover:bg-accent/30"
      />

      {execution.isPending ? (
        <Running onCancel={() => pending.current?.abort()} />
      ) : cancelled ? (
        <Idle>Cancelled — nothing came back.</Idle>
      ) : execution.isError ? (
        <div className="min-h-0 flex-1 overflow-auto p-3">
          <ErrorNote message={errText(execution.error, 'The statement failed.')} />
        </div>
      ) : execution.data ? (
        <Results
          key={execution.submittedAt}
          outcome={execution.data}
          fileName={`${storageKey.split('/').pop()}-query.csv`}
        />
      ) : (
        <Idle>
          Results show up here. <Kbd>⌘↵</Kbd> runs the selection or everything, one statement at a time; <Kbd>⇧⌘↵</Kbd>{' '}
          just the statement at the cursor.
        </Idle>
      )}
    </section>
  )
}

const placeholders: Record<SqlDialect, string> = {
  postgres: 'select * from …',
  mysql: 'select * from …',
  trino: 'select * from … limit 100',
}

function Kbd({ children }: { children: ReactNode }) {
  return <kbd className="rounded bg-raised px-1 font-mono text-[10px] text-ink-soft">{children}</kbd>
}

function Idle({ children }: { children: ReactNode }) {
  return <p className="min-h-0 flex-1 p-4 text-xs leading-relaxed text-muted">{children}</p>
}

function Running({ onCancel }: { onCancel: () => void }) {
  const [started] = useState(() => Date.now())
  const now = useNow(100)
  return (
    <div className="flex min-h-0 flex-1 flex-col items-center justify-center gap-3 p-6">
      <Spinner inline label={`Running… ${((now - started) / 1000).toFixed(1)}s`} />
      <Button kind="quiet" size="sm" onClick={onCancel}>
        Cancel
      </Button>
    </div>
  )
}

// What ran lately, a menu away: picking one puts it back in the editor.
function HistoryMenu({
  entries,
  onPick,
  onForget,
}: {
  entries: HistoryEntry[]
  onPick: (entry: HistoryEntry) => void
  onForget: () => void
}) {
  useNow(30_000)
  return (
    <Menu>
      <MenuButton
        title="Recent statements"
        aria-label="Recent statements"
        className="flex size-7 items-center justify-center rounded-md text-muted transition-colors hover:bg-line/60 hover:text-ink focus-visible:outline-2 focus-visible:outline-accent-strong data-open:bg-line/60 data-open:text-ink"
      >
        <History className="size-4" aria-hidden />
      </MenuButton>
      <MenuItems
        anchor="bottom end"
        className="z-50 w-[28rem] overflow-hidden rounded-lg border border-line bg-card shadow-xl [--anchor-gap:6px] focus:outline-none"
      >
        {entries.length === 0 ? (
          <p className="px-3 py-3 text-xs leading-relaxed text-muted">
            What you run shows up here — the last {historySize}, in this browser.
          </p>
        ) : (
          <>
            <div className="max-h-96 overflow-y-auto py-1">
              {entries.map((entry) => (
                <MenuItem key={`${entry.at}:${entry.sql}`}>
                  <button
                    type="button"
                    onClick={() => onPick(entry)}
                    title={entry.error ?? entry.sql}
                    className="block w-full px-3 py-1.5 text-left data-focus:bg-accent-soft"
                  >
                    <code className="line-clamp-2 font-mono text-[11px] leading-snug break-all text-ink-soft">
                      {entry.sql}
                    </code>
                    <span className="mt-0.5 flex items-center gap-1.5 text-[10px] text-muted">
                      <span>{ago(entry.at)} ago</span>
                      {entry.ok ? (
                        entry.rows !== undefined && <span>{plural(entry.rows, 'row')}</span>
                      ) : (
                        <span className="text-danger">failed</span>
                      )}
                      <span className="min-w-0 truncate font-mono">{entry.target}</span>
                    </span>
                  </button>
                </MenuItem>
              ))}
            </div>
            <div className="border-t border-line p-1">
              <MenuItem>
                <button
                  type="button"
                  onClick={onForget}
                  className="flex w-full items-center gap-2 rounded-md px-2 py-1.5 text-left text-xs text-muted data-focus:bg-danger/10 data-focus:text-danger"
                >
                  <Trash2 className="size-3.5" aria-hidden /> Forget the history
                </button>
              </MenuItem>
            </div>
          </>
        )}
      </MenuItems>
    </Menu>
  )
}

// A result's numbers line up on the right — guessed from the values, since
// a query's column types do not travel with it.
const numberLike = /^-?\d+(\.\d+)?(e[+-]?\d+)?$/i

function resultColumns(result: DbQueryResult): GridColumn[] {
  return result.columns.map((name, i) => {
    const sample = result.rows
      .slice(0, 100)
      .map((r) => r[i])
      .filter((v): v is string => v !== null)
    return { name, numeric: sample.length > 0 && sample.every((v) => numberLike.test(v)) }
  })
}

function compare(a: SqlCell, b: SqlCell): number {
  if (a === b) return 0
  if (a === null) return 1
  if (b === null) return -1
  if (numberLike.test(a) && numberLike.test(b)) return Number(a) - Number(b)
  return a.localeCompare(b)
}

/** A finished statement: the grid with its own sort, or what it did. */
function Results({ outcome, fileName }: { outcome: Outcome; fileName: string }) {
  const { result, ms, count } = outcome
  const columns = useMemo(() => resultColumns(result), [result])
  const [sort, setSort] = useState<GridSort>(null)
  const [selected, setSelected] = useState<GridCell | null>(null)
  const [inspect, setInspect] = useState(false)
  // Sorting here reorders what came back; the database is not asked again.
  const rows = useMemo(() => {
    const at = sort ? result.columns.indexOf(sort.column) : -1
    if (at < 0) return result.rows
    const sorted = [...result.rows].sort((a, b) => compare(a[at], b[at]))
    return sort?.desc ? sorted.reverse() : sorted
  }, [result, sort])

  const took = ms < 1000 ? `${Math.round(ms)} ms` : `${(ms / 1000).toFixed(1)} s`
  const statementsNote = count > 1 ? ` · ${count} statements, the last one's result` : ''

  if (result.columns.length === 0) {
    return (
      <Idle>
        <span className="text-ink-soft">
          Done — {plural(result.affected, 'row')} affected · {took}
          {statementsNote}.
        </span>
      </Idle>
    )
  }

  const cell = selected ? rows[selected.row]?.[selected.col] : undefined
  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <div className="flex h-10 shrink-0 items-center justify-between gap-3 border-b border-line pr-1.5 pl-3">
        <p className="min-w-0 truncate text-xs text-muted">
          <span className="text-ink-soft">{plural(rows.length, 'row')}</span> · {took}
          {statementsNote}
        </p>
        <Toolbar>
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
            label="Download the result as CSV"
            onClick={() => saveBlob(new Blob([toCsv(result.columns, rows)], { type: 'text/csv' }), fileName)}
          />
        </Toolbar>
      </div>
      {result.truncated && (
        <Callout tone="warning" icon={TriangleAlert} className="m-2 mb-0 py-2 text-xs">
          Only the first {plural(rows.length, 'row')} came back — add a <code className="font-mono">limit</code>, or
          narrow the query, to see the ones you are after.
        </Callout>
      )}
      <div className="flex min-h-0 flex-1">
        {rows.length === 0 ? (
          <p className="flex-1 p-4 text-xs text-muted">No rows.</p>
        ) : (
          <DataGrid
            label="Query result"
            columns={columns}
            rows={rows}
            sort={sort}
            onSort={(column) =>
              setSort((current) =>
                current?.column !== column ? { column, desc: false } : current.desc ? null : { column, desc: true },
              )
            }
            selected={selected}
            onSelect={setSelected}
            onOpen={() => setInspect(true)}
          />
        )}
        {inspect && (
          <SqlCellPanel
            column={selected ? columns[selected.col] : undefined}
            value={cell}
            where={selected ? `row ${selected.row + 1}` : undefined}
            editable={false}
            onClose={() => setInspect(false)}
          />
        )}
      </div>
    </div>
  )
}
