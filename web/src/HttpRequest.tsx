import { useRef, useState, type ClipboardEvent } from 'react'
import { Braces, CircleSlash, X } from 'lucide-react'
import { autoContentType, jsonProblem, prettyJSON, type BodyKind, type Draft, type Row } from './httpApi'
import { newId } from './httpStore'
import { Pane, PaneEmpty, PaneHeader, PaneTabs, Segmented } from './HttpWorkbench'
import { Button, IconButton } from './ui'

// The request pane of the HTTP console: its query parameters, headers and
// body, each a tab. The URL bar above it (HttpConsole) and the parameters
// table here are two views of the same query.

type RequestTab = 'params' | 'headers' | 'body'

export function RequestPane({ draft, onChange }: { draft: Draft; onChange: (draft: Draft) => void }) {
  const [tab, setTab] = useState<RequestTab>('params')
  const auto = autoContentType(draft)
  const count = (rows: Row[]) => rows.filter((r) => r.on && r.key).length
  return (
    <Pane label="Request">
      <PaneHeader flush>
        <PaneTabs
          label="Request"
          value={tab}
          onChange={setTab}
          tabs={[
            { id: 'params', label: 'Params', count: count(draft.params) },
            { id: 'headers', label: 'Headers', count: count(draft.headers) + (auto ? 1 : 0) },
            { id: 'body', label: 'Body', mark: draft.bodyKind !== 'none' },
          ]}
        />
      </PaneHeader>
      <div className="flex min-h-0 flex-1 flex-col overflow-y-auto">
        {tab === 'params' && (
          <RowsTable
            label="Query parameters"
            rows={draft.params}
            onChange={(params) => onChange({ ...draft, params })}
            keyPlaceholder="parameter"
            separator="="
            hint="Kept in step with the URL. Unticked parameters stay here and are not sent."
          />
        )}
        {tab === 'headers' && (
          <RowsTable
            label="Headers"
            rows={draft.headers}
            onChange={(headers) => onChange({ ...draft, headers })}
            keyPlaceholder="Header-Name"
            separator=":"
            names={headerNames}
            fixed={auto ? [{ key: 'Content-Type', value: auto, note: 'from the body' }] : []}
            hint="The service's credential is added by donkey's proxy — it never reaches this browser. Paste several lines of Name: value at once."
          />
        )}
        {tab === 'body' && <BodyEditor draft={draft} onChange={onChange} />}
      </div>
    </Pane>
  )
}

const headerNames = [
  'Accept',
  'Accept-Encoding',
  'Accept-Language',
  'Authorization',
  'Cache-Control',
  'Content-Type',
  'Cookie',
  'If-Match',
  'If-None-Match',
  'Idempotency-Key',
  'Origin',
  'Prefer',
  'User-Agent',
  'X-Request-Id',
]

/**
 * A key/value table: parameters, headers, form fields. A blank line at the
 * end takes the next entry; typing in it makes it a row of its own (under the
 * same React key, so the cursor stays where it is).
 */
function RowsTable({
  label,
  rows,
  onChange,
  keyPlaceholder,
  separator,
  names,
  fixed = [],
  hint,
}: {
  label: string
  rows: Row[]
  onChange: (rows: Row[]) => void
  keyPlaceholder: string
  /** How pasted lines split into key and value. */
  separator: ':' | '='
  names?: string[]
  fixed?: { key: string; value: string; note: string }[]
  hint?: string
}) {
  const [nextId, setNextId] = useState(newId)
  const listId = useRef(`names-${newId()}`).current
  const update = (id: string, patch: Partial<Row>) => {
    if (id === nextId) {
      onChange([...rows, { id, key: '', value: '', on: true, ...patch }])
      setNextId(newId())
    } else {
      onChange(rows.map((r) => (r.id === id ? { ...r, ...patch } : r)))
    }
  }
  // Several lines pasted into a key become several rows.
  const paste = (id: string, e: ClipboardEvent<HTMLInputElement>) => {
    const text = e.clipboardData.getData('text')
    if (!text.includes('\n')) return
    e.preventDefault()
    const pasted = text
      .split(/\r?\n/)
      .map((line) => line.trim())
      .filter(Boolean)
      .map((line): Row => {
        const at = separator === '=' ? line.search(/[=:]/) : line.indexOf(':')
        return at < 0
          ? { id: newId(), key: line, value: '', on: true }
          : { id: newId(), key: line.slice(0, at).trim(), value: line.slice(at + 1).trim(), on: true }
      })
    const at = rows.findIndex((r) => r.id === id)
    const blank = at >= 0 && !rows[at].key && !rows[at].value
    onChange(at < 0 ? [...rows, ...pasted] : [...rows.slice(0, at + (blank ? 0 : 1)), ...pasted, ...rows.slice(at + 1)])
  }

  const grid = 'grid grid-cols-[2rem_minmax(0,2fr)_minmax(0,3fr)_2rem] items-center'
  return (
    <div className="flex flex-col">
      <div role="table" aria-label={label} className="divide-y divide-line/60 border-b border-line/60">
        <div role="row" className={`${grid} h-8 bg-raised/40`}>
          <span role="columnheader">
            <span className="sr-only">Send</span>
          </span>
          <span role="columnheader" className="px-2 font-mono text-[10px] font-semibold tracking-[0.14em] text-muted uppercase">
            Key
          </span>
          <span role="columnheader" className="px-2 font-mono text-[10px] font-semibold tracking-[0.14em] text-muted uppercase">
            Value
          </span>
          <span />
        </div>
        {fixed.map((f) => (
          <div role="row" key={f.key} className={`${grid} h-8 text-muted`} title={`Set ${f.note}; add a ${f.key} header to override it.`}>
            <span className="flex justify-center">
              <input type="checkbox" checked disabled aria-label={`${f.key} is sent`} className="opacity-50" />
            </span>
            <span className="truncate px-2 font-mono text-xs">{f.key}</span>
            <span className="truncate px-2 font-mono text-xs">
              {f.value} <span className="font-sans italic">· {f.note}</span>
            </span>
            <span />
          </div>
        ))}
        {[...rows, { id: nextId, key: '', value: '', on: true }].map((row) => {
          const blank = row.id === nextId
          return (
            <div role="row" key={row.id} className={`${grid} h-8 ${row.on || blank ? '' : 'opacity-60'}`}>
              <span className="flex justify-center">
                {!blank && (
                  <input
                    type="checkbox"
                    checked={row.on}
                    onChange={(e) => update(row.id, { on: e.target.checked })}
                    aria-label={`Send ${row.key || 'this row'}`}
                  />
                )}
              </span>
              <input
                value={row.key}
                onChange={(e) => update(row.id, { key: e.target.value })}
                onPaste={(e) => paste(row.id, e)}
                placeholder={blank ? keyPlaceholder : ''}
                aria-label={`${label}: name`}
                list={names ? listId : undefined}
                spellCheck={false}
                autoCapitalize="none"
                autoComplete="off"
                className="h-8 min-w-0 bg-transparent px-2 font-mono text-xs text-ink outline-none placeholder:text-muted/60 focus:bg-accent-soft/30"
              />
              <input
                value={row.value}
                onChange={(e) => update(row.id, { value: e.target.value })}
                placeholder={blank ? 'value' : ''}
                aria-label={`${label}: value of ${row.key || 'new row'}`}
                spellCheck={false}
                autoCapitalize="none"
                autoComplete="off"
                className="h-8 min-w-0 bg-transparent px-2 font-mono text-xs text-ink outline-none placeholder:text-muted/60 focus:bg-accent-soft/30"
              />
              <span className="flex justify-center">
                {!blank && (
                  <IconButton
                    icon={X}
                    size="sm"
                    label={`Remove ${row.key || 'this row'}`}
                    onClick={() => onChange(rows.filter((r) => r.id !== row.id))}
                  />
                )}
              </span>
            </div>
          )
        })}
      </div>
      {names && (
        <datalist id={listId}>
          {names.map((n) => (
            <option key={n} value={n} />
          ))}
        </datalist>
      )}
      {hint && <p className="px-3 py-2.5 text-xs leading-relaxed text-muted">{hint}</p>}
    </div>
  )
}

const bodyKinds: { value: BodyKind; label: string }[] = [
  { value: 'none', label: 'None' },
  { value: 'json', label: 'JSON' },
  { value: 'text', label: 'Text' },
  { value: 'form', label: 'Form' },
]

function BodyEditor({ draft, onChange }: { draft: Draft; onChange: (draft: Draft) => void }) {
  const problem = draft.bodyKind === 'json' ? jsonProblem(draft.body) : ''
  const bodiless = draft.method === 'GET' || draft.method === 'HEAD'
  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <div className="flex shrink-0 items-center justify-between gap-2 px-3 py-2">
        <Segmented label="Body type" options={bodyKinds} value={draft.bodyKind} onChange={(bodyKind) => onChange({ ...draft, bodyKind })} />
        {draft.bodyKind === 'json' && (
          <Button
            kind="quiet"
            size="sm"
            icon={Braces}
            disabled={!draft.body.trim() || Boolean(problem)}
            onClick={() => onChange({ ...draft, body: prettyJSON(draft.body) })}
          >
            Format
          </Button>
        )}
      </div>
      {draft.bodyKind !== 'none' && bodiless && (
        <p className="px-3 pb-2 text-xs text-muted">{draft.method} requests rarely carry a body; many services ignore it.</p>
      )}
      {draft.bodyKind === 'none' && (
        <PaneEmpty icon={CircleSlash} title="No body">
          Pick a type to send one. JSON and form bodies set their Content-Type unless a header does.
        </PaneEmpty>
      )}
      {(draft.bodyKind === 'json' || draft.bodyKind === 'text') && (
        <>
          <textarea
            aria-label="Request body"
            value={draft.body}
            onChange={(e) => onChange({ ...draft, body: e.target.value })}
            placeholder={draft.bodyKind === 'json' ? '{\n  "name": "value"\n}' : 'Plain text'}
            spellCheck={false}
            autoCapitalize="none"
            className="min-h-24 flex-1 resize-none border-t border-line/60 bg-well p-3 font-mono text-xs leading-relaxed text-emphasis outline-none placeholder:text-muted/60 focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-accent-strong"
          />
          {draft.bodyKind === 'json' && draft.body.trim() && (
            <p
              role="status"
              className={`shrink-0 truncate border-t border-line/60 px-3 py-1.5 font-mono text-[11px] ${problem ? 'text-danger' : 'text-success'}`}
              title={problem}
            >
              {problem || 'Valid JSON'}
            </p>
          )}
        </>
      )}
      {draft.bodyKind === 'form' && (
        <RowsTable
          label="Form fields"
          rows={draft.form}
          onChange={(form) => onChange({ ...draft, form })}
          keyPlaceholder="field"
          separator="="
          hint="Sent URL-encoded (application/x-www-form-urlencoded)."
        />
      )}
    </div>
  )
}
