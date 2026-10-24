import { useState } from 'react'
import { Braces, PanelRightClose, Pencil } from 'lucide-react'
import type { GridColumn } from './DataGrid'
import { jsonOf, plural, type SqlCell } from './sqlText'
import { Button, CopyButton, ErrorNote, IconButton, TextArea, Toolbar } from './ui'

/**
 * The whole of one value, beside the grid: long text wrapped, JSON laid
 * out, and — where the row can be changed — a roomy editor for what does
 * not fit in a cell. It follows the grid's selection.
 */
export function SqlCellPanel({
  column,
  value,
  where,
  editable,
  busy = false,
  error = '',
  onSave,
  onClose,
}: {
  column?: GridColumn
  value?: SqlCell
  /** Which row this is, in words: "id = 42", "row 12". */
  where?: string
  editable: boolean
  busy?: boolean
  error?: string
  /** Null sets the column to NULL. Resolves true once it is stored. */
  onSave?: (value: string | null) => Promise<boolean>
  onClose: () => void
}) {
  const [draft, setDraft] = useState<string | null>(null)
  const [raw, setRaw] = useState(false)
  // A different cell drops an unsaved draft rather than carrying it over.
  const [shownFor, setShownFor] = useState({ column, where })
  if (shownFor.column !== column || shownFor.where !== where) {
    setShownFor({ column, where })
    setDraft(null)
  }

  const json = value === undefined ? undefined : jsonOf(value)
  const pretty = json !== undefined && !raw ? JSON.stringify(json, null, 2) : (value ?? '')
  // json and jsonb are re-encoded by the database anyway, so their editor
  // opens laid out; any other column keeps its text byte for byte.
  const structured = /^jsonb?$/i.test(column?.type ?? '')

  const save = async (next: string | null) => {
    if (onSave && (await onSave(next))) setDraft(null)
  }

  return (
    <aside className="flex w-80 shrink-0 flex-col border-l border-line bg-card" aria-label="Value">
      <div className="flex h-11 shrink-0 items-center gap-2 border-b border-line pr-1.5 pl-3">
        <div className="min-w-0 flex-1">
          <p className="truncate font-mono text-xs font-medium text-ink">{column?.name ?? 'Value'}</p>
          {(column?.type || where) && (
            <p className="truncate font-mono text-[10px] text-muted">
              {[column?.type, where].filter(Boolean).join(' · ')}
            </p>
          )}
        </div>
        <Toolbar>
          {json !== undefined && draft === null && (
            <IconButton
              icon={Braces}
              size="sm"
              active={!raw}
              label={raw ? 'Lay out the JSON' : 'Show the text as stored'}
              onClick={() => setRaw(!raw)}
            />
          )}
          {value !== undefined && value !== null && <CopyButton text={value} label="Copy the value" />}
          <IconButton icon={PanelRightClose} size="sm" label="Close the value panel" onClick={onClose} />
        </Toolbar>
      </div>

      {value === undefined ? (
        <p className="p-4 text-sm text-muted">Select a cell to see its whole value.</p>
      ) : draft !== null ? (
        <div className="flex min-h-0 flex-1 flex-col gap-2 p-3">
          <TextArea
            size="sm"
            autoFocus
            value={draft}
            onChange={(e) => setDraft(e.target.value)}
            onKeyDown={(e) => {
              if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') {
                e.preventDefault()
                void save(draft)
              }
              if (e.key === 'Escape') {
                e.preventDefault()
                setDraft(null)
              }
            }}
            spellCheck={false}
            aria-label={`New value of ${column?.name ?? 'the cell'}`}
            className="min-h-0 flex-1 resize-none whitespace-pre-wrap break-all"
          />
          <ErrorNote message={error} />
          <div className="flex items-center justify-between gap-2">
            <Button kind="quiet" size="sm" disabled={busy || column?.nullable === false} onClick={() => void save(null)}>
              Set to null
            </Button>
            <div className="flex gap-1.5">
              <Button kind="quiet" size="sm" disabled={busy} onClick={() => setDraft(null)}>
                Cancel
              </Button>
              <Button size="sm" disabled={busy} onClick={() => void save(draft)} title="Save (⌘↵)">
                {busy ? 'Saving…' : 'Save'}
              </Button>
            </div>
          </div>
        </div>
      ) : (
        <>
          <div className="min-h-0 flex-1 overflow-auto p-3">
            {value === null ? (
              <p className="font-mono text-xs text-muted italic">null</p>
            ) : value === '' ? (
              <p className="text-xs text-muted">An empty string.</p>
            ) : (
              <pre className="rounded-md bg-well p-3 font-mono text-xs leading-relaxed break-all whitespace-pre-wrap text-emphasis">
                {pretty}
              </pre>
            )}
          </div>
          <div className="flex min-h-11 shrink-0 items-center justify-between gap-2 border-t border-line px-3 py-1.5">
            <span className="text-[11px] text-muted">
              {value === null ? 'null' : `${plural(value.length, 'character')}${json !== undefined ? ' · JSON' : ''}`}
            </span>
            {editable && onSave && (
              <Button
                kind="quiet"
                size="sm"
                icon={Pencil}
                onClick={() => setDraft(value !== null && structured && json !== undefined ? JSON.stringify(json, null, 2) : (value ?? ''))}
              >
                Edit
              </Button>
            )}
          </div>
        </>
      )}
    </aside>
  )
}
